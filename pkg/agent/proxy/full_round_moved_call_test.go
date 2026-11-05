package proxy

import (
	"context"
	"errors"
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

// lookupHook is the manager whose holds run move once, first, in the miss's
// lookup named at: "not-owned" (the recording-made-elsewhere lookup, run
// after the report read the hold) or "repeat" (the repeat note's, the
// miss's last read).
type lookupHook struct {
	*MockManager
	at   string
	move func()
	ran  atomic.Bool
}

func (d *lookupHook) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	db, restaged := d.MockManager.HoldToRound(call)
	if hs, ok := db.(*heldScope); ok {
		return &lookupHeld{heldScope: hs, d: d}, restaged
	}
	return db, restaged
}

// lookupHeld is a call's hold whose lookup d.at runs d.move once, first.
type lookupHeld struct {
	*heldScope
	d *lookupHook
}

func (w *lookupHeld) in(at string) {
	if w.d.at == at && w.d.ran.CompareAndSwap(false, true) {
		w.d.move()
	}
}

func (w *lookupHeld) DiagnoseInCallView() integrations.MockMemDb {
	w.heldScope.DiagnoseInCallView()
	return w
}

func (w *lookupHeld) FindStagedNotOwnedByKeys(kind models.Kind, call integrations.CallRound, keys []string, match func(*models.Mock) bool) (string, bool) {
	w.in("not-owned")
	return w.heldScope.FindStagedNotOwnedByKeys(kind, call, keys, match)
}

func (w *lookupHeld) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	w.in("not-owned")
	return w.heldScope.FindStagedNotOwned(kind, call, match)
}

func (w *lookupHeld) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	w.in("repeat")
	return w.heldScope.FindAnsweredEarlier(kind, keys, match)
}

// A full-round miss whose view moves after the call was decided (its learn
// step, variable pass, extra-call count and pairing all read test-1's view,
// and the replay ends test-1, or ends it and begins test-2) names no
// recording and diffs the call against none, and says why: the replay
// ended test-1 (and began test-2) after the call was decided. That holds
// wherever the move lands: before the miss's report, after the report read
// the hold and named its closest (in the lookup of a recording made
// elsewhere), or in its last read (the repeat note's lookup): whatever the
// miss read before the move is dropped with it. The move is told once.
// With no move, the miss names test-1's own mock-own1. Before, a move
// before the report named the pool's mock-pool or test-2's mock-own2 as
// its closest, with a diff of a test-1 call against it; and a move after
// the report kept test-1's mock-own1 and its diff beside a sentence saying
// no recording is named.
func TestAFullRoundMissWhoseViewMovesAfterItsDecisionNamesNoRecording(t *testing.T) {
	type tc struct{ at, move string }
	cases := []tc{{"before the report", "never"}}
	for _, at := range []string{"before the report", "not-owned", "repeat"} {
		for _, move := range []string{"the test ends", "the next test begins"} {
			cases = append(cases, tc{at, move})
		}
	}
	for _, c := range cases {
		t.Run(c.at+"/"+c.move, func(t *testing.T) {
			p, mm := heldOrders(t, false)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			move := func() {
				if c.move == "never" {
					return
				}
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
				if c.move == "the next test begins" {
					mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
					p.ContinueTestErrorCapture()
				}
			}
			lg, db := zap.NewNop(), &lookupHook{MockManager: mm, at: c.at, move: move}
			if c.at == "before the report" {
				lg = onDebugLine("after matching the http request", move)
			}
			code, got := r3SendVia(t, context.Background(), httpint.New(lg), p, db, models.OutgoingOptions{Learn: true, LearnPool: true}, "GET /orders HTTP/1.1\r\nHost: deps.example\r\n\r\n")
			if code == http.StatusOK {
				t.Fatalf("answered %d %s", code, got)
			}
			if c.at != "before the report" && !db.ran.Load() {
				t.Fatalf("precondition: the miss's %s lookup never ran", c.at)
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
			}[c.move]
			switch {
			case c.move == "never":
				if m.ClosestMock != "mock-own1" || strings.Contains(m.NextSteps, "no recording is named") {
					t.Errorf("the unmoved miss names %q (next steps %q); want test-1's mock-own1, and no word of a move", m.ClosestMock, m.NextSteps)
				}
			case m.ClosestMock != "" || m.ClosestMockReq != "" || m.Diff != "" || len(m.FieldDiffs) > 0:
				t.Errorf("a test-1 miss whose view moved %s names %q (rendered %q, diff %q, fields %+v, next steps %q); want no recording", c.at, m.ClosestMock, m.ClosestMockReq, m.Diff, m.FieldDiffs, m.NextSteps)
			case m.Reason != models.MissViewReplaced || strings.Contains(m.NextSteps, "Re-record") || strings.Contains(m.NextSteps, "learn step"):
				t.Errorf("a test-1 miss whose view moved %s reads code %q, next steps %q; want %s, no learn step reason and no advice of a comparison", c.at, m.Reason, m.NextSteps, models.MissViewReplaced)
			case !strings.Contains(m.NextSteps, why) || strings.Count(m.NextSteps, "keploy record's full round") != 1:
				t.Errorf("the moved miss's next steps read %q; want them to say once %q", m.NextSteps, why)
			}
		})
	}
}

// A full-round miss of a call made elsewhere whose view moves after the
// call was decided names no recording either. In test-2, which recorded no
// call, a call that is exactly test-1's mock-own1 misses naming it: as made
// elsewhere for a late call of test-1, and with a repeat note for a repeat
// of a call mock-own1 answered in test-1. Once the replay ends test-2 (or
// ends it and begins test-3) before the miss's report, in its lookup of a
// recording made elsewhere, or after that lookup found mock-own1 (in the
// repeat note's lookup), the miss names no recording and notes no repeat:
// whatever the miss read of the view goes with it, a lookup that found its
// recording too. Nor is it given its learn step's reason (unmocked_call:
// test-2 recorded no such call) and that reason's advice to record again:
// it carries no code, as a call keploy could not judge, and its next steps
// are the move's alone.
func TestAMovedMissOfACallMadeElsewhereNamesNoRecording(t *testing.T) {
	type tc struct{ call, at, move string }
	var cases []tc
	for _, call := range []string{"a late call of test-1", "a repeat of a call test-1 answered"} {
		cases = append(cases, tc{call, "before the report", "never"})
		for _, at := range []string{"before the report", "not-owned", "repeat"} {
			for _, move := range []string{"the test ends", "the next test begins"} {
				cases = append(cases, tc{call, at, move})
			}
		}
	}
	for _, c := range cases {
		t.Run(c.call+"/"+c.at+"/"+c.move, func(t *testing.T) {
			p, mm := elsewhereOrders(t, false, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1, own2} }, nil)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			opts := models.OutgoingOptions{Learn: true, LearnPool: true}
			repeat := c.call == "a repeat of a call test-1 answered"
			if repeat {
				if code, got := r3SendVia(t, context.Background(), httpint.New(zap.NewNop()), p, mm, opts, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
					t.Fatalf("precondition: test-1's call was answered %d %s; want mock-own1's", code, got)
				}
			}
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			p.EndTestErrorCapture()
			mm.ServeExactTest("test-2", nil)
			p.ContinueTestErrorCapture()
			if _, err := p.GetMockErrors(context.Background()); err != nil {
				t.Fatal(err)
			}
			move := func() {
				if c.move == "never" {
					return
				}
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
				if c.move == "the next test begins" {
					mm.ServeExactTest("test-3", nil)
					p.ContinueTestErrorCapture()
				}
			}
			lg, db := zap.NewNop(), &lookupHook{MockManager: mm, at: c.at, move: move}
			if c.at == "before the report" {
				lg = onDebugLine("after matching the http request", move)
			}
			code, got := r3SendVia(t, context.Background(), httpint.New(lg), p, db, opts, roundPost(`{"item":"pear"}`))
			if code == http.StatusOK {
				t.Fatalf("answered %d %s", code, got)
			}
			if c.at != "before the report" && !db.ran.Load() {
				t.Fatalf("precondition: the miss's %s lookup never ran", c.at)
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
				"the test ends":        "decided this request in test-2, and the replay ended test-2 before keploy diagnosed its miss, so no recording is named for it",
				"the next test begins": "decided this request in test-2, and the replay ended test-2 and began test-3 before keploy diagnosed its miss, so no recording is named for it",
			}[c.move]
			switch {
			case c.move == "never":
				if m.ClosestMock != "mock-own1" || m.NotOwned == repeat || (m.Repeat != nil) != repeat || strings.Contains(m.NextSteps, "no recording is named") {
					t.Errorf("the unmoved miss names %q (made elsewhere %v, repeat %+v, next steps %q); want test-1's mock-own1, made elsewhere for the late call and noted as a repeat for the other, and no word of a move", m.ClosestMock, m.NotOwned, m.Repeat, m.NextSteps)
				}
			case m.ClosestMock != "" || m.ClosestMockReq != "" || m.NotOwned || m.Repeat != nil || m.Diff != "" || len(m.FieldDiffs) > 0:
				t.Errorf("a test-2 miss whose view moved %s names %q (made elsewhere %v, repeat %+v, rendered %q, diff %q, fields %+v, next steps %q); want no recording", c.at, m.ClosestMock, m.NotOwned, m.Repeat, m.ClosestMockReq, m.Diff, m.FieldDiffs, m.NextSteps)
			case strings.Contains(m.NextSteps, "mock-") || strings.Contains(m.NextSteps, "that recording's"):
				t.Errorf("the moved miss's next steps read %q; want no recording named or spoken of", m.NextSteps)
			case m.Reason != models.MissViewReplaced || m.NextSteps != "keploy record's full round "+why+": keploy reads nothing of a view a test's begin or end replaced.":
				t.Errorf("the moved miss reads code %q, next steps %q; want %s, and the move alone: %q", m.Reason, m.NextSteps, models.MissViewReplaced, why)
			}
		})
	}
}

// A miss of a call made elsewhere shows only the recording it names. In
// test-2, which owns mock-own2 (plum), a call that is exactly test-1's
// mock-own1 (pear) misses naming mock-own1 as made elsewhere, with its
// request rendered and no diff: none of mock-own1's fields differs. Before,
// it kept the diff its report built against mock-own2, the closest of
// test-2's own view, beside mock-own1's name. Once the replay ends test-2
// (or ends it and begins test-3) before the miss's report or in its lookup
// of a recording made elsewhere, after the report named mock-own2, the miss
// names neither, diffs against neither and gets no learn step reason
// (request_changed, from mock-own2): its next steps are the move's alone.
func TestAMissMadeElsewhereShowsOnlyTheRecordingItNames(t *testing.T) {
	type tc struct{ at, move string }
	cases := []tc{{"before the report", "never"}}
	for _, at := range []string{"before the report", "not-owned"} {
		for _, move := range []string{"the test ends", "the next test begins"} {
			cases = append(cases, tc{at, move})
		}
	}
	for _, c := range cases {
		t.Run(c.at+"/"+c.move, func(t *testing.T) {
			p, mm := elsewhereOrders(t, false, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1, own2} }, nil)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			p.EndTestErrorCapture()
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			p.ContinueTestErrorCapture()
			if _, err := p.GetMockErrors(context.Background()); err != nil {
				t.Fatal(err)
			}
			move := func() {
				if c.move == "never" {
					return
				}
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
				if c.move == "the next test begins" {
					mm.ServeExactTest("test-3", nil)
					p.ContinueTestErrorCapture()
				}
			}
			lg, db := zap.NewNop(), &lookupHook{MockManager: mm, at: c.at, move: move}
			if c.at == "before the report" {
				lg = onDebugLine("after matching the http request", move)
			}
			code, got := r3SendVia(t, context.Background(), httpint.New(lg), p, db, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`))
			if code == http.StatusOK {
				t.Fatalf("answered %d %s", code, got)
			}
			if c.at != "before the report" && !db.ran.Load() {
				t.Fatalf("precondition: the miss's %s lookup never ran", c.at)
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
				"the test ends":        "decided this request in test-2, and the replay ended test-2 before keploy diagnosed its miss, so no recording is named for it",
				"the next test begins": "decided this request in test-2, and the replay ended test-2 and began test-3 before keploy diagnosed its miss, so no recording is named for it",
			}[c.move]
			switch {
			case c.move == "never":
				if m.ClosestMock != "mock-own1" || !m.NotOwned || m.Diff != "" || len(m.FieldDiffs) > 0 || !strings.Contains(m.ClosestMockReq, "pear") || strings.Contains(m.ClosestMockReq+m.NextSteps, "plum") {
					t.Errorf("the unmoved miss names %q (made elsewhere %v), rendered %q, diff %q, fields %+v, next steps %q; want mock-own1 made elsewhere, rendered, with no diff and nothing of mock-own2", m.ClosestMock, m.NotOwned, m.ClosestMockReq, m.Diff, m.FieldDiffs, m.NextSteps)
				}
			case m.ClosestMock != "" || m.ClosestMockReq != "" || m.NotOwned || m.Diff != "" || len(m.FieldDiffs) > 0 || strings.Contains(m.NextSteps, "mock-"):
				t.Errorf("a test-2 miss whose view moved %s names %q (made elsewhere %v, rendered %q, diff %q, fields %+v, next steps %q); want no recording", c.at, m.ClosestMock, m.NotOwned, m.ClosestMockReq, m.Diff, m.FieldDiffs, m.NextSteps)
			case m.Reason != models.MissViewReplaced || m.NextSteps != "keploy record's full round "+why+": keploy reads nothing of a view a test's begin or end replaced.":
				t.Errorf("the moved miss reads code %q, next steps %q; want %s, and the move alone: %q", m.Reason, m.NextSteps, models.MissViewReplaced, why)
			}
		})
	}
}

// A moved full-round miss gets view_replaced, not its learn step's reason.
// Between tests, a call keploy cannot tell from test-2's mock-own2, a
// recording it cannot read back from disk, misses with the learn step's
// reason, which names mock-own2 and the pool's mock-pool it would pair the
// call with, and the next step for keploy failing to read its recordings.
// Once the replay begins test-2 before the miss's report, or in its lookup
// of a recording made elsewhere, the miss names neither and keeps no next
// step of that reason: the reason is what the learn step read of the pool's
// view, which went. Its code is view_replaced and its next steps are the
// move's alone. Before, the reason and its names stayed beside the sentence
// saying no recording is named; then the sentence that the learn step
// missed it did; then no code, so record's reason for the failed test did
// not say why it missed.
func TestAMovedMissGetsViewReplacedNotItsLearnReason(t *testing.T) {
	const why = "keploy cannot read every recording of the test set (read the recording mock-own2 kept on disk: disk mocks: short read)"
	const unread = "keploy failing to read its own recordings"
	const gone = "decided this request between tests, and the replay began test-2 before keploy diagnosed its miss, so no recording is named for it"
	for _, at := range []string{"never", "before the report", "not-owned"} {
		t.Run(at, func(t *testing.T) {
			p, mm := elsewhereOrders(t, false, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1} },
				func(own2 *models.Mock) RecordingSource {
					return keyedSource(func(*models.Mock) (*models.Mock, error) { return nil, errors.New("disk mocks: short read") }, own2)
				})
			elsewhereBetween(t, p, mm, "mock-own1")
			move := func() {
				mm.ServeExactTest("test-2", nil)
				p.ContinueTestErrorCapture()
			}
			lg, db := zap.NewNop(), &lookupHook{MockManager: mm, at: at, move: move}
			if at == "before the report" {
				lg = onDebugLine("after matching the http request", move)
			}
			code, got := r3SendVia(t, context.Background(), httpint.New(lg), p, db, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"fig"}`))
			if code == http.StatusOK {
				t.Fatalf("answered %d %s", code, got)
			}
			if at == "not-owned" && !db.ran.Load() {
				t.Fatalf("precondition: the miss's %s lookup never ran", at)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 {
				t.Fatalf("misses %+v; want 1", misses)
			}
			m := misses[0]
			switch {
			case at == "never":
				if !strings.Contains(m.NextSteps, why) || !strings.Contains(m.NextSteps, unread) || strings.Contains(m.NextSteps, "no recording is named") {
					t.Errorf("the unmoved miss reads %q; want the learn step's reason %q, its next step, and no word of a move", m.NextSteps, why)
				}
			case m.ClosestMock != "" || strings.Contains(m.NextSteps, "mock-") || strings.Contains(m.NextSteps, unread):
				t.Errorf("a miss whose view moved %s names %q, next steps %q; want no recording named and no next step of the learn step's reason", at, m.ClosestMock, m.NextSteps)
			case m.Reason != models.MissViewReplaced || m.NextSteps != "keploy record's full round "+gone+": keploy reads nothing of a view a test's begin or end replaced.":
				t.Errorf("the moved miss reads code %q, next steps %q; want %s, and the move alone: %q", m.Reason, m.NextSteps, models.MissViewReplaced, gone)
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
// again under the call; not the store's own mock-own2 or mock-sess2, which
// carry no view (told by name). Its MockSpent and Spent say so, so a
// parser's index never offers one. The pool's mock-pool, a shared scope's,
// it still reads. A call decided in the test the store opens next (the
// tracker ahead) reads test-2's as before, and a call a snapshot of the pool
// view read with mock-own2 in flight reads it once test-2 began. Before, the
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
				// The recordings carrying no view, as the store holds them
				// and as a parser builds a mock by name: still test-2's own,
				// told by name, while the view in force owns them.
				for _, rec := range []*models.Mock{own2, sess2} {
					if rec.TestModeInfo.ServedIn != 0 {
						t.Fatalf("precondition: the store's %s carries a view (%d)", rec.Name, rec.TestModeInfo.ServedIn)
					}
					if hs, ok := held.(*heldScope); ok {
						if got := hs.unhidden([]*models.Mock{rec}); len(got) > 0 {
							t.Errorf("the hold keeps test-2's %s as the store holds it, carrying no view", rec.Name)
						}
					}
					if held.DeleteFilteredMock(*rec) || held.MarkMockAsUsed(*rec) || held.DeleteUnFilteredMock(*rec) {
						t.Errorf("a consume door of the hold settled test-2's %s as the store holds it", rec.Name)
					}
				}
				if cur, _ := mm.GetPerTestMocksInWindow(); indexOfName(cur, "mock-own2") < 0 {
					t.Error("test-2's mock-own2 was consumed by test-1's call, through the store's own recording")
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

// A walk of the hold of a call marked from a round snapshot (record's full
// round) that a test's end straddles yields none of another test's own
// recordings after the end. The call was decided between tests with
// test-2's mock-own2 in flight, so mock-own2 is its own. Test-2 then began
// with mock-own2, mock-own3 and mock-sess2, and each walk starts in
// test-2's view: its first yield is mock-own2, and there the replay ends
// test-2. The walk's later copies, mock-own3 and mock-sess2, were served by
// test-2's view, which the call's view did not own, so they stay hidden,
// though the pool view now in force names neither of them.
func TestASnapshotMarkedCallsWalkThatATestsEndStraddlesYieldsNoOtherTestsOwnRecording(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, walk := range []string{"per-test walk", "keyed walk"} {
			t.Run(map[bool]string{false: "the manager", true: "a scoped view"}[scoped]+"/"+walk, func(t *testing.T) {
				t.Setenv(provenswitch.Env, "1")
				now := time.Now()
				pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
				own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
				own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
				own3 := r3Mock("mock-own3", now.Add(1505*time.Millisecond), "POST", "/orders", `{"item":"fig"}`, `{"order":3}`)
				sess2 := r3Mock("mock-sess2", now.Add(1510*time.Millisecond), "GET", "/config", "", `{"v":2}`)
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				mm.SeedRecordedWindows([]models.TestWindow{
					{Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
					{Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
				})
				mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2, own3}, []*models.Mock{sess2}, models.BaseTime, now.Add(time.Hour))
				mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"}, roundMock(mm, "mock-own2"))
				var store integrations.MockMemDb = mm
				if scoped {
					store = &scopedMockDb{MockMemDb: mm}
				}
				snap, ok := integrations.SnapshotRound(store, integrations.RoundQuery{})
				if !ok {
					t.Fatal("precondition: no round snapshot")
				}
				mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2"), roundMock(mm, "mock-own3"), roundMock(mm, "mock-sess2")})
				held, _ := integrations.HoldToRound(store, snap.CallRound())
				var yielded []string
				fn := func(mk *models.Mock) bool {
					if len(yielded) == 0 {
						mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
					}
					yielded = append(yielded, mk.Name)
					return true
				}
				switch walk {
				case "per-test walk":
					r, ok := held.(integrations.PerTestRanger)
					if !ok {
						t.Fatal("precondition: the hold has no per-test walk")
					}
					_ = r.RangePerTestMocksInWindow(fn)
				default:
					r, ok := held.(integrations.SessionKeyReader)
					if !ok {
						t.Fatal("precondition: the hold has no keyed walk")
					}
					ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"config"} }}
					_ = r.RangeSessionMocksWithKey(ix, "config", fn)
				}
				if len(yielded) == 0 || yielded[0] != "mock-own2" {
					t.Fatalf("precondition: the walk yielded %v; want the call's own mock-own2 first, where test-2 ends", yielded)
				}
				for _, name := range yielded[1:] {
					if name == "mock-own3" || name == "mock-sess2" {
						t.Errorf("after test-2 ended inside the walk, it yielded test-2's %s (all: %v)", name, yielded)
					}
				}
			})
		}
	}
}
