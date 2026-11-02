//go:build linux

package relay

import (
	"context"
	"crypto/tls"
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
	if !resetClientOnClose(context.Background(), netConnOf{proxy}, time.Second) {
		t.Fatal("resetClientOnClose = false through a NetConn wrapper with nothing outstanding; want a reset")
	}
	_ = proxy.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := app.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("the client read %v after the close; want ECONNRESET", err)
	}
}

// Through a TLS layer, as an HTTPS client's connection is: the proxy's close of
// the TLS conn would send a close_notify alert before the socket closes, which
// the client reads as the clean end of the stream. The socket is reset under
// it, so the client reads the reset the destination gave.
func TestResetClientOnCloseResetsUnderATLSLayerWithNoCloseNotify(t *testing.T) {
	app, proxy := tcpPair(t)
	srv := tls.Server(proxy, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	cli := tls.Client(app, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- a test's self-signed peer
	hs := make(chan error, 1)
	go func() { hs <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-hs; err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(cli, buf); err != nil {
		t.Fatal(err)
	}
	// The client's stack is in delayed-ACK mode after the handshake's
	// exchanges, so "head" can be unacknowledged still: it waits for the
	// acknowledgement, within the grace.
	if !resetClientOnClose(context.Background(), srv, 10*time.Second) {
		t.Fatal("resetClientOnClose = false under a TLS layer once the client read every byte; want a reset")
	}
	_ = srv.Close() // the proxy's close: the socket under it is closed already
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := cli.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("the HTTPS client read %v after the close; want ECONNRESET, not the close_notify's clean end", err)
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
	// The client reads nothing within the grace: the bytes stay outstanding.
	if resetClientOnClose(context.Background(), proxy, 100*time.Millisecond) {
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
	if resetClientOnClose(context.Background(), a, time.Second) {
		t.Error("resetClientOnClose = true for a conn with no TCP socket under it")
	}
}

// The recording's stop ends the wait for the client's acknowledgement at
// once, however long the grace: the close stays a FIN.
func TestResetClientOnCloseStopsWaitingAtTheRecordingsStop(t *testing.T) {
	app, proxy := tcpPair(t)
	_ = app.(*net.TCPConn).SetReadBuffer(4096)
	_ = proxy.(*net.TCPConn).SetWriteBuffer(4096)
	chunk := make([]byte, 64<<10)
	_ = proxy.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		if _, err := proxy.Write(chunk); err != nil {
			break
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- resetClientOnClose(ctx, proxy, time.Hour) }()
	stop()
	select {
	case reset := <-done:
		if reset {
			t.Error("resetClientOnClose = true with bytes outstanding at the recording's stop; want the close kept a FIN")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("resetClientOnClose still waited 30s after the recording's stop")
	}
}
