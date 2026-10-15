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
// (agent.ListenerCountTaker): the agent hands it the hooks under the proven
// switch only, before StartProxy. Unset, the listener is opened as it always
// was.
func (p *Proxy) SetListenerCounter(c agent.ListenerCounter) {
	p.listenerCounter = c
}

// listen opens the proxy's listener at addr: with no counter, as it always
// did, net.Listen, which Go makes MPTCP wherever the kernel has it; with one,
// ListenMarked. A mark that fails leaves the listener unmarked, its counts
// unknown, and the proxy listening all the same, with a WARN.
func (p *Proxy) listen(ctx context.Context, addr string) (net.Listener, error) {
	if p.listenerCounter == nil {
		return net.Listen("tcp", addr)
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

func (p *Proxy) warnUncounted(err error) {
	p.logger.Warn("keploy cannot count the connections waiting for its proxy: no test's end can be verified to have every call in the proxy's hands",
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
		// Never handled: nothing else would forget it, were it paired.
		p.paired.remove(conn)
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
	pairing, err := p.listenerCounter.ProxyConnPair(rc)
	if err != nil {
		p.failCount(err)
		return false
	}
	p.pairs.Store(pairKey(conn), pairing)
	if pairing.AppSeq != 0 && pairing.Dst != nil {
		// Held until the proxy is done with it: a test's end waits for the
		// bytes the application writes to it (pairbytes.go).
		p.paired.add(tc, pairing.AppSeq)
		p.proveAppVisible(tc)
	}
	p.accepts.Add(pairing.Seq)
	if _, err := p.accepts.Accepted(); err != nil {
		p.failCount(err)
		return false
	}
	return true
}

// pairKey names an accepted connection by its two addresses, unique while it
// lives.
func pairKey(conn net.Conn) string {
	return conn.RemoteAddr().String() + "|" + conn.LocalAddr().String()
}

// pairedDest is where an accepted connection of the counted listener is
// going, from its own socket (ProxyConnPairing), under the proven switch: ok
// says the counter had it. A connection the kernel tied to no application
// connection has no destination (dst nil): the proxy refuses it, a capture
// loss, rather than guess one from a key another connection may share. Taken
// once.
func (p *Proxy) pairedDest(conn net.Conn) (dst *agent.NetworkAddress, ok bool) {
	if p.listenerCounter == nil {
		return nil, false
	}
	v, ok := p.pairs.LoadAndDelete(pairKey(conn))
	if !ok {
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
func (p *Proxy) closeAccepts() {
	p.acceptsMu.Lock()
	p.acceptsLn = nil
	p.acceptsMu.Unlock()
	p.accepts.Fail(errListenerClosed)
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
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
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
	}}
	lc.SetMultipathTCP(false)
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

// ListenDrops is the kernel's count of the packets it dropped at the proxy's
// marked listener (models.ProxyConnCounts' ListenDrops). It fails as Accepted
// does, and where the platform cannot read it.
func (p *Proxy) ListenDrops() (uint64, error) {
	ln, err := p.markedListener()
	if err != nil {
		return 0, err
	}
	if _, err := p.accepts.Accepted(); err != nil {
		return 0, err
	}
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("%w: reach the proxy's listener: %v", models.ErrProxyConnsUnsupported, err)
	}
	return listenerDrops(rc)
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
)
