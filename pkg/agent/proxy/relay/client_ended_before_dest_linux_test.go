//go:build linux

package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// endsTold is what a relay told, in order, of a connection's ends: each side's
// end (Config.PeerEnded) and the client's end before the destination's
// reached it (Config.ClientEndedBeforeDest).
type endsTold struct {
	mu     sync.Mutex
	told   []string
	before chan struct{}
	once   sync.Once
}

func newEndsTold() *endsTold { return &endsTold{before: make(chan struct{})} }

func (e *endsTold) note(s string) {
	e.mu.Lock()
	e.told = append(e.told, s)
	e.mu.Unlock()
}

func (e *endsTold) config() Config {
	return Config{Logger: zap.NewNop(),
		PeerEnded: func(dir fakeconn.Direction, reset bool) {
			what := "the client's end"
			if dir == fakeconn.FromDest {
				what = "the destination's end"
			}
			if reset {
				what += " (reset)"
			}
			e.note(what)
		},
		ClientEndedBeforeDest: func() {
			e.note("the client's end before the destination's")
			e.once.Do(func() { close(e.before) })
		},
	}
}

func (e *endsTold) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.told...)
}

// runRelay runs r and returns a channel closed once it has ended; the
// streams are drained.
func runRelay(t *testing.T, r *Relay) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	go func() { _, _ = io.Copy(io.Discard, r.ClientStream()) }()
	go func() { _, _ = io.Copy(io.Discard, r.DestStream()) }()
	t.Cleanup(func() { cancel(); <-done })
	return done
}

func awaitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s within 10s", what)
	}
}

// As the destination's forwarder reads the destination's end, its FIN or its
// reset, it looks at the client's socket before it forwards that end: a client
// that had ended its side by then is told (Config.ClientEndedBeforeDest), once
// the destination's end is told, whether the relay had read the client's end
// yet or not: its FIN in at the socket while the client's forwarder had not
// read it (a busy host), its FIN read already, or the connection reset by the
// client. A client that ends its side only once it has the destination's end,
// and one whose socket cannot be asked, are not told of.
func TestTheClientsEndBeforeTheDestinationsIsToldByALookAtItsSocket(t *testing.T) {
	for _, row := range []struct {
		name string
		// client: how the client ends its side before the destination ends
		// its own: "fin", "reset", or "" (it closes only once it has the
		// destination's end).
		client string
		// unread: the client's forwarder does not read the client's end
		// until the destination's end is told.
		unread    bool
		destReset bool
		// want is what is told, in order; then, what may be told after it:
		// the client's end, which its forwarder reads once it is let go,
		// unless the relay's teardown, as it ends the destination's side,
		// stops it first.
		want []string
		then string
	}{
		{name: "its FIN, unread", client: "fin", unread: true,
			want: []string{"the destination's end", "the client's end before the destination's"}, then: "the client's end"},
		{name: "its FIN, unread, the destination resets", client: "fin", unread: true, destReset: true,
			want: []string{"the destination's end (reset)", "the client's end before the destination's"}, then: "the client's end"},
		{name: "its FIN, read", client: "fin",
			want: []string{"the client's end", "the destination's end", "the client's end before the destination's"}},
		{name: "its reset, unread", client: "reset", unread: true,
			want: []string{"the destination's end", "the client's end before the destination's"}, then: "the client's end (reset)"},
		{name: "after the destination's end", client: "",
			want: []string{"the destination's end", "the client's end"}},
		{name: "after the destination's reset", client: "", destReset: true,
			want: []string{"the destination's end (reset)"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			gate := &gatedConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			if !row.unread {
				release()
			}
			ends := newEndsTold()
			done := runRelay(t, New(ends.config(), gate, connseq.NewUpstream(proxyServer)))
			// Registered after runRelay's, so it runs first: a row that fails
			// with the client's forwarder held lets it go, and the relay ends.
			t.Cleanup(release)
			// The request: the client's forwarder's first read.
			if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			readN(t, server, len("GET / HTTP/1.1\r\n\r\n"))
			switch row.client {
			case "fin":
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE_WAIT, "the client's FIN")
				if !row.unread {
					_ = server.SetReadDeadline(time.Now().Add(10 * time.Second))
					if n, err := server.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
						t.Fatalf("the destination read %d byte(s), %v, want the client's FIN forwarded", n, err)
					}
				}
			case "reset":
				if err := app.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = app.Close()
				awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the client's reset")
			}
			if row.destReset {
				if err := server.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			_ = server.Close()
			if row.client == "" {
				_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
				n, err := app.Read(make([]byte, 1))
				if n != 0 || !row.destReset && !errors.Is(err, io.EOF) || row.destReset && !errors.Is(err, syscall.ECONNRESET) {
					t.Fatalf("the client read %d byte(s), %v, want the destination's end", n, err)
				}
				_ = app.Close()
			} else if row.unread {
				select {
				case <-ends.before:
				case <-time.After(10 * time.Second):
					t.Fatalf("not told within 10s that the client's end came first; told %q", ends.all())
				}
				release()
			}
			awaitDone(t, done, "the relay did not end")
			got := ends.all()
			if row.then != "" && len(got) == len(row.want)+1 && got[len(got)-1] == row.then {
				got = got[:len(row.want)]
			}
			if len(got) != len(row.want) {
				t.Fatalf("told %q, want %q (then %q, or not)", ends.all(), row.want, row.then)
			}
			for i := range got {
				if got[i] != row.want[i] {
					t.Fatalf("told %q, want %q (then %q, or not)", ends.all(), row.want, row.then)
				}
			}
		})
	}
}

// Where the client's socket cannot be asked (a client connection that is no
// TCP socket), the relay does not say the client's end came first, though it
// did: nothing is told but the ends, in the order they are read.
func TestTheClientsEndBeforeTheDestinationsIsNotToldWithNoSocketToAsk(t *testing.T) {
	t.Parallel()
	app, proxyClient := net.Pipe()
	proxyServer, server := loopbackPair(t)
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close() })
	ends := newEndsTold()
	done := runRelay(t, New(ends.config(), proxyClient, connseq.NewUpstream(proxyServer)))
	if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	readN(t, server, len("GET / HTTP/1.1\r\n\r\n"))
	_ = app.Close()
	_ = server.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := server.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("the destination read %d byte(s), %v, want the client's end forwarded", n, err)
	}
	_ = server.Close()
	awaitDone(t, done, "the relay did not end")
	for _, s := range ends.all() {
		if s == "the client's end before the destination's" {
			t.Fatalf("told %q with no socket to ask", ends.all())
		}
	}
}

// A relay told of no end before the destination's (Config.ClientEndedBeforeDest
// nil) takes no look at the client's socket as it reads the destination's end:
// the looks it takes are the ones it took before it could be told. One that is
// told takes that one look, and tells nothing of a client still open.
func TestTheDestinationsEndTakesALookOnlyForARelayToldOfIt(t *testing.T) {
	for _, told := range []bool{false, true} {
		t.Run(map[bool]string{false: "not told", true: "told"}[told], func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			ends := newEndsTold()
			cfg := ends.config()
			if !told {
				cfg.ClientEndedBeforeDest = nil
			}
			r := New(cfg, proxyClient, connseq.NewUpstream(proxyServer))
			looks := &countedLooks{clientSocket: r.client}
			r.client = looks
			done := runRelay(t, r)
			if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			readN(t, server, len("GET / HTTP/1.1\r\n\r\n"))
			_ = server.Close()
			_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
			if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("the client read %d byte(s), %v, want the destination's FIN", n, err)
			}
			want := int64(0)
			if told {
				want = 1
			}
			if n := looks.n.Load(); n != want {
				t.Fatalf("%d looks at the client's socket by the time the destination's FIN reached it, want %d", n, want)
			}
			_ = app.Close()
			awaitDone(t, done, "the relay did not end")
			for _, s := range ends.all() {
				if s == "the client's end before the destination's" {
					t.Fatalf("told %q of a client that ended its side only once it had the destination's end", ends.all())
				}
			}
		})
	}
}
