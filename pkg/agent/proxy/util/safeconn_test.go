package util

import (
	"bytes"
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
