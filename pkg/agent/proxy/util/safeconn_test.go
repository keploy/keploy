package util

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeConn is a minimal net.Conn for testing SafeConn behavior.
type fakeConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
	closed   bool
}

func newFakeConn(data string) *fakeConn {
	return &fakeConn{
		readBuf:  bytes.NewBufferString(data),
		writeBuf: &bytes.Buffer{},
	}
}

func (f *fakeConn) Read(p []byte) (int, error)         { return f.readBuf.Read(p) }
func (f *fakeConn) Write(p []byte) (int, error)        { return f.writeBuf.Write(p) }
func (f *fakeConn) Close() error                       { f.closed = true; return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5678} }
func (f *fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestSafeConnReadPassesThrough(t *testing.T) {
	fc := newFakeConn("hello world")
	sc := NewSafeConn(fc, zap.NewNop())

	buf := make([]byte, 5)
	n, err := sc.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}
}

func TestSafeConnWritePassesThrough(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	_, err := sc.Write([]byte("data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fc.writeBuf.String() != "data" {
		t.Fatalf("expected 'data' in underlying write buf, got %q", fc.writeBuf.String())
	}
}

func TestSafeConnCloseIsNoOp(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	err := sc.Close()
	if err != nil {
		t.Fatalf("Close should return nil, got: %v", err)
	}
	if fc.closed {
		t.Fatal("underlying connection should NOT be closed by SafeConn.Close()")
	}
}

func TestSafeConnDeadlinesAreNoOps(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if err := sc.SetDeadline(time.Now()); err != nil {
		t.Fatalf("SetDeadline should return nil: %v", err)
	}
	if err := sc.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("SetReadDeadline should return nil: %v", err)
	}
	if err := sc.SetWriteDeadline(time.Now()); err != nil {
		t.Fatalf("SetWriteDeadline should return nil: %v", err)
	}
}

func TestSafeConnWithReader(t *testing.T) {
	fc := newFakeConn("underlying")
	prefix := bytes.NewReader([]byte("prefix-"))
	sc := NewSafeConnWithReader(fc, io.MultiReader(prefix, fc), zap.NewNop())

	buf := make([]byte, 20)
	n, _ := sc.Read(buf)
	if !bytes.HasPrefix(buf[:n], []byte("prefix-")) {
		t.Fatalf("expected to read prefix first, got %q", string(buf[:n]))
	}
}

func TestSafeConnUnwrap(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if sc.Unwrap() != fc {
		t.Fatal("Unwrap should return the original connection")
	}
}

func TestSafeConnAddrs(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if sc.LocalAddr().String() != fc.LocalAddr().String() {
		t.Fatal("LocalAddr mismatch")
	}
	if sc.RemoteAddr().String() != fc.RemoteAddr().String() {
		t.Fatal("RemoteAddr mismatch")
	}
}

// A half-close through a wrapper answers for the conn it wraps: done where the
// leaf can (a TCP conn's FIN), and ErrCannotHalfClose, not nil, where it
// cannot, however the wrappers nest. A caller that keeps the other direction
// open for the peer's answer to the FIN must know the peer got one.
func TestCloseWriteIfPossibleAnswersForTheLeaf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	tcp, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	t.Cleanup(func() { _ = tcp.Close(); _ = peer.Close() })
	pipe, other := net.Pipe()
	t.Cleanup(func() { _ = pipe.Close(); _ = other.Close() })
	leaf := struct{ net.Conn }{pipe} // no CloseWrite

	wrapped := func(c net.Conn) net.Conn {
		s := NewSafeConn(c, zap.NewNop())
		return &Conn{Conn: s, Reader: s, Logger: zap.NewNop()}
	}
	for _, c := range []net.Conn{leaf, &Conn{Conn: leaf, Reader: leaf}, NewSafeConn(leaf, zap.NewNop()), wrapped(leaf)} {
		if err := CloseWriteIfPossible(c); !errors.Is(err, ErrCannotHalfClose) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("CloseWriteIfPossible(%T over a conn with no CloseWrite) = %v, want ErrCannotHalfClose", c, err)
		}
	}
	if err := CloseWriteIfPossible(wrapped(tcp)); err != nil {
		t.Fatalf("CloseWriteIfPossible(a TCP conn, wrapped) = %v, want nil", err)
	}
	if n, err := peer.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the peer read (%d, %v), want the FIN (0, io.EOF)", n, err)
	}
}
