package proxy

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// A call's miss is judged by the background recordings of the set it was
// marked in, not of the set staged when keploy decides it: the replay can
// stage the next set first. The install before the current one is kept for
// that; a call marked before both reads none (it counts), the zero
// CallRound reads the current set's, a restage within a set keeps its
// recordings, and a seed within one staging replaces that staging's alone.
func TestACallIsJudgedByTheBackgroundRecordingsOfTheSetItWasMarkedIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	reads := func(what string, call integrations.CallRound, want ...string) {
		t.Helper()
		if got := bgNames(mm.BackgroundRecordingsIn(call, models.Mongo)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the manager reads %v; want %v", what, got, want)
		}
		if got := bgNames(integrations.BackgroundRecordingsAt(view, call, models.Mongo)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: a test's view reads %v; want %v", what, got, want)
		}
	}

	// Set A.
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("a-poll", models.Mongo, 1)})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	a := mark()
	reads("a call of set A", a, "a-poll")
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now()) // a restage within A
	if mm.StagingEpoch() == a.Epoch() {
		t.Fatal("the restage started no staging epoch (this case's premise)")
	}
	reads("a call of set A marked before its restage", a, "a-poll")
	reads("a call of set A marked after its restage", mark(), "a-poll")

	// Set B is seeded: until it is staged, A's are in force.
	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("b-poll", models.Mongo, 2)})
	reads("a call of set A, B seeded and not staged", a, "a-poll")
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	b := mark()
	reads("a call of set A decided after B's staging", a, "a-poll")
	reads("a call of set B", b, "b-poll")
	reads("a call of no mark", integrations.CallRound{}, "b-poll")
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"b-poll"}) {
		t.Fatalf("the current set's are %v; want [b-poll]", got)
	}

	// Set C seeds none: B's call keeps B's, A's (two sets back) reads none.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	c := mark()
	reads("a call of set C, which seeded none", c)
	reads("a call of set B decided after C's staging", b, "b-poll")
	reads("a call of set A, two sets back", a)

	// A seed within C's staging replaces C's there, and keeps B's.
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("c-poll", models.Mongo, 3)})
	reads("a call of set C after its seed", c, "c-poll")
	reads("a call of set B after C's seed", b, "b-poll")
	// And one in a later staging of C keeps C's first for the calls before.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("c2-poll", models.Mongo, 4)})
	reads("a call of set C before its second seed", c, "c-poll")
	reads("a call of set C after its second seed", mark(), "c2-poll")
	reads("a call of set B after C's second seed", b)
}

// A call's background miss reads the bound values of the set it was marked
// in, as its background recordings: what that set's tests bound and the
// test each recording trails by that set's scope table, not the next set's,
// which the replay can begin first and which forgets every binding of the
// set before. The set before the current one is kept; a call marked before
// both reads none (its background miss counts); the zero CallRound and a
// call of the current set read the current set's; a test's view forwards
// it; and a new set begun again before its staging keeps the set before.
func TestACallReadsTheBoundValuesOfTheSetItWasMarkedIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	bind := func(name, value, mock string) models.VariableBinding {
		return models.VariableBinding{Name: name, Value: value, Recorded: "rec-" + name, Mock: mock, Place: "body.id"}
	}
	type read struct{ own, ended, trail, trailMocks string }
	text := func(bv integrations.BoundValues, endedTest, mock string) *read {
		if bv == nil {
			return nil
		}
		names := func(bs []models.VariableBinding) string {
			out := ""
			for _, b := range bs {
				out += b.Name + "=" + b.Value + ";"
			}
			return out
		}
		tr := bv.TrailOf(mock)
		return &read{own: names(bv.Own()), ended: names(bv.EndedOf(endedTest)), trail: tr.Test, trailMocks: fmt.Sprint(tr.Mocks)}
	}
	reads := func(what string, call integrations.CallRound, endedTest, mock string, want *read) {
		t.Helper()
		if got := text(mm.BoundValuesIn(call), endedTest, mock); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the manager reads %+v; want %+v", what, got, want)
		}
		if got := text(integrations.BoundValuesAt(view, call), endedTest, mock); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: a test's view reads %+v; want %+v", what, got, want)
		}
	}

	// Set A: test-a1 bound app=a1 at mock-1 and ended; test-a2 bound
	// own=a2. mock-9, a worker: recording, trails test-a1.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetPoolTests(map[string][]string{"test-a1": {"mock-1"}, "test-a2": {"mock-2"}})
	mm.SetPoolTrails(map[string]string{"mock-9": "test-a1"})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.Variables().StartOpeningTest("test-a1", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("app", "a1", "mock-1"))
	mm.Variables().StartOpeningTest("test-a2", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("own", "a2", "mock-2"))
	a := mark()
	setA := &read{own: "own=a2;", ended: "app=a1;", trail: "test-a1", trailMocks: "[mock-1]"}
	reads("a call of set A", a, "test-a1", "mock-9", setA)

	// Set B begins (twice: the replay's re-registration before staging),
	// with its own scope table, which trails nothing; staged, its test-b1
	// binds own=b1.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	reads("a call of set A, B begun and not staged", a, "test-a1", "mock-9", setA)
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	reads("a call of set A, B begun again and not staged", a, "test-a1", "mock-9", setA)
	mm.SetPoolTests(map[string][]string{"test-b1": {"mock-1"}})
	mm.SetPoolTrails(map[string]string{})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.StagingEpoch() == a.Epoch() {
		t.Fatal("set B's staging started no staging epoch (this case's premise)")
	}
	mm.Variables().StartOpeningTest("test-b1", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("own", "b1", "mock-1"))
	b := mark()
	setB := &read{own: "own=b1;", trailMocks: "[]"}
	reads("a call of set A decided in set B", a, "test-a1", "mock-9", setA)
	reads("a call of set B", b, "test-a1", "mock-9", setB)
	reads("a call of no mark", integrations.CallRound{}, "test-a1", "mock-9", setB)

	// Set C: B's call reads B's as B ended; A's, two sets back, none.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	reads("a call of set B decided in set C", b, "test-a1", "mock-9", setB)
	reads("a call of set A, two sets back", a, "test-a1", "mock-9", nil)
	reads("a call of set C", mark(), "test-a1", "mock-9", &read{trailMocks: "[]"})
}

// A test's call keploy decides after the replay began the set's next test
// reads, as its own test's bindings, the scope of the test its mark names
// (variables.Table.OwnOf), not the next test's, which is running when keploy
// decides it: at once, it read its own test's scope. So too once the next
// set began (the kept set's). A pool view's call decided at once reads the
// running scope, the ended test's; a mark of a test whose scope bound
// nothing reads none (its background miss counts); and while the running
// scope names no test, nothing tells it is not the call's, so it is read.
func TestACallReadsTheBindingsOfTheTestItWasMarkedIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	own2 := varMockOf("own-2", "/apps", "app-2", true, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, nil)
	own := func(what string, call integrations.CallRound, want string) {
		t.Helper()
		names := func(bv integrations.BoundValues) string {
			if bv == nil {
				return "<no set>"
			}
			out := ""
			for _, b := range bv.Own() {
				out += b.Name + "=" + b.Value + ";"
			}
			return out
		}
		if got := names(mm.BoundValuesIn(call)); got != want {
			t.Fatalf("%s: the manager reads %q as the call's own test's; want %q", what, got, want)
		}
		if got := names(integrations.BoundValuesAt(view, call)); got != want {
			t.Fatalf("%s: a test's view reads %q as the call's own test's; want %q", what, got, want)
		}
	}
	bind := func(name, value, mock string) {
		t.Helper()
		if _, ok := mm.Variables().Bind(variables.Binding{Name: name, Value: value, Recorded: "rec-" + name, Mock: mock, Place: "body.key"}); !ok {
			t.Fatalf("the running scope did not bind %s (this case's premise)", name)
		}
	}

	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("key", "one", "own-1")
	in1 := integrations.MarkCallRound(mm)
	if in1.Test() != "test-1" {
		t.Fatalf("a call read in test-1's view is marked of test %q; want test-1 (this case's premise)", in1.Test())
	}
	own("test-1's call decided in test-1", in1, "key=one;")
	mm.ServeExactPool(models.PoolKeyNone, nil)
	gap := integrations.MarkCallRound(mm)
	if gap.Test() != "" {
		t.Fatalf("a call read in the pool view is marked of test %q; want none (this case's premise)", gap.Test())
	}
	own("a call of the pool view after test-1, decided then", gap, "key=one;")
	mm.OpenExactTest("test-2", []*models.Mock{own2}, nil)
	bind("other", "two", "own-2")
	in2 := integrations.MarkCallRound(mm)
	own("test-1's call decided after test-2 began", in1, "key=one;")
	own("test-2's call", in2, "other=two;")
	ghost := integrations.NewCallRound(in1.Epoch(), nil).OfTest("test-9")
	own("a call of a test whose scope bound nothing", ghost, "")
	own("a call of no mark", integrations.CallRound{}, "other=two;")

	// The next set begins: the set's calls read the kept set's.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	if mm.StagingEpoch() == in1.Epoch() {
		t.Fatal("the next set's staging started no staging epoch (this case's premise)")
	}
	own("test-1's call decided in the next set", in1, "key=one;")
	own("test-2's call decided in the next set", in2, "other=two;")
	own("a call of a test whose scope bound nothing, in the next set", ghost, "")

	// A scope begun with no test named: nothing tells it is not test-3's.
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-3", []*models.Mock{own1}, nil)
	in3 := integrations.MarkCallRound(mm)
	mm.ResetVariables()
	bind("unnamed", "u", "own-1")
	own("test-3's call while a scope that names no test runs", in3, "unnamed=u;")
}

// boundOwnNames is bv's Own as name=value pairs, "<no set>" for none.
func boundOwnNames(bv integrations.BoundValues) string {
	if bv == nil {
		return "<no set>"
	}
	out := ""
	for _, b := range bv.Own() {
		out += b.Name + "=" + b.Value + ";"
	}
	return out
}

// A call marked in the pool view after test-1 ended names no test, and is
// test-1's at zero latency: test-1's variable scope runs until test-2's
// begins. Decided after test-2 began, or once the next set began, it reads
// test-1's bindings as its own, never test-2's; a pool-view call after
// test-2 reads test-2's, and one before the set's first test reads none.
func TestAPoolViewCallDecidedAfterTheNextTestBeganReadsTheEndedTestsScope(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	own2 := varMockOf("own-2", "/apps", "app-2", true, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, nil)
	own := func(what string, call integrations.CallRound, want string) {
		t.Helper()
		if got := boundOwnNames(mm.BoundValuesIn(call)); got != want {
			t.Fatalf("%s: the manager reads %q as the call's own test's; want %q", what, got, want)
		}
		if got := boundOwnNames(integrations.BoundValuesAt(view, call)); got != want {
			t.Fatalf("%s: a test's view reads %q as the call's own test's; want %q", what, got, want)
		}
	}
	bind := func(name, value, mock string) {
		t.Helper()
		if _, ok := mm.Variables().Bind(variables.Binding{Name: name, Value: value, Recorded: "rec-" + name, Mock: mock, Place: "body.key"}); !ok {
			t.Fatalf("premise: no bind of %s", name)
		}
	}
	pre := integrations.MarkCallRound(mm)
	if _, known := pre.VariableScope(); !known {
		t.Fatal("premise: a pool view's mark keeps no variable scope")
	}
	own("a pool-view call before the set's first test, decided then", pre, "")
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("key", "one", "own-1")
	own("a pool-view call before the set's first test, decided once it began", pre, "")
	mm.ServeExactPool(models.PoolKeyNone, nil)
	gap := integrations.MarkCallRound(mm)
	if gap.Test() != "" {
		t.Fatalf("premise: pool view mark names %q", gap.Test())
	}
	own("the pool-view call after test-1, decided at once", gap, "key=one;")
	mm.OpenExactTest("test-2", []*models.Mock{own2}, nil)
	bind("other", "two", "own-2")
	in2 := integrations.MarkCallRound(mm)
	own("the pool-view call after test-1, decided after test-2 began", gap, "key=one;")
	own("test-2's call", in2, "other=two;")
	mm.ServeExactPool(models.PoolKeyNone, nil)
	gap2 := integrations.MarkCallRound(mm)
	own("the pool-view call after test-2, decided at once", gap2, "other=two;")
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("key", "again", "own-1")
	own("the pool-view call after test-2, decided after test-1 ran again", gap2, "other=two;")
	own("the pool-view call after test-1's first run, decided in its second", gap, "key=one;")

	// The next set begins: the set's calls read the kept set's. A call of
	// the view the session's reset serves between the sets reads the scope
	// running then, the set's last test's, decided then or in the next set.
	mm.ResetForReplaySession()
	between := integrations.MarkCallRound(mm)
	own("a call between the sets, decided then", between, "key=again;")
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	if mm.StagingEpoch() == gap.Epoch() {
		t.Fatal("premise: the next set's staging started no staging epoch")
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("key", "next-set", "own-1")
	own("the pool-view call after test-1, decided in the next set", gap, "key=one;")
	own("the pool-view call after test-2, decided in the next set", gap2, "other=two;")
	own("test-2's call, decided in the next set", in2, "other=two;")
	own("the pool-view call before the first test, decided in the next set", pre, "")
	own("a call between the sets, decided in the next set", between, "key=again;")
}

// A test run twice in one set (keploy replay's mock-consistency retry
// cycles) has two variable scopes: a call marked in one run, decided once
// another test began, reads that run's bindings as its own, never the
// other run's nor both (two live values of one name read as no binding).
func TestARetriedTestsCallReadsTheRunItWasMarkedIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	own2 := varMockOf("own-2", "/apps", "app-2", true, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, nil)
	own := func(what string, call integrations.CallRound, want string) {
		t.Helper()
		if got := boundOwnNames(mm.BoundValuesIn(call)); got != want {
			t.Fatalf("%s: reads %q as the call's own test's; want %q", what, got, want)
		}
	}
	bind := func(value string) {
		t.Helper()
		if _, ok := mm.Variables().Bind(variables.Binding{Name: "key", Value: value, Recorded: "rec-key", Mock: "own-1", Place: "body.key"}); !ok {
			t.Fatalf("premise: no bind of %s", value)
		}
	}
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("first")
	run1 := integrations.MarkCallRound(mm)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-2", []*models.Mock{own2}, nil)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bind("second")
	run2 := integrations.MarkCallRound(mm)
	if run1.Test() != "test-1" || run2.Test() != "test-1" {
		t.Fatalf("premise: the runs' marks name %q and %q", run1.Test(), run2.Test())
	}
	own("the first run's call, decided in the second", run1, "key=first;")
	own("the second run's call, decided at once", run2, "key=second;")
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-2", []*models.Mock{own2}, nil)
	own("the first run's call, decided once another test began", run1, "key=first;")
	own("the second run's call, decided once another test began", run2, "key=second;")
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	own("the first run's call, decided in the next set", run1, "key=first;")
	own("the second run's call, decided in the next set", run2, "key=second;")
}
