//go:build linux

package relay

import (
	"errors"
	"net"
	"strings"
	"sync"
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
