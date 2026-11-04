package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// callViewRun is a store served exactly whose test-0 owns own, a PING its
// first call answered: test-0's view lists own as answered.
func callViewRun(t *testing.T) (*MockManager, *models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	own := ownRepeatExchange("mock-own-ping", "PING sk9LIVEsecret77", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}
	if got, miss := genericCall(t, mm, "PING sk9LIVEsecret77"); got != "OWN1" {
		t.Fatalf("precondition: test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	return mm, own
}

func answeredNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// A call held to its round is told the test mocks that answered in the view
// its reads are made in (heldScope.ConsumedTestMocks), as its other reads
// are: a call read in test-0's view is told test-0's answered recording after
// test-0 ended, the pool view in force listing none; through a connection's
// scoped view too. Such a call never reports its view moved
// (CallViewMoved). A call that reads the view in force (decided in the test
// the store opens next: TrackerAhead) is told the view in force's, and
// reports its view moved once a test's begin or end installed another view
// since it was held, and not before. Before, every held call was told the
// view in force's answered recordings: none once test-0 ended, so a miss
// report built then found no recording that redacted what the call sent.
func TestAHeldCallIsToldTheAnsweredRecordingsOfTheViewItReadsIn(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, ahead := range []bool{false, true} {
			t.Run(map[bool]string{false: "the manager", true: "a scoped view"}[scoped]+"/"+map[bool]string{false: "read in its own view", true: "reading the view in force"}[ahead], func(t *testing.T) {
				mm, own := callViewRun(t)
				var store integrations.MockMemDb = mm
				if scoped {
					store = &scopedMockDb{MockMemDb: mm}
				}
				call := integrations.MarkCallRound(store)
				if ahead {
					call = call.WithTrackerAhead(true)
				}
				held, _ := integrations.HoldToRound(store, call)
				er, ok := held.(integrations.ExactConsumedReader)
				if !ok {
					t.Fatalf("the held store %T tells no answered recordings", held)
				}
				if got := answeredNames(er.ConsumedTestMocks()); len(got) != 1 || got[0] != own.Name || integrations.CallViewMoved(held) {
					t.Fatalf("in test-0 the held call is told %v answered, moved %v; want [%s], not moved", got, integrations.CallViewMoved(held), own.Name)
				}
				mm.ServeExactPool(models.PoolKeyNone, nil)
				got, moved := answeredNames(er.ConsumedTestMocks()), integrations.CallViewMoved(held)
				if ahead {
					if len(got) != 0 || !moved {
						t.Fatalf("after test-0 ended a call reading the view in force is told %v answered, moved %v; want none, moved", got, moved)
					}
					return
				}
				if len(got) != 1 || got[0] != own.Name || moved {
					t.Fatalf("after test-0 ended the call read in test-0's view is told %v answered, moved %v; want [%s], not moved", got, moved, own.Name)
				}
			})
		}
	}
}

// markEnds ends mm's open test (installs the pool view) right after the next
// call's round is marked, once.
func markEnds(t *testing.T, mm *MockManager) *bool {
	ended := new(bool)
	mm.exact.onMarked = func() {
		mm.exact.onMarked = nil
		if mm.ExactTestServed() != "test-0" {
			t.Errorf("precondition: test-0 is not open as the call is marked: %q", mm.ExactTestServed())
		}
		mm.ServeExactPool(models.PoolKeyNone, nil)
		*ended = true
	}
	return ended
}

// Under the proven switch, an opaque TCP exchange identical to one test-0's
// own recording answered, marked in test-0's view, which the replay ends
// right after the mark, is read in test-0's view and told its answered
// recording there: it misses as one exchange more than the test recorded,
// recorded once, and its report carries no byte the call sent. Before, its
// miss read the pool view's answered recordings, none, and was reported as an
// exchange the test never recorded.
func TestAGenericExchangeMarkedInATestIsToldItsAnsweredRecordingsAfterTheTestEnds(t *testing.T) {
	mm, _ := callViewRun(t)
	ended := markEnds(t, mm)
	got, miss := genericCall(t, mm, "PING sk9LIVEsecret77")
	if !*ended || mm.ExactTestServed() != "" {
		t.Fatalf("precondition: test-0 did not end right after the call's mark (ended %v, open %q)", *ended, mm.ExactTestServed())
	}
	if got != "" {
		t.Fatalf("the second PING, one more than test-0 recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if r := fmt.Sprintf("%+v", *miss); strings.Contains(r, "sk9LIVE") {
		t.Fatalf("the miss's report carries the bytes the call sent: %s", r)
	}
}

// A call marked from a round snapshot (record's full round: its learn step's
// read, integrations.RoundSnapshot.CallRound, which keeps no sequence view)
// is held to the view that snapshot read, at every hold: a view a test's
// end installs after the snapshot, before a hold or between two holds, is a
// move to each of them, never taken for the view the call was read in. So
// HTTP's full round, which reads the store through such a hold for its
// variable pass, its pairing's own-repeat count and its miss, builds none of
// them from two views. Before, each hold took the view in force as it was
// made: a hold made after test-0 ended said the call had not moved, and its
// answered list was the pool view's, none, joined with what the learn step
// read in test-0's view.
func TestACallMarkedFromARoundSnapshotIsHeldToTheViewItRead(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, ahead := range []bool{false, true} {
			for _, when := range []string{"never", "before its hold", "between two holds"} {
				name := map[bool]string{false: "the manager", true: "a scoped view"}[scoped] + "/" + map[bool]string{false: "of the store's test", true: "the tracker ahead"}[ahead] + "/test-0 ends " + when
				t.Run(name, func(t *testing.T) {
					mm, _ := callViewRun(t)
					var store integrations.MockMemDb = mm
					if scoped {
						store = &scopedMockDb{MockMemDb: mm}
					}
					snap, ok := integrations.SnapshotRound(store, integrations.RoundQuery{})
					if !ok || snap.Test != "test-0" {
						t.Fatalf("precondition: the snapshot read %q (ok %v); want test-0's view", snap.Test, ok)
					}
					call := snap.CallRound().WithTrackerAhead(ahead)
					var holds []integrations.MockMemDb
					hold := func() {
						held, _ := integrations.HoldToRound(store, call)
						holds = append(holds, held)
					}
					switch when {
					case "before its hold":
						mm.ServeExactPool(models.PoolKeyNone, nil)
						hold()
					case "between two holds":
						hold()
						mm.ServeExactPool(models.PoolKeyNone, nil)
						hold()
					default:
						hold()
						hold()
					}
					for i, held := range holds {
						if moved := integrations.CallViewMoved(held); moved != (when != "never") {
							t.Errorf("hold %d of %d tells the call moved %v; want %v", i+1, len(holds), moved, when != "never")
						}
						if _, one := integrations.ConsumedInCallView(held); one != (when == "never") {
							t.Errorf("hold %d of %d lets its answered list join the call's other reads: %v; want %v", i+1, len(holds), one, when == "never")
						}
					}
				})
			}
		}
	}
}

// A call marked from a round snapshot of the pool view (record's full round
// reads a call between tests, no test open, and one the learn tracker began
// ahead is read there too) is held to the pool view: a test's begin that
// installs its view after the snapshot, before the call's hold, is a move to
// the hold, which joins no answered list with the call's other reads. With
// no install the hold tells no move. Through the store or a scoped view, of
// the store's round or with the tracker ahead.
func TestAPoolViewSnapshotsCallIsHeldToThePoolView(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, ahead := range []bool{false, true} {
			for _, install := range []bool{false, true} {
				name := map[bool]string{false: "the manager", true: "a scoped view"}[scoped] + "/" + map[bool]string{false: "of the store's round", true: "the tracker ahead"}[ahead] + "/" + map[bool]string{false: "no install", true: "test-1 opens before its hold"}[install]
				t.Run(name, func(t *testing.T) {
					mm, own := callViewRun(t)
					mm.ServeExactPool(models.PoolKeyNone, nil)
					var store integrations.MockMemDb = mm
					if scoped {
						store = &scopedMockDb{MockMemDb: mm}
					}
					snap, ok := integrations.SnapshotRound(store, integrations.RoundQuery{})
					if !ok || snap.Test != "" {
						t.Fatalf("precondition: the snapshot read %q (ok %v); want the pool view", snap.Test, ok)
					}
					call := snap.CallRound().WithTrackerAhead(ahead)
					if install {
						mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, own.Name)})
					}
					held, _ := integrations.HoldToRound(store, call)
					if moved := integrations.CallViewMoved(held); moved != install {
						t.Errorf("the hold tells the call moved %v; want %v", moved, install)
					}
					if _, one := integrations.ConsumedInCallView(held); one == install {
						t.Errorf("the hold lets its answered list join the call's other reads: %v; want %v", one, !install)
					}
				})
			}
		}
	}
}

// A round snapshot's mark is held to its view only by the store that made
// it: another manager held to it holds the view in force as it is held, and
// tells no move it did not see.
func TestACallMarkedFromAnotherStoresSnapshotIsHeldToTheViewInForce(t *testing.T) {
	other, _ := callViewRun(t)
	mm, _ := callViewRun(t)
	snap, ok := integrations.SnapshotRound(other, integrations.RoundQuery{})
	if !ok {
		t.Fatal("precondition: the other store made no snapshot")
	}
	held, _ := integrations.HoldToRound(mm, snap.CallRound())
	if integrations.CallViewMoved(held) {
		t.Fatal("a call held to another store's snapshot is told it moved, with no view installed since its hold")
	}
}
