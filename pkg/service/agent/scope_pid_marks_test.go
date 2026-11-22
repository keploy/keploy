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

// marked makes the run one recorded with the suite mark or an app-start mark
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
// the suite mark or an app-start mark, is refused, and fails whatever its harness
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
		err := a.BeginScope(ctx, test, 4242)
		if !errors.Is(err, ErrPidBeginWithoutMarks) {
			t.Fatalf("%s began with a pid in a run without marks: got %v, want ErrPidBeginWithoutMarks", test, err)
		}
		// It names the test and its pid, then the two ways out, one line each.
		want := test + " began with the pid 4242: keploy narrows a test to its own mocks by the pid of its test process only in a run recorded with the suite mark or an app-start mark: without one, a connection the app opened before the test began (its database pool at boot, or one opened between two tests) is served every test's mocks, so the test could pass on another test's recording.\n" +
			"Either add the suite mark, at record and at replay: begin a scope with \"suite\": true and the pid before the first test and end it after the last; when the tests start the app, also POST /agent/app/start {\"pid\": <the app's pid>} each time they do.\n" +
			"Or send the begin without a pid, and run the tests one at a time: keploy then narrows every connection to the open test."
		if err.Error() != want {
			t.Fatalf("%s's refusal:\n%s\nwant:\n%s", test, err, want)
		}
		// A harness that ignores the refusal runs the test: every call of
		// the worker's that keploy can tell is refused, naming why, and the
		// replay fails on the refusal (models.ScopeWindow.BeginRefused).
		if got := p.refused[4242]; got != want {
			t.Fatalf("%s's worker reads were refused with %q, want %q", test, got, want)
		}
		if got := beginRefusalsOf(t, a, test); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s's windows say keploy refused it with %q, want once with %q", test, got, want)
		}
		// Its end gives the worker's reads back.
		cleared := len(p.cleared)
		if err := a.EndScope(ctx, test, 4242); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.cleared[cleared:], []uint32{4242}) {
			t.Fatalf("%s's end cleared workers %v, want 4242", test, p.cleared[cleared:])
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

// Under the proven switch a test that begins with a worker pid (an in-process
// caller's: the wire refuses one) is served exactly its own mocks, as one
// without a pid is: every connection reads the manager's exact view, the ones
// the app opened at boot or between two tests included, so it is not refused
// and no per-worker view is made. Its end serves the pool between tests.
func TestAPidBeginUnderTheSwitchIsServedExactly(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	useHooks(t, &clockHooks{})
	a := newClockAgent(t)
	pool := &workerPool{}
	a.Proxy = pool
	loadMocks(a, "mock-a")
	ctx := context.Background()
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}, "TestC": {}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"TestA", "TestC"} {
		if err := a.BeginScope(ctx, test, 4242); err != nil {
			t.Fatalf("%s began with a pid under the switch: %v, want it served exactly", test, err)
		}
		if err := a.EndScope(ctx, test, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"test TestA", "pool", "test TestC", "pool"}; !slices.Equal(pool.served(), want) {
		t.Fatalf("exact serving put in %v, want %v", pool.served(), want)
	}
	if len(pool.narrowed) != 0 || len(pool.cleared) != 0 {
		t.Fatalf("worker views narrowed %v, cleared %v; want none under exact serving", pool.narrowed, pool.cleared)
	}
	for _, test := range []string{"TestA", "TestC"} {
		if got := outcomesOf(t, a, test); slices.Contains(got, models.ScopeOutcomeFailed) {
			t.Fatalf("%s's outcomes %v, want it not failed", test, got)
		}
	}
}

// beginRefusalsOf is why keploy refused each begin of name, in order
// (models.ScopeWindow.BeginRefused).
func beginRefusalsOf(t *testing.T, a *Agent, name string) []string {
	t.Helper()
	ws, err := a.GetScopeWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, w := range ws {
		if w.Name == name && w.BeginRefused != "" {
			out = append(out, w.BeginRefused)
		}
	}
	return out
}

// A refused pid begin moves no clock, and its end moves none either: a pid
// begin is refused only with the switch off, where no begin moves the clock
// (testClock), so there is nothing to give back.
func TestARefusedPidBeginMovesNoClock(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	pool := &workerPool{}
	a.Proxy = pool
	ctx := context.Background()
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginScope(ctx, "TestA", 4242); !errors.Is(err, ErrPidBeginWithoutMarks) {
		t.Fatalf("got %v, want ErrPidBeginWithoutMarks", err)
	}
	if len(h.moves) != 0 {
		t.Fatalf("a refused begin moved the clock: %v", h.moves)
	}
	if err := a.EndScope(ctx, "TestA", 4242); err != nil {
		t.Fatal(err)
	}
	if len(h.moves) != 0 {
		t.Fatalf("the end of a refused begin moved the clock: %v", h.moves)
	}
	if !slices.Equal(pool.refused, []uint32{4242}) {
		t.Fatalf("refused workers %v, want 4242", pool.refused)
	}
}

// Every begin keploy refuses says why on its window, so the replay fails on
// it whatever the harness does next: the clock that cannot move, and a
// narrowing that fails after the clock moved, as the pid begin without marks
// does (TestAPidBeginWithoutMarksIsRefusedAndFailsTheTest).
func TestEveryBeginKeployRefusesSaysWhyOnItsWindow(t *testing.T) {
	t.Run("the clock cannot move", func(t *testing.T) {
		t.Setenv("KEPLOY_PROVEN", "1")
		useHooks(t, &clockHooks{err: errors.New("disk full")})
		a := newClockAgent(t)
		err := a.BeginScope(context.Background(), "TestA", 0)
		if err == nil {
			t.Fatal("a begin whose clock could not move began")
		}
		if got := beginRefusalsOf(t, a, "TestA"); !slices.Equal(got, []string{err.Error()}) {
			t.Fatalf("its window says %q, want %q", got, err.Error())
		}
	})
	t.Run("the narrowing fails", func(t *testing.T) {
		t.Setenv("KEPLOY_PROVEN", "1")
		useHooks(t, &clockHooks{})
		a := newClockAgent(t)
		a.clientMocks.Delete(uint64(0))
		ctx := context.Background()
		if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-1"}}); err != nil {
			t.Fatal(err)
		}
		err := a.BeginScope(ctx, "TestA", 0)
		if err == nil {
			t.Fatal("a begin whose narrowing failed began")
		}
		if got := beginRefusalsOf(t, a, "TestA"); !slices.Equal(got, []string{err.Error()}) {
			t.Fatalf("its window says %q, want %q", got, err.Error())
		}
	})
}
