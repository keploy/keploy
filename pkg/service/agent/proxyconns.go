package agent

import (
	"context"
	"errors"
	"fmt"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// proxyConnSides are the two counters ProxyConns compares: the hooks' kernel
// count of the connections completed to the proxy's marked listener, and the
// proxy's count of what that listener accepted.
func (a *Agent) proxyConnSides() (coreAgent.ProxyConnsReporter, coreAgent.AcceptCounter, error) {
	h, ok := a.Hooks.(coreAgent.ProxyConnsReporter)
	if !ok {
		return nil, nil, models.ErrProxyConnsUnsupported
	}
	p, ok := a.Proxy.(coreAgent.AcceptCounter)
	if !ok {
		return nil, nil, models.ErrProxyConnsUnsupported
	}
	return h, p, nil
}

// ProxyConns reads the counts of the connections to the proxy
// (models.ProxyConnCounts): what the proxy accepted (every number up to
// Accepted), what the kernel dropped at its listener, what the kernel
// completed to it, then how far the application's own connections are
// settled and how many it has made, in that order. Each count read first is
// never higher than the one it is compared with, read after it: accept()
// returns a connection only after the kernel numbered it, and an application
// connection is settled only after it was numbered. A count read first that
// is higher means a count was lost (the kernel's program detached, its object
// replaced), and the answer is an error. An agent whose hooks or proxy cannot
// count, or whose listener is not marked or has closed, gives an error
// wrapping models.ErrProxyConnsUnsupported: "unknown", never 0.
func (a *Agent) ProxyConns(ctx context.Context) (models.ProxyConnCounts, error) {
	h, p, err := a.proxyConnSides()
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	acc, err := p.Accepted()
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	drops, err := p.ListenDrops()
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	est, err := h.ProxyConnsEstablished(ctx)
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	if acc > est {
		return models.ProxyConnCounts{}, fmt.Errorf("%w: the proxy accepted every connection up to %d and the kernel counted only %d: the kernel's count was lost", models.ErrProxyConnsUnsupported, acc, est)
	}
	settled, err := h.AppConnsSettled()
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	app, err := h.AppConnsEstablished(ctx)
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	if settled > app {
		return models.ProxyConnCounts{}, fmt.Errorf("%w: %d of the application's connections are settled and the kernel counted only %d: its count was lost", models.ErrProxyConnsUnsupported, settled, app)
	}
	return models.ProxyConnCounts{Established: est, Accepted: acc, ListenDrops: drops, AppConns: app, AppSettled: settled}, nil
}

// WaitProxySettled returns once every call the application had started when
// counts was read is in the proxy's hands: every application connection
// numbered up to counts.AppConns is settled (its proxy side completed, or it
// was lost, a capture loss charged to the open test), then every connection
// the kernel has completed to the proxy by that moment has been accepted, and
// then the proxy's side of every live connection paired with one of those has
// received the bytes the application had written to it (a call on a pooled
// connection, which opens no connection; bytes that can no longer arrive are a
// capture loss). An application connection whose connect() returned before
// the read is waited for even while its final ACK has not reached the proxy's
// side, and a call written to an open connection even while its bytes have
// not (a loaded host, a held or dropped packet). Connections made, and bytes
// written, after the wait began cannot hold it back. It has no deadline of its
// own, and ends with ctx's error, or with an error when the counts cannot be
// read, a count was lost, the bytes cannot be told, or the listener closes. A
// test's begin and end call it with the counts they read.
func (a *Agent) WaitProxySettled(ctx context.Context, counts models.ProxyConnCounts) error {
	return a.waitProxySettled(ctx, counts, nil)
}

// waitProxySettled is WaitProxySettled; accepting, when set, is told the
// count of accepts the wait then waits for, before it waits for them (the
// settle's watchdog weighs those accepts only: settle).
func (a *Agent) waitProxySettled(ctx context.Context, counts models.ProxyConnCounts, accepting func(n uint64)) error {
	h, _, err := a.proxyConnSides()
	if err != nil {
		return err
	}
	if _, err := a.ProxyConns(ctx); err != nil {
		return err
	}
	if err := h.WaitAppConnsSettled(ctx, counts.AppConns); err != nil {
		return err
	}
	// Every application connection read is now paired (its proxy side
	// numbered) or lost: the proxy's count read now covers the paired ones.
	now, err := a.ProxyConns(ctx)
	if err != nil {
		return err
	}
	accepts := max(now.Established, counts.Established)
	if accepting != nil {
		accepting(accepts)
	}
	if err := a.WaitProxyAccepted(ctx, accepts); err != nil {
		return err
	}
	// Every paired connection is now the proxy's: the bytes written to them.
	pb, ok := a.Proxy.(coreAgent.PairedBytesWaiter)
	if !ok {
		return fmt.Errorf("%w: this agent's proxy cannot wait for the bytes on its connections", models.ErrProxyConnsUnsupported)
	}
	return pb.WaitPairedBytes(ctx, counts.AppConns)
}

// WaitProxyAccepted returns once the proxy has accepted every connection the
// kernel numbered 1..n on its side. The kernel numbers each connection as it
// counts it, and the proxy reads the number from each socket it accepts, so
// the wait is for exactly the connections a read of n covered: connections
// completed later cannot hold it back, and one accepted ahead of an earlier
// one cannot end it early. It has no deadline of its own, and ends with ctx's
// error, or with an error when the counts cannot be read or the listener
// closes. It does not see an application connection whose proxy side is not
// complete yet: WaitProxySettled does.
func (a *Agent) WaitProxyAccepted(ctx context.Context, n uint64) error {
	_, p, err := a.proxyConnSides()
	if err != nil {
		return err
	}
	// The counts must be readable, and agree, before and after: a wait that
	// reaches n on a count that was lost is not settled.
	if _, err := a.ProxyConns(ctx); err != nil {
		return err
	}
	if err := p.WaitAccepted(ctx, n); err != nil {
		return err
	}
	_, err = a.ProxyConns(ctx)
	return err
}

// markProxyListener hands the proxy the hooks' count of its listener, under
// the proven switch or for an agent that settles its reads (settlesReads),
// when the hooks can count and the proxy can take it: the proxy then listens
// plain TCP, marks its socket before listen(), and reads each accepted
// connection's number. Otherwise it does nothing, and the proxy's listener is
// as it always was.
func (a *Agent) markProxyListener() {
	if !provenswitch.On() && !a.startedSettlingReads() {
		return
	}
	h, ok := a.Hooks.(coreAgent.ProxyConnsReporter)
	p, ok2 := a.Proxy.(coreAgent.ListenerCountTaker)
	if !ok || !ok2 {
		if !provenswitch.On() {
			a.noteReadsUnsettled()
		}
		return
	}
	p.SetListenerCounter(h)
}

// latchCounting records, once the agent's proxy is up, whether the kernel
// counted the calls waiting for the proxy when it came up: the proxy decided
// it right after its own check of the count, before it said it was ready
// (agent.CountingReporter). Where the hooks count and it did not (a mark the
// kernel refused, a canary it did not count, no sock_diag), the proxy has
// said why, with what to fix; the agent then reads each test's misses as
// before (settlesReads).
func (a *Agent) latchCounting() {
	if cr, ok := a.Proxy.(coreAgent.CountingReporter); ok && cr.CountedFromStart() {
		a.countsCalls.Store(true)
	}
}

// noteReadsUnsettled says, for an agent started settling reads whose hooks
// cannot count the calls waiting for its proxy (the macOS and Windows agents'),
// that it reads each test's misses at once, as before. That is what these
// hooks are, nothing to fix: an Info, not a warning.
func (a *Agent) noteReadsUnsettled() {
	if a.logger == nil || !a.startedSettlingReads() {
		return
	}
	a.logger.Info("keploy's hooks here do not count the calls waiting for its proxy, so it reads each test's misses at once, as before: a miss it decides after the application answered its test may be read with a later test")
}

// lostSinceLastRead is, for a read settled with the switch off, why it cannot
// vouch for every miss when the agent has had capture losses since the last
// such read (CaptureLosses: the proxy's, the hooks', the PID maps'): calls it
// could not capture, record or answer, such as an application connection
// lost before the proxy got it, a client's TLS
// handshake that failed, a parser that panicked, bytes the application wrote
// that never reached the proxy. Their misses are in no read. Under the switch
// they are charged to their test instead (markCaptureLosses). Counts that
// cannot be read are a read that cannot vouch; hooks that count no losses at
// all leave the read as it is, as the switch does.
func (a *Agent) lostSinceLastRead(ctx context.Context) error {
	losses, err := a.CaptureLosses(ctx)
	if errors.Is(err, models.ErrCaptureLossesUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keploy could not read what it failed to capture: %w", err)
	}
	var n uint64
	for _, l := range losses {
		n += l.Count
	}
	if prev := a.readLosses.Swap(n); n > prev {
		return fmt.Errorf("keploy could not capture, record or answer %d of the calls it intercepted since the last read (%v)", n-prev, losses)
	}
	return nil
}

// countingLost is why, with the switch off, the run's calls stopped being
// counted after the agent's start (a NAT'd connection: the proxy's
// CountingLost), nil while they are, and always nil under the switch, which
// refuses such a connection instead.
func (a *Agent) countingLost() error {
	if provenswitch.On() {
		return nil
	}
	cr, ok := a.Proxy.(coreAgent.CountingReporter)
	if !ok {
		return nil
	}
	_, err := cr.CountingLost()
	return err
}

// countingLostSignal is a channel closed once countingLost says why (never,
// under the switch or for a proxy that does not say).
func (a *Agent) countingLostSignal() <-chan struct{} {
	if provenswitch.On() {
		return nil
	}
	cr, ok := a.Proxy.(coreAgent.CountingReporter)
	if !ok {
		return nil
	}
	ch, _ := cr.CountingLost()
	return ch
}

// startedSettlingReads reports whether this agent was started for a run that
// learns noise from its misses (models.SetupOptions.SettleReads,
// --settle-reads): with the proven switch off, its reads of a test's misses
// settle the test's calls first, as the switch's do, which needs its
// proxy's listener marked, and every connection kept, from its start.
func (a *Agent) startedSettlingReads() bool {
	return a.config != nil && a.config.Agent.SettleReads
}

// settlesReads reports whether, with the proven switch off, this agent settles
// a read of a test's misses: it was started settling reads, and when its proxy
// came up the kernel counted the connections completed to the proxy's
// listener (latchCounting). Where the calls could not be counted from the
// start (hooks that do not count them, on another platform; a mark the kernel
// refused; a canary that failed), the read is as it always was, said once: no
// read of this agent's could ever be settled, and refusing every one would
// end noise learning there rather than make it right. An agent that counted
// from the start settles every read: one that then cannot vouch for every
// call (a count lost later, a parser that reads ahead without saying what it
// holds) is answered unread, never read as before.
func (a *Agent) settlesReads() bool {
	return a.startedSettlingReads() && a.countsCalls.Load()
}

// Compile-time proof that the agent offers begin and end the count: the
// background rules' waits type-assert for it.
var _ coreAgent.ProxyConnCounter = (*Agent)(nil)
