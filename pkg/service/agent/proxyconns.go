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
	if err := a.WaitProxyAccepted(ctx, max(now.Established, counts.Established)); err != nil {
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
