package manager

import (
	"testing"

	"go.uber.org/zap"
)

// Every TLS connection the ingress relayed to the app unread is counted on
// the manager, for the session's summary and end marker; a nil manager counts
// nothing and does not panic.
func TestEveryTLSConnectionRelayedUnreadIsCounted(t *testing.T) {
	m := New(zap.NewNop())
	for i := 0; i < 3; i++ {
		m.NoteTLSRelayedConn()
	}
	if got := m.TLSRelayedConns(); got != 3 {
		t.Fatalf("the manager counted %d TLS connections relayed unread, want 3", got)
	}
	if got := m.UnnamedCallerConns(); got != 0 {
		t.Fatalf("the TLS relays were counted as %d connections from unnamed callers, want 0", got)
	}
	var none *SyncMockManager
	none.NoteTLSRelayedConn()
	if got := none.TLSRelayedConns(); got != 0 {
		t.Fatalf("a nil manager reports %d, want 0", got)
	}
}
