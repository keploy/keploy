package orchestrator

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// mockConn is a simple mock for net.Conn
type mockConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func newMockConn(data []byte) *mockConn {
	return &mockConn{
		readBuf:  bytes.NewBuffer(data),
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockConn) Read(b []byte) (n int, err error)   { return m.readBuf.Read(b) }
func (m *mockConn) Write(b []byte) (n int, err error)  { return m.writeBuf.Write(b) }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestReadOnlyConn_Read(t *testing.T) {
	data := []byte("hello world")
	conn := newMockConn(data)
	roConn := NewReadOnlyConn(conn)

	buf := make([]byte, len(data))
	n, err := roConn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), n)
	}
	if string(buf) != string(data) {
		t.Fatalf("expected %q, got %q", data, buf)
	}
}

func TestReadOnlyConn_WriteBlocked(t *testing.T) {
	conn := newMockConn(nil)
	roConn := NewReadOnlyConn(conn)

	n, err := roConn.Write([]byte("should fail"))
	if err != ErrWriteNotAllowed {
		t.Fatalf("expected ErrWriteNotAllowed, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes written, got %d", n)
	}
}

func TestReadOnlyConn_WithCustomReader(t *testing.T) {
	conn := newMockConn(nil)
	customData := []byte("custom reader data")
	reader := bytes.NewReader(customData)

	roConn := NewReadOnlyConnWithReader(conn, reader)

	buf := make([]byte, len(customData))
	n, err := roConn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != string(customData) {
		t.Fatalf("expected %q, got %q", customData, buf[:n])
	}
}

func TestReadOnlyConn_Unwrap(t *testing.T) {
	conn := newMockConn(nil)
	roConn := NewReadOnlyConn(conn)

	unwrapped := roConn.Unwrap()
	if unwrapped != conn {
		t.Fatal("Unwrap should return the original connection")
	}

	// Unwrapped connection should allow writes
	_, err := unwrapped.Write([]byte("test"))
	if err != nil {
		t.Fatalf("unwrapped connection should allow writes: %v", err)
	}
}
