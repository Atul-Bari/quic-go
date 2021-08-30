package http3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/quicvarint"
	"github.com/marten-seemann/qpack"
)

// A MessageStream is a QUIC stream for processing HTTP/3 request and response messages.
type MessageStream interface {
	Stream() quic.Stream

	// TODO: integrate QPACK encoding and decoding with dynamic tables.

	// ReadHeaders reads the next HEADERS frame, used for HTTP request and
	// response headers and trailers. An interim response (status 100-199)
	// must be followed by one or more additional HEADERS frames.
	// If ReadHeaders encounters a DATA frame or an otherwise unhandled frame,
	// it will return a FrameTypeError.
	ReadHeaders() ([]qpack.HeaderField, error)

	// WriteHeaders writes a single HEADERS frame, used for HTTP request and
	// response headers and trailers.  It returns any errors that may occur,
	// including QPACK encoding or writes to the underlying quic.Stream.
	// WriteHeaders shoud not be called simultaneously with Write, ReadFrom, or
	// writes to the underlying quic.Stream.
	WriteHeaders([]qpack.HeaderField) error

	// Read reads DATA frames from he underlying quic.Stream.
	// If Read encounters a HEADERS frame or an otherwise unhandled frame,
	// it will return a FrameTypeError.
	Read([]byte) (int, error)

	// Write writes 0 or more DATA frames.
	// Used for writing an HTTP request or response body.
	// Should not be called concurrently with WriteFields or ReadFrom.
	Write([]byte) (int, error)

	// ReadFrom implements io.ReaderFrom. It reads data from an io.Reader
	// and writes DATA frames to the underlying quic.Stream.
	ReadFrom(io.Reader) (int64, error)

	// Close closes the MessageStream.
	Close() error

	// WebTransport returns a WebTransport interface, if supported.
	// TODO: should this method live here?
	WebTransport() (WebTransport, error)
}

type messageStream struct {
	conn *connection
	str  quic.Stream

	fr *FrameReader
	w  quicvarint.Writer

	messages chan *incomingMessage
	readErr  error

	// Used to synchronize reading DATA frames, used for HTTP message bodies
	dataReady chan struct{}
	dataRead  chan struct{}

	bodyReaderClosed chan struct{}
	readDone         chan struct{}
}

var (
	_ MessageStream = &messageStream{}
	_ io.Reader     = &messageStream{}
	_ io.Writer     = &messageStream{}
	_ io.ReaderFrom = &messageStream{}
	_ io.Closer     = &messageStream{}
)

// newMessageStream creates a new MessageStream.
// If a frame has already been partially consumed from str, t specifies
// the frame type and n the number of bytes remaining in the frame payload.
func newMessageStream(conn *connection, str quic.Stream, t FrameType, n int64) MessageStream {
	s := &messageStream{
		conn:             conn,
		str:              str,
		fr:               &FrameReader{R: str, Type: t, N: n},
		w:                quicvarint.NewWriter(str),
		messages:         make(chan *incomingMessage),
		dataReady:        make(chan struct{}),
		dataRead:         make(chan struct{}),
		bodyReaderClosed: make(chan struct{}),
		readDone:         make(chan struct{}),
	}
	return s
}

func (s *messageStream) Stream() quic.Stream {
	return s.str
}

// ReadHeaders reads the next HEADERS frame, used for HTTP request and
// response headers and trailers. An interim response (status 100-199)
// must be followed by one or more additional HEADERS frames.
// If ReadHeaders encounters a DATA frame or an otherwise unhandled frame,
// it will return a FrameTypeError.
func (s *messageStream) ReadHeaders() ([]qpack.HeaderField, error) {
	err := s.nextHeadersFrame()
	if err != nil {
		return nil, err
	}

	max := s.conn.maxHeaderBytes()
	if s.fr.N > int64(max) {
		return nil, &streamError{Code: errorFrameError, Err: &FrameLengthError{Type: s.fr.Type, Len: uint64(s.fr.N), Max: max}}
	}

	p := make([]byte, s.fr.N)
	_, err = io.ReadFull(s.fr, p)
	if err != nil {
		return nil, &streamError{Code: errorRequestIncomplete, Err: err}
	}

	dec := qpack.NewDecoder(nil)
	fields, err := dec.DecodeFull(p)
	if err != nil {
		return nil, &connError{Code: errorGeneralProtocolError, Err: err}
	}

	return fields, nil
}

// WriteHeaders writes a single QPACK-encoded HEADERS frame to s.
// It returns an error if the estimated size of the frame exceeds the peer’s
// MAX_FIELD_SECTION_SIZE. Headers are not modified or validated.
// It is the responsibility of the caller to ensure the fields are valid.
// It should not be called concurrently with Write or ReadFrom.
func (s *messageStream) WriteHeaders(fields []qpack.HeaderField) error {
	var l uint64
	for i := range fields {
		// https://quicwg.org/base-drafts/draft-ietf-quic-qpack.html#name-dynamic-table-size
		l += uint64(len(fields[i].Name) + len(fields[i].Value) + 32)
	}
	max := s.conn.peerMaxHeaderBytes()
	if l > max {
		return fmt.Errorf("HEADERS frame too large: %d bytes (max: %d)", l, max)
	}

	buf := &bytes.Buffer{}
	encoder := qpack.NewEncoder(buf)
	for i := range fields {
		encoder.WriteField(fields[i])
	}

	quicvarint.Write(s.w, uint64(FrameTypeHeaders))
	quicvarint.Write(s.w, uint64(buf.Len()))
	_, err := s.w.Write(buf.Bytes())
	return err
}

func (s *messageStream) Read(p []byte) (n int, err error) {
	for len(p) > 0 {
		for s.fr.N <= 0 {
			err = s.nextDataFrame()
			if err != nil {
				return n, err
			}
		}
		pp := p
		if s.fr.N < int64(len(p)) {
			pp = p[:s.fr.N]
		}
		x, err := s.fr.Read(pp)
		n += x
		p = p[x:]
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

const bodyCopyBufferSize = 8 * 1024

// Write writes bytes to DATA frames to the underlying quic.Stream.
func (s *messageStream) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		pp := p
		if len(p) > bodyCopyBufferSize {
			pp = p[:bodyCopyBufferSize]
		}
		x, err := s.writeDataFrame(pp)
		p = p[x:]
		n += x
		if err != nil {
			return n, err
		}
	}
	return n, err
}

// ReadFrom implements io.ReaderFrom. It reads from r until an error
// or io.EOF and writes DATA frames to the underlying quic.Stream.
func (s *messageStream) ReadFrom(r io.Reader) (n int64, err error) {
	buf := make([]byte, bodyCopyBufferSize)
	for {
		l, rerr := r.Read(buf)
		if l == 0 {
			if rerr == nil {
				continue
			} else if rerr == io.EOF {
				return n, nil
			}
			return n, rerr
		}
		x, err := s.writeDataFrame(buf[:l])
		n += int64(x)
		if err != nil {
			return n, err
		}
		if rerr == io.EOF {
			return n, nil
		}
	}
}

func (s *messageStream) AcceptDatagramContext(ctx context.Context) (DatagramContext, error) {
	return nil, errors.New("TODO: not supported yet")
}

func (s *messageStream) RegisterDatagramContext() (DatagramContext, error) {
	return nil, errors.New("TODO: not supported yet")
}

func (s *messageStream) DatagramNoContext() (DatagramContext, error) {
	return nil, errors.New("TODO: not supported yet")
}

func (s *messageStream) WebTransport() (WebTransport, error) {
	return newWebTransportSession(s.conn, s.str), nil
}

func (s *messageStream) Close() error {
	s.conn.cleanup(s.str.StreamID())
	// s.stream.CancelRead(quic.StreamErrorCode(errorNoError))
	return s.str.Close()
}

// nextHeadersFrame reads incoming HTTP/3 frames until it finds
// the next HEADERS frame. If it encouters a DATA frame prior to
// reading a HEADERS frame, it will return a frameTypeError.
func (s *messageStream) nextHeadersFrame() error {
	err := s.readFrames()
	if err != nil {
		return err
	}
	if s.fr.Type != FrameTypeHeaders {
		return &FrameTypeError{Want: FrameTypeHeaders, Type: s.fr.Type}
	}
	return nil
}

// nextDataFrame reads incoming HTTP/3 frames until it finds
// the next DATA frame. If it encouters a HEADERS frame prior to
// reading a DATA frame, it will return a frameTypeError.
func (s *messageStream) nextDataFrame() error {
	err := s.readFrames()
	if err != nil {
		return err
	}
	if s.fr.Type != FrameTypeData {
		return &FrameTypeError{Want: FrameTypeData, Type: s.fr.Type}
	}
	return nil
}

func (s *messageStream) readFrames() error {
	for {
		// Next discards any unread frame payload bytes
		err := s.fr.Next()
		if err != nil {
			return err
		}
		switch s.fr.Type {
		case FrameTypeHeaders, FrameTypeData:
			return nil
		case FrameTypePushPromise:
			// TODO: handle HTTP/3 pushes
		}
	}
}

func (s *messageStream) readBody(p []byte) (n int, err error) {
	select {
	// Get DATA frame from parseIncomingFrames loop
	case <-s.dataReady:
	case <-s.bodyReaderClosed:
		return 0, errAlreadyClosed
	case <-s.readDone:
		return 0, s.readErr
	}
	if s.fr.N < int64(len(p)) {
		n, err = s.fr.Read(p[:s.fr.N])
	} else {
		n, err = s.fr.Read(p)
	}
	// Hand control back to parseIncomingFrames loop
	s.dataRead <- struct{}{}
	return n, err
}

func (s *messageStream) closeBody() error {
	select {
	case <-s.bodyReaderClosed:
		return errAlreadyClosed
	default:
	}
	close(s.bodyReaderClosed)
	return nil
}

var errAlreadyClosed = errors.New("already closed")

func (s *messageStream) writeDataFrame(p []byte) (n int, err error) {
	quicvarint.Write(s.w, uint64(FrameTypeData))
	quicvarint.Write(s.w, uint64(len(p)))
	n, err = s.w.Write(p)
	return
}
