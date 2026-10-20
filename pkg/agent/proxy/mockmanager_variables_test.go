package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// runSet is one test set as exact serving stages it under the switch: a
// start-up call that binds key (boot), the first test's call that uses it,
// the second test's call in flight that uses it (fetch, answered in the gap
// before that test), and the second test's own call that uses it.
func runSet(t *testing.T) (mm *MockManager, use1, fetch, use2 *models.Mock) {
	t.Helper()
	boot := varMock("boot", "/session", false, models.LifetimeSession)
	use1 = varMock("use-1", "/use-1", true, models.LifetimePerTest)
	fetch = varMock("fetch", "/poll", true, models.LifetimePerTest)
	use2 = varMock("use-2", "/use-2", true, models.LifetimePerTest)
	return stagedManager(t, []*models.Mock{use1, fetch, use2}, []*models.Mock{boot}), use1, fetch, use2
}

// What a set's start-up binds before its first test is the run's: each test
// of the set begins (StartVariables) with it bound, and so does the scope the
// next test's calls in flight bind in before it begins. The next set forgets
// it, and its start-up binds the run's anew. Without NewVariableSet each test
// begins with nothing bound, as before.
func TestARunsBindingIsEveryTestsUntilTheNextSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, newSet := range []bool{true, false} {
		mm, use1, fetch, use2 := runSet(t)
		if newSet {
			mm.NewVariableSet()
		}
		mm.ServeExactPool("-", []string{"boot"})
		if got := postKey(t, mm, "/session", "x-1"); got != `200 {"echo":"x-1"}` {
			t.Fatalf("newSet %v: the start-up call was answered %s", newSet, got)
		}
		mm.ServeExactTest("test-1", []*models.Mock{use1})
		mm.StartVariables(nil)
		const want = `200 {"echo":"x-1"}`
		if got := postKey(t, mm, "/use-1", "x-1"); (got == want) != newSet {
			t.Fatalf("newSet %v: test-1's call that uses the start-up's key was answered %s", newSet, got)
		}
		if !newSet {
			continue
		}
		mm.ServeExactPool("-", nil, fetch)
		if got := postKey(t, mm, "/poll", "x-1"); got != want {
			t.Fatalf("test-2's call in flight that uses the start-up's key was answered %s in the gap; want %s", got, want)
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, use2})
		mm.StartVariables(nil)
		if got := boundNames(mm.BoundVariables()); got != "key" {
			t.Fatalf("test-2 begins with %q bound; want the run's key", got)
		}
		if got := postKey(t, mm, "/use-2", "x-9"); strings.HasPrefix(got, "200 ") {
			t.Fatalf("test-2's call that sent another value than the run's key was answered %s", got)
		}
		if got := postKey(t, mm, "/use-2", "x-1"); got != want {
			t.Fatalf("test-2's call that uses the start-up's key was answered %s; want %s", got, want)
		}

		// The next set: its mocks reuse the names, and its start-up has not
		// bound key yet.
		next1 := varMock("use-1", "/use-1", true, models.LifetimePerTest)
		mm.SetMocksWithWindow([]*models.Mock{next1},
			[]*models.Mock{varMock("boot", "/session", false, models.LifetimeSession)}, models.BaseTime, time.Now())
		mm.NewVariableSet()
		mm.ServeExactPool("-", nil)
		if got := boundNames(mm.BoundVariables()); got != "" {
			t.Fatalf("the next set begins with %q bound", got)
		}
		mm.ServeExactTest("test-1", []*models.Mock{next1})
		mm.StartVariables(nil)
		if got := postKey(t, mm, "/use-1", "x-1"); strings.HasPrefix(got, "200 ") {
			t.Fatalf("the next set's test-1 was answered %s with the previous set's run's key", got)
		}
	}
}

// namedVar is m with its variable key renamed name.
func namedVar(m *models.Mock, name string) *models.Mock {
	m.Spec.Variables = models.MockVariables{name: m.Spec.Variables["key"]}
	return m
}

// Before a set's first test, the first test's call in flight (answered in the
// gap before it, in the scope begun early) and the start-up's call bind in
// either order. Whichever sends second, as its variable, the live value the
// other bound as another variable recorded with another value is a miss: the
// first test never begins with two variables at one live value.
func TestTheStartUpAndTheFirstTestsCallInFlightKeepTheirValuesApart(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, aheadFirst := range []bool{true, false} {
		boot := namedVar(varMockOf("boot", "/session", "s-rec", false, models.LifetimeSession), "run-session")
		fetch := namedVar(varMockOf("fetch", "/order", "o-rec", false, models.LifetimePerTest), "order")
		mm := stagedManager(t, []*models.Mock{fetch}, []*models.Mock{boot})
		mm.NewVariableSet()
		mm.ServeExactPool("-", []string{"boot"}, fetch)
		first, second := "/order", "/session"
		if !aheadFirst {
			first, second = second, first
		}
		if got := postKey(t, mm, first, "V"); got != `200 {"echo":"V"}` {
			t.Fatalf("aheadFirst %v: %s was answered %s", aheadFirst, first, got)
		}
		if got := postKey(t, mm, second, "V"); strings.HasPrefix(got, "200 ") {
			t.Fatalf("aheadFirst %v: %s, which sent %s's live value as another variable, was answered %s", aheadFirst, second, first, got)
		}
		mm.ServeExactTest("test-1", []*models.Mock{fetch})
		mm.StartVariables(nil)
		bound := mm.BoundVariables()
		if len(bound) != 1 {
			t.Fatalf("aheadFirst %v: the first test begins with %+v bound; want only what %s bound", aheadFirst, bound, first)
		}
	}
}

// The replay reaches Mock once per test set (Agent.MockOutgoing): under the
// switch that begins the set's variables, so what the set's start-up binds
// before its first test is the run's, and the next Mock forgets it. With the
// switch off Mock leaves the variables as they were, and a test begins with
// nothing bound, as before.
func TestMockBeginsTheSetsVariablesUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		// Preseeded so Mock() leaves the host's /etc/nsswitch.conf alone (see
		// testProxy).
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		mm.Variables().BindAll(mm.Variables().Gen(), []models.VariableBinding{{Name: "session", Value: "S", Recorded: "s-1", Mock: "boot"}})
		mm.StartVariables(nil)
		_, kept := mm.Variables().Lookup("session")
		if kept != on {
			t.Fatalf("switch on %v: the start-up's binding is in the first test: %v", on, kept)
		}
		if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true, StartsTestSet: true}); err != nil {
			t.Fatal(err)
		}
		mm.StartVariables(nil)
		if _, ok := mm.Variables().Lookup("session"); ok {
			t.Fatalf("switch on %v: the previous set's run's binding is in the next set's first test", on)
		}
	}
}

// keploy test under the switch, in the order it begins a test set and its
// tests: Mock (the set's variables begin), the set's staging filter, the
// application's start-up, then each test case's filter, its calls, and the
// pool call after it. The agent's route begins a test's variables at a
// filter only when MockFilterParams.BeginsTestVariables says so
// (routes.UpdateMockParams), and filter applies that rule to each. The
// staging begins no test: what the start-up binds after it is the run's,
// every test's of the set, and the next set's Mock forgets it, its start-up
// binding the run's anew.
func TestKeployTestsStartUpBindsForEveryTestOfItsSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	// Preseeded so Mock() leaves the host's /etc/nsswitch.conf alone (see
	// testProxy).
	p.nsswitchData = []byte("preseeded-by-test")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	filter := func(params models.MockFilterParams) {
		t.Helper()
		if params.BeginsTestVariables() && !p.StartVariables(params.Variables) {
			t.Fatal("the proxy keeps no mock manager")
		}
	}
	for set, key := range []string{"x-1", "x-2"} {
		if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true, StartsTestSet: set > 0}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		boot := varMock("boot", "/session", false, models.LifetimeSession)
		uses := []*models.Mock{varMock("use-1", "/use-1", true, models.LifetimePerTest), varMock("use-2", "/use-2", true, models.LifetimePerTest)}
		// The staging (the agent's serveKeployTestExactly): the set's mocks
		// are in, and its pool is served.
		mm.SetMocksWithWindow(uses, []*models.Mock{boot}, models.BaseTime, time.Now())
		mm.ServeExactPool(models.PoolKeyNone, []string{"boot"})
		filter(models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true, Pool: []string{"boot"}}})
		want := `200 {"echo":"` + key + `"}`
		if got := postKey(t, mm, "/session", key); got != want {
			t.Fatalf("set %d: the start-up call was answered %s", set+1, got)
		}
		for i, use := range uses {
			name := "test-" + string(rune('1'+i))
			mm.ServeExactTest(name, []*models.Mock{use})
			filter(models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: []string{use.Name}}})
			path := "/use-" + string(rune('1'+i))
			if set > 0 {
				if got := postKey(t, mm, path, "x-1"); strings.HasPrefix(got, "200 ") {
					t.Fatalf("set %d: %s's call was answered %s with the previous set's run's value", set+1, name, got)
				}
			}
			if got := postKey(t, mm, path, key); got != want {
				t.Fatalf("set %d: %s's call that uses the start-up's value was answered %s; want %s", set+1, name, got, want)
			}
			mm.ServeExactPool(models.PoolKeyNone, []string{"boot"})
			filter(models.MockFilterParams{Exact: &models.ExactServing{}})
		}
	}
}
