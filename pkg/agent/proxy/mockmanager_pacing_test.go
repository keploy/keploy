package proxy

// Tests for the MockManager hooks a server-push protocol paces its deliveries
// with: the recorded windows of the whole set (O1), the window-change signal and
// staging epoch (O2), and the carry-over tier (C1).

import (
	"fmt"
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
	var db interface{} = &scopedMockDb{
		MockMemDb: r.mm,
		allow:     map[string]struct{}{"s1": {}},
		universe:  map[string]struct{}{"s1": {}, "s2": {}},
	}
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
