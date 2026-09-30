package proxy

// Tests for the MockManager hooks a server-push protocol paces its deliveries
// with: the recorded windows of the whole set (O1), the window-change signal and
// staging epoch (O2), and the carry-over tier (C1).

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// Consumers reach it by type assertion on the MockMemDb they are handed, which
// in a scoped worker is the wrapper, not the manager.
func TestRecordedWindowsSurvivesTheWorkerScopeWrap(t *testing.T) {
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
