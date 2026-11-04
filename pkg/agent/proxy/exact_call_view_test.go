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

// The miss of a call marked from a round snapshot (record's full round's
// learn step, integrations.RoundSnapshot.CallRound) reads, through its hold
// (integrations.DiagnoseInCallView), the view that snapshot read and no
// other. While test-1's view is in force the hold reads test-1's own
// mock-own1 and mock-sess1. Once the replay ends test-1 (the pool view in
// force) or ends it and begins test-2 (which owns mock-own2 and mock-sess2),
// before the hold or after it, every read of the hold finds nothing, its
// walks stop, the generation it reports is its own (the view in force's
// would file that nothing for the view's own calls), its answered
// redactions are not complete (so the miss is redacted by every place), and
// the move is no staging. A call decided in the test the store opens next (TrackerAhead)
// reads the view in force, as before. Before, the miss read the view in
// force: a test-1 call's named test-2's mock-own2, or a pool recording, as
// its closest.
func TestASnapshotMarkedCallsMissReadsOnlyTheViewItsSnapshotRead(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, ahead := range []bool{false, true} {
			for _, when := range []string{"never", "test-1 ends before its hold", "test-2 begins before its hold", "test-1 ends after its hold", "test-2 begins after its hold"} {
				name := map[bool]string{false: "the manager", true: "a scoped view"}[scoped] + "/" + map[bool]string{false: "of the store's test", true: "the tracker ahead"}[ahead] + "/" + when
				t.Run(name, func(t *testing.T) {
					t.Setenv(provenswitch.Env, "1")
					now := time.Now()
					pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
					own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
					sess1 := r3Mock("mock-sess1", now.Add(710*time.Millisecond), "GET", "/config", "", `{"v":1}`)
					own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
					sess2 := r3Mock("mock-sess2", now.Add(1510*time.Millisecond), "GET", "/config", "", `{"v":2}`)
					mm := NewMockManager(nil, nil, zap.NewNop())
					t.Cleanup(mm.Close)
					mm.SeedRecordedWindows([]models.TestWindow{
						{Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
						{Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
					})
					mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2}, []*models.Mock{sess1, sess2}, models.BaseTime, now.Add(time.Hour))
					mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
					mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
					mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1"), roundMock(mm, "mock-sess1")})
					var store integrations.MockMemDb = mm
					if scoped {
						store = &scopedMockDb{MockMemDb: mm}
					}
					snap, ok := integrations.SnapshotRound(store, integrations.RoundQuery{})
					if !ok || snap.Test != "test-1" {
						t.Fatalf("precondition: the snapshot read %q (ok %v); want test-1's view", snap.Test, ok)
					}
					call := snap.CallRound().WithTrackerAhead(ahead)
					move := func() {
						mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
						if strings.HasPrefix(when, "test-2") {
							mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2"), roundMock(mm, "mock-sess2")})
						}
					}
					before := strings.HasSuffix(when, "before its hold")
					if before {
						move()
					}
					held, restaged := integrations.HoldToRound(store, call)
					held = integrations.DiagnoseInCallView(held)
					if strings.HasSuffix(when, "after its hold") {
						move()
					}
					perTest, _ := held.GetPerTestMocksInWindow()
					session, _ := held.GetSessionScopedMocks()
					startup, _ := held.GetStartupMocks()
					walked, keyed := 0, 0
					if r, ok := held.(integrations.PerTestRanger); ok {
						_ = r.RangePerTestMocksInWindow(func(*models.Mock) bool { walked++; return true })
					}
					if r, ok := held.(integrations.SessionKeyReader); ok {
						ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"config"} }}
						_ = r.RangeSessionMocksWithKey(ix, "config", func(*models.Mock) bool { keyed++; return true })
					}
					read := answeredNames(append(append(append([]*models.Mock{}, perTest...), session...), startup...))
					has := func(name string) bool {
						for _, n := range read {
							if n == name {
								return true
							}
						}
						return false
					}
					switch {
					case when == "never":
						if !has("mock-own1") || has("mock-own2") || has("mock-sess2") || walked == 0 || keyed == 0 {
							t.Fatalf("with test-1's view in force the hold reads %v (walked %d, keyed %d); want test-1's mock-own1 and mock-sess1, none of test-2's", read, walked, keyed)
						}
					case ahead:
						// Decided in the test the store opens next: it reads
						// the view in force.
						if strings.HasPrefix(when, "test-2") && !has("mock-own2") {
							t.Fatalf("a call decided in the test the store opens next reads %v once test-2 began; want test-2's mock-own2", read)
						}
					default:
						if len(read) > 0 || walked > 0 || keyed > 0 {
							t.Fatalf("once %s the hold of a call its snapshot read in test-1's view reads %v (walked %d, keyed %d); want nothing", when, read, walked, keyed)
						}
						if b, ok := held.(integrations.ExactBackground); ok {
							if bg := b.Background(models.HTTP); len(bg) > 0 {
								t.Errorf("the hold's background reads %v; want nothing", answeredNames(bg))
							}
						}
						if b, ok := held.(interface {
							KindBackground(models.Kind) ([]*models.Mock, []*models.Mock)
						}); ok {
							if unused, spent := b.KindBackground(models.HTTP); len(unused)+len(spent) > 0 {
								t.Errorf("the hold's kind background reads %v and %v; want nothing", answeredNames(unused), answeredNames(spent))
							}
						}
						if !scoped {
							gen := held.(integrations.StagingGenerations).StagingGeneration()
							if gen == mm.StagingGeneration() || gen == integrations.RestagedGeneration {
								t.Errorf("the hold reports generation %d (the view in force's %d, restaged %d); want its own", gen, mm.StagingGeneration(), integrations.RestagedGeneration)
							}
						}
						if _, complete := integrations.AnsweredRedacting(held, models.HTTP); complete {
							t.Error("the hold tells its answered redactions complete, its view's pool unread; want not complete, so the miss is redacted by every place")
						}
						if restaged() {
							t.Error("a test's end or begin is told a staging")
						}
					}
				})
			}
		}
	}
}
