package proxy

// Tests for the MockManager hooks a server-push protocol paces its deliveries
// with: the recorded windows of the whole set (O1), the window-change signal and
// staging epoch (O2), and the carry-over tier (C1).

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var pace0 = time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)

func pms(ms int) time.Time { return pace0.Add(time.Duration(ms) * time.Millisecond) }

func windowsOf(names ...string) []models.TestWindow {
	out := make([]models.TestWindow, 0, len(names))
	for i, n := range names {
		start := pms(1000 * (i + 1))
		out = append(out, models.TestWindow{TestCase: n, Start: start, End: start.Add(100 * time.Millisecond)})
	}
	return out
}

func scheduleNames(s *models.WindowSchedule) []string {
	out := make([]string, 0, s.Len())
	for i := 0; i < s.Len(); i++ {
		out = append(out, s.Window(i).TestCase)
	}
	return out
}

// The seed follows SeedStartupCutoff's lifecycle: parked at a set boundary until
// that set's staging call, applied at once mid-set, and never inherited by a set
// that did not send one.
func TestSeedRecordedWindowsLifecycle(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	if mm.RecordedWindows() != nil {
		t.Fatal("a fresh manager has no recorded windows")
	}

	// Set A: reset, seed, stage.
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("a-1", "a-2"))
	if mm.RecordedWindows() != nil {
		t.Fatal("set A's windows were installed before its staging call; they must be parked " +
			"until the trees they describe are swapped in")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := scheduleNames(mm.RecordedWindows()); len(got) != 2 || got[0] != "a-1" {
		t.Fatalf("after staging set A: %v, want [a-1 a-2]", got)
	}

	// Mid-set seed (no boundary pending): applied at once.
	mm.SeedRecordedWindows(windowsOf("a-1", "a-2", "a-3"))
	if got := scheduleNames(mm.RecordedWindows()); len(got) != 3 {
		t.Fatalf("mid-set seed not applied: %v", got)
	}

	// Set B: reset and seed; set A's windows stay until B's staging call.
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("b-1"))
	if got := scheduleNames(mm.RecordedWindows()); len(got) != 3 {
		t.Fatalf("set B's seed replaced set A's before B was staged: %v", got)
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := scheduleNames(mm.RecordedWindows()); len(got) != 1 || got[0] != "b-1" {
		t.Fatalf("after staging set B: %v, want [b-1]", got)
	}

	// Set C sends no windows (an older CLI): it must not inherit B's.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.RecordedWindows() != nil {
		t.Fatalf("set C inherited set B's windows: %v", scheduleNames(mm.RecordedWindows()))
	}

	// A park whose staging never came is dropped by the next reset.
	mm.SeedRecordedWindows(windowsOf("c-1")) // mid-set: applied
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("d-1"))
	mm.ResetForReplaySession() // d's staging was aborted
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.RecordedWindows() != nil {
		t.Fatalf("an aborted set's parked windows were installed for the next set: %v",
			scheduleNames(mm.RecordedWindows()))
	}
}

// With no kind registered for carry-over nothing reads the windows, so the
// manager keeps none: a replay that stages them (every replay from a current
// CLI, Pulsar or not) holds no schedule for its sets, whether the seed is
// parked at a boundary or applied mid-set.
func TestRecordedWindowsNotKeptWithoutACarryOverKind(t *testing.T) {
	if models.CarryOverRegistered() {
		t.Fatal("precondition: a registration leaked from another test")
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("a-1", "a-2"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.RecordedWindows(); got != nil {
		t.Fatalf("staged set kept %v with no carry-over kind registered", scheduleNames(got))
	}
	mm.SeedRecordedWindows(windowsOf("a-1", "a-2", "a-3")) // mid-set
	if got := mm.RecordedWindows(); got != nil {
		t.Fatalf("a mid-set seed kept %v with no carry-over kind registered", scheduleNames(got))
	}

	// The gate follows the registry: a kind registered, the next set keeps them.
	unregister := models.RegisterCarryOver(pulsarKind, func(m *models.Mock) bool {
		return m.Spec.Metadata["commandType"] == "SEND"
	})
	t.Cleanup(unregister)
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("b-1"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := scheduleNames(mm.RecordedWindows()); len(got) != 1 || got[0] != "b-1" {
		t.Fatalf("with a carry-over kind registered: %v, want [b-1]", got)
	}

	// And back: with the registration gone, a seed leaves no schedule behind,
	// neither the installed one (mid-set) nor a parked one (at a boundary).
	unregister()
	mm.SeedRecordedWindows(windowsOf("b-1", "b-2"))
	if got := mm.RecordedWindows(); got != nil {
		t.Fatalf("a mid-set seed with nothing registered left %v installed", scheduleNames(got))
	}
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(windowsOf("c-1"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.RecordedWindows(); got != nil {
		t.Fatalf("a staged set with nothing registered kept %v", scheduleNames(got))
	}
}

// Consumers reach it by type assertion on the MockMemDb they are handed, which
// in a scoped worker is the wrapper, not the manager.
func TestRecordedWindowsSurvivesTheWorkerScopeWrap(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SeedRecordedWindows(windowsOf("t-1", "t-2"))

	var db interface{} = &scopedMockDb{MockMemDb: mm}
	r, ok := db.(integrations.RecordedWindowsReader)
	if !ok {
		t.Fatal("the worker-scope wrap erases RecordedWindows")
	}
	if got := scheduleNames(r.RecordedWindows()); len(got) != 2 {
		t.Fatalf("wrapped RecordedWindows = %v", got)
	}
	var mgr interface{} = mm
	if _, ok := mgr.(integrations.RecordedWindowsReader); !ok {
		t.Fatal("MockManager does not implement RecordedWindowsReader")
	}
}

// ---------------------------------------------------------------------------
// O2: WindowChanged and StagingEpoch
// ---------------------------------------------------------------------------

func fired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Every window switch closes the channel handed out before it, after the new
// window is visible, and hands out a fresh open one.
func TestWindowChangedFiresOnEveryWindowSwitch(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()

	steps := []struct {
		name string
		do   func()
		want time.Time // CurrentTestWindow start once woken; zero = no window
	}{
		{"staging", func() { mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now()) }, time.Time{}},
		{"test-1", func() { mm.SetMocksWithWindow(nil, nil, pms(1000), pms(1100)) }, pms(1000)},
		{"test-2", func() { mm.SetMocksWithWindow(nil, nil, pms(2000), pms(2100)) }, pms(2000)},
		{"the same window again", func() { mm.SetMocksWithWindow(nil, nil, pms(2000), pms(2100)) }, pms(2000)},
		{"SetCurrentTestWindow", func() { mm.SetCurrentTestWindow(pms(3000), pms(3100)) }, pms(3000)},
		{"three-tier", func() {
			mm.SetMocksWithWindowThreeTier(nil, nil, []*models.Mock{newMockForTest("boot", pms(10), models.LifetimePerTest)}, pms(4000), pms(4100))
		}, pms(4000)},
	}
	for _, s := range steps {
		ch := mm.WindowChanged()
		if fired(ch) {
			t.Fatalf("%s: channel closed before the switch", s.name)
		}
		s.do()
		if !fired(ch) {
			t.Fatalf("%s: WindowChanged did not fire", s.name)
		}
		if start, _ := mm.CurrentTestWindow(); !start.Equal(s.want) {
			t.Fatalf("%s: woken with window start %v, want %v (signal fired before the window was published)", s.name, start, s.want)
		}
		if fired(mm.WindowChanged()) {
			t.Fatalf("%s: the next channel is already closed", s.name)
		}
	}
	// The three-tier call signals after its startup additions land.
	ch := mm.WindowChanged()
	go mm.SetMocksWithWindowThreeTier(nil, nil, []*models.Mock{newMockForTest("boot-2", pms(20), models.LifetimePerTest)}, pms(5000), pms(5100))
	<-ch
	startup, _ := mm.GetStartupMocks()
	if !containsMockNamed(startup, "boot-2") {
		t.Fatal("three-tier signalled before its explicit startup mocks were inserted")
	}
}

// The epoch moves at every staging call — a boundary, and a mid-set restage — and
// never at a per-test call.
func TestStagingEpochChangesAtEachStagingCall(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	e0 := mm.StagingEpoch()
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	e1 := mm.StagingEpoch()
	if e1 == e0 {
		t.Fatal("the set's staging call did not change the epoch")
	}
	mm.SetMocksWithWindow(nil, nil, pms(1000), pms(1100))
	mm.SetCurrentTestWindow(pms(2000), pms(2100))
	if mm.StagingEpoch() != e1 {
		t.Fatal("a per-test window change moved the staging epoch")
	}
	// A reset alone is not a staging call.
	mm.ResetForReplaySession()
	if mm.StagingEpoch() != e1 {
		t.Fatal("ResetForReplaySession moved the epoch; the staging call is the boundary")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	e2 := mm.StagingEpoch()
	if e2 == e1 {
		t.Fatal("the next set's staging call did not change the epoch")
	}
	// A mid-set restage (no reset before it) is a new staging snapshot too.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.StagingEpoch() == e2 {
		t.Fatal("a mid-set staging call did not change the epoch")
	}
}

// Close wakes waiters once, and a waiter that does not check IsClosed blocks
// again rather than spinning.
func TestWindowChangedWakesOnClose(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	ch := mm.WindowChanged()
	mm.Close()
	if !fired(ch) {
		t.Fatal("Close did not wake the waiter")
	}
	if fired(mm.WindowChanged()) {
		t.Fatal("after Close WindowChanged returns a closed channel, so a waiter loop would spin")
	}
	mm.Close() // idempotent: no double close
}

func TestWindowPacerSurvivesTheWorkerScopeWrap(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	var mgr interface{} = mm
	if _, ok := mgr.(integrations.WindowPacer); !ok {
		t.Fatal("MockManager does not implement WindowPacer")
	}
	var db interface{} = &scopedMockDb{MockMemDb: mm}
	p, ok := db.(integrations.WindowPacer)
	if !ok {
		t.Fatal("the worker-scope wrap erases WindowPacer")
	}
	ch := p.WindowChanged()
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !fired(ch) || p.StagingEpoch() != mm.StagingEpoch() || p.StagingEpoch() == 0 {
		t.Fatal("the wrapped WindowPacer does not follow the manager")
	}
}

// -race: waiters looping on the documented take-channel-then-read pattern while
// windows switch concurrently. Every waiter must observe the final window.
func TestWindowChangedConcurrentWaiters(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	const switches, waiters = 200, 8
	final := pms(switches * 10)
	done := make(chan struct{})
	for w := 0; w < waiters; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				ch := mm.WindowChanged()
				if start, _ := mm.CurrentTestWindow(); start.Equal(final) {
					_ = mm.StagingEpoch()
					return
				}
				select {
				case <-ch:
				case <-time.After(10 * time.Second):
					t.Error("waiter missed the final window switch")
					return
				}
			}
		}()
	}
	for i := 1; i <= switches; i++ {
		if i%50 == 0 {
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		}
		mm.SetMocksWithWindow(nil, nil, pms(i*10), pms(i*10+5))
	}
	for w := 0; w < waiters; w++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// C1: the carry-over tier
// ---------------------------------------------------------------------------

// pulsarKind stands in for the enterprise Pulsar kind, which registers SEND.
const pulsarKind models.Kind = "TestPulsar"

func registerSends(t *testing.T) {
	t.Helper()
	unregister := models.RegisterCarryOver(pulsarKind, func(m *models.Mock) bool {
		return m.Spec.Metadata["commandType"] == "SEND"
	})
	t.Cleanup(unregister)
}

func brokerMockAt(name string, kind models.Kind, cmd string, reqMs int) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks", "commandType": cmd},
			ReqTimestampMock: pms(reqMs),
			ResTimestampMock: pms(reqMs).Add(time.Millisecond),
		},
	}
	m.DeriveLifetime()
	return m
}

// carryWindows: W1..W5 at 1000..5000 ms, 100 ms each.
func carryWindows() []models.TestWindow {
	return windowsOf("test-1", "test-2", "test-3", "test-4", "test-5")
}

func winStart(i int) time.Time { return pms(1000 * i) }
func winEnd(i int) time.Time   { return pms(1000*i + 100) }

// carryRig drives a MockManager the way the agent's UpdateMockParams does: the
// window's own mocks from the recording plus the carry-over lookahead range,
// fresh copies each call, minus what was consumed (filterOutDeleted).
type carryRig struct {
	t         *testing.T
	mm        *MockManager
	recording []*models.Mock
	consumed  map[string]models.MockState
	loads     [][2]time.Time
}

func newCarryRig(t *testing.T, recording ...*models.Mock) *carryRig {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	r := &carryRig{t: t, mm: mm, recording: recording, consumed: map[string]models.MockState{}}
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(carryWindows())
	r.stage()
	return r
}

func (r *carryRig) fresh(filter func(*models.Mock) bool) []*models.Mock {
	var out []*models.Mock
	for _, m := range r.recording {
		if !filter(m) {
			continue
		}
		if st, ok := r.consumed[m.Name]; ok && st.Usage == models.Deleted {
			continue
		}
		c := m.DeepCopy()
		out = append(out, c)
	}
	return out
}

func (r *carryRig) stage() {
	r.mm.SetMocksWithWindow(r.fresh(func(*models.Mock) bool { return true }), nil, models.BaseTime, time.Now())
}

func (r *carryRig) window(i int) {
	start, end := winStart(i), winEnd(i)
	in := func(m *models.Mock) bool {
		return !m.Spec.ReqTimestampMock.Before(start) && !m.Spec.ReqTimestampMock.After(end)
	}
	filtered := r.fresh(in)
	if from, to, ok := r.mm.CarryOverLoadRange(start); ok {
		r.loads = append(r.loads, [2]time.Time{from, to})
		have := map[string]bool{}
		for _, m := range filtered {
			have[m.Name] = true
		}
		for _, m := range r.fresh(func(m *models.Mock) bool {
			return models.IsCarryOver(m) && !m.Spec.ReqTimestampMock.Before(from) && !m.Spec.ReqTimestampMock.After(to)
		}) {
			if !have[m.Name] {
				filtered = append(filtered, m)
			}
		}
	}
	r.mm.SetMocksWithWindow(filtered, nil, start, end)
}

func (r *carryRig) drain() {
	for _, st := range r.mm.GetConsumedMocks() {
		r.consumed[st.Name] = st
	}
}

// consume matches the named mock the way a parser does: it reads the tiers in
// order (per-test, startup, carry-over), takes the mock from the first that
// holds it and consumes that through DeleteFilteredMock. A mock no tier holds
// is offered as the recorded copy, which no door should accept.
func (r *carryRig) consume(name string) bool {
	perTest, _ := r.mm.GetPerTestMocksInWindow()
	startup, _ := r.mm.GetStartupMocks()
	carry, _ := r.mm.GetCarryOverMocks()
	for _, tier := range [][]*models.Mock{perTest, startup, carry} {
		for _, m := range tier {
			if m.Name == name {
				ok := r.mm.DeleteFilteredMock(*m)
				r.drain()
				return ok
			}
		}
	}
	for _, m := range r.recording {
		if m.Name == name {
			ok := r.mm.DeleteFilteredMock(*m.DeepCopy())
			r.drain()
			return ok
		}
	}
	r.t.Fatalf("no recorded mock %s", name)
	return false
}

func pacedNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func (r *carryRig) carryNames() []string {
	ms, _ := r.mm.GetCarryOverMocksByKind(pulsarKind)
	return pacedNames(ms)
}

func (r *carryRig) perTestNames() []string {
	ms, _ := r.mm.GetPerTestMocksInWindow()
	return pacedNames(ms)
}

func eqNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *carryRig) wantCarry(step string, want ...string) {
	r.t.Helper()
	if got := r.carryNames(); !eqNames(got, want) {
		r.t.Fatalf("%s: carry-over = %v, want %v", step, got, want)
	}
}

// A registered per-test mock is reachable from the release window of its
// recorded time minus 1 s until it is consumed, is consumed once, and never
// appears earlier. Unregistered kinds, and the registered kind's other
// commands, are dropped when their window closes, as today.
func TestCarryOverReachableFromReleaseMinusLookaheadUntilConsumed(t *testing.T) {
	registerSends(t)
	r := newCarryRig(t,
		brokerMockAt("s1", pulsarKind, "SEND", 2050), // in W2; 2050-1s is in W1's gap -> from W1
		brokerMockAt("h1", models.HTTP, "", 2060),    // unregistered, in W2
		brokerMockAt("f1", pulsarKind, "FLOW", 2070), // registered kind, not a SEND, in W2
		brokerMockAt("s2", pulsarKind, "SEND", 2500), // gap after W2; 1500 -> W1
		brokerMockAt("s3", pulsarKind, "SEND", 3050), // in W3; 2050 -> W2
		brokerMockAt("s4", pulsarKind, "SEND", 4050), // in W4; 3050 -> W3
		brokerMockAt("s5", pulsarKind, "SEND", 5050), // in W5; 4050 -> W4
	)
	r.wantCarry("staging")

	r.window(1)
	r.wantCarry("W1: s1 and s2 are reachable 1 s ahead; s3 must not appear before W2", "s1", "s2")
	if got := r.loads[0]; !got[0].Equal(winStart(1)) || !got[1].Equal(winStart(2).Add(-time.Nanosecond).Add(time.Second)) {
		t.Fatalf("W1 lookahead range = %v..%v", got[0].Sub(pace0), got[1].Sub(pace0))
	}

	r.window(2)
	r.wantCarry("W2: s1 is W2's own now; s3 becomes reachable", "s2", "s3")
	if got := r.perTestNames(); !eqNames(got, []string{"s1", "h1", "f1"}) {
		t.Fatalf("W2 per-test = %v", got)
	}
	if !r.consume("s2") {
		t.Fatal("W2: s2 (a gap SEND) could not be consumed from the carry-over tier")
	}
	if r.consume("s2") {
		t.Fatal("s2 was consumed twice")
	}
	r.wantCarry("W2 after consuming s2", "s3")

	r.window(3)
	r.wantCarry("W3: W2 closed with s1 unconsumed -> carried; s3 is W3's own; s4 reachable", "s1", "s4")
	for _, gone := range []string{"h1", "f1"} {
		ms, _ := r.mm.GetFilteredMocks()
		st, _ := r.mm.GetStartupMocks()
		co, _ := r.mm.GetCarryOverMocks()
		if containsMockNamed(ms, gone) || containsMockNamed(st, gone) || containsMockNamed(co, gone) {
			t.Fatalf("W3: %s (not carry-over) outlived its window", gone)
		}
		if r.consume(gone) {
			t.Fatalf("W3: %s was consumable after its window closed", gone)
		}
	}
	if !r.consume("s1") {
		t.Fatal("W3: s1, carried from W2, could not be consumed")
	}

	r.window(4)
	r.wantCarry("W4", "s3", "s5")
	for _, n := range []string{"s3", "s4", "s5"} {
		if !r.consume(n) {
			t.Fatalf("W4: %s not consumable", n)
		}
	}
	r.wantCarry("W4 after consuming everything")

	want := map[string]bool{"s1": true, "s2": true, "s3": true, "s4": false, "s5": true}
	for n, carry := range want {
		st, ok := r.consumed[n]
		if !ok {
			t.Fatalf("%s: no consumed state", n)
		}
		if st.Usage != models.Deleted {
			t.Fatalf("%s: usage %s, want deleted (so the next load filters it out)", n, st.Usage)
		}
		if st.CarryOver != carry {
			t.Fatalf("%s: CarryOver=%v, want %v (only consumes outside the running window are flagged)", n, st.CarryOver, carry)
		}
	}

	r.window(5)
	r.wantCarry("W5: consumed mocks are never reloaded")
}

// A retry cycle (or the deferred streaming tests) goes back to an earlier
// window: the tier starts over and is refilled by a reload from the set's start
// that is filtered by what was consumed.
func TestCarryOverRestartsWhenAPassGoesBack(t *testing.T) {
	registerSends(t)
	r := newCarryRig(t,
		brokerMockAt("s1", pulsarKind, "SEND", 2050),
		brokerMockAt("s2", pulsarKind, "SEND", 2500),
		brokerMockAt("s4", pulsarKind, "SEND", 4050),
	)
	r.window(1)
	r.window(2)
	r.window(3)
	r.window(4)
	r.consume("s2")
	r.wantCarry("end of pass 1", "s1")

	// The retry cycle rewinds the CLI's consumed map to its baseline.
	r.consumed = map[string]models.MockState{}
	r.window(1)
	from, _ := r.loads[len(r.loads)-1][0], r.loads[len(r.loads)-1][1]
	if !from.Equal(winStart(1)) {
		t.Fatalf("a pass that went back must reload from the set's start, got %v", from.Sub(pace0))
	}
	r.wantCarry("pass 2, W1", "s1", "s2")
}

// Above the cap, carry-over stops in recorded order with one Warn naming the
// count left out; later overflows of the same set are Debug.
func TestCarryOverCapWarnsOnceWithTheCount(t *testing.T) {
	registerSends(t)
	prev := carryOverCapBytes
	t.Cleanup(func() { carryOverCapBytes = prev })
	one := approxMockBytes(brokerMockAt("sx", pulsarKind, "SEND", 0))
	carryOverCapBytes = 2*one + one/2 // room for two

	core, logs := observer.New(zap.DebugLevel)
	mm := NewMockManager(nil, nil, zap.New(core))
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	offer := func(win int, ms ...*models.Mock) {
		mm.SetMocksWithWindow(ms, nil, winStart(win), winEnd(win))
	}
	// W1: five reachable gap SENDs recorded out of order; the two earliest stay.
	offer(1,
		brokerMockAt("c", pulsarKind, "SEND", 1500),
		brokerMockAt("a", pulsarKind, "SEND", 1200),
		brokerMockAt("e", pulsarKind, "SEND", 1900),
		brokerMockAt("b", pulsarKind, "SEND", 1300),
		brokerMockAt("d", pulsarKind, "SEND", 1700),
	)
	got, _ := mm.GetCarryOverMocks()
	if !eqNames(pacedNames(got), []string{"a", "b"}) {
		t.Fatalf("held %v; the cap must keep the earliest-recorded", pacedNames(got))
	}
	warns := logs.FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("%d Warns, want exactly one", len(warns))
	}
	if n := warns[0].ContextMap()["leftOut"]; n != int64(3) {
		t.Fatalf("the Warn names leftOut=%v, want 3", n)
	}
	// W2: one more overflow in the same set is not another Warn.
	offer(2, brokerMockAt("f", pulsarKind, "SEND", 2500))
	if n := len(logs.FilterLevelExact(zap.WarnLevel).All()); n != 1 {
		t.Fatalf("%d Warns after a second overflow, want still 1", n)
	}
	dbg := logs.FilterMessageSnippet("carry-over tier still full").All()
	if len(dbg) != 1 || dbg[0].ContextMap()["leftOutThisSet"] != int64(4) {
		t.Fatalf("second overflow not reported at Debug with the running count: %+v", dbg)
	}
	// Consuming frees room; a new staging call re-arms the Warn.
	if !mm.DeleteFilteredMock(*brokerMockAt("a", pulsarKind, "SEND", 1200)) {
		t.Fatal("could not consume a")
	}
	if n, _ := mm.carry.size(); n != 1 {
		t.Fatalf("held %d after consuming one, want 1", n)
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if n, b := mm.carry.size(); n != 0 || b != 0 {
		t.Fatalf("staging did not empty the tier: %d mocks, %d bytes", n, b)
	}
}

// In lax mode the agent promotes out-of-window per-test mocks to the session
// tree; they are reachable there already and must not be held twice.
func TestCarryOverDoesNotDuplicateLaxSessionMocks(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	s := brokerMockAt("s", pulsarKind, "SEND", 2050)
	mm.SetMocksWithWindow([]*models.Mock{s.DeepCopy()}, nil, winStart(2), winEnd(2))
	// W3 in lax mode: s arrives promoted into the session slice.
	mm.SetMocksWithWindow(nil, []*models.Mock{s.DeepCopy()}, winStart(3), winEnd(3))
	if got, _ := mm.GetCarryOverMocks(); len(got) != 0 {
		t.Fatalf("held %v although the session tree serves it", pacedNames(got))
	}
}

// A late server push of a registered kind consumed through MarkMockAsUsed is
// flagged CarryOver; in-window, unregistered and no-window consumes are not.
func TestMarkMockAsUsedFlagsOnlyOutOfWindowRegisteredMocks(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	msg := brokerMockAt("msg", pulsarKind, "MESSAGE", 2050) // registered kind, not a SEND
	httpM := brokerMockAt("http", models.HTTP, "", 2050)
	httpM.TestModeInfo.Lifetime = models.LifetimePerTest
	cfg := brokerMockAt("cfg", pulsarKind, "PRODUCER", 2050)
	cfg.TestModeInfo.Lifetime = models.LifetimeSession

	check := func(step string, m *models.Mock, want bool) {
		t.Helper()
		mm.MarkMockAsUsed(*m)
		sts := mm.GetConsumedMocks()
		if len(sts) != 1 || sts[0].CarryOver != want {
			t.Fatalf("%s: consumed %+v, want CarryOver=%v", step, sts, want)
		}
	}
	check("no window", msg, false)
	mm.SetMocksWithWindow(nil, nil, winStart(2), winEnd(2))
	check("in its own window", msg, false)
	mm.SetMocksWithWindow(nil, nil, winStart(4), winEnd(4))
	check("delivered two windows late", msg, true)
	check("unregistered kind, late", httpM, false)
	check("session mock of the registered kind", cfg, false)
}

// With no kind registered, nothing about the tiers changes: out-of-window
// mocks are dropped, nothing is planned, nothing is held.
func TestCarryOverIsInertWithoutARegistration(t *testing.T) {
	if models.CarryOverRegistered() {
		t.Fatal("precondition: a registration leaked from another test")
	}
	r := newCarryRig(t,
		brokerMockAt("s1", pulsarKind, "SEND", 2050),
		brokerMockAt("s2", pulsarKind, "SEND", 2500),
	)
	r.window(1)
	r.window(2)
	r.window(3)
	if len(r.loads) != 0 {
		t.Fatalf("a lookahead was planned with nothing registered: %v", r.loads)
	}
	if got := r.carryNames(); len(got) != 0 {
		t.Fatalf("held %v with nothing registered", got)
	}
	if r.consume("s1") || r.consume("s2") {
		t.Fatal("an unregistered SEND was consumable after its window, which today it is not")
	}
}

// Registering one kind leaves every other kind exactly as it was: the same
// sequence yields the same tiers and consumed states with and without it.
func TestCarryOverLeavesUnregisteredKindsAlone(t *testing.T) {
	run := func() []string {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(carryWindows())
		rec := []*models.Mock{
			brokerMockAt("boot", models.HTTP, "", 500),
			brokerMockAt("h2", models.HTTP, "", 2050),
			brokerMockAt("h2gap", models.HTTP, "", 2500),
			brokerMockAt("h3", models.HTTP, "", 3050),
			brokerMockAt("pg", models.Postgres, "", 3060),
		}
		for _, m := range rec {
			m.TestModeInfo.Lifetime = models.LifetimePerTest
		}
		cp := func() []*models.Mock {
			out := make([]*models.Mock, 0, len(rec))
			for _, m := range rec {
				out = append(out, m.DeepCopy())
			}
			return out
		}
		var trace []string
		snap := func(step string) {
			f, _ := mm.GetFilteredMocks()
			s, _ := mm.GetStartupMocks()
			u, _ := mm.GetUnFilteredMocks()
			trace = append(trace, fmt.Sprintf("%s f=%v s=%v u=%v dropped=%d", step, pacedNames(f), pacedNames(s), pacedNames(u), mm.DroppedOutOfWindow()))
		}
		mm.SetMocksWithWindow(cp(), nil, models.BaseTime, time.Now())
		snap("staging")
		for i := 1; i <= 4; i++ {
			mm.SetMocksWithWindow(cp(), nil, winStart(i), winEnd(i))
			snap(fmt.Sprintf("W%d", i))
			for _, m := range cp() {
				trace = append(trace, fmt.Sprintf("  del %s=%v", m.Name, mm.DeleteFilteredMock(*m)))
				mm.MarkMockAsUsed(*m)
			}
			for _, st := range mm.GetConsumedMocks() {
				st.Timestamp = 0
				trace = append(trace, fmt.Sprintf("  consumed %+v", st))
			}
		}
		return trace
	}
	without := run()
	registerSends(t)
	with := run()
	if !eqNames(without, with) {
		for i := range without {
			if i >= len(with) || without[i] != with[i] {
				t.Fatalf("first divergence at %d:\n without: %s\n    with: %s", i, without[i], with[min(i, len(with)-1)])
			}
		}
		t.Fatalf("traces differ in length: %d vs %d", len(without), len(with))
	}
}

func TestCarryOverSurvivesTheWorkerScopeWrap(t *testing.T) {
	registerSends(t)
	r := newCarryRig(t,
		brokerMockAt("s1", pulsarKind, "SEND", 1500),
		brokerMockAt("s2", pulsarKind, "SEND", 1600),
		brokerMockAt("s3", pulsarKind, "SEND", 1700),
	)
	r.window(1)
	var db interface{} = workerScoped(t, r.mm, []string{"s1"}, []string{"s1", "s2"})
	cr, ok := db.(integrations.CarryOverReader)
	if !ok {
		t.Fatal("the worker-scope wrap erases CarryOverReader")
	}
	got, _ := cr.GetCarryOverMocksByKind(pulsarKind)
	if !eqNames(pacedNames(got), []string{"s1", "s3"}) {
		t.Fatalf("scoped carry-over = %v, want [s1 s3] (own + unmapped, not another test's s2)", pacedNames(got))
	}
	all, _ := cr.GetCarryOverMocks()
	if !eqNames(pacedNames(all), []string{"s1", "s3"}) {
		t.Fatalf("scoped carry-over (all kinds) = %v", pacedNames(all))
	}
}

// -race: parsers consume from the tier and read it while windows switch.
func TestCarryOverConcurrentConsumeAndFile(t *testing.T) {
	registerSends(t)
	var rec []*models.Mock
	for i := 0; i < 60; i++ {
		rec = append(rec, brokerMockAt(fmt.Sprintf("s%02d", i), pulsarKind, "SEND", 1000+i*70))
	}
	r := newCarryRig(t, rec...)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			held, _ := r.mm.GetCarryOverMocksByKind(pulsarKind)
			for _, m := range held {
				r.mm.DeleteFilteredMock(*m)
			}
			_ = r.mm.HasMocksByKind(pulsarKind, func(*models.Mock) bool { return false })
			_ = r.mm.MarkMockAsUsed(*rec[0])
		}
	}()
	for pass := 0; pass < 3; pass++ {
		for i := 1; i <= 5; i++ {
			r.window(i)
		}
	}
	close(stop)
	<-done
}

// The lookahead load reads only the registered entries of the range, however
// many other mocks the range holds.
func TestDiskMocksLoadCarryOverReadsOnlyRegistered(t *testing.T) {
	registerSends(t)
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, m := range []*models.Mock{
		brokerMockAt("s3", pulsarKind, "SEND", 3000),
		brokerMockAt("h1", models.HTTP, "", 1500),
		brokerMockAt("f1", pulsarKind, "FLOW", 1600),
		brokerMockAt("s1", pulsarKind, "SEND", 1000),
		brokerMockAt("s2", pulsarKind, "SEND", 2000),
	} {
		if m.Kind == models.HTTP {
			m.TestModeInfo.Lifetime = models.LifetimePerTest
		}
		if err := d.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.LoadCarryOver(pms(1000), pms(2000))
	if err != nil {
		t.Fatal(err)
	}
	if !eqNames(pacedNames(got), []string{"s1", "s2"}) {
		t.Fatalf("LoadCarryOver = %v, want [s1 s2] (inclusive, registered only, in order)", pacedNames(got))
	}
	all, _ := d.LoadCarryOver(pms(0), maxRecordedTime)
	if len(all) != 3 {
		t.Fatalf("unbounded LoadCarryOver = %v", pacedNames(all))
	}
	if d.Len() != 5 {
		t.Fatalf("the carry index changed what the store holds: %d", d.Len())
	}
}

// BenchmarkCarryOverSetPass replays one production-sized set through the manager
// the way the agent does — 2,406 windows 268 ms apart, 4,220 SENDs (every 10th
// never consumed) and 10,000 other per-test mocks — with and without the SEND
// registration, to put a number on what the carry-over tier costs per set.
func BenchmarkCarryOverSetPass(b *testing.B) {
	const windows, sends, others = 2406, 4220, 10000
	step := 268 * time.Millisecond
	ws := make([]models.TestWindow, windows)
	for i := range ws {
		s := pace0.Add(time.Duration(i+1) * step)
		ws[i] = models.TestWindow{TestCase: fmt.Sprintf("test-%d", i+1), Start: s, End: s.Add(17 * time.Millisecond)}
	}
	var rec []*models.Mock
	neverPublished := map[string]bool{}
	span := time.Duration(windows) * step
	for i := 0; i < sends; i++ {
		if i%10 == 0 {
			neverPublished[fmt.Sprintf("send-%d", i)] = true
		}
		at := int(time.Duration(i) * span / sends / time.Millisecond)
		m := brokerMockAt(fmt.Sprintf("send-%d", i), pulsarKind, "SEND", 1000+at)
		m.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Type: "binary", Data: string(make([]byte, 2800))}}}}
		rec = append(rec, m)
	}
	for i := 0; i < others; i++ {
		at := int(time.Duration(i) * span / others / time.Millisecond)
		m := brokerMockAt(fmt.Sprintf("other-%d", i), models.HTTP, "", 1000+at)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		rec = append(rec, m)
	}
	sort.Slice(rec, func(i, j int) bool { return rec[i].Spec.ReqTimestampMock.Before(rec[j].Spec.ReqTimestampMock) })

	for _, registered := range []bool{false, true} {
		b.Run(map[bool]string{false: "unregistered", true: "registered"}[registered], func(b *testing.B) {
			if registered {
				defer models.RegisterCarryOver(pulsarKind, func(m *models.Mock) bool { return m.Spec.Metadata["commandType"] == "SEND" })()
			}
			b.ReportAllocs()
			var peakHeld, peakBytes int
			for n := 0; n < b.N; n++ {
				mm := NewMockManager(nil, nil, zap.NewNop())
				mm.ResetForReplaySession()
				mm.SeedStartupCutoff(ws[0].Start)
				mm.SeedRecordedWindows(ws)
				mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
				lo := 0
				for _, w := range ws {
					for lo < len(rec) && rec[lo].Spec.ReqTimestampMock.Before(w.Start) {
						lo++
					}
					var filtered []*models.Mock
					for i := lo; i < len(rec) && !rec[i].Spec.ReqTimestampMock.After(w.End); i++ {
						filtered = append(filtered, rec[i])
					}
					if from, to, ok := mm.CarryOverLoadRange(w.Start); ok {
						i := sort.Search(len(rec), func(k int) bool { return !rec[k].Spec.ReqTimestampMock.Before(from) })
						for ; i < len(rec) && !rec[i].Spec.ReqTimestampMock.After(to); i++ {
							if models.IsCarryOver(rec[i]) {
								filtered = append(filtered, rec[i])
							}
						}
					}
					mm.SetMocksWithWindow(filtered, nil, w.Start, w.End)
					// The app publishes every SEND but every 10th, a window late.
					held, _ := mm.GetCarryOverMocks()
					for _, m := range held {
						if !neverPublished[m.Name] {
							mm.DeleteFilteredMock(*m)
						}
					}
					if c, by := mm.carry.size(); c > peakHeld {
						peakHeld, peakBytes = c, by
					}
				}
				mm.Close()
			}
			b.ReportMetric(float64(peakHeld), "peak-held")
			b.ReportMetric(float64(peakBytes)/(1<<20), "peak-MiB")
		})
	}
}

// A startup-band mock of a registered kind (a server push of the recording's
// startup) that is first consumed after a test fired goes through
// DeleteFilteredMock's startup fallback. It is flagged CarryOver like a late
// consume through MarkMockAsUsed, so the replay keeps it whatever the running
// test's verdict and maps it to the startup section. Consumed before any
// window, or of an unregistered kind, it is not flagged.
func TestDeleteStartupMockFlagsALateRegisteredConsume(t *testing.T) {
	registerSends(t)
	consume := func(t *testing.T, kind models.Kind, window int) models.MockState {
		t.Helper()
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(carryWindows())
		msg := brokerMockAt("startup-msg", kind, "MESSAGE", 500)
		msg.Spec.Metadata["serverPush"] = "true"
		msg.TestModeInfo.Lifetime = models.LifetimePerTest
		mm.SetMocksWithWindow([]*models.Mock{msg.DeepCopy()}, nil, models.BaseTime, time.Now())
		for i := 1; i <= window; i++ {
			// The agent's strict load: the window's own mocks plus the startup band.
			mm.SetMocksWithWindow([]*models.Mock{msg.DeepCopy()}, nil, winStart(i), winEnd(i))
		}
		_ = mm.GetConsumedMocks()
		if !mm.DeleteFilteredMock(*msg.DeepCopy()) {
			t.Fatal("the startup-band mock was not consumable")
		}
		sts := mm.GetConsumedMocks()
		if len(sts) != 1 {
			t.Fatalf("consumed %+v, want one state", sts)
		}
		return sts[0]
	}
	if st := consume(t, pulsarKind, 3); !st.CarryOver || st.Usage != models.Updated {
		t.Fatalf("consumed in test 3: %+v, want CarryOver and Usage Updated", st)
	}
	if st := consume(t, pulsarKind, 0); st.CarryOver {
		t.Fatalf("consumed before the first test: %+v, want no CarryOver", st)
	}
	if st := consume(t, models.HTTP, 3); st.CarryOver {
		t.Fatalf("unregistered kind: %+v, want no CarryOver", st)
	}
}

// Carry-over needs the recorded windows the replayer seeds at staging: without
// them there is no release rule and no lookahead, and a registered kind's
// unconsumed per-test mock of a closed window is dropped when the next window
// opens, exactly as for an unregistered kind (an older CLI, a windowed replay
// without RecordedWindows).
func TestCarryOverNeedsSeededWindows(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	s1 := brokerMockAt("s1", pulsarKind, "SEND", 1050)
	mm.SetMocksWithWindow([]*models.Mock{s1.DeepCopy()}, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{s1.DeepCopy()}, nil, winStart(1), winEnd(1))
	mm.SetMocksWithWindow(nil, nil, winStart(2), winEnd(2))
	if carry, _ := mm.GetCarryOverMocks(); len(carry) != 0 {
		t.Fatalf("held %v with no seeded windows", pacedNames(carry))
	}
	if mm.DeleteFilteredMock(*s1.DeepCopy()) {
		t.Fatal("test 1's unconsumed SEND is consumable in test 2 with no seeded windows; before carry-over it was dropped")
	}
	if _, _, ok := mm.CarryOverLoadRange(winStart(2)); ok {
		t.Fatal("a lookahead range was planned with no seeded windows")
	}
}

// A connection failure belongs to the test window it was recorded in. Even a
// carry-over registration for its kind cannot load it ahead of that window or
// keep it past it: either way it would fail a connect in a test that never
// saw the failure.
func TestConnectionFailuresNeverEnterTheCarryOverTier(t *testing.T) {
	registerSends(t)
	t.Cleanup(models.RegisterCarryOver(models.ConnectionFailure, func(*models.Mock) bool { return true }))
	r := newCarryRig(t,
		brokerMockAt("cf2", models.ConnectionFailure, "", 2050),   // in W2
		brokerMockAt("s2", pulsarKind, "SEND", 2060),              // in W2, the control
		brokerMockAt("cfgap", models.ConnectionFailure, "", 2500), // in the gap after W2
	)
	carried := func() []string {
		ms, _ := r.mm.GetCarryOverMocks()
		return pacedNames(ms)
	}

	r.window(1)
	if got := carried(); !eqNames(got, []string{"s2"}) {
		t.Fatalf("W1: carry-over = %v; want only the SEND, loaded a second ahead", got)
	}
	r.window(2)
	if got := r.perTestNames(); !eqNames(got, []string{"cf2", "s2"}) {
		t.Fatalf("W2: per-test = %v; a connection failure is served in its own window", got)
	}
	r.window(3)
	if got := carried(); !eqNames(got, []string{"s2"}) {
		t.Fatalf("W3: carry-over = %v; W2 closed with both unconsumed and only the SEND may be carried", got)
	}
	for _, n := range []string{"cf2", "cfgap"} {
		if r.consume(n) {
			t.Fatalf("W3: %s was consumable outside its own window", n)
		}
	}
}

// A StagingRelease need marks a consume outside the current window CarryOver
// only for the mocks its Match takes; its kind's other mocks are untouched,
// unlike a RegisterCarryOver kind's.
func TestAReleaseNeedMarksOnlyWhatItsMatchTakes(t *testing.T) {
	t.Cleanup(models.RegisterStagingNeed(models.HTTP, models.StagingNeed{Use: models.StagingRelease, Match: func(m *models.Mock) bool {
		return m.Spec.Metadata["poll"] == "1"
	}}))
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	poll := brokerMockAt("poll", models.HTTP, "", 2050)
	poll.Spec.Metadata["poll"] = "1"
	poll.TestModeInfo.Lifetime = models.LifetimePerTest
	plain := brokerMockAt("plain", models.HTTP, "", 2050)
	plain.TestModeInfo.Lifetime = models.LifetimePerTest
	mm.SetMocksWithWindow(nil, nil, winStart(4), winEnd(4))
	for _, c := range []struct {
		m    *models.Mock
		want bool
	}{{poll, true}, {plain, false}} {
		mm.MarkMockAsUsed(*c.m)
		sts := mm.GetConsumedMocks()
		if len(sts) != 1 || sts[0].CarryOver != c.want {
			t.Fatalf("%s consumed two windows late: %+v, want CarryOver=%v", c.m.Name, sts, c.want)
		}
	}
}

// At a set's staging, before its app starts, the planner loads what the app's
// startup reaches: the registered mocks the release rule makes reachable
// before every window, those recorded up to CarryOverLookahead after the
// set's first recorded window began (that end excluded), from the startup
// cutoff on. It reads the set's cutoff and windows as seeded for it before its
// staging installs them, and as installed for a set staged again. A cutoff
// earlier than the first window (a recorded test that opens no window) moves
// the range's start, not its end. With no windows, nothing.
func TestCarryOverLoadRangeAtStagingIsWhatTheStartupReaches(t *testing.T) {
	registerSends(t)
	end := winStart(1).Add(models.CarryOverLookahead - time.Nanosecond)
	want := func(mm *MockManager, step string, from time.Time) {
		t.Helper()
		gotFrom, gotTo, ok := mm.CarryOverLoadRange(models.BaseTime)
		if !ok || !gotFrom.Equal(from) || !gotTo.Equal(end) {
			t.Fatalf("%s: range [%v, %v] ok=%v; want [%v, %v]", step, gotFrom, gotTo, ok, from, end)
		}
	}
	for _, cutoff := range []time.Time{winStart(1), winStart(1).Add(-500 * time.Millisecond)} {
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(cutoff)
		mm.SeedRecordedWindows(carryWindows())
		want(mm, "seeded, not yet staged", cutoff)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		want(mm, "staged again", cutoff)
		mm.Close()
	}

	bare := NewMockManager(nil, nil, zap.NewNop())
	defer bare.Close()
	bare.ResetForReplaySession()
	bare.SeedStartupCutoff(winStart(1))
	if _, _, ok := bare.CarryOverLoadRange(models.BaseTime); ok {
		t.Fatal("a set with no recorded windows planned a staging load")
	}
}

// A carry-over mock served from the startup tier is consumed once: Deleted,
// so the next load filters it out, and CarryOver, so the replay attributes it
// by its own window and does not re-arm it. Another startup mock of the kind
// is consumed as before: Updated, and staged again.
func TestACarryOverMockServedFromTheStartupTierIsConsumedOnce(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(carryWindows())
	send := brokerMockAt("boot-send", pulsarKind, "SEND", 500)
	msg := brokerMockAt("boot-msg", pulsarKind, "MESSAGE", 600)
	mm.SetMocksWithWindow([]*models.Mock{send.DeepCopy(), msg.DeepCopy()}, nil, models.BaseTime, time.Now())
	for _, c := range []struct {
		m       *models.Mock
		usage   models.MockUsage
		carried bool
	}{{send, models.Deleted, true}, {msg, models.Updated, false}} {
		if !mm.DeleteFilteredMock(*c.m) {
			t.Fatalf("%s was not consumed from the startup tier", c.m.Name)
		}
		st := mm.GetPersistentConsumed()[c.m.Name]
		if st.Usage != c.usage || st.CarryOver != c.carried {
			t.Fatalf("%s consumed as %+v; want usage %v, CarryOver %v", c.m.Name, st, c.usage, c.carried)
		}
	}
}

// A carry-over mock consumed before the set's first test is consumed once,
// though the replay drains that consume only after the next staging: test-1's
// staging reloads the startup band and the carry range from the cutoff,
// handed it again, and leaves it out of every tier. One consumed after the
// first test is the replay's to filter (and to re-arm for a retry cycle). A
// set staged again keeps them out; the next set's fenced boundary forgets
// them.
func TestACarryOverMockConsumedAtStartupIsNotStagedAgain(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	boot, crossed := brokerMockAt("boot-send", pulsarKind, "SEND", 500), brokerMockAt("crossed-send", pulsarKind, "SEND", 1050)
	stage := func() {
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(carryWindows())
		mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), crossed.DeepCopy()}, nil, models.BaseTime, time.Now())
	}
	held := func(step string, want ...string) {
		t.Helper()
		var got []string
		startup, _ := mm.GetStartupMocks()
		perTest, _ := mm.GetFilteredMocks()
		carry, _ := mm.GetCarryOverMocks()
		for _, list := range [][]*models.Mock{startup, perTest, carry} {
			for _, mk := range list {
				got = append(got, mk.Name)
			}
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("%s: the tiers hold %v; want %v", step, got, want)
		}
	}
	stage()
	for _, mk := range []*models.Mock{boot, crossed} {
		if !mm.DeleteFilteredMock(*mk) {
			t.Fatalf("%s was not served as the app started", mk.Name)
		}
	}
	// Test-1, staged before the replay drained them: handed both again.
	mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), crossed.DeepCopy()}, nil, winStart(1), winEnd(1))
	held("test-1")
	// A set staged again (a re-registration) keeps them out.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), crossed.DeepCopy()}, nil, models.BaseTime, time.Now())
	held("the set staged again")
	// The next set's fenced boundary forgets them: its own recording is
	// staged whole.
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), crossed.DeepCopy()}, nil, models.BaseTime, time.Now())
	held("the next set", "boot-send", "crossed-send")
}

// A set boundary that starts another test set (ArmSetFence) drops what the
// previous set's pool was consumed of and nobody drained: mock names are a
// set's own, so a drain handing them to the next set marks its unrelated
// same-named mocks consumed. A re-registration of the current set (no fence)
// keeps them: what the app consumed as it started is the set's.
func TestAFencedSetBoundaryDropsThePreviousSetsUndrainedConsumes(t *testing.T) {
	registerSends(t)
	for _, fenced := range []bool{true, false} {
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(carryWindows())
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow(nil, nil, winStart(5), winEnd(5))
		// The set's app publishes once more after the replay's last drain.
		late := brokerMockAt("mock-7", pulsarKind, "MESSAGE", 1500)
		late.TestModeInfo.Lifetime = models.LifetimePerTest
		mm.MarkMockAsUsed(*late)
		mm.ResetForReplaySession()
		if fenced {
			mm.ArmSetFence()
		}
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		got := mm.GetConsumedMocks()
		if fenced && len(got) != 0 {
			t.Fatalf("the next set's drain was handed the previous set's %+v", got)
		}
		if !fenced && (len(got) != 1 || got[0].Name != "mock-7") {
			t.Fatalf("a re-registration's drain lost the set's own consume: %+v", got)
		}
		mm.Close()
	}
}

// The replay says which session starts a test set (OutgoingOptions.StartsTestSet,
// a set boundary) and which re-registers the current one; Proxy.Mock fences
// only the first.
func TestProxyMockFencesOnlyASessionThatStartsATestSet(t *testing.T) {
	registerSends(t)
	ctx := context.Background()
	for _, starts := range []bool{true, false} {
		p := testProxy(t)
		if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: true}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		late := brokerMockAt("mock-7", pulsarKind, "MESSAGE", 1500)
		late.TestModeInfo.Lifetime = models.LifetimePerTest
		mm.MarkMockAsUsed(*late)
		if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: starts}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := mm.GetConsumedMocks()
		if starts && len(got) != 0 {
			t.Fatalf("a session that starts a test set handed its drain the previous set's %+v", got)
		}
		if !starts && len(got) != 1 {
			t.Fatalf("a re-registration's drain lost the set's own consume: %+v", got)
		}
	}
}

// With nothing registered, as in every OSS replay, the fence keeps a set's
// leftovers out of the next set's drain all the same. Before it, a reusable
// mock the app consumed after the set's last drain (a background job's query
// against a mocked database, a scheduler's call) came back in the next set's
// startup drain under the previous set's name. The replay folded it into that
// set's consumed history and wrote it into that set's startup section, where it
// names the next set's unrelated same-named mock: --remove-unused-mocks kept
// that mock, every test's mock assertion left it out (dropStartupMockInfos),
// and the agent's filter stamped the previous set's ordering on it.
func TestWithNothingRegisteredANewSetsDrainHoldsNoneOfThePreviousSets(t *testing.T) {
	if models.CarryOverRegistered() {
		t.Fatal("a registration leaked into this test")
	}
	ctx := context.Background()
	for _, starts := range []bool{true, false} {
		p := testProxy(t)
		if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: true}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		cfg := &models.Mock{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"}, ReqTimestampMock: pms(500)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true}}
		mm.SetMocksWithWindow(nil, []*models.Mock{cfg}, models.BaseTime, time.Now())
		mm.SetMocksWithWindow(nil, []*models.Mock{cfg}, winStart(1), winEnd(1))
		_ = mm.GetConsumedMocks() // the set's last drain
		mm.MarkMockAsUsed(*cfg)   // a background job's query after it
		if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: starts}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := mm.GetConsumedMocks()
		if starts && len(got) != 0 {
			t.Fatalf("the next set's drain was handed the previous set's %+v", got)
		}
		if !starts && (len(got) != 1 || got[0].Name != "mock-3") {
			t.Fatalf("a re-registration's drain lost the set's own consume: %+v", got)
		}
	}
}

// pooledCopy is the copy a parser holds of the pooled mock named name in
// mocks, as served.
func pooledCopy(t *testing.T, mocks []*models.Mock, name string) models.Mock {
	t.Helper()
	for _, mk := range mocks {
		if mk.Name == name {
			return *mk.DeepCopy()
		}
	}
	t.Fatalf("%s is not pooled", name)
	return models.Mock{}
}

// A consume of a mock the previous set pooled, made once the next set's
// fenced boundary staging moved the set generation (a consume racing it, or a
// parser still holding the previous pool), is refused at every door: it is
// not this set's, and a door that matches by name would take this set's
// same-named mock for it. Nothing reaches this set's drain, and this set's
// own mocks are consumed as before.
func TestAConsumeOfAMockThePreviousSetPooledIsRefused(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	set := func(sendAt int) (send, cfg *models.Mock) {
		send = brokerMockAt("mock-7", pulsarKind, "SEND", sendAt)
		cfg = &models.Mock{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"}, ReqTimestampMock: pms(sendAt + 1)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true}}
		return send, cfg
	}
	sendN, cfgN := set(500)
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow([]*models.Mock{sendN}, []*models.Mock{cfgN}, models.BaseTime, time.Now())
	startup, _ := mm.GetStartupMocks()
	session, _ := mm.GetUnFilteredMocks()
	oldSend, oldCfg := pooledCopy(t, startup, "mock-7"), pooledCopy(t, session, "mock-3")

	sendN1, cfgN1 := set(700)
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SetMocksWithWindow([]*models.Mock{sendN1}, []*models.Mock{cfgN1}, models.BaseTime, time.Now())
	oldCfgCopy := oldCfg
	if mm.DeleteFilteredMock(oldSend) || mm.MarkMockAsUsed(oldCfg) || mm.DeleteUnFilteredMock(oldCfg) ||
		mm.UpdateUnFilteredMock(&oldCfgCopy, &oldCfg) {
		t.Fatal("a consume of the previous set's pool was taken")
	}
	if got := mm.GetConsumedMocks(); len(got) != 0 {
		t.Fatalf("the next set's drain holds the previous set's %+v", got)
	}
	startup, _ = mm.GetStartupMocks()
	newSend := pooledCopy(t, startup, "mock-7")
	if !newSend.Spec.ReqTimestampMock.Equal(pms(700)) {
		t.Fatal("the previous set's consume took this set's same-named mock")
	}
	if !mm.DeleteFilteredMock(newSend) {
		t.Fatal("this set's own mock was refused")
	}
}

// The fence moves the set generation with its drop, under the consumed
// list's lock: a flag of a mock pooled under the previous generation is
// refused, never recorded; one of this set's, or of a mock no staging pooled,
// is recorded.
func TestAFlagOfAMockPooledForAnEarlierSetIsRefused(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	prev := mm.setGen.Load()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if err := mm.flagMockAsUsed(models.MockState{Name: "mock-1", Usage: models.Deleted}, prev); !errors.Is(err, errPreviousSet) {
		t.Fatalf("a flag of the previous set's mock: %v, want errPreviousSet", err)
	}
	for _, g := range []uint64{mm.setGen.Load(), 0} {
		if err := mm.flagMockAsUsed(models.MockState{Name: fmt.Sprint("mock-", g), Usage: models.Deleted}, g); err != nil {
			t.Fatalf("a flag at generation %d: %v", g, err)
		}
	}
	if got := mm.GetConsumedMocks(); len(got) != 2 {
		t.Fatalf("consumed %+v; want this set's and the unpooled one", got)
	}
}

// A carry-over mock the app consumed as it started is consumed once: one it
// consumed from the startup tier it read before test-1's staging (which
// filed a copy into test-1's window meanwhile) is refused when test-1 is
// served it again, and a retry cycle does not stage it. A test's own
// carry-over mock is the replay's to re-arm: a retry cycle consumes it again.
func TestACarryOverMockIsConsumedOncePerPass(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	boot := brokerMockAt("boot-send", pulsarKind, "SEND", 1050) // in test-1's window
	own := brokerMockAt("own-send", pulsarKind, "SEND", 1060)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), own.DeepCopy()}, nil, models.BaseTime, time.Now())
	mm.treesMu.RLock()
	bootTier := mm.startup
	mm.treesMu.RUnlock()
	startup, _ := mm.GetStartupMocks()
	served := pooledCopy(t, startup, "boot-send")

	// Test-1 is staged with both before the app's consume lands.
	test1 := func() {
		mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy(), own.DeepCopy()}, nil, winStart(1), winEnd(1))
	}
	test1()
	if !mm.deleteStartupMockFrom(bootTier, served) {
		t.Fatal("the app's startup consume, of the tier it read, was refused")
	}
	perTest, _ := mm.GetFilteredMocks()
	if mm.DeleteFilteredMock(pooledCopy(t, perTest, "boot-send")) {
		t.Fatal("test-1 was served the boot SEND again")
	}
	if !mm.DeleteFilteredMock(pooledCopy(t, perTest, "own-send")) {
		t.Fatal("test-1's own SEND was refused")
	}
	// Test-2, then test-1 again: a retry cycle.
	mm.SetMocksWithWindow(nil, nil, winStart(2), winEnd(2))
	test1()
	perTest, _ = mm.GetFilteredMocks()
	for _, mk := range perTest {
		if mk.Name == "boot-send" {
			t.Fatal("the retry cycle staged the boot SEND, consumed as the app started")
		}
	}
	if !mm.DeleteFilteredMock(pooledCopy(t, perTest, "own-send")) {
		t.Fatal("the retry cycle refused test-1's own SEND, which its replay re-armed")
	}
	if got := mm.GetConsumedMocks(); len(got) != 2 {
		t.Fatalf("consumed %+v; want boot-send once and own-send", got)
	}
}

// The replay re-arms what a test consumed, and the agent serves it again: a
// retry cycle that starts with the window the last cycle ended on (a one-test
// set, or only the last test passed), and keploy mock replay's test run twice
// in a session (a scope opened again), each consume a test's carry-over mock
// again. A startup tier staged between scope tests is not the app's startup:
// what it serves is not kept from later tests.
func TestARearmedCarryOverMockIsServedAgain(t *testing.T) {
	registerSends(t)
	t.Run("a retry cycle starting where the last ended", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		own := brokerMockAt("own-send", pulsarKind, "SEND", 1060)
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(carryWindows())
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		for cycle := 1; cycle <= 2; cycle++ {
			mm.SetMocksWithWindow([]*models.Mock{own.DeepCopy()}, nil, winStart(1), winEnd(1))
			perTest, _ := mm.GetFilteredMocks()
			if !mm.DeleteFilteredMock(pooledCopy(t, perTest, "own-send")) {
				t.Fatalf("cycle %d refused the test's own SEND", cycle)
			}
		}
	})
	t.Run("a scope test run twice", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		send := brokerMockAt("own-send", pulsarKind, "SEND", 1060)
		pool := brokerMockAt("pool-send", pulsarKind, "SEND", 900)
		mm.ResetForReplaySession()
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		for run := 1; run <= 2; run++ {
			mm.SetScopeMocks([]*models.Mock{send.DeepCopy()}, nil, true)
			perTest, _ := mm.GetFilteredMocks()
			if !mm.DeleteFilteredMock(pooledCopy(t, perTest, "own-send")) {
				t.Fatalf("run %d refused the test's own SEND", run)
			}
			// Between tests: the pool, from the startup tier.
			mm.SetScopeMocks([]*models.Mock{pool.DeepCopy()}, nil, false)
			startup, _ := mm.GetStartupMocks()
			if !mm.DeleteFilteredMock(pooledCopy(t, startup, "pool-send")) {
				t.Fatalf("run %d: between tests the pool's SEND was refused", run)
			}
		}
	})
}

// RestartExactKey serves a retried package's pool afresh: a carry-over mock of
// it the app consumed as it started is the package's to consume again.
func TestRestartExactKeyForgetsTheStartupConsumesOfItsPool(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	boot := brokerMockAt("boot-send", pulsarKind, "SEND", 500)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SetMocksWithWindow([]*models.Mock{boot.DeepCopy()}, nil, models.BaseTime, time.Now())
	startup, _ := mm.GetStartupMocks()
	served := pooledCopy(t, startup, "boot-send")
	if !mm.DeleteFilteredMock(served) {
		t.Fatal("the app's startup consume was refused")
	}
	if !mm.bootConsumed(&served) {
		t.Fatal("the startup consume was not recorded")
	}
	mm.RestartExactKey("pkg-a", []string{"boot-send"})
	if mm.bootConsumed(&served) {
		t.Fatal("the retried package's pool kept its startup consume")
	}
}
