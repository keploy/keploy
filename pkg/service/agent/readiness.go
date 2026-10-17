package agent

import (
	"context"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// AppReadiness is what the hooks know of the application's readiness and of
// its start's progress, under the proven switch (models.AppReadiness).
func (a *Agent) AppReadiness(_ context.Context, since uint64) (models.AppReadiness, error) {
	r, ok := a.Hooks.(coreAgent.ReadinessReporter)
	if !ok || a.Hooks == nil {
		return models.AppReadiness{}, models.ErrReadinessUnsupported
	}
	return r.AppReadiness(since)
}

// MarkReadiness is the hooks' mark for a set whose application keploy starts
// anew (models.AppReadiness's counts from then on).
func (a *Agent) MarkReadiness(_ context.Context) (models.AppReadiness, error) {
	r, ok := a.Hooks.(coreAgent.ReadinessReporter)
	if !ok || a.Hooks == nil {
		return models.AppReadiness{}, models.ErrReadinessUnsupported
	}
	return r.MarkReadiness()
}
