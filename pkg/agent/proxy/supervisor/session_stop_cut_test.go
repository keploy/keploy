package supervisor

import (
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// A span the recording's stop cut leaves out goes to the session's manager as
// a stop cut's and an orphan span (SyncMockManager.RecordStopCutWindow), so
// routes/record.go names the test cases over it. A session whose caller keeps
// its own spans (Orphans) gives it to them alone, as RecordOrphanWindow does,
// and nothing to the manager; a nil session takes it as a no-op.
func TestSession_RecordStopCutWindowRoutesAsRecordOrphanWindowDoes(t *testing.T) {
	t.Parallel()
	from := time.Unix(1_700_000_000, 0)
	to := from.Add(time.Second)

	mgr := syncMock.New(nil)
	(&Session{Mgr: mgr}).RecordStopCutWindow(from, to)
	if !mgr.WasStopCutInWindow(from, to) {
		t.Error("the session's manager holds no stop cut's span over what the stop cut left out")
	}
	if over, _ := mgr.WasMockOrphanedInWindow(from, to); !over {
		t.Error("the session's manager holds no orphan span over what the stop cut left out")
	}

	own := &spanLog{}
	other := syncMock.New(nil)
	(&Session{Mgr: other, Orphans: own}).RecordStopCutWindow(from, to)
	if got := own.all(); len(got) != 1 || !got[0][0].Equal(from) || !got[0][1].Equal(to) {
		t.Errorf("the caller's own spans got %v, want the one span [%v, %v]", got, from, to)
	}
	if over, _ := other.WasMockOrphanedInWindow(from, to); over || other.WasStopCutInWindow(from, to) {
		t.Error("a session whose caller keeps its own spans gave the stop cut's span to its manager too")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a nil session's RecordStopCutWindow does not no-op: %v", r)
		}
	}()
	var none *Session
	none.RecordStopCutWindow(from, to)
}
