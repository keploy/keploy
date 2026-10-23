package agent

import (
	"context"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// RelayedLive is the connections the agent's proxy relayed live to a
// dependency since it started, instead of answering them from a recording,
// per reason and destination (coreAgent.RelayedLiveReporter). A proxy that
// does not count them gives models.ErrRelayedLiveUnsupported: "unknown",
// never "none".
func (a *Agent) RelayedLive(context.Context) (models.RelayedLives, error) {
	r, ok := a.Proxy.(coreAgent.RelayedLiveReporter)
	if !ok {
		return nil, models.ErrRelayedLiveUnsupported
	}
	return r.RelayedLive(), nil
}
