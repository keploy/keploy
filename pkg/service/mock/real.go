package mock

import (
	"context"
	"maps"
	"slices"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/realclient"
)

// RealDepsAgent is realclient.Agent: the agent client's mock.real calls.
type RealDepsAgent = realclient.Agent

// checkRealDeps refuses, before anything runs, a mock.real keploy cannot read.
func (m *mockService) checkRealDeps() error { return realclient.Check(m.config.Mock.Real) }

// pushRealDeps hands the agent the run's mock.real (realclient.Push).
func (m *mockService) pushRealDeps(ctx context.Context) error {
	return realclient.Push(ctx, m.logger, m.instrumentation, m.config.Mock.Real)
}

// realRun reads what the run saw of mock.real beyond its tests
// (realclient.Run).
func (m *mockService) realRun(ctx context.Context) (*models.RealRun, error) {
	return realclient.Run(ctx, m.instrumentation, m.config.Mock.Real)
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
