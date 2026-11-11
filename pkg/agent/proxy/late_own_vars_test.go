package proxy

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// keploy can decide a call after the replay began the next test. The call
// is still its own test's, and its variables are its own test's too: the
// variable pass checks it in the scope of the view it was read in, and binds
// what it binds there, as when keploy decides it at once (heldScope.
// Variables), never in the next test's.

// varMockNamed is varMockOf with its variable named name.
func varMockNamed(mock, path, name, recorded string, use bool) *models.Mock {
	m := varMockOf(mock, path, recorded, use, models.LifetimePerTest)
	m.Spec.Variables = models.MockVariables{name: m.Spec.Variables["key"]}
	return m
}

// postRaw is POST path with body {"key":key}.
func postRaw(path, key string) string {
	body := `{"key":"` + key + `"}`
	return "POST " + path + " HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// lateVarsRun is an agent store under the proven switch with test-1 opened,
// its variables begun, owning own (by name), the rest of staged staged for
// test-2, and, on a set served by the background rules, the pool holding
// pool. The call that binds key, test-1's POST /charge, was answered
// key=x-1. The calls go through db, which runs its move between a call's
// mark and its matcher's reads, once.
func lateVarsRun(t *testing.T, own []string, staged []*models.Mock, pool ...*models.Mock) (*viewMovedAtMark, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm := stagedManager(t, append([]*models.Mock{varMock("own-charge", "/charge", false, models.LifetimePerTest)}, staged...), pool)
	if len(pool) > 0 {
		var names []string
		for _, mk := range pool {
			names = append(names, mk.Name)
		}
		mm.SeedRecordedWindows([]models.TestWindow{
			{TestCase: "test-1", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)},
			{TestCase: "test-2", Start: exactT0.Add(6 * time.Second), End: exactT0.Add(8 * time.Second)},
		})
		mm.ServeExactBackground(models.RecordedWithS12Routing, names, nil)
		mm.ServeExactPool(models.PoolKeyNone, names)
	}
	owned := []*models.Mock{roundMock(mm, "own-charge")}
	for _, name := range own {
		owned = append(owned, roundMock(mm, name))
	}
	mm.OpenExactTest("test-1", owned, nil)
	if len(pool) > 0 && !integrations.S12Background(mm) {
		t.Fatal("precondition: test-1 is not served by the background rules")
	}
	if got := postKey(t, mm, "/charge", "x-1"); got != `200 {"echo":"x-1"}` {
		t.Fatalf("test-1's POST /charge was answered %s", got)
	}
	return &viewMovedAtMark{MockManager: mm}, mm
}

// openTest2 opens test-2, its variables begun, owning names.
func openTest2(mm *MockManager, names ...string) func() {
	return func() {
		var owned []*models.Mock
		for _, n := range names {
			owned = append(owned, roundMock(mm, n))
		}
		mm.OpenExactTest("test-2", owned, nil)
	}
}

// boundOf is bs as name=value;...
func boundOf(bs []models.VariableBinding) string {
	out := ""
	for _, b := range bs {
		out += b.Name + "=" + b.Value + ";"
	}
	return out
}

// test-1's POST /use, which uses key, read in test-1's view and decided after
// the replay began test-2, is checked against test-1's key: with a value
// test-1 never bound (key is x-1 there) it misses on test-1's own recording,
// whose variable did not hold, and test-2's own POST /charge then binds
// test-2's key. Before, the late call was checked in test-2's scope, where
// key was not bound, so test-1's recording fit it; its miss named test-2's
// POST /charge as the closest recording, with no word of key.
func TestALateCallIsCheckedAgainstItsOwnTestsVariables(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest), varMock("own2-charge", "/charge", false, models.LifetimePerTest)})
	db.move = openTest2(mm, "own2-charge")
	got, miss := httpMissSend(t, db, postRaw("/use", "x-9"))
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got != "" || miss.MarkedTest != "test-1" || miss.ClosestMock != "own-use" || miss.MatchPhase != models.MatchPhaseVariable || !strings.Contains(miss.Diff, `variable key is "x-1" in this test`) {
		t.Fatalf("test-1's late POST /use key=x-9 was answered %q (miss %+v); want a miss of test-1's on own-use, whose key (x-1 in test-1) did not hold", got, miss)
	}
	if got := postKey(t, mm, "/charge", "x-2"); got != `200 {"echo":"x-2"}` {
		t.Fatalf("test-2's own POST /charge key=x-2 was answered %s; want x-2", got)
	}
	if got := boundOf(mm.BoundVariables()); got != "key=x-2;" {
		t.Fatalf("test-2's variables are %q; want key=x-2 only", got)
	}
	if got := boundOf(mm.Variables().EndedOf("test-1")); got != "key=x-1;" {
		t.Fatalf("test-1's ended scope holds %q; want key=x-1", got)
	}
}

// test-1's POST /use key=x-1 once more than test-1 recorded, read in
// test-1's view and decided after the replay began test-2 and test-2 bound
// key=x-2, is test-1's own call: the own-call check matches it against
// test-1's answered POST /use with test-1's key, so it misses as one call
// more than test-1 recorded, and the pool's copy of the call is never spent
// on it. Before, the check read test-2's key, test-1's recording did not
// fit, and the pool's copy answered the call: a request sent twice passed.
func TestALateCallOnceMoreIsOwnByItsOwnTestsVariables(t *testing.T) {
	poolUse := varMockOf("pool-use", "/use", "x-1", true, models.LifetimeSession)
	poolUse.Spec.Variables = nil
	poolUse.Spec.ReqTimestampMock, poolUse.Spec.ResTimestampMock = exactT0.Add(time.Second), exactT0.Add(time.Second+time.Millisecond)
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest), varMock("own2-charge", "/charge", false, models.LifetimePerTest)}, poolUse)
	if got := postKey(t, mm, "/use", "x-1"); got != `200 {"echo":"x-1"}` {
		t.Fatalf("test-1's POST /use was answered %s; want its own", got)
	}
	db.move = func() {
		openTest2(mm, "own2-charge")()
		if got := postKey(t, mm, "/charge", "x-2"); got != `200 {"echo":"x-2"}` {
			t.Errorf("test-2's own POST /charge key=x-2 was answered %s", got)
		}
	}
	got, miss := httpMissSend(t, db, postRaw("/use", "x-1"))
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got != "" {
		t.Fatalf("test-1's POST /use once more, decided after test-2 bound key=x-2, was answered %q; want a miss: one call more than test-1 recorded", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 || miss.MarkedTest != "test-1" {
		t.Fatalf("its miss is kind %q, recorded %d, marked in %q; want %q, recorded 1, marked in test-1", miss.MissKind, miss.Recorded, miss.MarkedTest, models.MissExtraCall)
	}
	if mm.MockSpent("pool-use") {
		t.Fatal("the pool's POST /use was spent on a call it did not answer")
	}
}

// A call of the gap after test-1 that the pool view answers as test-2's call
// in flight (its recording is test-2's), decided after the replay began
// test-2, binds in test-2's scope, as test-2 would have carried it in at
// once: never in test-1's, the scope the gap's other calls read.
func TestALateCallInFlightForTheNextTestBindsInThatTest(t *testing.T) {
	db, mm := lateVarsRun(t, nil, []*models.Mock{varMockNamed("own2-fetch", "/fetch", "id", "k-5", false)})
	mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "own2-fetch"))
	db.move = openTest2(mm, "own2-fetch")
	if got, miss := httpMissSend(t, db, postRaw("/fetch", "x-7")); got != `{"echo":"x-7"}` {
		t.Fatalf("test-2's call in flight was answered %q (miss %+v); want test-2's recording", got, miss)
	}
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got := boundOf(mm.BoundVariables()); got != "id=x-7;" {
		t.Fatalf("test-2's variables are %q; want id=x-7, bound by its call in flight", got)
	}
	if got := boundOf(mm.Variables().EndedOf("test-1")); got != "key=x-1;" {
		t.Fatalf("test-1's ended scope holds %q; want key=x-1 only", got)
	}
}
