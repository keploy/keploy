//go:build linux || darwin || windows

package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func newDrainObserver() (*zap.Logger, chan struct{}) {
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

// writeResetRaceConn wraps a real TCPConn (optionally under a *tls.Conn) so
// that once gate is set, Read parks until either a failing Write (data or
// close_notify) consumes the socket's pending reset error, or SetReadDeadline
// is entered before installing the past deadline. Nothing fakes io.EOF: Read
// calls TCPConn.Read directly on the real kernel socket.
type writeResetRaceConn struct {
	*net.TCPConn
	gate         atomic.Bool
	onDeadline   bool
	readEntered  chan struct{}
	releaseRead  chan struct{}
	readReturned chan struct{}
	abort        chan struct{}
	enterOnce    sync.Once
	releaseOnce  sync.Once
	readOnce     sync.Once
	mu           sync.Mutex
	readErr      error
}

func (c *writeResetRaceConn) Read(p []byte) (int, error) {
	if c.gate.Load() {
		if c.readEntered != nil {
			c.enterOnce.Do(func() { close(c.readEntered) })
		}
		select {
		case <-c.releaseRead:
		case <-c.abort:
		}
		n, err := c.TCPConn.Read(p)
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
		c.readOnce.Do(func() { close(c.readReturned) })
		return n, err
	}
	return c.TCPConn.Read(p)
}

func (c *writeResetRaceConn) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	if !c.onDeadline && c.gate.Load() && err != nil {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return n, err
}

func (c *writeResetRaceConn) SetReadDeadline(t time.Time) error {
	if c.onDeadline && c.gate.Load() && !t.IsZero() && t.Before(time.Now()) {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.TCPConn.SetReadDeadline(t)
}

func (c *writeResetRaceConn) observedReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// assertSocketReadAfterWriteReset checks that on Linux and macOS the real
// kernel socket actually returned io.EOF (proving the write consumed sk_err
// and the relay recovered the reset from peerWriteState), while on Windows
// Winsock may return either io.EOF or a sticky peer reset error.
func assertSocketReadAfterWriteReset(t *testing.T, readErr error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Logf("windows underlying socket Read after failed Write returned: %v", readErr)
		if !errors.Is(readErr, io.EOF) && !neterr.IsPeerReset(readErr) {
			t.Fatalf("underlying socket Read after failed Write returned %v, want io.EOF or peer reset", readErr)
		}
		return
	}
	if !errors.Is(readErr, io.EOF) {
		t.Fatalf("underlying socket Read after failed Write returned %v, want io.EOF", readErr)
	}
}

// TestWriteEatsPeerResetOnRealSocketsIsToldAsReset proves on real loopback TCP
// and TLS 1.2 sockets that whenever any relay write (plain data Write or TLS
// close_notify) fails with a peer reset and consumes the socket's pending
// sk_err, the opposite forwarder reports the peer's reset (and resets the
// client when the destination reset) rather than telling a clean close.
func TestWriteEatsPeerResetOnRealSocketsIsToldAsReset(t *testing.T) {
	t.Run("plain TCP data write eats destination reset", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		dest := &writeResetRaceConn{
			TCPConn:      proxyServer.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		dest.gate.Store(true)
		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		counted := CountClientWrites(proxyClient)
		r := New(cfg, &proxyutil.Conn{Conn: counted, Reader: counted, Logger: zap.NewNop()}, connseq.NewUpstream(dest))
		done := runRelay(t, r)
		t.Cleanup(func() { close(abort) })

		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("destination forwarder did not enter Read within 10s")
		}
		if err := server.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
		awaitTCPState(t, proxyServer, stateClosed, "the destination's reset")
		if _, err := app.Write([]byte("more request bytes")); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")

		assertSocketReadAfterWriteReset(t, dest.observedReadErr())
		select {
		case <-drains:
			t.Fatal("the relay drained after a data write met the destination's reset")
		default:
		}
		want := []string{"the destination's end (reset)"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q", got, want)
		}
		if !r.destReset.Load() {
			t.Fatal("destReset is false, want true so the client receives a reset")
		}
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := app.Read(make([]byte, 1)); n != 0 || !neterr.IsPeerReset(err) {
			t.Fatalf("client read %d bytes, %v; want a reset propagated from the destination", n, err)
		}
	})

	t.Run("TLS data write eats destination reset", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		dest := &writeResetRaceConn{
			TCPConn:      proxyServer.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		cert := selfSignedCert(t)
		tlsServer := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12})
		tlsDest := tls.Client(dest, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // test self-signed peer
		handshook := make(chan error, 1)
		go func() { handshook <- tlsServer.Handshake() }()
		if err := tlsDest.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-handshook; err != nil {
			t.Fatal(err)
		}
		dest.gate.Store(true)

		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		counted := CountClientWrites(proxyClient)
		r := New(cfg, &proxyutil.Conn{Conn: counted, Reader: counted, Logger: zap.NewNop()}, connseq.NewUpstream(tlsDest))
		done := runRelay(t, r)
		t.Cleanup(func() { close(abort) })

		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("destination forwarder did not enter Read within 10s")
		}
		if err := server.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
		awaitTCPState(t, proxyServer, stateClosed, "the destination's reset")
		if _, err := app.Write([]byte("more tls request bytes")); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")

		assertSocketReadAfterWriteReset(t, dest.observedReadErr())
		select {
		case <-drains:
			t.Fatal("the relay drained after a TLS data write met the destination's reset")
		default:
		}
		want := []string{"the destination's end (reset)"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q", got, want)
		}
		if !r.destReset.Load() {
			t.Fatal("destReset is false, want true so the client receives a reset")
		}
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := app.Read(make([]byte, 1)); n != 0 || !neterr.IsPeerReset(err) {
			t.Fatalf("client read %d bytes, %v; want a reset propagated from the destination", n, err)
		}
	})

	for _, tc := range []struct {
		name       string
		onDeadline bool
	}{
		{name: "TLS close_notify eats destination reset during write", onDeadline: false},
		{name: "TLS close_notify eats destination reset before read deadline", onDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
			abort := make(chan struct{})
			readEntered := make(chan struct{})
			dest := &writeResetRaceConn{
				TCPConn:      proxyServer.(*net.TCPConn),
				onDeadline:   tc.onDeadline,
				readEntered:  readEntered,
				releaseRead:  make(chan struct{}),
				readReturned: make(chan struct{}),
				abort:        abort,
			}
			cert := selfSignedCert(t)
			tlsServer := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12})
			tlsDest := tls.Client(dest, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // test self-signed peer
			handshook := make(chan error, 1)
			go func() { handshook <- tlsServer.Handshake() }()
			if err := tlsDest.Handshake(); err != nil {
				t.Fatal(err)
			}
			if err := <-handshook; err != nil {
				t.Fatal(err)
			}
			dest.gate.Store(true)

			logger, drains := newDrainObserver()
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
			r := New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(tlsDest))
			done := runRelay(t, r)
			t.Cleanup(func() { close(abort) })

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
			select {
			case <-readEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("destination forwarder did not enter Read within 10s")
			}
			close(destEnded)
			awaitDone(t, done, "the relay did not end")

			assertSocketReadAfterWriteReset(t, dest.observedReadErr())
			select {
			case <-drains:
				t.Fatal("the relay drained after a close_notify that met the destination's reset, want teardown")
			default:
			}
			want := []string{"the client's end", "the destination's end (reset)", "the client's end before the destination's"}
			if got := ends.all(); !slices.Equal(got, want) {
				t.Fatalf("told %q, want %q", got, want)
			}
			if !r.destReset.Load() {
				t.Fatal("destReset is false, want true")
			}
		})
	}

	t.Run("TLS close_notify eats client reset during write", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &writeResetRaceConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		cert := selfSignedCert(t)
		tlsProxyClient := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12})
		tlsApp := tls.Client(app, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // test self-signed peer
		handshook := make(chan error, 1)
		go func() { handshook <- tlsProxyClient.Handshake() }()
		if err := tlsApp.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-handshook; err != nil {
			t.Fatal(err)
		}
		client.gate.Store(true)

		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		destEndRead, clientEnded := make(chan struct{}), make(chan struct{})
		told := cfg.PeerEnded
		cfg.PeerEnded = func(dir fakeconn.Direction, reset bool) {
			told(dir, reset)
			if dir != fakeconn.FromDest {
				return
			}
			close(destEndRead)
			select {
			case <-clientEnded:
			case <-time.After(10 * time.Second):
			}
		}
		done := runRelay(t, New(cfg, tlsProxyClient, connseq.NewUpstream(proxyServer)))
		t.Cleanup(func() { close(abort) })

		if err := server.Close(); err != nil {
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
		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter Read within 10s")
		}
		close(clientEnded)
		awaitDone(t, done, "the relay did not end")

		assertSocketReadAfterWriteReset(t, client.observedReadErr())
		select {
		case <-drains:
			t.Fatal("the relay drained after a close_notify that met the client's reset, want teardown")
		default:
		}
		want := []string{"the destination's end", "the client's end (reset)"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q", got, want)
		}
	})

	t.Run("plain TCP data write eats client reset", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &writeResetRaceConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		client.gate.Store(true)
		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		done := runRelay(t, New(cfg, client, connseq.NewUpstream(proxyServer)))
		t.Cleanup(func() { close(abort) })

		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter Read within 10s")
		}
		if err := app.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		awaitTCPState(t, proxyClient, stateClosed, "the client's reset")
		if _, err := server.Write([]byte("more response bytes")); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")

		assertSocketReadAfterWriteReset(t, client.observedReadErr())
		select {
		case <-drains:
			t.Fatal("the relay drained after a data write met the client's reset")
		default:
		}
		want := []string{"the client's end (reset)"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q", got, want)
		}
	})
}

// simulatedWriteResetConn deterministically interleaves a failing Write or
// CloseWrite (returning writeErr) with Read returning io.EOF on every OS,
// including Windows where Winsock may keep recv errors sticky.
type simulatedWriteResetConn struct {
	*net.TCPConn
	failDataWrite bool
	onDeadline    bool
	writeErr      error
	readEntered   chan struct{}
	releaseRead   chan struct{}
	readReturned  chan struct{}
	abort         chan struct{}
	enterOnce     sync.Once
	releaseOnce   sync.Once
	readOnce      sync.Once
}

func (c *simulatedWriteResetConn) SyscallConn() (syscall.RawConn, error) {
	return nil, errors.New("simulated conn has no raw socket")
}

func (c *simulatedWriteResetConn) Read(_ []byte) (int, error) {
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

func (c *simulatedWriteResetConn) Write(p []byte) (int, error) {
	if c.failDataWrite {
		if !c.onDeadline {
			c.releaseOnce.Do(func() { close(c.releaseRead) })
			select {
			case <-c.readReturned:
			case <-c.abort:
			}
		}
		return 0, c.writeErr
	}
	return c.TCPConn.Write(p)
}

func (c *simulatedWriteResetConn) CloseWrite() error {
	if !c.failDataWrite && !c.onDeadline {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.writeErr
}

func (c *simulatedWriteResetConn) SetReadDeadline(t time.Time) error {
	if c.onDeadline && !t.IsZero() && t.Before(time.Now()) {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.TCPConn.SetReadDeadline(t)
}

// closeWriteSpyConn counts CloseWrite calls while delegating to TCPConn.
type closeWriteSpyConn struct {
	*net.TCPConn
	calls atomic.Int32
}

func (c *closeWriteSpyConn) CloseWrite() error {
	c.calls.Add(1)
	return c.TCPConn.CloseWrite()
}

// nudgeSpyConn records when a past read deadline is installed on TCPConn.
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

// releaseBeforeDeadlineConn holds reads after the first until SetReadDeadline
// is called with a past deadline, then lets Read execute against the real
// socket BEFORE installing the past deadline so a genuine peer FIN already in
// the socket is read after stopping is closed. It exposes NetConn so
// clientSocketOf still inspects the real client TCP socket.
type releaseBeforeDeadlineConn struct {
	*net.TCPConn
	reads        atomic.Int32
	readEntered  chan struct{}
	releaseRead  chan struct{}
	readReturned chan struct{}
	abort        chan struct{}
	enterOnce    sync.Once
	releaseOnce  sync.Once
	readOnce     sync.Once
}

func (c *releaseBeforeDeadlineConn) NetConn() net.Conn { return c.TCPConn }

func (c *releaseBeforeDeadlineConn) Read(p []byte) (int, error) {
	if c.reads.Add(1) > 1 {
		c.enterOnce.Do(func() { close(c.readEntered) })
		select {
		case <-c.releaseRead:
		case <-c.abort:
		}
		n, err := c.TCPConn.Read(p)
		c.readOnce.Do(func() { close(c.readReturned) })
		return n, err
	}
	return c.TCPConn.Read(p)
}

func (c *releaseBeforeDeadlineConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.TCPConn.SetReadDeadline(t)
}

// blockingWriteConn blocks Write on an open TCP socket (simulating a full send
// buffer) while exposing NetConn so socketKnownOpenForWrite sees TCP_INFO.
type blockingWriteConn struct {
	*net.TCPConn
	writeEntered chan struct{}
	unblock      chan struct{}
	enterOnce    sync.Once
}

func (c *blockingWriteConn) NetConn() net.Conn { return c.TCPConn }

func (c *blockingWriteConn) Write(p []byte) (int, error) {
	c.enterOnce.Do(func() { close(c.writeEntered) })
	<-c.unblock
	return c.TCPConn.Write(p)
}

// TestWriteResetAndGenuineFINOrderingAcrossOSes pins every portable B1 and B2
// invariant across Linux, macOS, and Windows:
//   - Write/CloseWrite reset errors (ECONNRESET and EPIPE) are recovered as
//     resets when the opposite Read returns io.EOF.
//   - A genuine client FIN racing a destination reset is still reported as a
//     FIN identically across Linux, macOS, and Windows (never dropped on
//     stopping), while onForwarderExit skips half-close and drain once stopping
//     is closed.
//   - A client FIN that arrived before a write-observed reset still reports
//     the client's FIN (not a reset), with the reset handed to ClientEndError.
//   - A genuine peer EOF racing context cancellation still reports the peer's
//     FIN while skipping half-close and drain.
//   - A blocked data Write on an open/half-closed TCP socket does not block a
//     genuine destination FIN.
func TestWriteResetAndGenuineFINOrderingAcrossOSes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failDataWrite bool
		onDeadline    bool
		writeErr      error
		want          []string
	}{
		{
			name:          "dest read EOF during failed CloseWrite with ECONNRESET tells reset",
			failDataWrite: false,
			writeErr:      syscall.ECONNRESET,
			want:          []string{"the client's end", "the destination's end (reset)", "the client's end before the destination's"},
		},
		{
			name:          "dest read EOF during failed CloseWrite with EPIPE tells reset",
			failDataWrite: false,
			writeErr:      syscall.EPIPE,
			want:          []string{"the client's end", "the destination's end (reset)", "the client's end before the destination's"},
		},
		{
			name:          "dest read EOF after failed CloseWrite before deadline tells reset",
			failDataWrite: false,
			onDeadline:    true,
			writeErr:      syscall.ECONNRESET,
			want:          []string{"the client's end", "the destination's end (reset)", "the client's end before the destination's"},
		},
		{
			name:          "dest read EOF during failed data Write with EPIPE tells reset",
			failDataWrite: true,
			writeErr:      syscall.EPIPE,
			want:          []string{"the destination's end (reset)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
			abort := make(chan struct{})
			readEntered := make(chan struct{})
			dest := &simulatedWriteResetConn{
				TCPConn:       proxyServer.(*net.TCPConn),
				failDataWrite: tc.failDataWrite,
				onDeadline:    tc.onDeadline,
				writeErr:      tc.writeErr,
				readEntered:   readEntered,
				releaseRead:   make(chan struct{}),
				readReturned:  make(chan struct{}),
				abort:         abort,
			}
			logger, drains := newDrainObserver()
			ends := newEndsTold()
			cfg := ends.config()
			cfg.Logger = logger
			r := New(cfg, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(dest))
			done := runRelay(t, r)
			t.Cleanup(func() { close(abort) })

			select {
			case <-readEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("destination forwarder did not enter Read within 10s")
			}
			if tc.failDataWrite {
				if _, err := app.Write([]byte("trigger write reset")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
			}
			awaitDone(t, done, "the relay did not end")
			select {
			case <-drains:
				t.Fatal("the relay drained after a write-observed reset, want teardown")
			default:
			}
			if got := ends.all(); !slices.Equal(got, tc.want) {
				t.Fatalf("told %q, want %q", got, tc.want)
			}
			if !r.destReset.Load() {
				t.Fatal("destReset is false, want true")
			}
		})
	}

	t.Run("client read EOF during failed CloseWrite tells reset", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &simulatedWriteResetConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			writeErr:     syscall.ECONNRESET,
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		done := runRelay(t, New(cfg, client, connseq.NewUpstream(proxyServer)))
		t.Cleanup(func() { close(abort) })

		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter Read within 10s")
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")
		select {
		case <-drains:
			t.Fatal("the relay drained after a failing CloseWrite to client, want teardown")
		default:
		}
		want := []string{"the destination's end", "the client's end (reset)"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q", got, want)
		}
	})

	t.Run("client FIN racing destination reset tells both ends identically across OSes", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &releaseBeforeDeadlineConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		destSpy := &closeWriteSpyConn{TCPConn: proxyServer.(*net.TCPConn)}
		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		r := New(cfg, &proxyutil.Conn{Conn: client, Reader: client, Logger: zap.NewNop()}, connseq.NewUpstream(destSpy))
		done := runRelay(t, r)
		t.Cleanup(func() { close(abort) })

		req := "GET / HTTP/1.1\r\n\r\n"
		if _, err := app.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		readN(t, server, len(req))
		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter second Read within 10s")
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN")
		if err := server.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
		awaitDone(t, done, "the relay did not end")

		want := []string{"the destination's end (reset)", "the client's end before the destination's", "the client's end"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q (identical verdict on Linux, macOS, and Windows)", got, want)
		}
		if r.order.ClientClosed() == 0 {
			t.Fatal("order.ClientClosed was not recorded for the genuine client FIN")
		}
		if got := destSpy.calls.Load(); got != 0 {
			t.Fatalf("CloseWrite called %d times after teardown started, want 0", got)
		}
		select {
		case <-drains:
			t.Fatal("the relay drained after teardown had already started")
		default:
		}
	})

	t.Run("client FIN before write reset still tells client FIN", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &releaseBeforeDeadlineConn{
			TCPConn:      proxyClient.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		var endErrMu sync.Mutex
		var endErrs []error
		ends := newEndsTold()
		cfg := ends.config()
		cfg.ClientEndError = func(err error) {
			endErrMu.Lock()
			endErrs = append(endErrs, err)
			endErrMu.Unlock()
		}
		counted := CountClientWrites(client)
		r := New(cfg, &proxyutil.Conn{Conn: counted, Reader: counted, Logger: zap.NewNop()}, connseq.NewUpstream(proxyServer))
		done := runRelay(t, r)
		t.Cleanup(func() { close(abort) })

		if _, err := app.Write([]byte("req")); err != nil {
			t.Fatal(err)
		}
		readN(t, server, 3)
		select {
		case <-readEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter second Read within 10s")
		}
		// Client closes its socket (sends FIN); proxyClient enters CLOSE_WAIT.
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN")
		// First chunk from server causes clientFINBefore() to observe
		// CLOSE_WAIT (setting r.clientFIN=true), and the closed client resets
		// proxyClient on receipt.
		if _, err := server.Write([]byte("first")); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the first chunk")
		// Second chunk from server hits the reset client socket: FromDest's
		// Write fails with EPIPE/ECONNRESET, recording r.srcWrites.resetErr
		// and triggering teardown (which nudges client and releases Read).
		if _, err := server.Write([]byte("second")); err != nil {
			t.Fatal(err)
		}
		awaitDone(t, done, "the relay did not end")

		got := ends.all()
		if !slices.Contains(got, "the client's end") || slices.Contains(got, "the client's end (reset)") {
			t.Fatalf("told %q, want the client's FIN (not a reset) since FIN arrived before the reset", got)
		}
		endErrMu.Lock()
		defer endErrMu.Unlock()
		if len(endErrs) != 1 || !isWritePeerReset(endErrs[0]) {
			t.Fatalf("ClientEndError = %v, want once with the client's reset", endErrs)
		}
	})

	t.Run("EOF forwarder skips half-close and drain once stopping is closed", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		clientSpy := &nudgeSpyConn{TCPConn: proxyClient.(*net.TCPConn), nudged: make(chan struct{})}
		destSpy := &closeWriteSpyConn{TCPConn: proxyServer.(*net.TCPConn)}
		logger, drains := newDrainObserver()
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

	t.Run("genuine EOF racing ctx cancel tells FIN without draining", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		clientSpy := &closeWriteSpyConn{TCPConn: proxyClient.(*net.TCPConn)}
		dest := &simulatedWriteResetConn{
			TCPConn:      proxyServer.(*net.TCPConn),
			readEntered:  readEntered,
			releaseRead:  make(chan struct{}),
			readReturned: make(chan struct{}),
			abort:        abort,
		}
		logger, drains := newDrainObserver()
		ends := newEndsTold()
		cfg := ends.config()
		cfg.Logger = logger
		r := New(cfg, &proxyutil.Conn{Conn: clientSpy, Reader: clientSpy, Logger: zap.NewNop()}, connseq.NewUpstream(dest))
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
		want := []string{"the destination's end"}
		if got := ends.all(); !slices.Equal(got, want) {
			t.Fatalf("told %q, want %q: a genuine peer EOF must not be dropped on cancel", got, want)
		}
		if got := clientSpy.calls.Load(); got != 0 {
			t.Fatalf("CloseWrite called %d times after ctx cancel, want 0", got)
		}
		select {
		case <-drains:
			t.Fatal("the relay drained after ctx cancel")
		default:
		}
	})

	t.Run("blocked data Write on open socket does not block genuine destination FIN", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		dest := &blockingWriteConn{
			TCPConn:      proxyServer.(*net.TCPConn),
			writeEntered: make(chan struct{}),
			unblock:      make(chan struct{}),
		}
		var unblockOnce sync.Once
		unblock := func() { unblockOnce.Do(func() { close(dest.unblock) }) }
		ends := newEndsTold()
		done := runRelay(t, New(ends.config(), &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(dest)))
		t.Cleanup(unblock)

		if _, err := app.Write([]byte("blocked upload")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-dest.writeEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client forwarder did not enter blocked Write within 10s")
		}
		// Destination half-closes cleanly (CLOSE_WAIT, not TCP_CLOSE) while
		// FromClient's data Write is still blocked.
		if err := server.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_ = app.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("client read %d bytes, %v; want io.EOF forwarded promptly without waiting on blocked data Write", n, err)
		}
		unblock()
		_ = app.Close()
		awaitDone(t, done, "the relay did not end")
		if got := ends.all(); !slices.Contains(got, "the destination's end") {
			t.Fatalf("told %q, want the destination's genuine FIN", got)
		}
	})
}
