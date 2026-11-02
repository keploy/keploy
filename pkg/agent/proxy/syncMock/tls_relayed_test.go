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
		m.NoteTLSRelayedConn(8443)
	}
	if got := m.TLSRelayedConns(); got != 3 {
		t.Fatalf("the manager counted %d TLS connections relayed unread, want 3", got)
	}
	if got := m.UnnamedCallerConns(); got != 0 {
		t.Fatalf("the TLS relays were counted as %d connections from unnamed callers, want 0", got)
	}
	var none *SyncMockManager
	if !none.NoteTLSRelayedConn(8443) {
		t.Fatal("a nil manager does not call a TLS relay the first to its port, so it would be said only at Debug")
	}
	if got := none.TLSRelayedConns(); got != 0 {
		t.Fatalf("a nil manager reports %d, want 0", got)
	}
	if got := none.StartTLSRelayedSession(); got != 0 {
		t.Fatalf("a nil manager starts a session at %d, want 0", got)
	}
}

// A recording session's first TLS connection to each app port is its first,
// whatever an earlier session on the same manager relayed: the ingress says
// it at WARN as it comes. The session starts at the count so far.
func TestEachSessionsFirstTLSConnectionToAPortIsItsFirst(t *testing.T) {
	m := New(zap.NewNop())
	type note struct {
		port  uint16
		first bool
	}
	sessions := [][]note{
		{{8443, true}, {8443, false}, {9443, true}, {8443, false}},
		{{8443, true}, {9443, true}, {9443, false}},
	}
	var total uint64
	for s, notes := range sessions {
		if before := m.StartTLSRelayedSession(); before != total {
			t.Fatalf("session %d started at %d, want the %d counted before it", s+1, before, total)
		}
		for i, n := range notes {
			if got := m.NoteTLSRelayedConn(n.port); got != n.first {
				t.Fatalf("session %d, connection %d to %d: first=%v, want %v", s+1, i+1, n.port, got, n.first)
			}
			total++
		}
		if got := m.TLSRelayedConns(); got != total {
			t.Fatalf("after session %d the manager counted %d, want %d", s+1, got, total)
		}
	}
}
