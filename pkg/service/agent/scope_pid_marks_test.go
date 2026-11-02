package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// marked makes the run one recorded with the suite and app-start marks
// (starts.Default.Active) for the test, and undoes it after.
func marked(t *testing.T, tests ...string) {
	t.Helper()
	owned := map[string][]models.Owned{}
	for _, n := range tests {
		owned[n] = []models.Owned{{Name: "mock-" + n}}
	}
	starts.Default.SetTable("", map[string]models.SetTable{"": {Tests: owned}})
	t.Cleanup(starts.Default.Reset)
}

// A replayed test that begins with its test process's pid, in a run without
// the suite and app-start marks, is refused, and fails whatever its harness
// does with the refusal. A pid begin narrows only connections the app opens
// while the test is open: one it opened at boot or between two tests keeps
// the bare manager, and would serve the test every test's mocks. (Without a
// pid the open test narrows that manager: TestScopeBeginOpensTheTestsWindow.)
func TestAPidBeginWithoutMarksIsRefusedAndFailsTheTest(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	p := &workerProxy{}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, []*models.Mock{stageMock("mock-a", "/q", stageT0)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}, "TestC": {}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"TestA", "TestC"} {
		if err := a.BeginScope(ctx, test, 4242); !errors.Is(err, ErrPidBeginWithoutMarks) {
			t.Fatalf("%s began with a pid in a run without marks: got %v, want ErrPidBeginWithoutMarks", test, err)
		}
		// A harness that ignores the refusal runs the test and ends it.
		if err := a.EndScope(ctx, test, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.workerScopes) != 0 || len(p.scoped) != 0 || len(p.windowed) != 0 {
		t.Fatalf("a refused begin narrowed worker scopes %v, scopes %v, windows %v; want none", p.workerScopes, p.scoped, p.windowed)
	}
	for _, test := range []string{"TestA", "TestC"} {
		if got := outcomesOf(t, a, test); !slices.Equal(got, []string{models.ScopeOutcomeFailed}) {
			t.Fatalf("%s's outcomes %v, want it failed once", test, got)
		}
	}
	// A test the replay has no mapping for is served the whole pool with a
	// pid or without one: it begins as before.
	if err := a.BeginScope(ctx, "TestZ", 4242); err != nil {
		t.Fatalf("an unmapped test began with a pid: %v, want it begun", err)
	}
}
