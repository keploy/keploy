//go:build windows

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
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
	if ps.err != nil || ps.received != n || ps.state != winTCPCloseWait || !peerEnded(ps.state) || dataReceived(ps) != n {
		t.Fatalf("after the FIN: %+v (%s); want %d received, the FIN taken and not counted", ps, tcpStateName(ps.state), n)
	}
	if u, err := readUnread(side); err != nil || u != 0 {
		t.Fatalf("unread after the read: %d, %v", u, err)
	}
}

// TestOnWindowsASideInTimeWaitIsReadAndHasTakenTheFIN: a proxy's side that
// sent its FIN first (CloseWrite) and then took the application's is in
// TIME_WAIT, TCPSTATE's last value, with its socket still open: read, and
// counted as having taken the FIN.
func TestOnWindowsASideInTimeWaitIsReadAndHasTakenTheFIN(t *testing.T) {
	app, side := winPair(t)
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
	if err := side.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, app); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	until(t, "TIME_WAIT", func() bool { ps := readProxySide(side); return ps.err != nil || ps.state == winTCPTimeWait })
	if ps := readProxySide(side); ps.err != nil || ps.state != winTCPTimeWait || ps.received != 4 || !peerEnded(ps.state) {
		t.Fatalf("in TIME_WAIT: %+v (%s); want read, 4 received, the FIN taken", ps, tcpStateName(ps.state))
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
	if targets[0].base.sending || targets[0].base.looks() {
		t.Fatal("the target still waits for send calls once the counts say none is pending")
	}
}

// TestOnWindowsAWaitIsMovedByTheSendsTheShimSeesComplete: a wait on a
// connection whose send call is pending looks on its own, and a send the shim
// saw complete since the last look is progress, so the wait never answers
// unknown while the counts move, however long it runs. It runs on a clock the
// test moves (stall.ManualClock); each look is 3/4 of the wait's budget after
// the last.
func TestOnWindowsAWaitIsMovedByTheSendsTheShimSeesComplete(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	clk := &stall.ManualClock{}
	p.paired.clk = clk
	f := &fakeSends{}
	p.listenerCounter = f
	app, side := winPair(t)
	p.paired.add(side, 1, 0)
	const part = 10_000
	if _, err := app.Write(make([]byte, part)); err != nil {
		t.Fatal(err)
	}
	until(t, "the part at the proxy", func() bool { return readProxySide(side).received == part })
	f.set(appSends{started: 1 << 20, pending: 1}, nil)
	targets, err := p.pairTargets(f, 1)
	if err != nil || len(targets) != 1 || !targets[0].base.looks() {
		t.Fatalf("targets: %d, %v; want the connection's, looked at on its own", len(targets), err)
	}
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets) }()
	look := func(why string) {
		t.Helper()
		for clk.Pending() == 0 {
			select {
			case err := <-done:
				t.Fatalf("the wait answered (%v) %s", err, why)
			default:
				runtime.Gosched()
			}
		}
	}
	look("at its first look")
	for i := uint64(1); i <= 4; i++ {
		f.set(appSends{done: i * 1000, started: 1 << 20, pending: 1}, nil)
		clk.Advance(orphanFINBudget * 3 / 4)
		look(fmt.Sprintf("with the shim's done count moved to %d since its last look", i*1000))
	}
	f.set(appSends{done: part, started: 1 << 20}, nil)
	clk.Pass()
	if err := <-done; err != nil {
		t.Fatalf("the wait = %v; want settled: the call ended with %d, all at the proxy", err, part)
	}
}

// TestOnWindowsAWaitWithNoSendPendingNeedsNoCounts: once no send call is
// pending, the wait wants what the calls completed with and never asks the
// counts again (a process that exited no longer has them); it is reached when
// exactly that has arrived, or when the side took the application's FIN, which
// comes after everything it wrote.
func TestOnWindowsAWaitWithNoSendPendingNeedsNoCounts(t *testing.T) {
	setup := func(t *testing.T) (*Proxy, *fakeSends, *pairTarget, *net.TCPConn, *net.TCPConn) {
		p := &Proxy{logger: zap.NewNop(), settleReads: true}
		f := &fakeSends{}
		p.listenerCounter = f
		app, side := winPair(t)
		p.paired.add(side, 1, 0)
		if _, err := app.Write(make([]byte, 18)); err != nil {
			t.Fatal(err)
		}
		until(t, "the first call at the proxy", func() bool { return readProxySide(side).received == 18 })
		f.set(appSends{done: 36, started: 36}, nil)
		targets, err := p.pairTargets(f, 1)
		if err != nil || len(targets) != 1 || targets[0].want != 36 || targets[0].base.sending {
			t.Fatalf("targets: %d, %v; want the connection's, wanting 36, nothing pending", len(targets), err)
		}
		f.set(appSends{}, errors.New("the process exited"))
		return p, f, targets[0], app, side
	}
	t.Run("short, then everything", func(t *testing.T) {
		p, f, tg, app, side := setup(t)
		asked := f.timesAsked()
		if reached, err := p.pairReached(tg); reached || err != nil {
			t.Fatalf("18 of 36 at the proxy: reached %v, %v; want waiting", reached, err)
		}
		if _, err := app.Write(make([]byte, 18)); err != nil {
			t.Fatal(err)
		}
		until(t, "the second call at the proxy", func() bool { return readProxySide(side).received == 36 })
		if reached, err := p.pairReached(tg); !reached || err != nil {
			t.Fatalf("36 of 36 at the proxy: reached %v, %v; want reached", reached, err)
		}
		if n := f.timesAsked() - asked; n != 0 {
			t.Fatalf("the counts were asked %d times with no call pending; want none", n)
		}
	})
	t.Run("short, then the FIN", func(t *testing.T) {
		p, f, tg, app, side := setup(t)
		asked := f.timesAsked()
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		until(t, "the FIN", func() bool { return peerEnded(readProxySide(side).state) })
		if reached, err := p.pairReached(tg); !reached || err != nil {
			t.Fatalf("the FIN after 18 of 36: reached %v, %v; want reached", reached, err)
		}
		if n := f.timesAsked() - asked; n != 0 {
			t.Fatalf("the counts were asked %d times with no call pending; want none", n)
		}
	})
}

// TestOnWindowsAConnectionTheProxyClosedNeedsNoCountsAndItsCloseReadsThem: a
// wait asks no counts of a connection the proxy closed (its close settled
// it), and the close reads what the application had written as it began:
// what its calls completed with, or every byte handed to one still pending.
func TestOnWindowsAConnectionTheProxyClosedNeedsNoCountsAndItsCloseReadsThem(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	f := &fakeSends{}
	p.listenerCounter = f
	app, side := winPair(t)
	p.paired.add(side, 1, 0)
	if _, err := app.Write(make([]byte, 18)); err != nil {
		t.Fatal(err)
	}
	until(t, "the call at the proxy", func() bool { return readProxySide(side).received == 18 })
	pcs, err := p.paired.upTo(1)
	if err != nil || len(pcs) != 1 {
		t.Fatalf("paired: %d, %v", len(pcs), err)
	}
	pc := pcs[0]
	for _, c := range []struct {
		s    appSends
		want uint64
	}{{appSends{done: 18, started: 40}, 18}, {appSends{done: 18, started: 40, pending: 1}, 40}} {
		pc.wroteAtClose.Store(nil)
		f.set(c.s, nil)
		p.noteWroteAtClose(pc)
		if w := pc.wroteAtClose.Load(); w == nil || w.want != c.want {
			t.Fatalf("the close read %+v from %+v; want %d", w, c.s, c.want)
		}
	}
	ps := readProxySide(side)
	pc.atClose.Store(&ps)
	p.paired.remove(side)
	f.set(appSends{}, errors.New("the process exited"))
	asked := f.timesAsked()
	if tg, err := pairTargetOf(f, pc); tg != nil || err != nil {
		t.Fatalf("closed: target %v, %v; want none", tg, err)
	}
	if n := f.timesAsked() - asked; n != 0 {
		t.Fatalf("the counts were asked %d times for a connection the proxy closed; want none", n)
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
