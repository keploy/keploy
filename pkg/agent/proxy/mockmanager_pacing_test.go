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
