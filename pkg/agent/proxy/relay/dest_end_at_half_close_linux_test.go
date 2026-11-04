//go:build linux

package relay

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// heldDest is the destination's socket under the relay, its reads held until
// the relay has decided what the client's end does to the destination's
// forwarder: a teardown nudges its read (a read deadline in the past), a
// drain lets it read on (the relay says it drains the opposite direction).
type heldDest struct {
	*net.TCPConn
	release chan struct{}
	nudged  chan struct{}
	once    sync.Once
}

func (d *heldDest) Read(p []byte) (int, error) {
	<-d.release
	return d.TCPConn.Read(p)
}

func (d *heldDest) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		d.once.Do(func() { close(d.nudged) })
	}
	return d.TCPConn.SetReadDeadline(t)
}

// The destination resets the connection as the relay forwards the client's
// FIN: the reset is in the destination's socket when the client's forwarder,
// having read the client's end, half-closes the destination (CloseWrite),
// which fails, as the connection is over, before the destination's forwarder
// has read the reset (a busy host). The destination's end is then read and
// told (Config.PeerEnded), and so is the look at the client's socket as it is
// read (Config.ClientEndedBeforeDest): the client's FIN came first. The
// connection being over is no broken half-close: the relay reads the end in
// the destination's socket, at once, rather than tearing the connection down
// and stopping the destination's forwarder before it reads it. Before, the
// teardown nudged the destination's read, which gave a timeout, and the
// forwarder stopped with the reset unread: neither was told.
func TestADestinationsResetInItsSocketAsTheClientsFINIsForwardedIsReadAndTold(t *testing.T) {
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
	dest := &heldDest{TCPConn: proxyServer.(*net.TCPConn), release: make(chan struct{}), nudged: make(chan struct{})}
	drains := make(chan struct{})
	var drainOnce sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "draining the opposite direction") {
			drainOnce.Do(func() { close(drains) })
		}
		return nil
	}))
	ends := newEndsTold()
	cfg := ends.config()
	cfg.Logger = logger
	clientEndRead, destEnded := make(chan struct{}), make(chan struct{})
	told := cfg.PeerEnded
	cfg.PeerEnded = func(dir fakeconn.Direction, reset bool) {
		told(dir, reset)
		if dir != fakeconn.FromClient {
			return
		}
		// The client's forwarder, having read the client's end and before it
		// half-closes the destination, waits for the destination's reset to
		// be in the destination's socket.
		close(clientEndRead)
		select {
		case <-destEnded:
		case <-time.After(10 * time.Second):
		}
	}
	done := runRelay(t, New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(dest)))
	// Let the destination's forwarder read once the relay has decided:
	// nudged by a teardown, or let read on by a drain.
	go func() {
		select {
		case <-dest.nudged:
		case <-drains:
		case <-done:
		}
		close(dest.release)
	}()
	if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	readN(t, server, len("GET / HTTP/1.1\r\n\r\n"))
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientEndRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the client's FIN within 10s")
	}
	if err := server.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	awaitTCPState(t, proxyServer, unix.BPF_TCP_CLOSE, "the destination's reset")
	close(destEnded)
	awaitDone(t, done, "the relay did not end")
	want := []string{"the client's end", "the destination's end (reset)", "the client's end before the destination's"}
	got := ends.all()
	if len(got) != len(want) {
		t.Fatalf("told %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("told %q, want %q", got, want)
		}
	}
}

// gatedDest is heldDest whose reads are held only once gate is set: the TLS
// handshake reads through it first.
type gatedDest struct {
	heldDest
	gate atomic.Bool
}

func (d *gatedDest) Read(p []byte) (int, error) {
	if d.gate.Load() {
		<-d.release
	}
	return d.TCPConn.Read(p)
}

// Over TLS the relay's half-close of the destination is a close_notify
// alert, written, not a shutdown(2): met by the destination's reset in its
// socket, that write fails with the reset itself (or EPIPE), never ENOTCONN,
// and takes the reset out of the socket, so a read after it finds only its
// end, an EOF. The relay keeps the teardown there: the destination's end is
// left untold rather than told as a close. Draining as for a plain socket
// whose connection is over would read that EOF and tell the reset as the
// destination's close.
func TestOverTLSADestinationsResetMetAsTheClientsFINIsForwardedIsNotToldAsAClose(t *testing.T) {
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
	dest := &gatedDest{heldDest: heldDest{TCPConn: proxyServer.(*net.TCPConn), release: make(chan struct{}), nudged: make(chan struct{})}}
	// TLS 1.2: no ticket follows the handshake, so nothing is left unread in
	// the destination's socket but its end.
	cert := selfSignedCert(t)
	tlsServer := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12})
	tlsDest := tls.Client(dest, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // a test's self-signed peer
	handshook := make(chan error, 1)
	go func() { handshook <- tlsServer.Handshake() }()
	if err := tlsDest.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshook; err != nil {
		t.Fatal(err)
	}
	dest.gate.Store(true)
	drains := make(chan struct{})
	var drainOnce sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "draining the opposite direction") {
			drainOnce.Do(func() { close(drains) })
		}
		return nil
	}))
	ends := newEndsTold()
	cfg := ends.config()
	cfg.Logger = logger
	clientEndRead, destEnded := make(chan struct{}), make(chan struct{})
	told := cfg.PeerEnded
	cfg.PeerEnded = func(dir fakeconn.Direction, reset bool) {
		told(dir, reset)
		if dir != fakeconn.FromClient {
			return
		}
		close(clientEndRead)
		select {
		case <-destEnded:
		case <-time.After(10 * time.Second):
		}
	}
	done := runRelay(t, New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(tlsDest)))
	go func() {
		select {
		case <-dest.nudged:
		case <-drains:
		case <-done:
		}
		close(dest.release)
	}()
	req := "GET / HTTP/1.1\r\n\r\n"
	if _, err := app.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	readN(t, tlsServer, len(req))
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientEndRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the client's FIN within 10s")
	}
	if err := server.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	awaitTCPState(t, proxyServer, unix.BPF_TCP_CLOSE, "the destination's reset")
	close(destEnded)
	awaitDone(t, done, "the relay did not end")
	select {
	case <-drains:
		t.Fatal("the relay drained after a close_notify that met the destination's reset, want the teardown")
	default:
	}
	if got := ends.all(); len(got) != 1 || got[0] != "the client's end" {
		t.Fatalf("told %q, want only the client's end: the destination's reset, taken by the failed close_notify, is never told as its close", got)
	}
}

// failingCloseWrite is a destination conn whose half-close fails, its
// connection not over: the FIN is not forwarded, so the destination never
// learns the request ended.
type failingCloseWrite struct{ net.Conn }

func (failingCloseWrite) CloseWrite() error { return errors.New("the half-close failed") }

// noCloseWrite is a destination conn that cannot half-close: only net.Conn's
// methods, and no conn beneath to ask (NetConn).
type noCloseWrite struct{ net.Conn }

// A FIN the relay cannot forward to a destination whose connection is not
// over keeps the teardown, at once: the destination never learns the request
// ended, and may never answer or close, so the relay does not drain the
// opposite direction for its half-close grace, however long that is, but
// stops the destination's forwarder and ends.
func TestAFINThatCannotBeForwardedToAnOpenDestinationTearsDownAtOnce(t *testing.T) {
	for _, row := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"its half-close fails", func(c net.Conn) net.Conn { return failingCloseWrite{c} }},
		{"it cannot half-close", func(c net.Conn) net.Conn { return noCloseWrite{c} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
			ends := newEndsTold()
			cfg := ends.config()
			cfg.HalfCloseGrace = time.Hour
			done := runRelay(t, New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()},
				connseq.NewUpstream(row.wrap(proxyServer))))
			if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			readN(t, server, len("GET / HTTP/1.1\r\n\r\n"))
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitDone(t, done, "the relay, with a FIN it could not forward to an open destination, did not tear down at once")
			if got := ends.all(); len(got) != 1 || got[0] != "the client's end" {
				t.Fatalf("told %q, want only the client's end: the destination never ended its side", got)
			}
		})
	}
}
