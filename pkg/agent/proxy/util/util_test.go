package util

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestSafeCloseConn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		conn      net.Conn
		wantCalls int
	}{
		{
			name: "nil interface",
			conn: nil,
		},
		{
			name: "typed nil tls conn",
			conn: func() net.Conn {
				var tlsConn *tls.Conn
				return tlsConn
			}(),
		},
		{
			name:      "normal conn",
			conn:      &testConn{},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := SafeCloseConn(tt.conn)
			if err != nil {
				t.Fatalf("SafeCloseConn() error = %v", err)
			}

			mock, ok := tt.conn.(*testConn)
			if !ok {
				return
			}
			if mock.closeCalls != tt.wantCalls {
				t.Fatalf("SafeCloseConn() close calls = %d, want %d", mock.closeCalls, tt.wantCalls)
			}
		})
	}
}

type testConn struct {
	closeCalls int
}

func (c *testConn) Read(_ []byte) (int, error)         { return 0, nil }
func (c *testConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *testConn) Close() error                       { c.closeCalls++; return nil }
func (c *testConn) LocalAddr() net.Addr                { return testAddr("local") }
func (c *testConn) RemoteAddr() net.Addr               { return testAddr("remote") }
func (c *testConn) SetDeadline(_ time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(_ time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }
