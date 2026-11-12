package manager

import (
	"sync"
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

// The ingress relays each TLS connection on its own goroutine, and a client
// opens several at once while a recording's stream may open: notes made at
// once, and a session start among them, are safe together (the race detector
// decides: the workers have no order between them but the manager's lock).
// Every connection is counted, each port has exactly one first between two
// session starts, and the start returns a count it saw.
func TestTLSRelaysNotedAtOnceAndASessionStartAreSafeAndEachCounted(t *testing.T) {
	m := New(zap.NewNop())
	const n, ports = 64, 4
	note := func(firsts map[uint16]int, mu *sync.Mutex) (start chan struct{}, wg *sync.WaitGroup) {
		start, wg = make(chan struct{}), &sync.WaitGroup{}
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(port uint16) {
				defer wg.Done()
				<-start
				if m.NoteTLSRelayedConn(port) && firsts != nil {
					mu.Lock()
					firsts[port]++
					mu.Unlock()
				}
			}(uint16(8000 + i%ports))
		}
		return start, wg
	}

	var mu sync.Mutex
	firsts := map[uint16]int{}
	start, wg := note(firsts, &mu)
	close(start)
	wg.Wait()
	if got := m.TLSRelayedConns(); got != n {
		t.Fatalf("the manager counted %d of %d TLS connections noted at once", got, n)
	}
	if len(firsts) != ports {
		t.Fatalf("%d of the %d ports had a first among TLS connections noted at once: %v", len(firsts), ports, firsts)
	}
	for p, c := range firsts {
		if c != 1 {
			t.Fatalf("port %d had %d firsts among TLS connections noted at once, want 1", p, c)
		}
	}

	// A recording's stream opens among them.
	start, wg = note(nil, nil)
	var before uint64
	wg.Add(1)
	go func() { defer wg.Done(); <-start; before = m.StartTLSRelayedSession() }()
	close(start)
	wg.Wait()
	if got := m.TLSRelayedConns(); got != 2*n {
		t.Fatalf("the manager counted %d TLS connections, want %d: some noted as a session started were lost", got, 2*n)
	}
	if before < n || before > 2*n {
		t.Fatalf("the session started at %d, want a count it saw, from %d to %d", before, n, 2*n)
	}
}
