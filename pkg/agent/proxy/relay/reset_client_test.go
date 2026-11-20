//go:build linux || darwin || windows

package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
)

// The rows run on a client connection as the proxy accepts it: its writes
// counted from the accept (CountClientWrites), which Windows reads what was
// written to the socket from (reset_client_windows.go). A client's read of the
// reset is Winsock's WSAECONNRESET on Windows (neterr.IsConnReset).

// netConnOf wraps a conn and says so (NetConn), as tls.Conn does.
type netConnOf struct{ net.Conn }

func (c netConnOf) NetConn() net.Conn { return c.Conn }

// readsReset is nil when the client's next read of app is a reset, and what
// it read otherwise: a byte (as a close_notify alert's would be, read ahead of
// the reset), the end of the stream, or another error.
func readsReset(t *testing.T, app net.Conn) error {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	n, err := app.Read(make([]byte, 1))
	switch {
	case n > 0:
		return errors.New("a byte")
	case neterr.IsConnReset(err):
		return nil
	case err == nil:
		return errors.New("no byte and no error")
	}
	return err
}

func TestResetClientOnCloseResetsWhenTheClientHoldsEveryByte(t *testing.T) {
	t.Parallel()
	app, raw := loopbackPair(t)
	proxy := CountClientWrites(raw)
	if _, err := proxy.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(app, buf); err != nil {
		t.Fatal(err)
	}
	// Read; acknowledged at once, or within the client's delayed ACK, which
	// the look waits for.
	if !resetClientOnClose(context.Background(), netConnOf{proxy}, tcpSettle) {
		t.Fatal("resetClientOnClose = false through a NetConn wrapper once the client read every byte; want a reset")
	}
	_ = proxy.Close()
	if err := readsReset(t, app); err != nil {
		t.Errorf("the client read %v after the close; want a reset", err)
	}
}

// Through a TLS layer, as an HTTPS client's connection is: the proxy's close of
// the TLS conn would send a close_notify alert before the socket closes, which
// the client reads as the clean end of the stream. The socket is reset under
// it, so the client reads the reset the destination gave. The row also counts
// what reaches the socket under the TLS layer (seen): the reset must write
// nothing there first. The client's read alone cannot tell an alert sent
// ahead of the reset on Windows, whose stack drops what its client has not
// read yet when the reset comes.
func TestResetClientOnCloseResetsUnderATLSLayerWithNoCloseNotify(t *testing.T) {
	t.Parallel()
	app, raw := loopbackPair(t)
	seen := &seenWrites{Conn: CountClientWrites(raw)}
	srv := tls.Server(seen, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
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
	before := seen.n.Load()
	if !resetClientOnClose(context.Background(), srv, tcpSettle) {
		t.Fatal("resetClientOnClose = false under a TLS layer once the client read every byte; want a reset")
	}
	if n := seen.n.Load() - before; n != 0 {
		t.Errorf("the reset wrote %d bytes to the client's socket before it (a close_notify alert's); want none", n)
	}
	_ = srv.Close() // the proxy's close: the socket under it is closed already
	if err := readsReset(t, cli); err != nil {
		t.Errorf("the HTTPS client read %v after the close; want a reset, not the close_notify's clean end", err)
	}
}

// A socket that can no longer say what its client took (a failed SIOCOUTQ,
// SO_NWRITE or SIO_TCP_INFO, or counts outside the fence on Windows) ends the
// wait at once, however long the grace: the proxy's close stays a FIN.
func TestResetOnceTakenStopsWhenTheSocketCannotSay(t *testing.T) {
	t.Parallel()
	_, raw := loopbackPair(t)
	tc := tcpConnUnder(raw)
	if tc == nil {
		t.Fatal("no TCP socket under the proxy's end")
	}
	done := make(chan bool, 1)
	go func() {
		done <- resetOnceTaken(context.Background(), raw, tc, time.Hour, func(uintptr) (all, known bool) { return false, false })
	}()
	select {
	case reset := <-done:
		if reset {
			t.Error("resetOnceTaken = true when the socket cannot say; want the close kept a FIN")
		}
	case <-time.After(tcpSettle):
		t.Fatalf("resetOnceTaken still waited %v after the socket could not say", tcpSettle)
	}
}

// fillPastTheClient writes to proxy, the proxy's end of dialSmallWindow's
// connection, more than the client's socket can come to hold while it reads
// nothing: the rest stays in the proxy's send buffer, which holds it, so the
// write returns. It returns what it wrote.
func fillPastTheClient(t *testing.T, app, proxy net.Conn) []byte {
	t.Helper()
	sent := make([]byte, receiveBufferBoundOf(t, app)+64<<10)
	for i := range sent {
		sent[i] = byte(i)
	}
	_ = proxy.SetWriteDeadline(time.Now().Add(tcpSettle)) // turns a write that cannot fit into a failure
	if _, err := proxy.Write(sent); err != nil {
		t.Fatalf("writing %d bytes the proxy's send buffer holds: %v", len(sent), err)
	}
	return sent
}

func TestResetClientOnCloseKeepsAFINWhileBytesAreOutstanding(t *testing.T) {
	app, raw := dialSmallWindow(t)
	proxy := CountClientWrites(raw)
	sent := fillPastTheClient(t, app, proxy)
	// The client reads nothing within the grace: the bytes stay outstanding.
	if resetClientOnClose(context.Background(), proxy, 100*time.Millisecond) {
		t.Fatal("resetClientOnClose = true with bytes the client has not acknowledged; want the close kept a FIN")
	}
	_ = proxy.Close()
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	got, err := io.ReadAll(app)
	if err != nil || len(got) != len(sent) {
		t.Errorf("the client read %d of %d bytes, %v; want every byte, then EOF", len(got), len(sent), err)
	}
}

func TestResetClientOnCloseNotATCPSocket(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if resetClientOnClose(context.Background(), a, time.Second) {
		t.Error("resetClientOnClose = true for a conn with no TCP socket under it")
	}
}

// The recording's stop ends the wait for the client's acknowledgement at
// once, however long the grace: the close stays a FIN.
func TestResetClientOnCloseStopsWaitingAtTheRecordingsStop(t *testing.T) {
	app, raw := dialSmallWindow(t)
	proxy := CountClientWrites(raw)
	fillPastTheClient(t, app, proxy)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- resetClientOnClose(ctx, proxy, time.Hour) }()
	stop()
	select {
	case reset := <-done:
		if reset {
			t.Error("resetClientOnClose = true with bytes outstanding at the recording's stop; want the close kept a FIN")
		}
	case <-time.After(tcpSettle):
		t.Fatalf("resetClientOnClose still waited %v after the recording's stop", tcpSettle)
	}
}

// Through the relay, as keploy record runs it: a destination that resets the
// connection after its answer has the client read the answer, then the reset,
// as it would from the destination, not the end of the stream.
func TestADestinationsResetReachesTheClientAsAReset(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	client := CountClientWrites(proxyClient)
	r := New(Config{Logger: zap.NewNop()},
		&proxyutil.Conn{Conn: client, Reader: client, Logger: zap.NewNop()}, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	go func() { go streamOf(t, r.ClientStream()); streamOf(t, r.DestStream()) }()
	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination read the request: %v", err)
	}
	if _, err := server.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "answer" {
		t.Fatalf("the client read %q, %v; want the answer", got, err)
	}
	if err := server.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	if err := readsReset(t, app); err != nil {
		t.Errorf("the client read %v after the destination's reset; want a reset", err)
	}
	select {
	case <-done:
	case <-time.After(tcpSettle):
		t.Fatal("the relay did not end")
	}
}
