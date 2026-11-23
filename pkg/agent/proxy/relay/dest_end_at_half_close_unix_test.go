//go:build linux || darwin

package relay

import (
	"crypto/tls"
	"errors"
	"io"
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
)

// tlsCloseNotifyRaceConn wraps a real TCPConn under a *tls.Conn so that once
// armed (after the TLS handshake), the opposite forwarder's real Read runs
// either immediately after the failing Write(close_notify) consumes the
// socket's pending reset error (before CloseWrite returns) or inside
// SetReadDeadline before the past deadline is installed on the socket.
// Neither path simulates EOF: the real kernel socket returns io.EOF because
// Write(close_notify) consumed sk_err.
type tlsCloseNotifyRaceConn struct {
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

func (c *tlsCloseNotifyRaceConn) Read(p []byte) (int, error) {
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

func (c *tlsCloseNotifyRaceConn) Write(p []byte) (int, error) {
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

func (c *tlsCloseNotifyRaceConn) SetReadDeadline(t time.Time) error {
	if c.onDeadline && c.gate.Load() && !t.IsZero() && t.Before(time.Now()) {
		c.releaseOnce.Do(func() { close(c.releaseRead) })
		select {
		case <-c.readReturned:
		case <-c.abort:
		}
	}
	return c.TCPConn.SetReadDeadline(t)
}

func (c *tlsCloseNotifyRaceConn) observedReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// TestOverTLSRealSocketResetEatenByCloseNotifyIsNotToldAsAClose proves on real
// loopback TCP sockets and real TLS 1.2 connections that when a peer's reset
// is consumed by the relay's TLS close_notify write (leaving the kernel socket
// to return bare io.EOF on the opposite forwarder's Read), the relay never
// reports that reset as a clean close and never drains the opposite direction.
func TestOverTLSRealSocketResetEatenByCloseNotifyIsNotToldAsAClose(t *testing.T) {
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
		{name: "destination reset eaten during close_notify write", onDeadline: false},
		{name: "destination reset eaten before read deadline takes effect", onDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
			abort := make(chan struct{})
			readEntered := make(chan struct{})
			dest := &tlsCloseNotifyRaceConn{
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

			logger, drains := newDrainLogger()
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

			if err := dest.observedReadErr(); !errors.Is(err, io.EOF) {
				t.Fatalf("underlying socket Read after failed close_notify returned %v, want io.EOF", err)
			}
			select {
			case <-drains:
				t.Fatal("the relay drained after a close_notify that met the destination's reset, want teardown")
			default:
			}
			if got := ends.all(); len(got) != 1 || got[0] != "the client's end" {
				t.Fatalf("told %q, want only the client's end", got)
			}
		})
	}

	t.Run("client reset eaten during close_notify write over TLS", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close(); _ = proxyServer.Close(); _ = server.Close() })
		abort := make(chan struct{})
		readEntered := make(chan struct{})
		client := &tlsCloseNotifyRaceConn{
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

		logger, drains := newDrainLogger()
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

		if err := client.observedReadErr(); !errors.Is(err, io.EOF) {
			t.Fatalf("underlying client socket Read after failed close_notify returned %v, want io.EOF", err)
		}
		select {
		case <-drains:
			t.Fatal("the relay drained after a close_notify that met the client's reset, want teardown")
		default:
		}
		if got := ends.all(); len(got) != 1 || got[0] != "the destination's end" {
			t.Fatalf("told %q, want only the destination's end", got)
		}
	})
}
