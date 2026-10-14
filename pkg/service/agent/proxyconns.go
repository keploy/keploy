package agent

import (
	"context"
	"errors"
	"net/netip"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// proxyConnSides are the two counters ProxyConns compares: the hooks' kernel
// count and the proxy's accepts, at the same addresses.
func (a *Agent) proxyConnSides() (coreAgent.ProxyConnsReporter, coreAgent.AcceptCounter, []netip.AddrPort, error) {
	h, ok := a.Hooks.(coreAgent.ProxyConnsReporter)
	if !ok {
		return nil, nil, nil, models.ErrProxyConnsUnsupported
	}
	p, ok := a.Proxy.(coreAgent.AcceptCounter)
	if !ok {
		return nil, nil, nil, models.ErrProxyConnsUnsupported
	}
	targets := h.ProxyConnTargets()
	if len(targets) == 0 {
		return nil, nil, nil, errors.New("the hooks have not been told where the proxy is: they are not loaded")
	}
	return h, p, targets, nil
}

// ProxyConns reads the connections the kernel has completed to the proxy's
// listener at the addresses the connect hooks redirect to, then how many of
// them the proxy has accepted (models.ProxyConnCounts). Both only ever grow
// for the life of the agent. An agent whose hooks or proxy cannot count gives
// models.ErrProxyConnsUnsupported, "unknown". It is cheap: one map lookup and
// one locked read.
func (a *Agent) ProxyConns(ctx context.Context) (models.ProxyConnCounts, error) {
	h, p, targets, err := a.proxyConnSides()
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	// The kernel's count first: a connection completed after it is read and
	// accepted before the proxy's is read only raises Accepted, which a wait
	// for Accepted >= Established reads as done, as it is for that read.
	est, err := h.ProxyConnsEstablished(ctx)
	if err != nil {
		return models.ProxyConnCounts{}, err
	}
	return models.ProxyConnCounts{Established: est, Accepted: p.AcceptedTo(targets)}, nil
}

// WaitProxyAccepted returns once the proxy has accepted n connections at the
// addresses the hooks count, or with ctx's error when ctx ends first. It has
// no deadline of its own: each accept wakes it. A test's begin and end call it
// with the Established they read from ProxyConns.
func (a *Agent) WaitProxyAccepted(ctx context.Context, n uint64) error {
	_, p, targets, err := a.proxyConnSides()
	if err != nil {
		return err
	}
	return p.WaitAcceptedTo(ctx, targets, n)
}

// Compile-time proof that the agent offers begin and end the count: S12's
// waits type-assert for it.
var _ coreAgent.ProxyConnCounter = (*Agent)(nil)
