package manager

import (
	"testing"

	"go.uber.org/zap"
)

// Every call the proxy could not name the caller of is counted on the manager,
// for the session's summary and end marker; a nil manager counts nothing and
// does not panic.
func TestEveryCallFromAnUnnamedCallerIsCounted(t *testing.T) {
	m := New(zap.NewNop())
	for i := 0; i < 3; i++ {
		m.NoteUnnamedCaller()
	}
	if got := m.UnnamedCallers(); got != 3 {
		t.Fatalf("the manager counted %d calls from unnamed callers, want 3", got)
	}
	var none *SyncMockManager
	none.NoteUnnamedCaller()
	if got := none.UnnamedCallers(); got != 0 {
		t.Fatalf("a nil manager reports %d, want 0", got)
	}
}
