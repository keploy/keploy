package manager

import (
	"testing"

	"go.uber.org/zap"
)

// Every connection the proxy could not name the caller of is counted on the
// manager, for the session's summary and end marker; a nil manager counts
// nothing and does not panic.
func TestEveryConnectionFromAnUnnamedCallerIsCounted(t *testing.T) {
	m := New(zap.NewNop())
	for i := 0; i < 3; i++ {
		m.NoteUnnamedCallerConn()
	}
	if got := m.UnnamedCallerConns(); got != 3 {
		t.Fatalf("the manager counted %d connections from unnamed callers, want 3", got)
	}
	var none *SyncMockManager
	none.NoteUnnamedCallerConn()
	if got := none.UnnamedCallerConns(); got != 0 {
		t.Fatalf("a nil manager reports %d, want 0", got)
	}
}
