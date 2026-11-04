package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// onDebugLine is a debug logger that runs f once, on the first line logged
// with the message msg.
func onDebugLine(msg string, f func()) *zap.Logger {
	var once atomic.Bool
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	return zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msg && once.CompareAndSwap(false, true) {
			f()
		}
		return nil
	}))
}

// heldHook is the manager whose first hold of a call (HoldToRound) runs
// onHold first: record's full round holds a call once its learn step
// decided it, so a view onHold installs replaces the call's view after
// that decision, before the variable pass, the pairing and the miss read
// the hold.
type heldHook struct {
	*MockManager
	onHold func()
	once   atomic.Bool
}

func (d *heldHook) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	if d.once.CompareAndSwap(false, true) {
		d.onHold()
	}
	return d.MockManager.HoldToRound(call)
}

// A full-round miss whose view moves after the call was decided (its learn
// step, variable pass, extra-call count and pairing all read test-1's view,
// and the replay ends test-1, or ends it and begins test-2, before the miss
// is diagnosed) names no recording: none of the pool's, none of test-2's,
// and diffs the call against none. It says why: the replay ended test-1
// (and began test-2) after the call was decided. With no move, the miss
// names test-1's own mock-own1. Before, the moved miss named the pool's
// mock-pool or test-2's mock-own2 as its closest, with a diff of a test-1
// call against it.
func TestAFullRoundMissWhoseViewMovesAfterItsDecisionNamesNoRecording(t *testing.T) {
	for _, when := range []string{"never", "the test ends", "the next test begins"} {
		t.Run(when, func(t *testing.T) {
			p, mm := heldOrders(t, false)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			lg := onDebugLine("after matching the http request", func() {
				if when == "never" {
					return
				}
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
				if when == "the next test begins" {
					mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
					p.ContinueTestErrorCapture()
				}
			})
			code, got := r3SendVia(t, context.Background(), httpint.New(lg), p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, "GET /orders HTTP/1.1\r\nHost: deps.example\r\n\r\n")
			if code == http.StatusOK {
				t.Fatalf("answered %d %s", code, got)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 {
				t.Fatalf("misses %+v; want 1", misses)
			}
			m := misses[0]
			why := map[string]string{
				"the test ends":        "decided this request in test-1, and the replay ended test-1 before keploy diagnosed its miss, so no recording is named for it",
				"the next test begins": "decided this request in test-1, and the replay ended test-1 and began test-2 before keploy diagnosed its miss, so no recording is named for it",
			}[when]
			switch {
			case when == "never":
				if m.ClosestMock != "mock-own1" || strings.Contains(m.NextSteps, "no recording is named") {
					t.Errorf("the unmoved miss names %q (next steps %q); want test-1's mock-own1, and no word of a move", m.ClosestMock, m.NextSteps)
				}
			case m.ClosestMock != "" || m.Diff != "" || len(m.FieldDiffs) > 0:
				t.Errorf("a test-1 miss whose view moved after it was decided names %q (diff %q, fields %+v); want no recording", m.ClosestMock, m.Diff, m.FieldDiffs)
			case !strings.Contains(m.NextSteps, why):
				t.Errorf("the moved miss's next steps read %q; want them to say %q", m.NextSteps, why)
			}
		})
	}
}

// In record's full round a call whose view moved after its learn step
// decided it (the replay ended test-1, the call's test, between that
// decision and the call's hold) is never answered from another test's own
// recordings, by the variable pass or any read of its hold: neither by
// test-2's mock-var2, which declares a variable the call's path binds, once
// test-2 began, nor by that recording in flight as test-1 ended (the pool
// view's ahead). It misses, naming no recording, and says the replay ended
// test-1. It still reads the shared scopes in the view in force: the worker
// scope's mock-wvar answers a call of /jobs it fits. And a call decided
// between tests, where test-2's mock-var2 was in flight, is answered by it
// once test-2 began: that recording was its view's own too. Before, the
// variable pass read the view in force and answered the test-1 call with
// test-2's recording.
func TestAFullRoundCallWhoseViewMovedIsNeverAnsweredByAnotherTestsOwnRecording(t *testing.T) {
	for _, c := range []struct {
		name, target, want, why string
		between                 bool
		move                    func(*Proxy, *MockManager)
	}{
		{name: "test-1's call, never moved", target: "/items/a1"},
		{name: "test-1's call, the next test begins", target: "/items/a1", why: "ended test-1 and began test-2", move: func(p *Proxy, mm *MockManager) {
			mm.ServeExactPool(models.PoolKeyNone, nil)
			p.EndTestErrorCapture()
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-var2")})
			p.ContinueTestErrorCapture()
		}},
		{name: "test-1's call, the test ends with test-2's call in flight", target: "/items/a1", why: "ended test-1 before", move: func(p *Proxy, mm *MockManager) {
			mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "mock-var2"))
			p.EndTestErrorCapture()
		}},
		{name: "test-1's worker call, the next test begins", target: "/jobs/j1", want: `{"w":true}`, move: func(p *Proxy, mm *MockManager) {
			mm.ServeExactPool(models.PoolKeyNone, nil)
			p.EndTestErrorCapture()
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-var2")})
			p.ContinueTestErrorCapture()
		}},
		{name: "a call between tests, test-2 begins", target: "/none/x", why: "decided this request between tests, and the replay began test-2 before", between: true, move: func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-var2")})
			p.ContinueTestErrorCapture()
		}},
		{name: "a call between tests, another pool view", target: "/none/x", why: "decided this request between tests, and the replay served another view of its recordings between tests before", between: true, move: func(_ *Proxy, mm *MockManager) {
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}},
		{name: "a call between tests with test-2's in flight, never moved", target: "/items/a1", want: `{"v":"two"}`, between: true},
		{name: "a call between tests with test-2's in flight, test-2 begins", target: "/items/a1", want: `{"v":"two"}`, between: true, move: func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-var2")})
			p.ContinueTestErrorCapture()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			start := time.Now().Add(time.Second)
			own1 := r3Mock("mock-own1", start.Add(100*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
			var2 := r3Mock("mock-var2", start.Add(1100*time.Millisecond), "GET", "/items/r2", "", `{"v":"two"}`)
			var2.Spec.Variables = models.MockVariables{"id": {Recorded: "r2", Bind: "url.path[1]"}}
			wvar := r3Mock("mock-wvar", start.Add(-500*time.Millisecond), "GET", "/jobs/r9", "", `{"w":true}`)
			wvar.Spec.Variables = models.MockVariables{"id": {Recorded: "r9", Bind: "url.path[1]"}}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows([]models.TestWindow{
				{TestCase: "test-1", Start: start, End: start.Add(900 * time.Millisecond)},
				{TestCase: "test-2", Start: start.Add(time.Second), End: start.Add(1900 * time.Millisecond)},
			})
			mm.SetMocksWithWindow(nil, []*models.Mock{own1, var2, wvar}, models.BaseTime, start.Add(time.Hour))
			mm.ServeExactWorker([]string{wvar.Name})
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			if c.between {
				mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "mock-var2"))
			} else {
				mm.ServeExactPool(models.PoolKeyNone, nil)
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
			}
			db := &heldHook{MockManager: mm, onHold: func() {}}
			if c.move != nil {
				db.onHold = func() { c.move(p, mm) }
			}
			code, got := r3SendVia(t, context.Background(), httpint.New(zap.NewNop()), p, db, models.OutgoingOptions{Learn: true, LearnPool: true}, httpGet(c.target))
			if !db.once.Load() {
				t.Fatal("precondition: the full round never held the call")
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if c.want != "" {
				if code != http.StatusOK || got != c.want || len(misses) > 0 {
					t.Fatalf("GET %s got %d %s (misses %+v); want %s", c.target, code, got, misses, c.want)
				}
				return
			}
			if code == http.StatusOK {
				t.Fatalf("test-1's GET %s was answered %s: another test's own recording answered a call of test-1", c.target, got)
			}
			if len(misses) != 1 {
				t.Fatalf("misses %+v; want 1", misses)
			}
			if m := misses[0]; c.why != "" && (m.ClosestMock != "" || !strings.Contains(m.NextSteps, c.why)) {
				t.Errorf("the moved miss names %q, its next steps %q; want no recording, and that the replay %s", m.ClosestMock, m.NextSteps, c.why)
			}
		})
	}
}

// The hold of a call marked from a round snapshot of test-1's view (record's
// full round) whose view a test's begin or end replaced holds none of
// another test's own recordings in any read, and settles none through a
// consume door: not test-2's mock-own2 or mock-sess2 once test-2 began,
// before the hold or after it; not mock-own2 in flight as test-1 ended (the
// pool view's ahead); not a copy test-2's view served before test-2 ended
// again under the call. Its MockSpent and Spent say so, so a parser's index
// never offers one. The pool's mock-pool, a shared scope's, it still
// reads. A call decided in the test the store opens next (the tracker
// ahead) reads test-2's as before, and a call a snapshot of the pool view
// read with mock-own2 in flight reads it once test-2 began. Before, the
// hold read and consumed test-2's own recordings in the view in force.
func TestASnapshotMarkedCallsHoldReadsNoOtherTestsOwnRecording(t *testing.T) {
	type tc struct {
		name           string
		ahead, between bool
		move           func(mm *MockManager)
		own2           bool // the hold reads and consumes mock-own2
	}
	begin2 := func(mm *MockManager) {
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
		mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2"), roundMock(mm, "mock-sess2")})
	}
	cases := []tc{
		{name: "test-2 begins before its hold", move: begin2},
		{name: "test-1 ends with test-2's in flight", move: func(mm *MockManager) {
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"}, roundMock(mm, "mock-own2"), roundMock(mm, "mock-sess2"))
		}},
		{name: "the tracker ahead, test-2 begins", ahead: true, move: begin2, own2: true},
		{name: "between tests with test-2's in flight, test-2 begins", between: true, move: begin2, own2: true},
	}
	for _, scoped := range []bool{false, true} {
		for _, c := range cases {
			t.Run(map[bool]string{false: "the manager", true: "a scoped view"}[scoped]+"/"+c.name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, "1")
				now := time.Now()
				pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
				own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
				own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
				sess2 := r3Mock("mock-sess2", now.Add(1510*time.Millisecond), "GET", "/config", "", `{"v":2}`)
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				mm.SeedRecordedWindows([]models.TestWindow{
					{Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
					{Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
				})
				mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2}, []*models.Mock{sess2}, models.BaseTime, now.Add(time.Hour))
				mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
				if c.between {
					mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"}, roundMock(mm, "mock-own2"), roundMock(mm, "mock-sess2"))
				} else {
					mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
					mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				}
				var store integrations.MockMemDb = mm
				if scoped {
					store = &scopedMockDb{MockMemDb: mm}
				}
				snap, ok := integrations.SnapshotRound(store, integrations.RoundQuery{})
				if !ok {
					t.Fatal("precondition: no round snapshot")
				}
				call := snap.CallRound().WithTrackerAhead(c.ahead)
				var copy1 *models.Mock
				if cur, _ := mm.GetPerTestMocksInWindow(); indexOfName(cur, "mock-own1") >= 0 {
					copy1 = cur[indexOfName(cur, "mock-own1")]
				}
				c.move(mm)
				held, _ := integrations.HoldToRound(store, call)
				perTest, _ := held.GetPerTestMocksInWindow()
				//nolint:staticcheck // SA1019: a parser's per-test read (the generic matcher's) is one of the hold's reads.
				filtered, _ := held.GetFilteredMocks()
				//nolint:staticcheck // SA1019: a parser's session read is one of the hold's reads.
				unfiltered, _ := held.GetUnFilteredMocks()
				session, _ := held.GetSessionScopedMocks()
				//nolint:staticcheck // SA1019: a parser's session read (HTTP's strict pass) is one of the hold's reads.
				sessionAll, _ := held.GetSessionMocks()
				var walked []*models.Mock
				if r, ok := held.(integrations.PerTestRanger); ok {
					_ = r.RangePerTestMocksInWindow(func(mk *models.Mock) bool { walked = append(walked, mk); return true })
				}
				var keyed []*models.Mock
				if r, ok := held.(integrations.SessionKeyReader); ok {
					ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"config"} }}
					_ = r.RangeSessionMocksWithKey(ix, "config", func(mk *models.Mock) bool { keyed = append(keyed, mk); return true })
				}
				reads := map[string][]*models.Mock{"per-test": perTest, "filtered": filtered, "unfiltered": unfiltered, "session-scoped": session, "session": sessionAll, "walk": walked, "keyed walk": keyed}
				var copy2 *models.Mock
				for what, read := range reads {
					for _, name := range []string{"mock-own2", "mock-sess2"} {
						i := indexOfName(read, name)
						if c.own2 && i >= 0 && name == "mock-own2" {
							copy2 = read[i]
						}
						if !c.own2 && i >= 0 {
							t.Errorf("once %s the hold's %s read holds test-2's %s", c.name, what, name)
						}
					}
				}
				if indexOfName(perTest, "mock-pool") < 0 && indexOfName(session, "mock-pool") < 0 && indexOfName(sessionAll, "mock-pool") < 0 {
					t.Errorf("the hold reads none of the pool's mock-pool (per-test %v, session %v); want it: a shared scope is read in the view in force", answeredNames(perTest), answeredNames(sessionAll))
				}
				if hs, ok := held.(*heldScope); ok && copy1 != nil {
					// A copy the call's own view served, read before the
					// move: the call's own still.
					if got := hs.unhidden([]*models.Mock{copy1}); len(got) != 1 || hs.Spent(copy1) {
						t.Error("the hold hides test-1's own mock-own1, a copy its call's view served")
					}
				}
				defer func() {
					why := integrations.CallViewGoneWhy(integrations.DiagnoseInCallView(held))
					want := ""
					switch {
					case c.between:
						want = "decided this request between tests, and the replay began test-2"
					case !c.ahead:
						want = "decided this request in test-1, and the replay ended test-1"
					}
					if want == "" && why != "" || want != "" && !strings.Contains(why, want) {
						t.Errorf("the moved miss is told %q; want %q", why, want)
					}
				}()
				spent, _ := held.(interface{ MockSpent(string) bool })
				if c.own2 {
					if copy2 == nil {
						t.Fatalf("the hold reads %v; want test-2's mock-own2", answeredNames(perTest))
					}
					if spent != nil && spent.MockSpent("mock-own2") {
						t.Error("the hold's MockSpent says mock-own2, its call's own, is spent")
					}
					if !held.DeleteFilteredMock(*copy2) {
						t.Error("the hold refused its call's own mock-own2")
					}
					return
				}
				if spent != nil && !spent.MockSpent("mock-own2") {
					t.Error("the hold's MockSpent leaves test-2's mock-own2 to a parser's index")
				}
				// A copy the view in force serves: the door refuses it,
				// and it stays unanswered there.
				var served *models.Mock
				if cur, _ := mm.GetPerTestMocksInWindow(); indexOfName(cur, "mock-own2") >= 0 {
					served = cur[indexOfName(cur, "mock-own2")]
				}
				if served == nil {
					t.Fatal("precondition: the view in force serves no mock-own2")
				}
				if sr, ok := held.(integrations.SpentReader); ok && !sr.Spent(served) {
					t.Error("the hold's Spent leaves test-2's mock-own2 to a parser's index")
				}
				if held.DeleteFilteredMock(*served) || held.MarkMockAsUsed(*served) || held.DeleteUnFilteredMock(*served) {
					t.Error("a consume door of the hold settled test-2's mock-own2")
				}
				if cur, _ := mm.GetPerTestMocksInWindow(); indexOfName(cur, "mock-own2") < 0 {
					t.Error("test-2's mock-own2 was consumed by test-1's call")
				}
				if !strings.HasPrefix(c.name, "test-2") {
					return
				}
				// A copy test-2's view served, read after test-2 ended
				// under the call: still another test's own.
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				if hs, ok := held.(*heldScope); ok {
					if got := hs.unhidden([]*models.Mock{served}); len(got) > 0 {
						t.Error("after test-2 ended, the hold keeps a copy test-2's view served")
					}
				}
				if sr, ok := held.(integrations.SpentReader); ok && !sr.Spent(served) {
					t.Error("after test-2 ended, the hold's Spent leaves a copy test-2's view served")
				}
			})
		}
	}
}

// indexOfName is the position of the mock named name in mocks, -1 for none.
func indexOfName(mocks []*models.Mock, name string) int {
	for i, mk := range mocks {
		if mk != nil && mk.Name == name {
			return i
		}
	}
	return -1
}
