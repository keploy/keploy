package agent

import (
	"context"
	"time"

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

// realDepsKeeper is a proxy that installs a later part of a run's mock.real
// without starting its bookkeeping afresh when it already holds it.
type realDepsKeeper interface {
	KeepRealDeps(ctx context.Context, real []string) error
}

// KeepRealDeps is SetRealDeps for a later push of the same run (keploy test's
// next test set): a proxy that already holds the list keeps what the run saw.
// One that cannot keep installs it afresh.
func (a *Agent) KeepRealDeps(ctx context.Context, real []string) error {
	if k, ok := a.Proxy.(realDepsKeeper); ok && a.Proxy != nil {
		return k.KeepRealDeps(ctx, real)
	}
	return a.SetRealDeps(ctx, real)
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

// RealUsedDuring is the mock.real destinations the run used from from to to
// (realdeps.Tracker.UsedDuring): those of a keploy record test case whose
// request came at from and whose response went at to. nil without a
// relaying proxy, or when nothing real was used then.
func (a *Agent) RealUsedDuring(from, to time.Time) []string {
	if t := a.realTracker(); t != nil {
		return t.UsedDuring(from, to)
	}
	return nil
}

// RealTest ends keploy test's open test case for mock.real (the window
// Begin/ContinueTestErrorCapture opened): the REAL destinations it used, and
// the connections keploy could neither capture nor relay as real while it
// ran, charged as its misses are. An agent without a relaying proxy has
// nothing to report.
func (a *Agent) RealTest(_ context.Context) (models.RealTest, error) {
	t := a.realTracker()
	if t == nil {
		return models.RealTest{}, nil
	}
	uses, incomplete := t.EndTest()
	return models.RealTest{Real: uses, Incomplete: incomplete}, nil
}

// KeepRealUses says whether the run's REAL uses are kept, to name keploy
// record's test cases by them (realdeps.Tracker.KeepUses): while its incoming
// stream is open.
func (a *Agent) KeepRealUses(keep bool) {
	if t := a.realTracker(); t != nil {
		t.KeepUses(keep)
	}
}
