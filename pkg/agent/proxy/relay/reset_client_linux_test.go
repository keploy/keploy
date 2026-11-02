//go:build linux

package relay

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// netConnOf wraps a conn and says so (NetConn), as tls.Conn does.
type netConnOf struct{ net.Conn }

func (c netConnOf) NetConn() net.Conn { return c.Conn }

func tcpPair(t *testing.T) (app, proxy net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); got <- c }()
	if app, err = net.Dial("tcp", l.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if proxy = <-got; proxy == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = proxy.Close() })
	return app, proxy
}

func TestResetClientOnCloseResetsWhenTheClientHoldsEveryByte(t *testing.T) {
	app, proxy := tcpPair(t)
	if _, err := proxy.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(app, buf); err != nil {
		t.Fatal(err)
	}
	// Read and so acknowledged: nothing is outstanding.
	if !resetClientOnClose(netConnOf{proxy}) {
		t.Fatal("resetClientOnClose = false through a NetConn wrapper with nothing outstanding; want a reset")
	}
	_ = proxy.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := app.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("the client read %v after the close; want ECONNRESET", err)
	}
}

func TestResetClientOnCloseKeepsAFINWhileBytesAreOutstanding(t *testing.T) {
	app, proxy := tcpPair(t)
	_ = app.(*net.TCPConn).SetReadBuffer(4096)
	_ = proxy.(*net.TCPConn).SetWriteBuffer(4096)
	// Fill the way to the client, which reads nothing yet: the deadline
	// only stops the write once nothing more fits.
	chunk := make([]byte, 64<<10)
	written := 0
	_ = proxy.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		n, err := proxy.Write(chunk)
		written += n
		if err != nil {
			break
		}
	}
	if resetClientOnClose(proxy) {
		t.Fatal("resetClientOnClose = true with bytes the client has not acknowledged; want the close kept a FIN")
	}
	_ = proxy.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(app)
	if err != nil || len(got) != written {
		t.Errorf("the client read %d of %d bytes, %v; want every byte, then EOF", len(got), written, err)
	}
}

func TestResetClientOnCloseNotATCPSocket(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if resetClientOnClose(a) {
		t.Error("resetClientOnClose = true for a conn with no TCP socket under it")
	}
}
