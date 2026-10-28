//go:build darwin

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// darwinPair is a connection as the proxy holds a paired one under the proven
// switch, on macOS: the application's end (app) and the proxy's (proxySide),
// paired as application connection 1, its reads seen through reader
// (inflight.go), its close noted (pairClosing) as the accept loop notes it.
func darwinPair(t *testing.T) (p *Proxy, app, proxySide *net.TCPConn, reader *trackedConn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop()}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app = c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide = s.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	p.paired.add(proxySide, 1)
	reader = p.inflight.track(proxySide)
	reader.beforeClose = func() { p.pairClosing(context.Background(), proxySide, reader) }
	t.Cleanup(func() { p.inflight.forget(reader) })
	return p, app, proxySide, reader
}

// untilUnread returns once conn's socket holds n bytes unread: loopback hands
// them to the proxy's socket on its own input thread, not within the
// application's write.
func untilUnread(t *testing.T, conn *net.TCPConn, n int) {
	t.Helper()
	for {
		got, err := readUnread(conn)
		if err != nil {
			t.Fatalf("SO_NREAD: %v", err)
		}
		if got == n {
			return
		}
		runtime.Gosched()
	}
}

// untilStateLeaves returns the TCP state of conn's socket once it is none of
// from.
func untilStateLeaves(t *testing.T, conn *net.TCPConn, from ...uint8) uint8 {
	t.Helper()
	rc, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, state, ok := socketReceived(rc)
		if !ok {
			t.Fatal("TCP_CONNECTION_INFO cannot be read")
		}
		left := true
		for _, f := range from {
			left = left && state != f
		}
		if left {
			return state
		}
		runtime.Gosched()
	}
}

// macOS has no sock_diag, and nothing yet reads how much the application
// wrote to a paired connection: the wait for those bytes, and the proxy's
// account of it, say they cannot tell, so no test under the proven switch is
// judged settled on bytes nobody counted (a nil here was a settle never
// performed).
func TestTheBytesOnPairedConnectionsAreUnknownOnDarwin(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("PairedBytesErr() = %v, want models.ErrProxyConnsUnsupported", err)
	}
	if err := p.WaitPairedBytes(context.Background(), 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("WaitPairedBytes() = %v, want models.ErrProxyConnsUnsupported", err)
	}
}

// The proxy's handler closes a paired connection with bytes the application
// wrote that it never read: the close tosses them, so it charges the loss,
// once; a close with nothing unread charges none, and neither does one the
// proxy serves with no session.
func TestAProxysCloseWithBytesUnreadIsALossOnDarwin(t *testing.T) {
	t.Run("bytes unread", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\npublish 2\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil { // the handler takes the first call only
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 10)
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged at the close, want 1: 10 bytes the application wrote were never read, and the close tossed them", l)
		}
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1: once per connection", l)
		}
	})
	t.Run("all read", func(t *testing.T) {
		p, app, _, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want 0: the proxy read every byte", l)
		}
	})
	t.Run("no session", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		p.paired.servedWithoutSession(proxySide)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 10)
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want 0: a connection served with no session captures nothing by design", l)
		}
	})
}

// XNU's received count (tcpi_rxbytes) is the payload alone, never the FIN:
// the application sends 100 bytes and its FIN, the proxy's handler reads 99,
// and one byte is still untaken, so the connection still holds work. (Taking
// one off for the FIN, as Linux's count needs, read it as none.)
func TestAnUnreadByteBesideTheFINIsUntakenOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	if _, err := app.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 99)); err != nil {
		t.Fatal(err)
	}
	if s := untilStateLeaves(t, proxySide, xnuTCPEstablished); s != xnuTCPCloseWait {
		t.Fatalf("the proxy's side is in state %d after the application's FIN, want CLOSE_WAIT (%d)", s, xnuTCPCloseWait)
	}
	untilUnread(t, proxySide, 1)
	if n := reader.untaken(reader.view()); n != 1 {
		t.Fatalf("untaken = %d with 1 byte unread beside the FIN, want 1", n)
	}
	if !reader.busyAt(reader.view(), false) {
		t.Fatal("the connection holds no work with a byte the application sent unread")
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d with every byte read, want 0", n)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after the last byte: %v, want EOF", err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d after the EOF, want 0", n)
	}
}

// keploy half-closes a connection, then its client sends its FIN: the socket
// the handler still holds is in TIME_WAIT on macOS (Linux shows TCP_CLOSE),
// and the client has gone, so a hang-up held on it is seen without waiting
// for a read to return the stream's end.
func TestAClientsFINAfterKeploysHalfCloseIsAHangUpOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	release := reader.HoldHangUp()
	defer release()
	if reader.hungUp(reader.view()) {
		t.Fatal("hung up before anyone closed anything")
	}
	if err := reader.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after keploy's half-close: %v, want EOF", err)
	}
	if reader.hungUp(reader.view()) {
		t.Fatal("hung up on keploy's own half-close, the client not yet done")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if s := untilStateLeaves(t, proxySide, xnuTCPFinWait1, xnuTCPFinWait2); s != xnuTCPTimeWait {
		t.Fatalf("the proxy's side is in state %d after the client's FIN, want TIME_WAIT (%d)", s, xnuTCPTimeWait)
	}
	if !reader.hungUp(reader.view()) {
		t.Fatal("the client's FIN after keploy's half-close (TIME_WAIT) is not seen as its hang-up")
	}
}
