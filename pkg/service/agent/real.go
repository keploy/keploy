package agent

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// realDepsProxy is a proxy that relays the run's mock.real destinations
// (design v14 §4).
type realDepsProxy interface {
	SetRealDeps(ctx context.Context, real []string) error
	RealRun() models.RealRun
}

// SetRealDeps installs the run's mock.real, which the CLI sends as the run
// starts. A proxy that cannot relay real destinations refuses a list that
// names any (models.ErrNoRealRelay), so the run fails rather than record or
// mock them; an empty list asks nothing of it.
func (a *Agent) SetRealDeps(ctx context.Context, real []string) error {
	p, ok := a.Proxy.(realDepsProxy)
	if !ok || a.Proxy == nil {
		if len(real) == 0 {
			return nil
		}
		return models.ErrNoRealRelay
	}
	return p.SetRealDeps(ctx, real)
}

// RealRun is what the run saw of mock.real beyond its tests. It takes the
// capture_incomplete evidence charged to no test: a second read does not
// repeat it.
func (a *Agent) RealRun(_ context.Context) (models.RealRun, error) {
	p, ok := a.Proxy.(realDepsProxy)
	if !ok || a.Proxy == nil {
		return models.RealRun{}, nil
	}
	return p.RealRun(), nil
}
