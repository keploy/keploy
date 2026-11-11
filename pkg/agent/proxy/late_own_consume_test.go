package proxy

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy can decide a call after the replay ended its test, or began the
// next. At zero latency it is decided in its own test's view, so its consume
// of one of its own test's recordings settles in that view
// (roundGuard.ownView): a recording its test left unanswered answers it,
// and the next test's never does. Before, the consume settled in the view
// in force, which refused it: the call missed with its own recording
// unanswered, or read on in the view in force and was answered by the next
// test's, bound in the next test's scope.

// test-1's POST /open, which binds id, read in test-1's view and decided
// after the replay began test-2, binds id in test-1's scope, as at once:
// test-1's ended scope holds it, test-2's does not, and test-2's own POST
// /open binds test-2's id. Before, test-2's own POST /open answered the
// late call and id was bound in test-2's scope, and test-2's own call with
// its own value was refused.
func TestALateCallBindsInItsOwnTestsScope(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-open"}, []*models.Mock{varMockNamed("own-open", "/open", "id", "k-5", false), varMockNamed("own2-open", "/open", "id", "k-5", false)})
	db.move = openTest2(mm, "own2-open")
	if got, miss := httpMissSend(t, db, postRaw("/open", "x-5")); got != `{"echo":"x-5"}` {
		t.Fatalf("test-1's late POST /open was answered %q (miss %+v); want its own", got, miss)
	}
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got := boundOf(mm.BoundVariables()); got != "" {
		t.Fatalf("test-2's variables are %q after test-1's late call; want none", got)
	}
	if got := boundOf(mm.Variables().EndedOf("test-1")); got != "id=x-5;key=x-1;" {
		t.Fatalf("test-1's ended scope holds %q; want id=x-5 and key=x-1", got)
	}
	if got := postKey(t, mm, "/open", "x-6"); got != `200 {"echo":"x-6"}` {
		t.Fatalf("test-2's own POST /open id=x-6 was answered %s; want x-6", got)
	}
	if got := boundOf(mm.BoundVariables()); got != "id=x-6;" {
		t.Fatalf("test-2's variables are %q; want id=x-6", got)
	}
}

// test-1's POST /use key=x-1, read in test-1's view and decided after the
// replay began test-2, is answered by test-1's own POST /use, which test-1
// left unanswered, checked against test-1's key: the call passes as it does
// decided at once. test-2's own POST /charge then binds test-2's key.
// Before, test-2's view refused test-1's recording and the call missed.
func TestALateCallIsAnsweredByItsOwnTestsUnansweredRecording(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest), varMock("own2-charge", "/charge", false, models.LifetimePerTest)})
	db.move = openTest2(mm, "own2-charge")
	if got, miss := httpMissSend(t, db, postRaw("/use", "x-1")); got != `{"echo":"x-1"}` {
		t.Fatalf("test-1's late POST /use key=x-1 was answered %q (miss %+v); want test-1's own POST /use", got, miss)
	}
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got := postKey(t, mm, "/charge", "x-2"); got != `200 {"echo":"x-2"}` {
		t.Fatalf("test-2's own POST /charge key=x-2 was answered %s; want x-2", got)
	}
	if got := boundOf(mm.BoundVariables()); got != "key=x-2;" {
		t.Fatalf("test-2's variables are %q; want key=x-2 only", got)
	}
}

// test-1's late POST /use, decided after test-1 ended, answered by test-1's
// own recording, is in test-1's outcome as it was kept when the test ended
// (ExactTestOutcome): the recording answered, and none is left unanswered,
// as when keploy decides the call at once. Before, the call missed and the
// outcome said test-1 never made it.
func TestALateCallsOwnAnswerIsInItsTestsOutcome(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest)})
	db.move = func() { mm.ServeExactPool(models.PoolKeyNone, nil) }
	if got, miss := httpMissSend(t, db, postRaw("/use", "x-1")); got != `{"echo":"x-1"}` {
		t.Fatalf("test-1's late POST /use key=x-1 was answered %q (miss %+v); want test-1's own", got, miss)
	}
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "test-1" || !slices.Equal(consumed, []string{"own-charge", "own-use"}) || len(unconsumed) != 0 {
		t.Fatalf("test-1's outcome is %q, answered %v, unanswered %v (%v); want test-1, own-charge and own-use answered, none unanswered", test, consumed, unconsumed, ok)
	}
}

// test-1's late POST /use, decided after test-2 began and ended, is answered
// by test-1's own recording, and the outcome kept is still test-2's, the
// latest test's: a late call renews only its own test's outcome.
func TestALateCallsOwnAnswerLeavesALaterTestsOutcome(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest), varMock("own2-charge", "/charge", false, models.LifetimePerTest)})
	db.move = func() {
		openTest2(mm, "own2-charge")()
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}
	if got, miss := httpMissSend(t, db, postRaw("/use", "x-1")); got != `{"echo":"x-1"}` {
		t.Fatalf("test-1's late POST /use key=x-1 was answered %q (miss %+v); want test-1's own", got, miss)
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "test-2" || len(consumed) != 0 || !slices.Equal(unconsumed, []string{"own2-charge"}) {
		t.Fatalf("the outcome kept is %q, answered %v, unanswered %v (%v); want test-2's, own2-charge unanswered", test, consumed, unconsumed, ok)
	}
}

// Two of test-1's POST /open read in test-1's view, both decided after the
// replay began test-2: the first takes test-1's one recording, and the
// second, its consume refused in test-1's view, reads on there, where the
// recording is answered, and misses as test-1's call. test-2's own POST
// /open is left for test-2's call. Read on in the view in force, test-2's
// recording would answer test-1's second call.
func TestALateCallRefusedInItsOwnViewNeverTakesTheNextTestsRecording(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-open"}, []*models.Mock{varMockNamed("own-open", "/open", "id", "k-5", false), varMockNamed("own2-open", "/open", "id", "k-5", false)})
	first := mm.MarkCallRound()
	db.move = openTest2(mm, "own2-open")
	took, hooked := false, false
	mm.OnConsume(func(name string) {
		if name != "own-open" || hooked {
			return
		}
		hooked = true
		h, _ := mm.HoldToRound(first)
		took = h.DeleteFilteredMock(*roundMock(mm, "own-open"))
	})
	t.Cleanup(func() { mm.OnConsume(nil) })
	got, miss := httpMissSend(t, db, postRaw("/open", "x-5"))
	if db.move != nil || !hooked {
		t.Fatal("the call was never marked, or never consumed; the row is about a late call whose consume is refused in its own view")
	}
	if !took {
		t.Fatal("precondition: test-1's first late call did not take test-1's own POST /open in test-1's view")
	}
	if got != "" || miss == nil || miss.MarkedTest != "test-1" {
		t.Fatalf("test-1's second late POST /open was answered %q (miss %+v); want a miss of test-1's", got, miss)
	}
	if got := postKey(t, mm, "/open", "x-6"); got != `200 {"echo":"x-6"}` {
		t.Fatalf("test-2's own POST /open id=x-6 was answered %s; want x-6", got)
	}
	if got := boundOf(mm.BoundVariables()); got != "id=x-6;" {
		t.Fatalf("test-2's variables are %q; want id=x-6", got)
	}
}

// A late call whose own test's set is staged again between its read and its
// consume is refused: the staging's recordings are another set's, so test-1's
// recording answers no call of test-1 that keploy decides after it, and
// test-1's view keeps it unanswered.
func TestALateCallsOwnConsumeIsRefusedOnceTheSetIsStagedAgain(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest), varMock("own2-charge", "/charge", false, models.LifetimePerTest)})
	test1 := mm.exactView()
	db.move = openTest2(mm, "own2-charge")
	hooked := false
	mm.OnConsume(func(name string) {
		if name == "own-use" && !hooked {
			hooked = true
			mm.bumpStagingEpoch()
		}
	})
	t.Cleanup(func() { mm.OnConsume(nil) })
	if got, _ := httpMissSend(t, db, postRaw("/use", "x-1")); got != "" {
		t.Fatalf("test-1's late POST /use, its set staged again before its consume, was answered %q; want a miss", got)
	}
	if !hooked {
		t.Fatal("precondition: the call never went to consume test-1's own POST /use")
	}
	test1.mu.Lock()
	o := test1.outcomeLocked()
	test1.mu.Unlock()
	if o.test != "test-1" || !slices.Equal(o.unconsumed, []string{"own-use"}) {
		t.Fatalf("test-1's view says %+v; want own-use unanswered", o)
	}
}

// A call of the gap that the pool view answers as test-2's call in flight,
// decided after the replay began test-2, is test-2's answer: its consume
// settles in the view in force, test-2's, which counts its recording
// answered, never in the pool view the call was read in, which test-2's
// begin closed after carrying what it had answered into test-2's view.
func TestALateCallInFlightForTheNextTestIsThatTestsAnswer(t *testing.T) {
	db, mm := lateVarsRun(t, nil, []*models.Mock{varMockNamed("own2-fetch", "/fetch", "id", "k-5", false)})
	mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "own2-fetch"))
	db.move = openTest2(mm, "own2-fetch")
	if got, miss := httpMissSend(t, db, postRaw("/fetch", "x-7")); got != `{"echo":"x-7"}` {
		t.Fatalf("test-2's call in flight was answered %q (miss %+v); want test-2's recording", got, miss)
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "test-2" || !slices.Equal(consumed, []string{"own2-fetch"}) || len(unconsumed) != 0 {
		t.Fatalf("test-2's outcome is %q, answered %v, unanswered %v (%v); want test-2 with own2-fetch answered", test, consumed, unconsumed, ok)
	}
}

// A late call of a replay session that ended is refused its own test's
// recording, unanswered: exact serving belongs to one session, and once the
// agent reset it (ResetForReplaySession) no data-plane recording of that
// session answers, however late keploy decides a call made in it. Refused,
// the call reads the view in force, the reset's, which shows none of
// test-1's recordings: it is not offered the refused one again. The reset
// may land before the consume is routed to test-1's view, or after,
// between the routing and the round lock the consume settles under: either
// way the consume is refused.
func TestALateCallsOwnConsumeIsRefusedOnceTheSessionEnded(t *testing.T) {
	db, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest)})
	db.move = mm.ResetForReplaySession
	if got, _ := httpMissSend(t, db, postRaw("/use", "x-1")); got != "" {
		t.Fatalf("test-1's late POST /use, decided after the session was reset, was answered %q; want a miss", got)
	}
	if db.move != nil {
		t.Fatal("precondition: the call was never marked")
	}

	for _, at := range []string{"before the consume", "between its routing and its lock"} {
		t.Run(at, func(t *testing.T) {
			_, mm := lateVarsRun(t, []string{"own-use"}, []*models.Mock{varMock("own-use", "/use", true, models.LifetimePerTest)})
			h, _ := mm.HoldToRound(mm.MarkCallRound())
			own := roundMock(mm, "own-use")
			if at == "before the consume" {
				mm.ResetForReplaySession()
			} else {
				round := &mm.exact.round
				mm.OnConsume(func(string) {
					mm.OnConsume(nil)
					round.onWait = func() {
						round.onWait = nil
						mm.ResetForReplaySession()
					}
				})
				t.Cleanup(func() { mm.OnConsume(nil); round.onWait = nil })
			}
			if h.DeleteUnFilteredMock(*own) {
				t.Fatal("test-1's late call took test-1's own recording after the session was reset; want it refused")
			}
			if mm.exact.round.onWait != nil {
				t.Fatal("precondition: the session was not reset inside the consume")
			}
			if pt, _ := h.GetPerTestMocksInWindow(); slices.ContainsFunc(pt, func(mk *models.Mock) bool { return mk.Name == own.Name }) {
				t.Fatalf("after the refusal the call was offered %v; want the reset view's reads, without own-use", exactNames(pt))
			}
		})
	}
}

// A call marked in the view the session reset installs, which no variable
// scope stamps (ResetForReplaySession: the next session's scopes are not
// known yet), reads the table in force however late keploy decides it, as
// a call whose mark kept no view does: decided after the next session
// opened test-1, test-1's given id. The view's scope number, 0, names no
// scope of the set: read as one, the call read an empty table.
func TestACallMarkedInTheSessionResetsViewReadsTheTableInForce(t *testing.T) {
	mm := stagedManager(t, nil, nil)
	mm.ServeExactTest("test-0", nil)
	mm.ResetForReplaySession()
	h, _ := mm.HoldToRound(mm.MarkCallRound())
	if in := h.(*heldScope).g.readIn; in == nil || in.scoped {
		t.Fatal("precondition: the call was not read in the session reset's view, stamped with no scope")
	}
	mm.OpenExactTest("test-1", nil, []models.VariableBinding{{Name: "id", Value: "x-1"}})
	var got []string
	for _, b := range h.(integrations.VariableStore).Variables().Snapshot() {
		got = append(got, b.Name+"="+b.Value)
	}
	if !slices.Equal(got, []string{"id=x-1"}) {
		t.Fatalf("the call read %v; want the table in force's id=x-1", got)
	}
}
