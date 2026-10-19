package tls

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// chunkConn is a connection whose reads return as much of data as the
// caller's buffer takes, as a socket holding all of it at once would.
type chunkConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *chunkConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// record is one TLS record of type typ with body.
func record(typ byte, body []byte) []byte {
	return append([]byte{typ, 0x03, 0x03, byte(len(body) >> 8), byte(len(body))}, body...)
}

// recordConn hands over no byte past the record it belongs to, however much
// the socket holds and the caller asks for: crypto/tls, reading a record
// whole, then has read exactly the handshake's records when the handshake
// ends, and what the client sent after its Finished is counted as it is read.
// After that first byte the reads go straight through.
func TestARecordConnHandsOverARecordAtATime(t *testing.T) {
	finished := record(0x16, bytes.Repeat([]byte{1}, 40))
	closeNotify := record(0x15, []byte{1, 0})
	data := record(0x17, bytes.Repeat([]byte{2}, 30))
	all := append(append(append([]byte(nil), finished...), closeNotify...), data...)
	c := newRecordConn(&chunkConn{r: bytes.NewReader(all)})
	buf := make([]byte, 1<<10)
	read := func() []byte {
		t.Helper()
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), buf[:n]...)
	}
	if h := read(); !bytes.Equal(h, finished[:recordHeaderLen]) {
		t.Fatalf("first read %x, want the record's header alone", h)
	}
	if b := read(); !bytes.Equal(b, finished[recordHeaderLen:]) {
		t.Fatalf("second read %d bytes, want the record's body alone (%d), nothing of the next", len(b), len(finished)-recordHeaderLen)
	}
	if c.after.Load() != 0 {
		t.Fatal("bytes counted after the handshake before it ended")
	}
	c.handshakeDone()
	if h := read(); !bytes.Equal(h, closeNotify[:recordHeaderLen]) {
		t.Fatalf("the read after the handshake %x, want the next record's header", h)
	}
	if got := c.after.Load(); got != recordHeaderLen {
		t.Fatalf("%d bytes counted after the handshake, want %d", got, recordHeaderLen)
	}
	rest, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(rest, all[len(finished)+recordHeaderLen:]) {
		t.Fatalf("the rest: %d bytes, %v; want everything left, straight through", len(rest), err)
	}
}

// Whether a client sent anything since its TLS handshake ended: nothing, when
// it hangs up then (curl, libcurl and Node.js under TLS 1.2 reject keploy's
// certificate so); something, when it sends its close_notify -- however soon
// after its Finished -- or a call. A connection keploy's TLS server did not
// serve says nothing, nor does one it served with the proven switch off,
// which it serves as before, reading nothing of its own.
func TestSilentSinceHandshake(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name   string
		max    uint16
		client func(c *tls.Conn, raw net.Conn)
		silent bool
	}{
		{"TLS 1.2, hangs up once the handshake is done", tls.VersionTLS12, func(_ *tls.Conn, raw net.Conn) { _ = raw.Close() }, true},
		{"TLS 1.3, hangs up once the handshake is done", tls.VersionTLS13, func(_ *tls.Conn, raw net.Conn) { _ = raw.Close() }, true},
		{"TLS 1.2, its close_notify at once", tls.VersionTLS12, func(c *tls.Conn, _ net.Conn) { _ = c.Close() }, false},
		{"TLS 1.3, its close_notify at once", tls.VersionTLS13, func(c *tls.Conn, _ net.Conn) { _ = c.Close() }, false},
		{"TLS 1.3, a call, then it hangs up", tls.VersionTLS13, func(c *tls.Conn, raw net.Conn) { _, _ = c.Write([]byte("GET / HTTP/1.1\r\n\r\n")); _ = raw.Close() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, srv := tcpPair(t)
			go func() {
				c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MaxVersion: tc.max})
				if err := c.Handshake(); err != nil {
					_ = app.Close()
					return
				}
				tc.client(c, app)
			}()
			conn, _, err := HandleTLSConnection(context.Background(), zap.NewNop(), srv, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(conn)
			if !IsClientGiveUp(err) && err != nil {
				t.Fatalf("the read ended %v", err)
			}
			if got := SilentSinceHandshake(conn); got != tc.silent {
				t.Fatalf("SilentSinceHandshake = %v, want %v", got, tc.silent)
			}
		})
	}
	if SilentSinceHandshake(&chunkConn{}) {
		t.Fatal("a connection keploy's TLS server did not serve is never silent since a handshake")
	}

	t.Run("the switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "0")
		app, srv := tcpPair(t)
		go func() {
			c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example"})
			_ = c.Handshake()
			_ = app.Close()
		}()
		conn, _, err := HandleTLSConnection(context.Background(), zap.NewNop(), srv, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(conn)
		if SilentSinceHandshake(conn) {
			t.Fatal("with the switch off, keploy's TLS server reads the client as before, and says nothing of what it sent")
		}
		if _, ok := conn.(*tls.Conn).NetConn().(*recordConn); ok {
			t.Fatal("with the switch off, the client's connection is wrapped")
		}
	})
}

// tcpPair is both ends of a loopback TCP connection: crypto/tls's server
// reads its client's address as a TCP one.
func tcpPair(t *testing.T) (app, srv net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	app, err = net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	return app, srv
}
