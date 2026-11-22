// Package memconn provides an in-memory net.Conn pair that supports
// independent half-close (CloseWrite) and peer reset (Reset) without OS
// sockets.
//
// Every blocking wait uses bubble-local channels so testing/synctest treats
// blocked Reads and Writes as durably blocked, allowing drain tests to run on
// real time.Timer and RelayDrainIdle inside a synctest bubble.
//
// The package is for tests only: nothing outside a _test.go file imports it.
package memconn

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// ErrReset is the read error delivered to the peer after Reset is called.
var ErrReset = errors.New("read: connection reset by peer")

var (
	_ net.Conn                        = (*Conn)(nil)
	_ interface{ CloseWrite() error } = (*Conn)(nil)
)

// stream is one unidirectional byte pipe of an in-memory net.Conn pair.
// Every blocking wait uses bubble-local channels so testing/synctest treats
// blocked Reads and Writes as durably blocked.
type stream struct {
	data   chan []byte
	closed chan struct{}

	mu           sync.Mutex
	isClosed     bool
	readerClosed bool
	reset        bool
	closeErr     error
	rem          []byte
}

func newStream() *stream {
	return &stream{
		data:   make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (s *stream) write(p []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, net.ErrClosed
	default:
	}
	if len(p) == 0 {
		return 0, nil
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case <-s.closed:
		return 0, net.ErrClosed
	case s.data <- cp:
		return len(p), nil
	}
}

func (s *stream) read(p []byte) (int, error) {
	if err := s.abortErr(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	if len(s.rem) > 0 {
		n := copy(p, s.rem)
		s.rem = s.rem[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	// Drain already-buffered bytes before reporting a clean EOF so data
	// written prior to CloseWrite is always delivered first.
	select {
	case chunk := <-s.data:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		return s.consume(p, chunk), nil
	default:
	}

	select {
	case chunk := <-s.data:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		return s.consume(p, chunk), nil
	case <-s.closed:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		select {
		case chunk := <-s.data:
			return s.consume(p, chunk), nil
		default:
			s.mu.Lock()
			err := s.closeErr
			s.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
	}
}

func (s *stream) consume(p, chunk []byte) int {
	n := copy(p, chunk)
	if n < len(chunk) {
		s.mu.Lock()
		s.rem = append(s.rem[:0], chunk[n:]...)
		s.mu.Unlock()
	}
	return n
}

func (s *stream) abortErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readerClosed {
		return net.ErrClosed
	}
	if s.reset {
		return s.closeErr
	}
	return nil
}

func (s *stream) closeWrite() {
	s.mu.Lock()
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = io.EOF
		close(s.closed)
	}
	s.mu.Unlock()
}

func (s *stream) resetWrite(err error) {
	s.mu.Lock()
	s.reset = true
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = err
		close(s.closed)
	} else if !s.readerClosed {
		s.closeErr = err
	}
	s.mu.Unlock()
}

func (s *stream) closeRead() {
	s.mu.Lock()
	s.readerClosed = true
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = net.ErrClosed
		close(s.closed)
	}
	s.mu.Unlock()
}

// Conn is an in-memory net.Conn that supports independent half-close
// (CloseWrite) and peer reset (Reset) without OS sockets.
type Conn struct {
	rd *stream
	wr *stream
}

// Pair returns two connected in-memory endpoints.
func Pair() (*Conn, *Conn) {
	aToB := newStream()
	bToA := newStream()
	return &Conn{rd: bToA, wr: aToB}, &Conn{rd: aToB, wr: bToA}
}

// Read reads from the peer's write stream.
func (c *Conn) Read(p []byte) (int, error) { return c.rd.read(p) }

// Write writes to the peer's read stream.
func (c *Conn) Write(p []byte) (int, error) { return c.wr.write(p) }

// CloseWrite half-closes the write side so the peer sees io.EOF after draining
// any buffered data.
func (c *Conn) CloseWrite() error { c.wr.closeWrite(); return nil }

// LocalAddr returns a placeholder in-memory network address.
func (c *Conn) LocalAddr() net.Addr { return addr("mem") }

// RemoteAddr returns a placeholder in-memory network address.
func (c *Conn) RemoteAddr() net.Addr { return addr("mem") }

// SetDeadline is a no-op; synctest callers coordinate via channels and fake time.
func (c *Conn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline is a no-op.
func (c *Conn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline is a no-op.
func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

// Close closes both directions of the endpoint cleanly.
func (c *Conn) Close() error {
	c.rd.closeRead()
	c.wr.closeWrite()
	return nil
}

// Reset aborts the connection, causing peer reads to fail with ErrReset.
func (c *Conn) Reset() {
	c.rd.closeRead()
	c.wr.resetWrite(ErrReset)
}

type addr string

func (a addr) Network() string { return string(a) }
func (a addr) String() string  { return string(a) }
