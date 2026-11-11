package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// keploy can decide a call after the replay began the next test: the
// call's first read (its mark) found its own test's view, and its matcher
// reads once the next test's view is in force. The call is still its own
// test's, and the own-call check (integrations.OwnRepeatRead) reads the
// recordings of the view the call was read in, as when keploy decides it at
// once, never "no test's own" because another view is in force now.

// lateOwnRun is an agent store under the proven switch, on a set served by
// the background rules, with test-1 open and owning one GET /x answered
// "own", next staged for test-2, and the pool holding pool. The calls go
// through db, which runs its move between a call's mark and its matcher's
// reads, once.
func lateOwnRun(t *testing.T, next []*models.Mock, pool ...*models.Mock) (db *viewMovedAtMark, mm *MockManager, own *models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	own = workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	var poolNames []string
	for _, mk := range pool {
		poolNames = append(poolNames, mk.Name)
	}
	mm = stagedManager(t, append([]*models.Mock{own}, next...), pool)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-1", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)},
		{TestCase: "test-2", Start: exactT0.Add(6 * time.Second), End: exactT0.Add(8 * time.Second)},
	})
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() || !integrations.S12Background(mm) {
		t.Fatal("precondition: test-1 is not served exactly, by the background rules")
	}
	return &viewMovedAtMark{MockManager: mm}, mm, own
}

// lateOwnPools are the two sets a late call's row runs on: one whose pool
// holds a recording of the very call (GET /x answered "pool"), and one with
// no background.
var lateOwnPools = []struct {
	name string
	pool bool
}{
	{name: "the pool holds the call's copy", pool: true},
	{name: "no background"},
}

// A GET /x one more than test-1 recorded, read in test-1's view and decided
// after the replay began test-2, is test-1's own call: it misses as one call
// more than test-1 recorded, marked in test-1, and the pool's copy is never
// spent on it, whether or not test-2 owns a GET /x of its own. Before, the
// held store said it owned nothing once test-2's view was in force: the
// pool's copy answered the call (a request sent twice passed), and with no
// background its miss did not say it was a call made once more than
// recorded.
func TestALateCallIsCheckedAgainstTheRecordingsOfTheViewItWasReadIn(t *testing.T) {
	for _, next := range []string{"test-2 owns none", "test-2 owns a GET /x"} {
		for _, p := range lateOwnPools {
			t.Run(p.name+", "+next, func(t *testing.T) {
				var pool []*models.Mock
				if p.pool {
					pool = append(pool, workerProbe("mock-pool-x", time.Second, "/x", "pool"))
				}
				var nextOwn []*models.Mock
				if next != "test-2 owns none" {
					nextOwn = []*models.Mock{workerProbe("mock-own2-x", 7*time.Second, "/x", "own2")}
				}
				db, mm, _ := lateOwnRun(t, nextOwn, pool...)
				if got, miss := httpMissSend(t, db, "GET /x HTTP/1.1\r\nHost: deps\r\n\r\n"); got != "own" {
					t.Fatalf("test-1's GET /x was answered %q (miss %+v); want its own", got, miss)
				}
				db.move = func() {
					var own2 []*models.Mock
					for _, mk := range nextOwn {
						own2 = append(own2, roundMock(mm, mk.Name))
					}
					mm.ServeExactTest("test-2", own2)
				}
				got, miss := httpMissSend(t, db, "GET /x HTTP/1.1\r\nHost: deps\r\n\r\n")
				if db.move != nil {
					t.Fatal("the call was never marked; the row is about a call decided after its mark")
				}
				if got != "" {
					t.Fatalf("test-1's GET /x once more, decided after test-2 began, was answered %q; want a miss: one call more than test-1 recorded", got)
				}
				if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 || !miss.MarkedInTest || miss.MarkedTest != "test-1" {
					t.Fatalf("its miss is kind %q, recorded %d, marked in test %v %q; want %q, recorded 1, marked in test-1", miss.MissKind, miss.Recorded, miss.MarkedInTest, miss.MarkedTest, models.MissExtraCall)
				}
				if p.pool && mm.MockSpent("mock-pool-x") {
					t.Fatal("the pool's GET /x was spent on a call it did not answer")
				}
			})
		}
	}
}

// The other direction: a late GET /y, a call test-1 never recorded, read in
// test-1's view and decided after the replay began test-2, whose own
// recordings hold a GET /y, is no call of test-1's own: the pool's copy
// answers it, as on time, and test-2's own recording is neither read nor
// spent for it.
func TestALateCallTheTestNeverRecordedIsNotCountedByTheNextTestsRecordings(t *testing.T) {
	pool := workerProbe("mock-pool-y", time.Second, "/y", "pool-y")
	nextY := workerProbe("mock-own2-y", 7*time.Second, "/y", "own2-y")
	db, mm, _ := lateOwnRun(t, []*models.Mock{nextY}, pool)
	db.move = func() { mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, nextY.Name)}) }
	got, miss := httpMissSend(t, db, "GET /y HTTP/1.1\r\nHost: deps\r\n\r\n")
	if db.move != nil {
		t.Fatal("the call was never marked; the row is about a call decided after its mark")
	}
	if got != "pool-y" {
		t.Fatalf("a late GET /y test-1 never recorded was answered %q (miss %+v); want the pool's pool-y, as on time", got, miss)
	}
	if got, miss := httpMissSend(t, db, "GET /y HTTP/1.1\r\nHost: deps\r\n\r\n"); got != "own2-y" {
		t.Fatalf("test-2's own GET /y was answered %q (miss %+v); want its own own2-y, unspent by test-1's late call", got, miss)
	}
}

// A held store marked in a test's view reads that view's own recordings
// (integrations.SharedReads' owned reads) while the next test's view is in
// force, as it reads the view's other recordings (heldScope.readView): the
// view's number and list are the ones the store gave on time, and what is
// left of them unanswered is that view's, a late answer of one of them
// included. It reads none once the staging moved, none for a call marked
// between tests (a pool view owns none), and the next test's own for a call
// marked in the next test's view, or for a call of the pool view whose
// consume the next test's view refused.
func TestAHeldStoreReadsTheOwnRecordingsOfTheViewItsCallWasReadIn(t *testing.T) {
	_, mm, own := lateOwnRun(t, nil)
	onTime, _ := mm.HoldToRound(mm.MarkCallRound())
	view, list, ok := onTime.(integrations.SharedReads).OwnedRecordings()
	if !ok || len(list) != 1 || list[0].Name != own.Name {
		t.Fatalf("on time, the held store's own recordings are %v (%v); want test-1's %s", exactNames(list), ok, own.Name)
	}
	held, _ := mm.HoldToRound(mm.MarkCallRound())
	settled, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactTest("test-2", nil)
	sr := held.(integrations.SharedReads)
	lateView, late, ok := sr.OwnedRecordings()
	if !ok || lateView != view || len(late) != 1 || late[0] != list[0] {
		t.Fatalf("after test-2 began, the held store's own recordings are view %d %v (%v); want test-1's view %d %v, as on time", lateView, exactNames(late), ok, view, exactNames(list))
	}
	if left, ok := sr.OwnedUnansweredAt(view, []int{0}); !ok || len(left) != 1 || left[0] != 0 {
		t.Fatalf("what is left unanswered of test-1's own is %v (%v); want [0]: its recording never answered", left, ok)
	}
	if left, ok := sr.OwnedUnanswered(); !ok || len(left) != 1 || left[0].Name != own.Name {
		t.Fatalf("test-1's own unanswered recordings read after test-2 began are %v (%v); want %s", exactNames(left), ok, own.Name)
	}
	if _, ok := sr.OwnedUnansweredAt(view+1, []int{0}); ok {
		t.Fatal("the held store read what is left of a view it was not read in")
	}
	inNext, _ := mm.HoldToRound(mm.MarkCallRound())
	if v, l, ok := inNext.(integrations.SharedReads).OwnedRecordings(); !ok || v == view || len(l) != 0 {
		t.Fatalf("a call marked in test-2's view reads own recordings %v of view %d (%v); want test-2's: none, another view", exactNames(l), v, ok)
	}

	// A call of test-1 whose consume of test-1's own recording lands after
	// test-2 began is answered in test-1's view (roundGuard.ownView), and
	// reads there still: its owned reads are test-1's, the recording now
	// answered.
	if !settled.DeleteUnFilteredMock(*own) {
		t.Fatal("test-1's late call was refused test-1's own recording, unanswered; want it answered in test-1's view")
	}
	if v, l, ok := settled.(integrations.SharedReads).OwnedRecordings(); !ok || v != view || len(l) != 1 || l[0] != list[0] {
		t.Fatalf("after its own answer the held store reads own recordings %v of view %d (%v); want test-1's view %d %v", exactNames(l), v, ok, view, exactNames(list))
	}
	if left, ok := settled.(integrations.SharedReads).OwnedUnanswered(); !ok || len(left) != 0 {
		t.Fatalf("test-1's own unanswered after its late call's answer are %v (%v); want none", exactNames(left), ok)
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	between, _ := mm.HoldToRound(mm.MarkCallRound())
	if _, l, ok := between.(integrations.SharedReads).OwnedRecordings(); ok {
		t.Fatalf("a call marked between tests reads own recordings %v; want none: a pool view owns none", exactNames(l))
	}

	// A call of the pool view whose consume of the pool view's copy of the
	// next test's recording test-2's view refused reads the view in force
	// from then on (heldScope.settled), as a call made now does: its owned
	// reads too, test-2's.
	t2own := httpCall("own2-z", 3*time.Second, "/z")
	_, pc, p := aheadCallHeldAcrossNextTest(t, t2own)
	if _, l, ok := pc.(integrations.SharedReads).OwnedRecordings(); ok {
		t.Fatalf("precondition: before its consume, the pool view's call reads own recordings %v; want none", exactNames(l))
	}
	if pc.DeleteUnFilteredMock(*p) {
		t.Fatal("precondition: T2's view settled the consume of the copy of a-0 the pool view served; want it refused")
	}
	if _, l, ok := pc.(integrations.SharedReads).OwnedRecordings(); !ok || !reflect.DeepEqual(exactNames(l), []string{"a-0", t2own.Name}) {
		t.Fatalf("after T2's view refused its consume, the held store reads own recordings %v (%v); want T2's a-0 and %s", exactNames(l), ok, t2own.Name)
	}

	mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, time.Now())
	if _, l, ok := sr.OwnedRecordings(); ok {
		t.Fatalf("after the next staging the held store reads own recordings %v; want none: the staging moved", exactNames(l))
	}
	if _, ok := sr.OwnedUnansweredAt(view, []int{0}); ok {
		t.Fatal("after the next staging the held store read what is left of test-1's own; want nothing: the staging moved")
	}
	if _, ok := sr.OwnedUnanswered(); ok {
		t.Fatal("after the next staging the held store read test-1's own unanswered; want nothing: the staging moved")
	}
}

// A call marked from a round snapshot (record's full round's learn step)
// reads the own recordings of the view that snapshot read, test-1's, after
// the replay began test-2 too, never test-2's: its other reads are made in
// the view in force, its own-call check is not. Once its miss is diagnosed
// (DiagnoseInCallView) in a view a test's begin replaced, it reads none, as
// it reads nothing else then (heldScope.viewGone).
func TestACallMarkedFromARoundSnapshotReadsTheOwnRecordingsOfTheViewTheSnapshotRead(t *testing.T) {
	next := workerProbe("mock-own2-x", 7*time.Second, "/x", "own2")
	_, mm, own := lateOwnRun(t, []*models.Mock{next})
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{})
	if !ok || snap.Test != "test-1" {
		t.Fatalf("precondition: the round snapshot is of %q (%v); want test-1's view", snap.Test, ok)
	}
	held, _ := mm.HoldToRound(snap.CallRound())
	mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, next.Name)})
	if _, l, ok := held.(integrations.SharedReads).OwnedRecordings(); !ok || len(l) != 1 || l[0].Name != own.Name {
		t.Fatalf("a call marked from test-1's round snapshot, read after test-2 began, reads own recordings %v (%v); want test-1's %s", exactNames(l), ok, own.Name)
	}
	diag := held.(integrations.CallViewDiagnoser).DiagnoseInCallView()
	if _, l, ok := diag.(integrations.SharedReads).OwnedRecordings(); ok {
		t.Fatalf("its miss, diagnosed after test-2 replaced the view its snapshot read, reads own recordings %v; want none", exactNames(l))
	}
	if _, ok := diag.(integrations.SharedReads).OwnedUnanswered(); ok {
		t.Fatal("its miss, diagnosed after test-2 replaced the view its snapshot read, reads own unanswered recordings; want none")
	}
}
