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
		mm.Variables().Bind(models.VariableBinding{Name: "session", Value: "S", Recorded: "s-1", Mock: "boot"})
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
