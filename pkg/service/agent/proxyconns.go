package agent

import (
	"context"
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

// ProxyConns reads the counts of the connections to the proxy's marked
// listener (models.ProxyConnCounts): what it accepted (every number up to
// Accepted), what the kernel dropped at it, and what the kernel completed to
// it, in that order. accept() returns a connection only after the kernel
// numbered it, so Accepted, read first, is never higher than Established,
// read after it: if it is, the kernel's count was lost (its program detached,
// its object replaced), and the answer is an error. An agent whose hooks or
// proxy cannot count, or whose listener is not marked or has closed, gives an
// error wrapping models.ErrProxyConnsUnsupported: "unknown", never 0.
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
	return models.ProxyConnCounts{Established: est, Accepted: acc, ListenDrops: drops}, nil
}

// WaitProxyAccepted returns once the proxy has accepted every connection the
// kernel numbered 1..n. The kernel numbers each connection as it counts it,
// and the proxy reads the number from each socket it accepts, so the wait is
// for exactly the connections a read of n covered: connections completed
// later cannot hold it back, and one accepted ahead of an earlier one cannot
// end it early. It has no deadline of its own, and ends with ctx's error, or
// with an error when the counts cannot be read or the listener closes. A
// test's begin and end call it with the Established they read from
// ProxyConns.
//
// What it cannot see: a connection the application's connect() has completed
// whose final ACK the kernel has not yet processed on the proxy's side is not
// numbered yet. If the kernel's TCP layer dropped that ACK at the listener
// (its accept queue full, among the causes), the listener's drops grow, and
// the test is not verified (ListenDrops); a drop before TCP (the device's
// backlog, a veth, netfilter) does not show there. If the ACK is only late (a
// loaded host defers it to ksoftirqd), the window is that delay: a call made
// without waiting for its answer (fire and forget) at a test's very end can
// reach the proxy after the end read, in the next test. The application's own
// count of its completed connects, paired by source port, closes that gap.
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
// the proven switch, when the hooks can count and the proxy can take it: the
// proxy then listens plain TCP, marks its socket before listen(), and reads
// each accepted connection's number. Otherwise it does nothing, and the
// proxy's listener is as it always was.
func (a *Agent) markProxyListener() {
	if !provenswitch.On() {
		return
	}
	h, ok := a.Hooks.(coreAgent.ProxyConnsReporter)
	if !ok {
		return
	}
	p, ok := a.Proxy.(coreAgent.ListenerCountTaker)
	if !ok {
		return
	}
	p.SetListenerCounter(h)
}

// Compile-time proof that the agent offers begin and end the count: S12's
// waits type-assert for it.
var _ coreAgent.ProxyConnCounter = (*Agent)(nil)
