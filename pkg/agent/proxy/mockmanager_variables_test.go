package proxy

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// exactManager is a manager staged as the agent stages a set under exact
// serving: exact serving goes on with no pool (serveKeployTestExactly's
// Stage, servePoolExactly), then every mock of the set goes in, once, at
// BaseTime. The start-up is served the pool alone: what it binds is the
// run's.
func exactManager(t *testing.T, perTest, session []*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.SetMocksWithWindow(perTest, session, models.BaseTime, time.Now())
	return mm
}

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
	return exactManager(t, []*models.Mock{use1, fetch, use2}, []*models.Mock{boot}), use1, fetch, use2
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
		mm := exactManager(t, []*models.Mock{fetch}, []*models.Mock{boot})
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
			t.Setenv(provenswitch.Env, "0")
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
// pool call after it. A filter that opens a test case opens it with its
// variables in one step (Agent.OpenTestCase, Proxy.OpenExactTest); no other
// filter begins a test's variables (MockFilterParams.BeginsTestVariables),
// and filter checks that rule of each. The staging begins no test: what the
// start-up binds after it is the run's, every test's of the set, and the
// next set's Mock forgets it, its start-up binding the run's anew.
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
		if params.BeginsTestVariables() {
			t.Fatalf("%+v begins a test's variables: only a test case's open does, as it opens", params)
		}
	}
	open := func(params models.MockFilterParams, owned ...*models.Mock) {
		t.Helper()
		if !params.BeginsTestVariables() {
			t.Fatalf("%+v, a test case's open, begins no test's variables", params)
		}
		if err := p.OpenExactTest(params.Exact.Test, owned, params.Variables); err != nil {
			t.Fatal(err)
		}
	}
	for set, key := range []string{"x-1", "x-2"} {
		if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true, StartsTestSet: set > 0}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		boot := varMock("boot", "/session", false, models.LifetimeSession)
		uses := []*models.Mock{varMock("use-1", "/use-1", true, models.LifetimePerTest), varMock("use-2", "/use-2", true, models.LifetimePerTest)}
		// The staging (the agent's serveKeployTestExactly): exact serving
		// goes on with no pool, the set's mocks go in, and its pool is
		// served.
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.SetMocksWithWindow(uses, []*models.Mock{boot}, models.BaseTime, time.Now())
		mm.ServeExactPool(models.PoolKeyNone, []string{"boot"})
		filter(models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true, Pool: []string{"boot"}}})
		want := `200 {"echo":"` + key + `"}`
		if got := postKey(t, mm, "/session", key); got != want {
			t.Fatalf("set %d: the start-up call was answered %s", set+1, got)
		}
		for i, use := range uses {
			name := "test-" + string(rune('1'+i))
			id := "u-" + string(rune('1'+i))
			open(models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: []string{use.Name}},
				Variables: []models.VariableBinding{{Name: "id", Value: id, Recorded: "u-0", Mock: "test case " + name, Place: "request"}}}, use)
			if b, ok := mm.Variables().Lookup("id"); !ok || b.Value != id || !b.Given {
				t.Fatalf("set %d: %s opens with id %+v (%v); want the %s its request gave", set+1, name, b, ok, id)
			}
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

// testOpenings are the ways a replay opens a test under the switch, each with
// its variables in one step: under exact serving (keploy test's test case,
// keploy mock replay's test: OpenExactTest) and as a test's window (keploy
// test's test case without exact serving: OpenTestWithWindow).
var testOpenings = []struct {
	name string
	open func(mm *MockManager, test string, own *models.Mock, given []models.VariableBinding)
}{
	{"exact serving", func(mm *MockManager, test string, own *models.Mock, given []models.VariableBinding) {
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.OpenExactTest(test, []*models.Mock{own}, given)
	}},
	{"a test's window", func(mm *MockManager, _ string, own *models.Mock, given []models.VariableBinding) {
		mm.OpenTestWithWindow([]*models.Mock{own}, nil, exactT0.Add(-time.Second), exactT0.Add(time.Second), given)
	}},
}

// A set's first test opens, and a call its own mock answers binds key as soon
// as the test's mocks are live (#4827, round 4). The test opens with its
// variables in one step, so the bind is the test's: the test reads it beside
// the value its request gave, and the next test, which only uses key, misses,
// as on main. Opened in two steps (the test's mocks live, then
// StartVariables), the bind landed in the run's, which every later test of
// the set reads: the next test's call was answered with this test's value, a
// false pass. A call checked before the test opened binds stale once it did,
// and is checked again.
func TestATestsOwnBindAsItOpensIsTheTestsNeverTheRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, o := range testOpenings {
		t.Run(o.name, func(t *testing.T) {
			own1 := varMock("own-1", "/own-1", false, models.LifetimePerTest)
			use2 := varMock("use-2", "/use-2", true, models.LifetimePerTest)
			mm := stagedManager(t, []*models.Mock{own1, use2}, nil)
			mm.NewVariableSet()
			before := mm.Variables().Gen()
			o.open(mm, "test-1", own1, []models.VariableBinding{{Name: "id", Value: "u1", Recorded: "u0", Mock: "test case test-1", Place: "request"}})
			if _, _, stale, ok := mm.Variables().BindAll(before, []models.VariableBinding{{Name: "late", Value: "l-1", Recorded: "l-0", Mock: "boot", Place: "body.key"}}); ok || !stale {
				t.Fatalf("a call checked before test-1 opened bound once it had: ok %v stale %v", ok, stale)
			}
			if got := postKey(t, mm, "/own-1", "x-1"); got != `200 {"echo":"x-1"}` {
				t.Fatalf("test-1's call was answered %s", got)
			}
			if got := boundNames(mm.BoundVariables()); got != "id,key" {
				t.Fatalf("test-1 holds %q; want the id its request gave and the key its own call bound", got)
			}
			o.open(mm, "test-2", use2, nil)
			if b, leaked := mm.Variables().Lookup("key"); leaked {
				t.Fatalf("test-1's binding %+v is in force in test-2", b)
			}
			if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
				t.Fatalf("test-2's call, which only uses key, was answered %s with test-1's value", got)
			}
		})
	}
}

// liveLine is a logger that calls the hook armed (once) as it writes msg: a
// line the mock manager logs while it makes a test's mocks live.
func liveLine(msg string) (*zap.Logger, *atomic.Pointer[func()]) {
	var armed atomic.Pointer[func()]
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	return zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if e.Message == msg {
			if hook := armed.Swap(nil); hook != nil {
				(*hook)()
			}
		}
		return nil
	})), &armed
}

// A test's mocks go live inside the step that begins its variables: a bind
// that reaches the table while they are live, of a call checked before the
// test opened, is stale, never the run's. Each opening is held while the
// test's mocks are live, on the line the manager logs there, until the bind
// is on its way. Opened in two steps, the bind raced the variables' begin,
// and landed in the run's whenever it got there first.
func TestATestsMocksGoLiveInTheStepThatBeginsItsVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name, line string
		exact      bool
		open       func(mm *MockManager, own, boot *models.Mock)
	}{
		{"exact serving", "exact serving", true, func(mm *MockManager, own, _ *models.Mock) {
			mm.OpenExactTest("test-1", []*models.Mock{own}, nil)
		}},
		{"a test's window", "routed startup-init mocks into startup tree", false, func(mm *MockManager, own, boot *models.Mock) {
			mm.OpenTestWithWindow([]*models.Mock{boot, own}, nil, exactT0.Add(-time.Second), exactT0.Add(time.Second), nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The bind races the begin only when the two are apart: each
			// round gives it one more chance to win.
			for round := 0; round < 64; round++ {
				logger, armed := liveLine(tc.line)
				mm := NewMockManager(nil, nil, logger)
				own := varMock("own-1", "/own-1", false, models.LifetimePerTest)
				// Recorded before the test's window: the window's staging
				// routes it to the startup tier, and says so.
				boot := newMockForTest("boot", exactT0.Add(-10*time.Second), models.LifetimePerTest)
				mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, time.Now())
				mm.NewVariableSet()
				if tc.exact {
					mm.ServeExactPool(models.PoolKeyNone, nil)
				}
				before := mm.Variables().Gen()
				live, onItsWay := make(chan struct{}), make(chan struct{})
				hold := func() { close(live); <-onItsWay }
				armed.Store(&hold)
				opened := make(chan struct{})
				go func() { defer close(opened); tc.open(mm, own, boot) }()
				select {
				case <-live:
				case <-opened:
					mm.Close()
					t.Fatalf("the opening logged no %q as the test's mocks went live: this test's hold is gone", tc.line)
				}
				close(onItsWay)
				_, _, stale, ok := mm.Variables().BindAll(before, []models.VariableBinding{{Name: "key", Value: "x-1", Recorded: "k-1", Mock: "own-1", Place: "body.key"}})
				<-opened
				_, bound := mm.Variables().Lookup("key")
				mm.Close()
				if ok || !stale || bound {
					t.Fatalf("round %d: a call checked before test-1 opened bound while its mocks were live: ok %v stale %v, key bound %v", round, ok, stale, bound)
				}
			}
		})
	}
}

// A test opens carrying what the answers of its calls in flight bound in the
// gap before it (the scope begun early, AheadVariables), each as made, a
// value its request gave winning over a carried one of its name; never what
// a call it did not answer bound there.
func TestATestOpensCarryingWhatItsCallsInFlightBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, given := range [][]models.VariableBinding{nil, {{Name: "g", Value: "given"}, {Name: "id", Value: "from-the-request"}}} {
		fetch := exactMock("fetch", time.Second, models.LifetimePerTest)
		own := exactMock("own-2", 2*time.Second, models.LifetimePerTest)
		mm := stagedManager(t, []*models.Mock{fetch, own}, nil)
		mm.NewVariableSet()
		mm.ServeExactPool(models.PoolKeyNone, nil, fetch)
		mm.AheadVariables().Bind(models.VariableBinding{Name: "id", Value: "live-7", Recorded: "rec-7", Mock: "fetch"})
		mm.AheadVariables().Bind(models.VariableBinding{Name: "stray", Value: "s", Recorded: "s-0", Mock: "unanswered"})
		pt, _ := mm.GetPerTestMocksInWindow()
		if len(pt) != 1 || !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight was not answered in the gap")
		}
		mm.OpenExactTest("test-2", []*models.Mock{fetch, own}, given)
		got := mm.BoundVariables()
		if given == nil {
			if len(got) != 1 || got[0].Name != "id" || got[0].Value != "live-7" || got[0].Given {
				t.Fatalf("test-2 opens with %+v; want id bound to live-7, as its call in flight made it", got)
			}
			continue
		}
		if boundNames(got) != "g,id" {
			t.Fatalf("test-2 opens with %s; want its given values and what its answered call in flight bound", boundNames(got))
		}
		for _, b := range got {
			if b.Name == "id" && (b.Value != "from-the-request" || !b.Given) {
				t.Fatalf("a given value wins over a carried one: %+v", b)
			}
		}
	}
}

// A new set begins the scope the next test's calls in flight bind in afresh,
// as it begins the run's (NewVariableSet): a call of the previous set checked
// in that scope before the set began binds stale in it, and the new set's
// start-up is checked beside nothing that scope held, so it binds the run's
// values freely.
func TestANewSetBeginsTheScopeBegunEarlyAfresh(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := stagedManager(t, nil, nil)
	mm.NewVariableSet()
	ahead := mm.AheadVariables()
	gen := ahead.Gen()
	if _, _, _, ok := ahead.BindAll(gen, []models.VariableBinding{{Name: "order", Value: "V", Recorded: "o-0", Mock: "fetch", Place: "body.id"}}); !ok {
		t.Fatal("the next test's call in flight did not bind in the scope begun early")
	}
	mm.NewVariableSet()
	if _, _, stale, ok := ahead.BindAll(gen, []models.VariableBinding{{Name: "late", Value: "L", Recorded: "l-0", Mock: "fetch", Place: "body.id"}}); ok || !stale {
		t.Fatalf("a call of the previous set, checked before the next began, bound in the next set's early scope: ok %v stale %v", ok, stale)
	}
	if got := mm.Variables().Beside(); len(got) != 0 {
		t.Fatalf("the next set's start-up is checked beside %+v, of the previous set", got)
	}
	run := mm.Variables()
	if _, _, _, ok := run.BindAll(run.Gen(), []models.VariableBinding{{Name: "run-session", Value: "V", Recorded: "s-0", Mock: "boot", Place: "body.session"}}); !ok {
		t.Fatal("the next set's start-up was refused a value the previous set's early scope held")
	}
}

// A test set staged without exact serving arms every test's recordings for
// its start-up (setMocksWithWindowLocked): a start-up call one of them
// answers binds that test's value, which cannot be told from the run's. The
// set keeps no run's then: what its start-up binds is the first test's to
// forget, and a later test, whose call only uses the variable, misses. Kept
// as the run's, that later test was answered with the value, a false pass.
// Under exact serving the start-up is served the pool alone, and its
// bindings are the run's (TestKeployTestsStartUpBindsForEveryTestOfItsSet).
func TestAStartUpServedEveryTestsRecordingsKeepsNoRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMock("own-1", "/own-1", false, models.LifetimePerTest)
	use2 := varMock("use-2", "/use-2", true, models.LifetimePerTest)
	use2.Spec.ReqTimestampMock, use2.Spec.ResTimestampMock = exactT0.Add(5*time.Second), exactT0.Add(5*time.Second+time.Millisecond)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, use2}, nil, models.BaseTime, time.Now())
	if got := postKey(t, mm, "/own-1", "x-1"); got != `200 {"echo":"x-1"}` {
		t.Fatalf("the start-up call was answered %s by test-1's recording, staged for the start-up", got)
	}
	mm.OpenTestWithWindow([]*models.Mock{own1, use2}, nil, exactT0.Add(-time.Second), exactT0.Add(time.Second), nil)
	if got := boundNames(mm.BoundVariables()); got != "" {
		t.Fatalf("test-1 opens with %q bound: the start-up's binding was kept as the run's", got)
	}
	mm.OpenTestWithWindow([]*models.Mock{own1, use2}, nil, exactT0.Add(4*time.Second), exactT0.Add(6*time.Second), nil)
	if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("test-2's call, which only uses key, was answered %s with the start-up's value", got)
	}
}

// A set's Mock that comes again with no new staging (a re-registration)
// keeps what its staging decided of the run's: a staging from BaseTime
// without exact serving (either staging call), or a test's window opened
// from BaseTime, armed
// every test's recordings for the start-up, and they are still staged. A
// start-up call one of them answers is the first test's to forget, and a
// later test, whose call only uses the variable, misses. Once exact serving
// serves the pool, the start-up's bindings are the run's again.
func TestASetsMockAgainKeepsWhatItsStagingDecidedOfTheRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, staging := range []string{"a staging", "a three-tier staging", "a test's window opened from BaseTime"} {
		own1 := varMock("own-1", "/own-1", false, models.LifetimePerTest)
		use2 := varMock("use-2", "/use-2", true, models.LifetimePerTest)
		use2.Spec.ReqTimestampMock, use2.Spec.ResTimestampMock = exactT0.Add(5*time.Second), exactT0.Add(5*time.Second+time.Millisecond)
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.NewVariableSet()
		switch staging {
		case "a staging":
			mm.SetMocksWithWindow([]*models.Mock{own1, use2}, nil, models.BaseTime, time.Now())
		case "a three-tier staging":
			mm.SetMocksWithWindowThreeTier([]*models.Mock{own1, use2}, nil, nil, models.BaseTime, time.Now())
		default:
			mm.OpenTestWithWindow([]*models.Mock{own1, use2}, nil, models.BaseTime, time.Now(), nil)
		}
		mm.ResetForReplaySession()
		mm.NewVariableSet()
		if got := postKey(t, mm, "/own-1", "x-1"); got != `200 {"echo":"x-1"}` {
			t.Fatalf("%s: the start-up call was answered %s by test-1's recording, staged for the start-up", staging, got)
		}
		mm.OpenTestWithWindow([]*models.Mock{own1, use2}, nil, exactT0.Add(-time.Second), exactT0.Add(time.Second), nil)
		if got := boundNames(mm.BoundVariables()); got != "" {
			t.Fatalf("%s: test-1 opens with %q bound: the start-up's binding was kept as the run's", staging, got)
		}
		mm.OpenTestWithWindow([]*models.Mock{own1, use2}, nil, exactT0.Add(4*time.Second), exactT0.Add(6*time.Second), nil)
		if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
			t.Fatalf("%s: test-2's call, which only uses key, was answered %s with the start-up's value", staging, got)
		}
	}
}

// Exact serving going on after a set was staged without it does not turn the
// run's back on for that set: a start-up call checked against the trees that
// armed every test's recordings may bind after it, and would land in the
// run's. Nor for the next set, whose Mock comes before its exact staging
// says the start-up is served exactly (AllowRun): it keeps none, a false
// fail, never a false pass, and the same miss as main. The set after it keeps
// the run's again. Each set is begun in keploy test's order: Mock, exact
// serving on, the staging, the pool.
func TestGoingExactAfterAStartUpServedEveryTestsRecordingsKeepsNoRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMock("own-1", "/own-1", false, models.LifetimePerTest)
	use2 := varMock("use-2", "/use-2", true, models.LifetimePerTest)
	use2.Spec.ReqTimestampMock, use2.Spec.ResTimestampMock = exactT0.Add(5*time.Second), exactT0.Add(5*time.Second+time.Millisecond)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, use2}, nil, models.BaseTime, time.Now())
	// A start-up call answered by own-1 from the startup tier, checked now.
	gen := mm.Variables().Gen()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if _, _, _, ok := mm.Variables().BindAll(gen, []models.VariableBinding{{Name: "key", Value: "x-1", Recorded: "k-1", Mock: "own-1", Place: "body.key"}}); !ok {
		t.Fatal("the start-up call's bind did not take")
	}
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	if got := boundNames(mm.BoundVariables()); got != "" {
		t.Fatalf("test-1 opens with %q bound: the start-up's bind was kept as the run's", got)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-2", []*models.Mock{use2}, nil)
	if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("test-2's call, which only uses key, was answered %s with a value test-1's recording answered at start-up", got)
	}

	// The next sets, each staged exactly: the first keeps no run's, the one
	// after it does.
	for set, keeps := range []bool{false, true} {
		boot := varMock("boot", "/session", false, models.LifetimeSession)
		next := varMock("use-1", "/use-1", true, models.LifetimePerTest)
		mm.ResetForReplaySession()
		mm.NewVariableSet()
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.SetMocksWithWindow([]*models.Mock{next}, []*models.Mock{boot}, models.BaseTime, time.Now())
		mm.ServeExactPool(models.PoolKeyNone, []string{"boot"})
		if got := postKey(t, mm, "/session", "s-1"); got != `200 {"echo":"s-1"}` {
			t.Fatalf("set %d: the start-up call was answered %s", set+2, got)
		}
		mm.OpenExactTest("test-1", []*models.Mock{next}, nil)
		if got := postKey(t, mm, "/use-1", "s-1"); (got == `200 {"echo":"s-1"}`) != keeps {
			t.Fatalf("set %d: test-1, which uses the start-up's value, was answered %s; the run's kept: %v", set+2, got, keeps)
		}
	}
}

// keploy's proxy opens a test with its variables in one step, with the values
// its request gave bound, as a test's window and under exact serving
// (Proxy.OpenTestWithWindow, Proxy.OpenExactTest): what the test's own call
// binds is the test's, and the next test, whose call only uses it, misses.
// With no mock manager there is nothing to open: a test given values is
// refused, a window given none stages nothing, as SetMocksWithWindow.
func TestTheProxyOpensATestWithItsVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	// Preseeded so Mock() leaves the host's /etc/nsswitch.conf alone (see
	// testProxy).
	p.nsswitchData = []byte("preseeded-by-test")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	given := []models.VariableBinding{{Name: "id", Value: "u1", Recorded: "u0", Mock: "test case test-1", Place: "request"}}
	start, end := exactT0.Add(-time.Second), exactT0.Add(time.Second)
	if err := p.OpenTestWithWindow(ctx, nil, nil, start, end, given); err == nil {
		t.Fatal("a proxy with no mock manager took the values a test's request gave")
	}
	if err := p.OpenTestWithWindow(ctx, nil, nil, start, end, nil); err != nil {
		t.Fatalf("a proxy with no mock manager refused a window given nothing: %v", err)
	}
	if err := p.OpenExactTest("test-1", nil, nil); err == nil {
		t.Fatal("a proxy with no mock manager opened a test under exact serving")
	}
	for _, exact := range []bool{false, true} {
		if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true, StartsTestSet: exact}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		own1 := varMock("own-1", "/own-1", false, models.LifetimePerTest)
		use2 := varMock("use-2", "/use-2", true, models.LifetimePerTest)
		use2.Spec.ReqTimestampMock, use2.Spec.ResTimestampMock = exactT0.Add(5*time.Second), exactT0.Add(5*time.Second+time.Millisecond)
		open := func(test string, own *models.Mock, from time.Time, given []models.VariableBinding) {
			t.Helper()
			var err error
			if exact {
				err = p.OpenExactTest(test, []*models.Mock{own}, given)
			} else {
				err = p.OpenTestWithWindow(ctx, []*models.Mock{own}, nil, from, from.Add(2*time.Second), given)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if exact {
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
		if err := p.SetMocksWithWindow(ctx, []*models.Mock{own1, use2}, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		if exact {
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
		open("test-1", own1, start, given)
		if got := boundNames(mm.BoundVariables()); got != "id" {
			t.Fatalf("exact %v: test-1 opens with %q bound; want the id its request gave", exact, got)
		}
		if got := postKey(t, mm, "/own-1", "x-1"); got != `200 {"echo":"x-1"}` {
			t.Fatalf("exact %v: test-1's call was answered %s", exact, got)
		}
		if got := boundNames(mm.BoundVariables()); got != "id,key" {
			t.Fatalf("exact %v: test-1 holds %q; want id and the key its own call bound", exact, got)
		}
		if exact {
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
		open("test-2", use2, exactT0.Add(4*time.Second), nil)
		if got := boundNames(mm.BoundVariables()); got != "" {
			t.Fatalf("exact %v: test-2 opens with %q bound", exact, got)
		}
		if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
			t.Fatalf("exact %v: test-2's call, which only uses key, was answered %s with test-1's value", exact, got)
		}
	}
}
