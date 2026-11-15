//go:build windows

package proxy

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// fakeSends stands in for the shim's counts (agent.AppSendCounter) of every
// connection, and counts how often it was asked.
type fakeSends struct {
	mu    sync.Mutex
	s     appSends
	err   error
	asked int
}

func (f *fakeSends) MarkProxyListener(syscall.RawConn) error { return nil }
func (f *fakeSends) ProxyConnPair(syscall.RawConn) (agent.ProxyConnPairing, error) {
	return agent.ProxyConnPairing{}, errors.New("not paired here")
}

func (f *fakeSends) AppSent(netip.AddrPort) (uint64, uint64, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	return f.s.done, f.s.started, f.s.pending, f.err
}

func (f *fakeSends) set(s appSends, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s, f.err = s, err
}

func (f *fakeSends) timesAsked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// countsNone is hooks that count no send calls.
type countsNone struct{}

func (countsNone) MarkProxyListener(syscall.RawConn) error { return nil }
func (countsNone) ProxyConnPair(syscall.RawConn) (agent.ProxyConnPairing, error) {
	return agent.ProxyConnPairing{}, errors.New("not paired here")
}

// winPair is a loopback connection: the application's end and the proxy's.
func winPair(t *testing.T) (app, side *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Errorf("close the pair's listener: %v", err)
		}
	}()
	app, err = net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	side, err = ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(); _ = side.Close() })
	return app, side
}

// until waits, for at most 20 s, for cond.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); !cond(); {
		if time.Now().After(end) {
			t.Fatalf("waited 20 s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOnWindowsTheProxysSideCountsWhatArrivedAndNoFIN holds the proxy's side's
// count (readProxySide: SIO_TCP_INFO's BytesIn) to what the wait compares
// with the shim's counts: every byte that arrived, read or not, and never the
// application's FIN, which moves its state to CLOSE_WAIT (peerEnded).
func TestOnWindowsTheProxysSideCountsWhatArrivedAndNoFIN(t *testing.T) {
	app, side := winPair(t)
	const n = 50_000
	if _, err := app.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	until(t, "the bytes to arrive", func() bool { u, err := readUnread(side); return err == nil && u == n })
	ps := readProxySide(side)
	if ps.err != nil || ps.received != n || ps.state != winTCPEstablished || peerEnded(ps.state) {
		t.Fatalf("unread: %+v; want %d received, ESTABLISHED", ps, n)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.Copy(io.Discard, side)
	if err != nil || got != n {
		t.Fatalf("read %d, %v; want %d to the FIN", got, err, n)
	}
	ps = readProxySide(side)
	if ps.err != nil || ps.received != n || !peerEnded(ps.state) || dataReceived(ps) != n {
		t.Fatalf("after the FIN: %+v (%s); want %d received, the FIN taken and not counted", ps, tcpStateName(ps.state), n)
	}
	if u, err := readUnread(side); err != nil || u != 0 {
		t.Fatalf("unread after the read: %d, %v", u, err)
	}
}

// TestOnWindowsAWaitHoldsWhileTheApplicationsSendIsPending is the 8 MB write
// that the proxy's side has received only part of while the application's
// send call is still pending: the wait wants every byte handed to the call,
// asks the counts again at each look, and is reached only once the side has
// received what the call completed with. A wait that took what had completed
// (nothing) would let the test end with the write still on its way.
func TestOnWindowsAWaitHoldsWhileTheApplicationsSendIsPending(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	f := &fakeSends{}
	p.listenerCounter = f
	app, side := winPair(t)
	if err := side.SetReadBuffer(256 << 10); err != nil {
		t.Fatal(err)
	}
	p.paired.add(side, 1, 0)
	const n = 8 << 20
	wrote := make(chan error, 1)
	go func() { _, err := app.Write(make([]byte, n)); wrote <- err }()
	until(t, "the first bytes at the proxy", func() bool { return readProxySide(side).received > 0 })
	f.set(appSends{started: n, pending: 1}, nil)
	targets, err := p.pairTargets(f, 1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %d, %v; want the connection's", len(targets), err)
	}
	tg := targets[0]
	if tg.want != n || !tg.base.sending || !tg.base.looks() {
		t.Fatalf("target: want %d, sending %v; want %d, sending", tg.want, tg.base.sending, n)
	}
	asked := f.timesAsked()
	if reached, err := p.pairReached(tg); reached || err != nil {
		t.Fatalf("with %d of %d at the proxy and the call pending: reached %v, %v", readProxySide(side).received, n, reached, err)
	}
	if f.timesAsked() == asked {
		t.Fatal("the counts were not asked again while the call was pending")
	}
	read := make(chan int64, 1)
	go func() { got, _ := io.CopyN(io.Discard, side, n); read <- got }()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if got := <-read; got != n {
		t.Fatalf("the proxy read %d of %d", got, n)
	}
	f.set(appSends{done: n, started: n}, nil)
	if reached, err := p.pairReached(tg); !reached || err != nil {
		t.Fatalf("with everything at the proxy and the call completed: reached %v, %v", reached, err)
	}
}

// TestOnWindowsACancelledSendIsWaitedForUpToWhatItCompletedWith: once the
// pending call ends having sent part of its bytes (a write deadline's cancel),
// the wait wants what it completed with, and is reached with what arrived.
func TestOnWindowsACancelledSendIsWaitedForUpToWhatItCompletedWith(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	f := &fakeSends{}
	p.listenerCounter = f
	app, side := winPair(t)
	p.paired.add(side, 1, 0)
	const part = 10_000
	if _, err := app.Write(make([]byte, part)); err != nil {
		t.Fatal(err)
	}
	until(t, "the part at the proxy", func() bool { return readProxySide(side).received == part })
	f.set(appSends{done: 0, started: 1 << 20, pending: 1}, nil)
	targets, err := p.pairTargets(f, 1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %d, %v", len(targets), err)
	}
	if reached, err := p.pairReached(targets[0]); reached || err != nil {
		t.Fatalf("pending: reached %v, %v", reached, err)
	}
	f.set(appSends{done: part, started: 1 << 20}, nil)
	if reached, err := p.pairReached(targets[0]); !reached || err != nil || targets[0].want != part {
		t.Fatalf("cancelled after %d: reached %v, %v, want %d", part, reached, err, targets[0].want)
	}
}

// TestOnWindowsAWaitThatCannotTellIsUnknown: counts that cannot be read,
// a side that received more than was handed to send calls, and hooks that
// count no send calls each answer unknown; a side that took the
// application's FIN is reached without asking the counts, which a process
// that exited no longer has.
func TestOnWindowsAWaitThatCannotTellIsUnknown(t *testing.T) {
	setup := func(t *testing.T) (*Proxy, *fakeSends, *net.TCPConn, *net.TCPConn) {
		p := &Proxy{logger: zap.NewNop(), settleReads: true}
		f := &fakeSends{}
		p.listenerCounter = f
		app, side := winPair(t)
		p.paired.add(side, 1, 0)
		if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		until(t, "the call at the proxy", func() bool { return readProxySide(side).received == 18 })
		return p, f, app, side
	}
	t.Run("counts unreadable", func(t *testing.T) {
		p, f, _, _ := setup(t)
		f.set(appSends{}, errors.New("the process exited"))
		if _, err := p.pairTargets(f, 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("pairTargets: %v; want unknown", err)
		}
	})
	t.Run("received more than was handed", func(t *testing.T) {
		p, f, _, _ := setup(t)
		f.set(appSends{done: 17, started: 17}, nil)
		if _, err := p.pairTargets(f, 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("pairTargets: %v; want unknown", err)
		}
	})
	t.Run("everything received", func(t *testing.T) {
		p, f, _, _ := setup(t)
		f.set(appSends{done: 18, started: 18}, nil)
		if targets, err := p.pairTargets(f, 1); err != nil || len(targets) != 0 {
			t.Fatalf("pairTargets: %d, %v; want none", len(targets), err)
		}
	})
	t.Run("the FIN taken", func(t *testing.T) {
		p, f, app, side := setup(t)
		_ = app.CloseWrite()
		until(t, "the FIN", func() bool { return peerEnded(readProxySide(side).state) })
		f.set(appSends{}, errors.New("the process exited"))
		if targets, err := p.pairTargets(f, 1); err != nil || len(targets) != 0 || f.timesAsked() != 0 {
			t.Fatalf("pairTargets: %d, %v, counts asked %d times; want none, not asked", len(targets), err, f.timesAsked())
		}
	})
	t.Run("hooks count no send calls", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop(), settleReads: true}
		p.listenerCounter = countsNone{}
		if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("PairedBytesErr: %v; want unknown", err)
		}
		if err := p.WaitPairedBytes(t.Context(), 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("WaitPairedBytes: %v; want unknown", err)
		}
		p.listenerCounter = &fakeSends{}
		if err := p.PairedBytesErr(); err != nil {
			t.Fatalf("PairedBytesErr with counts: %v", err)
		}
	})
}

// TestOnWindowsAConnectionClosedShortIsSettledOnlyByWhatItsCloseRead: the
// proxy closed its side short of the wait's want: settled when its side had
// received what the application had written as the close began, or when the
// close charged a loss; unknown otherwise.
func TestOnWindowsAConnectionClosedShortIsSettledOnlyByWhatItsCloseRead(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:50000")}
	ps := proxySide{received: 100, counted: true, state: winTCPEstablished}
	if _, err := p.closedReached(pc, ps); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("nothing read at the close: %v; want unknown", err)
	}
	pc.wroteAtClose.Store(&appWrote{want: 101})
	if _, err := p.closedReached(pc, ps); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("one byte short at the close: %v; want unknown", err)
	}
	pc.wroteAtClose.Store(&appWrote{want: 100})
	if reached, err := p.closedReached(pc, ps); !reached || err != nil {
		t.Fatalf("everything at the close: %v, %v", reached, err)
	}
	pc.wroteAtClose.Store(&appWrote{want: 101})
	pc.lost.Store(true)
	if reached, err := p.closedReached(pc, ps); !reached || err != nil {
		t.Fatalf("a loss charged at the close: %v, %v", reached, err)
	}
}
