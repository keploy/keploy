package proxy

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call of test-1 that is exactly test-1's own mock-own1 and its twin
// mock-own1b (the same request, another answer) chooses mock-own1 first.
// As the learn step asks the learn tracker to take it, another call of
// test-1 takes mock-own1 first, and the replay ends test-1 (or ends it and
// begins test-2) under the take. At once the call looks again in test-1's
// view and is answered by mock-own1b. Late, it looks again in its own
// test's view too, the one it would have read at once, and gets the same
// answer. Before, the look again read the round the replay moved on to, and
// the call missed as moved: a false red under load. Both orders of the end,
// under today's rules and proven routing.
func TestALateRetakeOfAnOwnRecordingIsDecidedAsAtOnce(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, move := range []string{"at once", "the store ends it first", "the tracker ends it first", "the store ends it first, then test-2 begins", "the tracker ends it first, then test-2 begins"} {
			name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" + move
			t.Run(name, func(t *testing.T) {
				p, mm := heldOrdersWith(t, routed, zap.NewNop(), func(now time.Time) []*models.Mock {
					return []*models.Mock{r3Mock("mock-own1b", now.Add(750*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":3}`)}
				})
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1"), roundMock(mm, "mock-own1b")})
				p.ContinueTestErrorCapture()
				var asked, taken atomic.Bool
				p.learnTracker.OnAnswer(func(uint64) {
					if !asked.CompareAndSwap(false, true) {
						return
					}
					// Another call of test-1 takes mock-own1 first.
					taken.Store(mm.DeleteFilteredMock(*roundMock(mm, "mock-own1")))
					if move == "at once" {
						return
					}
					if strings.HasPrefix(move, "the store ends it first") {
						mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
						p.EndTestErrorCapture()
					} else {
						p.EndTestErrorCapture()
						mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
					}
					if strings.HasSuffix(move, "test-2 begins") {
						mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
						p.ContinueTestErrorCapture()
					}
				})
				t.Cleanup(func() { p.learnTracker.OnAnswer(nil) })
				code, got := sendPear(t, p, mm, false)
				if !asked.Load() || !taken.Load() {
					t.Fatalf("precondition: the call's take was never asked of the tracker, or the other call could not take mock-own1 first (got %d %s)", code, got)
				}
				ownAnswered(t, p, mm, code, got, `{"order":3}`)
			})
		}
	}
}

// A read of the round in the view an earlier read found (RoundQuery.Of) is
// that view as it stands now, in the earlier read's round: a late call's
// look again after another call of its test took its choice. Once the
// replay ended T1 (or ended it and began T2), it shows T1's own recording
// left unanswered and not the one taken, of the earlier read's round, test
// and tracker reading, and a take held to it settles that recording in
// T1's view. It reads nothing where the call could not look again in its
// view at once: after a restage, of a read of the pool's view, of another
// store's read, once the agent reset the replay session.
func TestARoundReadOfAnEarlierReadsViewIsThatViewNowInItsRound(t *testing.T) {
	for _, tc := range []struct {
		name string
		// of is the earlier read; move is what the replay does after it.
		of   func(t *testing.T, s *roundStore) integrations.RoundSnapshot
		move func(t *testing.T, s *roundStore)
		read bool
	}{
		{"the store ends T1", nil, func(t *testing.T, s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}, true},
		{"the tracker ends T1, then the store", nil, func(t *testing.T, s *roundStore) {
			s.end()
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}, true},
		{"T1 ends and T2 begins", nil, func(t *testing.T, s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
			s.end()
			s.begin()
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		}, true},
		{"a restage", nil, func(t *testing.T, s *roundStore) {
			var all []*models.Mock
			for _, m := range s.ms {
				all = append(all, m)
			}
			s.mm.SetMocksWithWindow(all, nil, models.BaseTime, time.Now().Add(time.Hour))
			s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"], s.ms["own2"]})
		}, false},
		{"the agent resets the replay session", nil, func(t *testing.T, s *roundStore) {
			s.mm.ResetForReplaySession()
		}, false},
		{"a read of the pool's view", func(t *testing.T, s *roundStore) integrations.RoundSnapshot {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
			s.end()
			return s.snapshot(t)
		}, func(t *testing.T, s *roundStore) {
			s.begin()
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		}, false},
		{"another store's read", func(t *testing.T, s *roundStore) integrations.RoundSnapshot {
			other := newRoundStore(t)
			other.inTest()
			return other.snapshot(t)
		}, func(t *testing.T, s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			s.inTest()
			of := s.snapshot(t)
			if tc.of != nil {
				of = tc.of(t, s)
			} else if !of.Open || of.Test != "T1" {
				t.Fatalf("precondition: the earlier read is of %q, open %v; want T1's view, open", of.Test, of.Open)
			}
			// Another call of T1 takes own first, in T1's view.
			if tc.of == nil && !s.mm.DeleteFilteredMock(*s.ms["own"]) {
				t.Fatal("precondition: another call could not take own in T1's view")
			}
			tc.move(t, s)
			if now := s.snapshot(t); tc.of == nil && now.At.Gen == of.At.Gen {
				t.Fatalf("precondition: the round did not move since the earlier read (%+v)", now.At)
			}
			got, ok := s.mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Tracker: s.tr, Of: &of})
			if ok != tc.read {
				t.Fatalf("a read of the earlier read's view read %v (%q); want %v", ok, got.Test, tc.read)
			}
			if !ok {
				return
			}
			names := exactNames(append(append([]*models.Mock(nil), got.PerTest...), got.Session...))
			if got.Test != "T1" || !got.Open || got.At.Epoch != of.At.Epoch || got.At.Gen != of.At.Gen || got.At.Bounds != of.At.Bounds ||
				got.Learn.Number != of.Learn.Number || got.Learn.Open != of.Learn.Open || got.ViewGen() != of.ViewGen() {
				t.Fatalf("the read is of %q, open %v, at %+v, learn %d/%v, view %d; want the earlier read's T1 at %+v, learn %d/%v, view %d",
					got.Test, got.Open, got.At, got.Learn.Number, got.Learn.Open, got.ViewGen(), of.At, of.Learn.Number, of.Learn.Open, of.ViewGen())
			}
			if !slices.Contains(names, "own2") || slices.Contains(names, "own") || got.At.Taken <= of.At.Taken {
				t.Fatalf("T1's view shows %v, %d taken; want own2 and not own, taken since %d", names, got.At.Taken, of.At.Taken)
			}
			if got.Owner(s.ms["own2"]) != integrations.OwnerTest {
				t.Fatalf("own2 is %v's; want T1's own", got.Owner(s.ms["own2"]))
			}
			scope := integrations.WithinRoundOf(s.mm, got)
			if !scope.DeleteFilteredMock(*s.ms["own2"]) {
				t.Fatalf("a take of own2 held to the read was refused (%s); want it settled in T1's view", scope.Moved())
			}
			if scope.DeleteFilteredMock(*s.ms["next"]) {
				t.Fatal("a take of T2's next held to the read settled; want it refused")
			}
		})
	}
}
