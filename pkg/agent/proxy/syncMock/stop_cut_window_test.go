package manager

import (
	"testing"
	"time"
)

// A span of requests a parser never read because the recording's stop cut it
// short (RecordStopCutWindow) is an orphan span, which leaves out the test
// cases over it, and a stop cut's, by which routes/record.go names them. An
// orphan span of any other kind is no stop cut's.
func TestRecordStopCutWindow_IsAnOrphanSpanAndAStopCuts(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Now()
	m.RecordOrphanWindow(base, base.Add(time.Second))
	cutFrom, cutTo := base.Add(10*time.Second), base.Add(11*time.Second)
	m.RecordStopCutWindow(cutFrom, cutTo)

	if over, _ := m.WasMockOrphanedInWindow(cutFrom.Add(500*time.Millisecond), cutFrom.Add(600*time.Millisecond)); !over {
		t.Error("a test case over the stop cut's span is not left out: no orphan span covers it")
	}
	if !m.WasStopCutInWindow(cutFrom.Add(500*time.Millisecond), cutFrom.Add(600*time.Millisecond)) {
		t.Error("a test case over the stop cut's span is not read as the stop cut's")
	}
	if m.WasStopCutInWindow(base, base.Add(500*time.Millisecond)) {
		t.Error("a test case over an orphan span of another kind is read as the stop cut's")
	}
	if m.WasStopCutInWindow(cutTo.Add(time.Millisecond), cutTo.Add(time.Second)) {
		t.Error("a test case after the stop cut's span is read as the stop cut's")
	}
}

// A nil manager records nothing and reads no stop cut, as its other span
// methods do.
func TestRecordStopCutWindow_OnANilManagerIsANoOp(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a nil manager's stop cut methods do not no-op: %v", r)
		}
	}()
	var m *SyncMockManager
	now := time.Now()
	m.RecordStopCutWindow(now, now.Add(time.Second))
	if m.WasStopCutInWindow(now, now.Add(time.Second)) {
		t.Error("a nil manager reads a stop cut")
	}
}
