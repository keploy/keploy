package proxy

import (
	"context"
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent"
)

// AcceptedTo is how many connections the proxy's listener has accepted at any
// of targets, since it started. It only ever grows.
func (p *Proxy) AcceptedTo(targets []netip.AddrPort) uint64 {
	return p.accepts.To(targets)
}

// WaitAcceptedTo returns once AcceptedTo(targets) >= n, or with ctx's error.
// It has no deadline of its own: each accept wakes it.
func (p *Proxy) WaitAcceptedTo(ctx context.Context, targets []netip.AddrPort, n uint64) error {
	return p.accepts.WaitTo(ctx, targets, n)
}

// Compile-time proof that the proxy counts its accepts: were a method renamed,
// the agent's type assertion would stop matching and every begin and end
// would read "unknown".
var _ agent.AcceptCounter = (*Proxy)(nil)
