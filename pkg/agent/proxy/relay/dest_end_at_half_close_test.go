//go:build linux || darwin || windows

package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	err := d.TCPConn.SetReadDeadline(t)
	if !t.IsZero() && t.Before(time.Now()) {
		d.once.Do(func() { close(d.nudged) })
	}
	return err
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
	awaitTCPState(t, proxyServer, stateClosed, "the destination's reset")
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
	awaitTCPState(t, proxyServer, stateClosed, "the destination's reset")
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

// heldClient is the proxy's end of the client connection under the relay, its
// reads held until release, so the client's forwarder reads nothing until the
// relay has decided what the destination's end does to the client. It wraps
// the TCP socket (NetConn), as the proxy's own wrappers do.
type heldClient struct {
	*net.TCPConn
	release chan struct{}
}

func (c *heldClient) Read(p []byte) (int, error) {
	<-c.release
	return c.TCPConn.Read(p)
}

func (c *heldClient) NetConn() net.Conn { return c.TCPConn }

// The client resets the connection as the relay forwards the destination's
// FIN. Windows' socket says so before the half-close (closedBeforeFIN), and
// the relay takes the connection as over; Linux's and macOS's are not asked,
// and their half-close of the client conn fails, the connection being over.
// No FIN went, so the client's socket is not told of one (noteClientFINSent):
// told, a Windows socket would take one of its bytes in flight for a FIN the
// client never got. On every OS the relay drains as for a half-close, and its
// log says the FIN was not forwarded.
func TestAClientsResetAsTheDestinationsFINIsForwardedIsNoFINSent(t *testing.T) {
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
	client := &heldClient{TCPConn: proxyClient.(*net.TCPConn), release: make(chan struct{})}
	const drainMsg = "draining the opposite direction"
	drains := make(chan struct{})
	var drainOnce sync.Once
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, drainMsg) {
			drainOnce.Do(func() { close(drains) })
		}
		return nil
	}))
	destEndRead, clientReset := make(chan struct{}), make(chan struct{})
	cfg := Config{Logger: logger, PeerEnded: func(dir fakeconn.Direction, _ bool) {
		if dir != fakeconn.FromDest {
			return
		}
		// The destination's forwarder, having read the destination's FIN
		// and before it half-closes the client, waits for the client's
		// reset to be in the client's socket.
		close(destEndRead)
		select {
		case <-clientReset:
		case <-time.After(10 * time.Second):
		}
	}}
	r := New(cfg, client, connseq.NewUpstream(proxyServer))
	s := &finSentRecorder{inner: r.client}
	r.client = s
	done := runRelay(t, r)
	if err := server.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-destEndRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the destination's FIN within 10s")
	}
	if err := app.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateClosed, "the client's reset")
	close(clientReset)
	// The client's forwarder reads on once the relay has decided: drained,
	// or torn down (the relay then ends with the client's reads held).
	select {
	case <-drains:
	case <-done:
	case <-time.After(10 * time.Second):
	}
	close(client.release)
	awaitDone(t, done, "the relay did not end")
	entries := logs.FilterMessageSnippet(drainMsg).All()
	if len(entries) != 1 {
		t.Fatalf("the relay drained %d times, want once, for the destination's FIN", len(entries))
	}
	if over, ok := entries[0].ContextMap()["over"]; !ok || over == "" {
		t.Fatalf("the relay drained with the FIN forwarded (fields %v), want it not forwarded: the client's connection was over", entries[0].ContextMap())
	}
	if got := s.asked.Load(); got != 1 {
		t.Fatalf("the client's socket was asked %d times before the half-close, want once", got)
	}
	if got := s.toldTimes(); got != 0 {
		t.Fatalf("the client's socket was told %d times of a FIN that was not sent, want none", got)
	}
}

// A client socket that says its connection closed before the proxy's FIN
// (closedBeforeFIN) is not half-closed, on every OS: the relay takes the
// connection as over, as a failed half-close says on Linux and macOS, where a
// half-close would succeed on Windows and send nothing. Here the client is in
// fact open, so the FIN a half-close would send would reach it: it reads
// none. The client's socket is not told of a FIN, and the relay drains as for
// a half-close, its log saying the FIN was not forwarded.
func TestAClientSocketThatSaysItsConnectionClosedIsNotHalfClosed(t *testing.T) {
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
	const drainMsg = "draining the opposite direction"
	core, logs := observer.New(zapcore.DebugLevel)
	r := New(Config{Logger: zap.New(core)}, proxyClient, connseq.NewUpstream(proxyServer))
	s := &finSentRecorder{closed: true}
	r.client = s
	done := runRelay(t, r)
	if err := server.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for logs.FilterMessageSnippet(drainMsg).Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = app.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the client read %d byte(s), %v; want nothing, no FIN sent it", n, err)
	}
	_ = app.Close()
	awaitDone(t, done, "the relay did not end")
	entries := logs.FilterMessageSnippet(drainMsg).All()
	if len(entries) == 0 {
		t.Fatal("the relay did not drain for the destination's FIN")
	}
	if over, ok := entries[0].ContextMap()["over"].(string); !ok || !strings.Contains(over, "says its connection is closed") {
		t.Fatalf("the relay drained with fields %v, want the FIN not forwarded, the client's socket saying its connection closed", entries[0].ContextMap())
	}
	if got := s.asked.Load(); got != 1 {
		t.Fatalf("the client's socket was asked %d times before the half-close, want once", got)
	}
	if got := s.toldTimes(); got != 0 {
		t.Fatalf("the client's socket was told %d times of a FIN that was not sent, want none", got)
	}
}

// eofRaceConn wraps a TCPConn to deterministically interleave a failing
// CloseWrite (or the teardown SetReadDeadline that follows it) with an io.EOF
// returned by the opposite forwarder's Read.
type eofRaceConn struct {
	*net.TCPConn
	onDeadline   bool
	readEntered  chan struct{}
	releaseRead  chan struct{}
	readReturned chan struct{}
	abort        chan struct{}
	enterOnce    sync.Once
	releaseOnce  sync.Once
	readOnce     sync.Once
}

func (c *eofRaceConn) Read(_ []byte) (int, error) {
	if c.readEntered != nil {
		c.enterOnce.Do(func() { close(c.readEntered) })
	}
	select {
	case <-c.releaseRead:
	case <-c.abort:
	}
	c.readOnce.Do(func() { close(c.readReturned) })
	return 0, io.EOF
}

func (c *eofRaceConn) CloseWrite() error {
	if !c.onDeadline {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return syscall.ECONNRESET
}

func (c *eofRaceConn) SetReadDeadline(t time.Time) error {
	if c.onDeadline && !t.IsZero() && t.Before(time.Now()) {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.TCPConn.SetReadDeadline(t)
}

// closeWriteSpyConn counts CloseWrite calls while delegating to the underlying
// TCPConn.
type closeWriteSpyConn struct {
	*net.TCPConn
	calls atomic.Int32
}

func (c *closeWriteSpyConn) CloseWrite() error {
	c.calls.Add(1)
	return c.TCPConn.CloseWrite()
}

// nudgeSpyConn records when a past read deadline is set on the wrapped conn.
type nudgeSpyConn struct {
	*net.TCPConn
	nudged chan struct{}
	once   sync.Once
}

func (c *nudgeSpyConn) SetReadDeadline(t time.Time) error {
	err := c.TCPConn.SetReadDeadline(t)
	if !t.IsZero() && t.Before(time.Now()) {
		c.once.Do(func() { close(c.nudged) })
	}
	return err
}

// heldUntilSecondNudgeConn keeps Read blocked across the first past
// SetReadDeadline (cancelNudge's) and releases it on the second (from the
// opposite forwarder's exit) or when release/abort closes.
type heldUntilSecondNudgeConn struct {
	*net.TCPConn
	nudges  atomic.Int32
	release chan struct{}
	abort   chan struct{}
	once    sync.Once
}

func (c *heldUntilSecondNudgeConn) Read(p []byte) (int, error) {
	select {
	case <-c.release:
	case <-c.abort:
	}
	return c.TCPConn.Read(p)
}

func (c *heldUntilSecondNudgeConn) SetReadDeadline(t time.Time) error {
	err := c.TCPConn.SetReadDeadline(t)
	if !t.IsZero() && t.Before(time.Now()) && c.nudges.Add(1) >= 2 {
		c.once.Do(func() { close(c.release) })
	}
	return err
}

// TestHalfCloseTeardownSynchronizesWithOppositeEOF deterministically pins
// every ordering window where a failing half-close or an already-started
// teardown races an io.EOF on the opposite forwarder, on every OS.
func TestHalfCloseTeardownSynchronizesWithOppositeEOF(t *testing.T) {
	newDrainLogger := func() (*zap.Logger, chan struct{}) {
		drains := make(chan struct{})
		var drainOnce sync.Once
		core, _ := observer.New(zapcore.DebugLevel)
		logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
			if strings.Contains(e.Message, "draining the opposite direction") {
				drainOnce.Do(func() { close(drains) })
			}
			return nil
		}))
		return logger, drains
	}

	for _, tc := range []struct {
		name       string
		onDeadline bool
	}{
		{name: "dest read EOF during failed CloseWrite", onDeadline: false},
		{name: "dest read EOF after teardown before read deadline takes effect", onDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
			abort := make(chan struct{})
			dest := &eofRaceConn{
				TCPConn:      proxyServer.(*net.TCPConn),
				onDeadline:   tc.onDeadline,
				releaseRead:  make(chan struct{}),
				readReturned: make(chan struct{}),
				abort:        abort,
			}
			logger, drains := newDrainLogger()
			ends := newEndsTold()
			cfg := ends.config()
			cfg.Logger = logger
			done := runRelay(t, New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(dest)))
			t.Cleanup(func() { close(abort) })
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitDone(t, done, "the relay did not end")
			select {
			case <-drains:
				t.Fatal("the relay drained after a failing half-close, want teardown")
			default:
			}
			if got := ends.all(); len(got) != 1 || got[0] != "the client's end" {
				t.Fatalf("told %q, want only the client's end", got)
			}
		})
	}

	t.Run("client read EOF during failed CloseWrite", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		client := &eofRaceConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		logger, drains := newDrainLogger()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		done := runRelay(t, New(cfg, client, connseq.NewUpstream(proxyServer)))
		t.Cleanup(func() { close(abort) })
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")
		select {
		case <-drains:
			t.Fatal("the relay drained after a failing half-close to the client, want teardown")
		default:
		}
		if got := ends.all(); len(got) != 1 || got[0] != "the destination's end" {
			t.Fatalf("told %q, want only the destination's end", got)
		}
	})

	t.Run("EOF forwarder skips half-close and drain once stopping is closed", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		clientSpy := &nudgeSpyConn{TCPConn: proxyClient.(*net.TCPConn), nudged: make(chan struct{})}
		destSpy := &closeWriteSpyConn{TCPConn: proxyServer.(*net.TCPConn)}
		logger, drains := newDrainLogger()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		clientEndRead, destStopping := make(chan struct{}), make(chan struct{})
		told := cfg.PeerEnded
		cfg.PeerEnded = func(dir fakeconn.Direction, reset bool) {
			told(dir, reset)
			if dir != fakeconn.FromClient {
				return
			}
			close(clientEndRead)
			select {
			case <-destStopping:
			case <-time.After(10 * time.Second):
			}
		}
		done := runRelay(t, New(cfg, &proxyutil.Conn{Conn: clientSpy, Reader: clientSpy, Logger: zap.NewNop()}, connseq.NewUpstream(destSpy)))
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
		select {
		case <-clientSpy.nudged:
		case <-time.After(10 * time.Second):
			t.Fatal("the destination's reset did not trigger teardown within 10s")
		}
		close(destStopping)
		awaitDone(t, done, "the relay did not end")
		if got := destSpy.calls.Load(); got != 0 {
			t.Fatalf("CloseWrite called %d times after teardown already started, want 0", got)
		}
		select {
		case <-drains:
			t.Fatal("the relay drained after stopping was already closed")
		default:
		}
	})

	t.Run("cancelled context suppresses opposite EOF peer end and drain", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &heldUntilSecondNudgeConn{
			TCPConn: proxyClient.(*net.TCPConn),
			release: make(chan struct{}),
			abort:   abort,
		}
		dest := &eofRaceConn{
			TCPConn:      proxyServer.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		logger, drains := newDrainLogger()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		told := cfg.PeerEnded
		cfg.PeerEnded = func(dir fakeconn.Direction, reset bool) {
			told(dir, reset)
			client.once.Do(func() { close(client.release) })
		}
		r := New(cfg, &proxyutil.Conn{Conn: client, Reader: client, Logger: zap.NewNop()}, connseq.NewUpstream(dest))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		go func() { _, _ = io.Copy(io.Discard, r.ClientStream()) }()
		go func() { _, _ = io.Copy(io.Discard, r.DestStream()) }()
		t.Cleanup(func() { cancel(); close(abort); <-done })
		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("destination forwarder did not enter Read within 10s")
		}
		cancel()
		dest.releaseOnce.Do(func() { close(dest.releaseRead) })
		awaitDone(t, done, "the relay did not end on cancel")
		select {
		case <-drains:
			t.Fatal("the relay drained after context cancellation")
		default:
		}
		if got := ends.all(); len(got) != 0 {
			t.Fatalf("told %q after context cancellation, want none", got)
		}
	})
}
