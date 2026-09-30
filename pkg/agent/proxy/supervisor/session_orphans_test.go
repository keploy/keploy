package supervisor

import (
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// A caller that keeps its spans elsewhere (a DaemonSet agent, per pod) gets a
// parser's spans there, not on the manager its mocks go to.
func TestSessionOrphansTakesTheSpans(t *testing.T) {
	t.Parallel()
	mgr := syncMock.New(nil)
	spans := &syncMock.Spans{}
	s := &Session{Mgr: mgr, Orphans: spans}

	base := time.Now().Add(-time.Minute)
	s.RecordOrphanWindow(base, base.Add(time.Second))
	closeIt := s.OpenOrphanWindow(base.Add(10 * time.Second))
	closeIt()

	if c, o := spans.Counts(); c != 2 || o != 0 {
		t.Fatalf("Orphans holds (closed=%d, open=%d), want (2, 0)", c, o)
	}
	if _, c, o := mgr.OrphanRangeCount(); c != 0 || o != 0 {
		t.Fatalf("the manager got spans (closed=%d, open=%d) that belong to Orphans", c, o)
	}

	// Unset, the manager takes them, as before.
	s2 := &Session{Mgr: mgr}
	s2.RecordOrphanWindow(base, base.Add(time.Second))
	if _, c, _ := mgr.OrphanRangeCount(); c != 1 {
		t.Fatalf("without Orphans the manager holds %d spans, want 1", c)
	}
}
