package mock

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// RealDepsAgent is an optional Instrumentation extension: under the proven
// switch the CLI hands its agent the run's mock.real as the run starts, and
// reads at its end what the run saw of it beyond its tests (design v14 §4).
type RealDepsAgent interface {
	PushRealDeps(ctx context.Context, real []string) error
	RealRun(ctx context.Context) (models.RealRun, error)
}

// checkRealDeps refuses, before anything runs, a mock.real keploy cannot read
// (proven switch only; without it the key is not read).
func (m *mockService) checkRealDeps() error {
	if !provenswitch.On() {
		return nil
	}
	if _, err := realdeps.Parse(m.config.Mock.Real); err != nil {
		return fmt.Errorf("keploy.yml mock.real: %w", err)
	}
	return nil
}

// pushRealDeps hands the agent the run's mock.real (proven switch only), an
// empty list too: it clears what an agent that served an earlier run kept.
// An agent client or an agent that cannot take a list that names anything is
// an error: those destinations would be recorded and mocked instead of
// reached. An agent without the route (another build) has nothing to clear
// when the list is empty.
func (m *mockService) pushRealDeps(ctx context.Context) error {
	if !provenswitch.On() {
		return nil
	}
	empty := len(m.config.Mock.Real) == 0
	a, ok := m.instrumentation.(RealDepsAgent)
	if !ok {
		if empty {
			return nil
		}
		return errors.New("this keploy's agent client cannot hand its agent mock.real")
	}
	if err := a.PushRealDeps(ctx, m.config.Mock.Real); err != nil {
		if empty && errors.Is(err, models.ErrNoRealRoute) {
			return nil
		}
		return err
	}
	if empty {
		return nil
	}
	m.logger.Info("these destinations are reached for real in this run, never recorded or mocked (mock.real)", zap.Strings("real", m.config.Mock.Real))
	return nil
}

// realRun reads what the run saw of mock.real beyond its tests, after the
// run's last windows (proven switch only; nil without it). A read that fails
// is an error: a REAL destination never reached, or a connection keploy
// could not capture outside every test, must not go unsaid.
func (m *mockService) realRun(ctx context.Context) (*models.RealRun, error) {
	if !provenswitch.On() {
		return nil, nil
	}
	if len(m.config.Mock.Real) == 0 {
		// Nothing was real: nothing to reach, and no name to fail to read.
		return nil, nil
	}
	a, ok := m.instrumentation.(RealDepsAgent)
	if !ok {
		return nil, errors.New("this keploy's agent client cannot read the run's mock.real destinations")
	}
	run, err := a.RealRun(ctx)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// realDests are the destinations a test's REAL rows name: its recording's
// REAL list.
func realDests(uses []models.RealUse) []string {
	if len(uses) == 0 {
		return nil
	}
	out := make([]string, 0, len(uses))
	for _, u := range uses {
		out = append(out, u.Dest)
	}
	return out
}

// mergeSorted is the sorted union of a and b, each once.
func mergeSorted(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// withReal puts on each test's line the mock.real destinations it used while
// open (models.ScopeWindow.Real): its REAL rows. A test that ran more than
// once counts the connections of every attempt.
func withReal(tests []TestReceipt, windows []models.ScopeWindow) []TestReceipt {
	byName := map[string]map[string]*models.RealUse{}
	for _, w := range windows {
		for _, u := range w.Real {
			if byName[w.Name] == nil {
				byName[w.Name] = map[string]*models.RealUse{}
			}
			got := byName[w.Name][u.Dest]
			if got == nil {
				got = &models.RealUse{Dest: u.Dest}
				byName[w.Name][u.Dest] = got
			}
			got.Conns += u.Conns
			got.Failed += u.Failed
			got.H2 = got.H2 || u.H2
		}
	}
	for i := range tests {
		uses := byName[tests[i].Name]
		for _, dest := range slices.Sorted(maps.Keys(uses)) {
			tests[i].Real = append(tests[i].Real, *uses[dest])
		}
	}
	return tests
}

// withRealListed puts on each test's line the mock.real destinations its
// recording lists (listed, by test ID).
func withRealListed(tests []TestReceipt, listed map[string][]string) []TestReceipt {
	for i := range tests {
		if l := listed[tests[i].Name]; len(l) > 0 {
			tests[i].RealListed = slices.Clone(l)
		}
	}
	return tests
}
