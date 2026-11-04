package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/plaintcp"
)

// errListenerNotMarked is Accepted's answer while the proxy's listener is not
// one the hooks count the connections of: nothing asked for the mark, the
// proxy has no listener, or it is not listening yet.
var errListenerNotMarked = fmt.Errorf("%w: the proxy's listener is not marked for the kernel's count", models.ErrProxyConnsUnsupported)

// errListenerClosed is every count's answer once the proxy's listener has
// closed: no connection the kernel counted after it, or before it and never
// accepted, will be accepted.
var errListenerClosed = fmt.Errorf("%w: the proxy's listener has closed", models.ErrProxyConnsUnsupported)

// SetListenerCounter makes the proxy have its listener counted by the hooks
// (agent.ListenerCountTaker): the agent hands it hooks that can count, before
// StartProxy, so each connection is routed by the kernel's pairing. Unset,
// the listener is opened as it always was.
func (p *Proxy) SetListenerCounter(c agent.ListenerCounter) {
	p.listenerCounter = c
}

// listen opens the proxy's listener at addr, plain TCP either way (never Go's
// default MPTCP): with no counter, unmarked; with one, ListenMarked. A mark
// that fails leaves the listener unmarked, its counts unknown, and the proxy
// listening all the same, with a WARN.
func (p *Proxy) listen(ctx context.Context, addr string) (net.Listener, error) {
	if p.listenerCounter == nil {
		return plaintcp.Listen(context.Background(), "tcp", addr)
	}
	ln, markErr, err := ListenMarked(ctx, addr, p.listenerCounter.MarkProxyListener)
	if err != nil {
		return nil, err
	}
	p.acceptsMu.Lock()
	if markErr != nil {
		p.acceptsErr = fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, markErr)
	} else {
		p.acceptsErr, p.acceptsLn = nil, ln
	}
	p.acceptsMu.Unlock()
	if markErr != nil {
		p.warnUncounted(markErr)
	}
	return ln, nil
}

// decideCounted records, once the canary has run and before the proxy says it
// is ready, whether the kernel counts the calls waiting for the proxy: the
// listener is marked, the canary was counted, and, for a proxy that holds its
// paired connections for their bytes (holdsPairedBytes), those bytes can be
// read. A count lost later does not change it (a read that then cannot vouch
// for every call says so: models.ErrProxyConnsUnsupported); with the switch
// off a NAT'd connection does, for good (dropCounting).
func (p *Proxy) decideCounted() {
	if _, err := p.Accepted(); err != nil {
		return
	}
	if p.holdsPairedBytes() && p.PairedBytesErr() != nil {
		return
	}
	p.countedFromStart.Store(true)
}

// CountedFromStart reports whether, when the proxy came up, the kernel counted
// the calls waiting for it (decideCounted): agent.CountingReporter.
func (p *Proxy) CountedFromStart() bool {
	return p.countedFromStart.Load() || p.countingLost.Load() != nil
}

// CountingLost is why, with the switch off, the run's calls stopped being
// counted after its start (dropCounting), nil while they are: every read of
// a test's misses from then on, and one in flight then, cannot vouch for
// every miss (agent.CountingReporter). The channel is closed then.
func (p *Proxy) CountingLost() (<-chan struct{}, error) {
	if e := p.countingLost.Load(); e != nil {
		return p.countingLostCh, *e
	}
	return p.countingLostCh, nil
}

// dropCounting is, with the switch off, a counted proxy's answer to a
// connection the kernel tied to none of the application's (a NAT between
// them): it cannot be paired, and the application's connection it is the
// proxy's side of will never settle. The proxy routes that connection by its
// source port, as it always did, so the application's call goes on working;
// every other connection still goes where its own pairing says (usesPairing).
// The run is counted no more, and an agent that settles reads answers every
// later read unread (CountingLost). The first such connection is said once,
// a WARN where a read waits for the run's calls and an INFO otherwise; each
// later one at DEBUG. It reports whether the connection is routed by its
// source port (false for a proxy that was not counting).
func (p *Proxy) dropCounting(conn net.Conn) bool {
	// The count before the reason: the caller that drops it sets the reason
	// first and clears the count after, so a count read as cleared here has
	// its reason set by the time it is read below.
	counted := p.countedFromStart.Load()
	if p.countingLost.Load() != nil {
		p.logger.Debug("keploy routes a connection to its proxy that the kernel could not tie to one of the application's by its source port",
			zap.String("from", conn.RemoteAddr().String()))
		return true
	}
	if !counted {
		return false
	}
	// The reason first, and by one caller only: a connection that drops the
	// count beside this one finds it set, and is routed too; and
	// CountedFromStart (so usesPairing), which reads either, never sees
	// neither.
	err := fmt.Errorf("%w: keploy found a connection to its proxy, from %s, that the kernel could not tie to one of the application's (a NAT between them?), so it cannot attribute this run's calls to its tests", models.ErrProxyConnsUnsupported, conn.RemoteAddr())
	if !p.countingLost.CompareAndSwap(nil, &err) {
		return true
	}
	p.countedFromStart.Store(false)
	if p.countingLostCh != nil {
		close(p.countingLostCh)
	}
	if !p.holdsPairedBytes() {
		// No read of this run's waits for its calls: nothing it learns
		// depends on the count.
		p.logger.Info("keploy found a connection to its proxy that the kernel could not tie to one of the application's (a NAT between them?): it routes each such connection by its source port, as before, and every other one by its own pairing",
			zap.String("from", conn.RemoteAddr().String()),
			zap.String("next_step", "keploy's proxy must see the application's own addresses: check for a NAT between the application and the agent"))
		return true
	}
	p.logger.Warn("keploy found a NAT'd connection it cannot attribute: it routes each such connection by its source port, as before, and every other one by its own pairing, but every later read of a test's misses is unread, so no noise is learned for the test sets read after it",
		zap.String("from", conn.RemoteAddr().String()),
		zap.String("next_step", "keploy's proxy must see the application's own addresses: check for a NAT between the application and the agent"))
	return true
}

func (p *Proxy) warnUncounted(err error) {
	if !p.holdsPairedBytes() {
		// No read of this run's waits for its calls: the proxy routes them
		// by their source port, as it always did.
		p.logger.Info("keploy's hooks cannot pair the connections to its proxy with the application's, so it routes them by their source port, as before",
			zap.Error(err))
		return
	}
	p.logger.Warn("keploy cannot count the connections waiting for its proxy, so it cannot tell that every call of a test is in its hands when the test's misses are read",
		zap.Error(err),
		zap.String("next_step", "check the agent's log for why its hooks could not count the proxy's listener, and report it"))
}

// canary proves the kernel's count works before the proxy says it is ready: it
// connects to the proxy's own marked listener, accepts that connection, and
// requires its sequence number. A count that is silently broken (the mark not
// copied, k_sockops not attached) then reads as unknown from the start, never
// as settled. A connection that arrives at the listener before the canary's is
// kept for the accept loop, counted there. It does nothing for an unmarked
// listener.
//
// The proxy's stop (ctx) ends it wherever it stands: its connect, which a
// listener whose queue is full holds for as long as the kernel retries the
// SYN, takes ctx, and its accept, which waits for a connection that may never
// come, ends as the stop closes the listener (a stopping proxy accepts nothing
// more). The proxy's shutdown, which removes the run's CA, runs only once the
// proxy has said it is ready. Nothing else ended them, and a stop that came
// then waited until the CLI killed the agent for want of progress, the CA
// still installed.
func (p *Proxy) canary(ctx context.Context, ln net.Listener) {
	if _, err := p.markedListener(); err != nil {
		return
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()
	port := ln.Addr().(*net.TCPAddr).Port
	var c net.Conn
	var err error
	var d net.Dialer
	for _, host := range []string{"127.0.0.1", "::1"} {
		if c, err = d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
			break
		}
	}
	if err != nil {
		if p.canaryStopped(ctx) {
			return
		}
		err = fmt.Errorf("connect to the proxy's own listener to check the kernel's count: %w", err)
		p.accepts.Fail(fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err))
		p.warnUncounted(err)
		return
	}
	defer c.Close()
	mine := c.LocalAddr().(*net.TCPAddr).Port
	for {
		conn, err := ln.Accept()
		if err != nil {
			if p.canaryStopped(ctx) {
				return
			}
			err = fmt.Errorf("accept the proxy's own check of the kernel's count: %w", err)
			p.accepts.Fail(fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err))
			p.warnUncounted(err)
			return
		}
		if ra, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || ra.Port != mine || !ra.IP.IsLoopback() {
			p.keepEarly(conn)
			continue
		}
		ok := p.countAccept(conn)
		if ok {
			p.checkPairedBytes(c, conn)
		}
		_ = conn.Close()
		// Never handled: nothing else would forget it, or its pairing.
		p.paired.remove(conn)
		p.pairs.Delete(pairKey(conn))
		if ok {
			p.logger.Debug("the kernel counts the connections to the proxy's listener")
		}
		return
	}
}

// canaryStopped reports whether the proxy's stop (ctx) is what ended the
// canary. Then the count was never checked, and is unknown; and the
// connections it kept for the accept loop are closed: that loop sees the stop
// and returns, and must not hand one to a handler first.
func (p *Proxy) canaryStopped(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	p.accepts.Fail(fmt.Errorf("%w: the proxy was stopped before it checked the kernel's count", models.ErrProxyConnsUnsupported))
	p.closeEarly()
	p.logger.Debug("the proxy was stopped while it checked the kernel's count of its connections")
	return true
}

// keepEarly keeps a connection the canary accepted before its own, for the
// accept loop.
func (p *Proxy) keepEarly(c net.Conn) {
	p.earlyMu.Lock()
	defer p.earlyMu.Unlock()
	p.early = append(p.early, c)
}

// takeEarly hands the accept loop a connection the canary accepted before its
// own, in the order they came; none once the proxy's stop has closed them
// (closeEarly).
func (p *Proxy) takeEarly() net.Conn {
	p.earlyMu.Lock()
	defer p.earlyMu.Unlock()
	if len(p.early) == 0 {
		return nil
	}
	c := p.early[0]
	p.early = p.early[1:]
	return c
}

// closeEarly closes every connection the canary kept that the accept loop has
// not taken: the proxy has stopped, and nothing will take them. Under earlyMu,
// so a take in the loop's last round either gets a connection (which that
// round hands over, or closes at the stop) or none.
func (p *Proxy) closeEarly() {
	p.earlyMu.Lock()
	defer p.earlyMu.Unlock()
	for _, c := range p.early {
		_ = c.Close()
	}
	p.early = nil
}

// countAccept records an accepted connection's sequence number, as the kernel
// gave it, when the listener is the marked one. A number that cannot be read
// makes every count unknown from then on (with a WARN), and is false.
func (p *Proxy) countAccept(conn net.Conn) bool {
	if _, err := p.markedListener(); err != nil {
		return false
	}
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		p.failCount(fmt.Errorf("an accepted connection is not TCP (%T)", conn))
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		p.failCount(fmt.Errorf("reach an accepted connection's socket: %w", err))
		return false
	}
	var pairing agent.ProxyConnPairing
	if ap, ok := p.listenerCounter.(agent.AcceptedPeerPairer); ok {
		// The address accept() returned, kept by the conn: a connection
		// reset before the accept still has it.
		peer, _ := tc.RemoteAddr().(*net.TCPAddr)
		pairing, err = ap.ProxyConnPairPeer(rc, peer)
	} else {
		pairing, err = p.listenerCounter.ProxyConnPair(rc)
	}
	if err != nil {
		p.failCount(err)
		return false
	}
	// Kept only for a proxy that takes destinations from it: otherwise
	// pairedDest never takes the entry back.
	if p.usesPairing() {
		p.pairs.Store(pairKey(conn), pairing)
	}
	if pairing.AppSeq != 0 && pairing.Dst != nil && p.keepsConnections() {
		// Held until the proxy is done with it: a test's end waits for the
		// bytes the application writes to it (pairbytes.go). A proxy that
		// keeps no connection (an agent that settles no read) only routes
		// by the pairing: nothing would wait for the bytes.
		p.paired.add(tc, pairing.AppSeq, pairing.Dst.Instance)
		p.proveAppVisible(tc)
	}
	p.accepts.Add(pairing.Seq)
	if _, err := p.accepts.Accepted(); err != nil {
		p.failCount(err)
		return false
	}
	return true
}

// pairKey names an accepted connection by the connection itself, as the
// listener gave it to the accept loop: the handler is handed that connection,
// or the trackedConn that keeps it (inflight.go). Its two addresses would not
// do. An application connection reset while it waited for the proxy, and one
// the application then made from the same source port to the same proxy, are
// two accepted connections with the same two addresses, both waiting for the
// proxy; by them, the later one's pairing would replace the earlier one's
// before its handler took it, and send it to the later one's destination.
func pairKey(conn net.Conn) net.Conn {
	if tc, ok := conn.(*trackedConn); ok {
		return tc.Conn
	}
	return conn
}

// usesPairing reports whether the proxy takes its connections' destinations
// from the kernel's pairing: under the proven switch, and with it off for a
// proxy whose calls were counted from its start (CountedFromStart), whether
// or not its agent settles reads, and whether or not a NAT'd connection has
// since dropped the count (dropCounting): only that connection, which the
// kernel paired with none of the application's, is routed by its source
// port. One that was not counted is the proxy it always was, routing by
// source port. Which paired connections it holds for the bytes on them is
// keepsConnections'.
func (p *Proxy) usesPairing() bool {
	return provenswitch.On() || p.CountedFromStart()
}

// holdsPairedBytes reports whether the proxy is one whose reads wait for the
// bytes on its paired connections, so that whether those bytes can be read
// decides whether it is counted: under the proven switch, or for an agent
// that settles reads (settleReads). Any other proxy uses the pairing only to
// route.
func (p *Proxy) holdsPairedBytes() bool {
	return provenswitch.On() || p.settleReads
}

// pairedDest is where an accepted connection of the counted listener is
// going, from its own socket (ProxyConnPairing), when the proxy uses the
// pairing (usesPairing): ok says the counter had it. A connection the kernel
// tied to no application connection has no destination (dst nil): under the
// switch the proxy refuses it, a capture loss, rather than guess one from a
// key another connection may share; with it off that one connection is
// routed by its source port (dropCounting), and every other keeps its own
// pairing. Taken once.
func (p *Proxy) pairedDest(conn net.Conn) (dst *agent.NetworkAddress, ok bool) {
	if p.listenerCounter == nil {
		return nil, false
	}
	// Taken whether or not it is used: one stored before the count was
	// dropped (dropCounting) is not left behind.
	v, ok := p.pairs.LoadAndDelete(pairKey(conn))
	if !ok || !p.usesPairing() {
		return nil, false
	}
	pairing := v.(agent.ProxyConnPairing)
	if pairing.AppSeq == 0 || pairing.Dst == nil {
		return nil, true
	}
	return pairing.Dst, true
}

// refuseUnpaired counts, and says, an accepted connection the kernel tied to
// no connection of the application's: keploy cannot tell where it is going.
func (p *Proxy) refuseUnpaired(conn net.Conn) {
	p.losses.Add(models.CaptureLossUnpairedConn, "from "+conn.RemoteAddr().String())
	p.logger.Warn("keploy refused a connection to its proxy that the kernel could not tie to a connection of the application's: it cannot tell where the connection was going",
		zap.String("from", conn.RemoteAddr().String()),
		zap.String("next_step", "keploy's proxy must see the application's own addresses: check for a NAT between the application and the agent"))
}

func (p *Proxy) failCount(err error) {
	if _, was := p.accepts.Accepted(); was != nil {
		return // said once
	}
	p.accepts.Fail(fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err))
	p.warnUncounted(err)
}

// closeAccepts ends the counts when the listener closes: a connection still
// waiting will never be accepted, so every wait ends, now, with that said,
// rather than waiting on a count that cannot grow. Called right after the
// listener is closed, before the shutdown's slower steps.
//
// The counts fail as the listener is dropped, under acceptsMu, which every
// read of them takes first (markedListener): a read finds the listener, or
// the counts failed, never neither, which it would take for a listener never
// marked.
func (p *Proxy) closeAccepts() {
	p.acceptsMu.Lock()
	p.acceptsLn = nil
	p.accepts.Fail(errListenerClosed)
	p.acceptsMu.Unlock()
	p.paired.fail(errListenerClosed)
}

// ListenMarked listens at addr the way the proxy does when the hooks count the
// connections completed to its listener: plain TCP, never MPTCP (an MPTCP
// listener's mark would reach none of its connections), with its socket handed
// to mark before listen(), so that every connection the listener can ever
// accept is one the kernel counts. markErr says why the listener is not
// marked; the listener is open all the same. err is listen's own failure.
func ListenMarked(ctx context.Context, addr string, mark func(rc syscall.RawConn) error) (ln net.Listener, markErr error, err error) {
	marked := false
	lc := plaintcp.Config(func(_, _ string, rc syscall.RawConn) error {
		// One socket per listen; a second would be a listener the
		// first's mark does not cover.
		if marked || markErr != nil {
			markErr = errors.New("the proxy's listen made more than one socket")
			return nil
		}
		if err := mark(rc); err != nil {
			markErr = fmt.Errorf("marking it failed: %w", err)
			return nil
		}
		marked = true
		return nil
	})
	ln, err = lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if markErr == nil && !marked {
		markErr = errors.New("its listen never offered its socket to be marked")
	}
	// No TCP Fast Open, whatever the kernel's default: a Fast Open
	// connection's proxy side completes at its SYN, before the kernel numbers
	// the application's side of it, so the two would never pair.
	if markErr == nil {
		markErr = noFastOpen(ln)
	}
	return ln, markErr, nil
}

// markedListener is the marked listener, or why there is none.
func (p *Proxy) markedListener() (net.Listener, error) {
	p.acceptsMu.Lock()
	defer p.acceptsMu.Unlock()
	if p.acceptsLn != nil {
		return p.acceptsLn, nil
	}
	if p.acceptsErr != nil {
		return nil, p.acceptsErr
	}
	if _, err := p.accepts.Accepted(); err != nil {
		return nil, err
	}
	return nil, errListenerNotMarked
}

// Accepted is the highest n such that every connection the kernel numbered
// 1..n has been accepted by the proxy's marked listener. It only ever grows.
// It fails while the listener is not marked, once it has closed, or once a
// number could not be read: "unknown", never 0.
func (p *Proxy) Accepted() (uint64, error) {
	if _, err := p.markedListener(); err != nil {
		return 0, err
	}
	return p.accepts.Accepted()
}

// WaitAccepted returns once Accepted() >= n, or with ctx's error, or with
// Accepted's, at once or when it comes. It has no deadline of its own: each
// accept that completes a run of numbers wakes it.
func (p *Proxy) WaitAccepted(ctx context.Context, n uint64) error {
	if _, err := p.markedListener(); err != nil {
		return err
	}
	return p.accepts.WaitAccepted(ctx, n)
}

// ListenDrops are the kernel's counts of what it dropped at the proxy's
// marked listener (models.ListenDrops): the listener's own, then its network
// namespace's. It fails as Accepted does, and where the platform cannot read
// the listener's; the namespace's it cannot read is said in NetUnread, and
// every drop at the listener then counts.
func (p *Proxy) ListenDrops() (models.ListenDrops, error) {
	ln, err := p.markedListener()
	if err != nil {
		return models.ListenDrops{}, err
	}
	if _, err := p.accepts.Accepted(); err != nil {
		return models.ListenDrops{}, err
	}
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return models.ListenDrops{}, fmt.Errorf("%w: reach the proxy's listener: %v", models.ErrProxyConnsUnsupported, err)
	}
	own, err := listenerDrops(rc)
	if err != nil {
		return models.ListenDrops{}, err
	}
	d := models.ListenDrops{Listener: own}
	if d.Net, err = netListenDrops(rc); err != nil {
		d.Net, d.NetUnread = 0, err.Error()
	} else {
		d.NetKnown = true
	}
	return d, nil
}

// Serve is the proxy's listener and accept loop, which StartProxy runs (and
// the hooks' root test drives with the kernel's count): it listens, says on
// ready that it is listening, or why not, and accepts until ctx ends.
func (p *Proxy) Serve(ctx context.Context, ready chan<- error) error {
	return p.start(ctx, ready)
}

// Compile-time proof that the proxy counts its accepts and takes the hooks'
// counter: were a method renamed, the agent's type assertion would stop
// matching and every begin and end would read "unknown".
var (
	_ agent.AcceptCounter      = (*Proxy)(nil)
	_ agent.ListenerCountTaker = (*Proxy)(nil)
	_ agent.PairedBytesWaiter  = (*Proxy)(nil)
	_ agent.PairedBytesChecker = (*Proxy)(nil)
	_ agent.CountingReporter   = (*Proxy)(nil)
)
