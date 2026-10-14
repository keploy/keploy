package proxy

import (
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts, by kind, the calls the proxy intercepted and could not
// record or mock since it started: a client-side TLS handshake that failed
// against keploy's CA, a TLS upgrade it could not make. Each only ever grows.
func (p *Proxy) CaptureLosses() models.CaptureLosses {
	return p.losses.Snapshot()
}

// Compile-time proof that the proxy reports its capture losses: were the
// method renamed, the agent's type assertion would stop matching and those
// losses would go unreported.
var _ agent.CaptureLossReporter = (*Proxy)(nil)
