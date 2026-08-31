package models

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestTLSHandshakeStore_LastSurvivesConsumption pins the last-greeting cache
// contract that the MySQL post-TLS stitch relies on when a connection's own
// raw-leg entry is lost (dropped capture event): Last must keep returning the
// most recent pushed entry for a key even after the consumable queue was
// drained by other connections, and a newer Push must overwrite it.
func TestTLSHandshakeStore_LastSurvivesConsumption(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeStoreKey("", 3306)

	if _, ok := s.Last(key); ok {
		t.Fatal("Last on an empty store must miss")
	}

	entry1 := TLSHandshakeEntry{
		RespPackets:  [][]byte{[]byte("greeting-1")},
		ReqPackets:   [][]byte{[]byte("sslreq-1")},
		ReqTimestamp: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	}
	s.Push(key, entry1)

	// Consume the queue the way another connection's PopWait would.
	if _, ok := s.PopWait(key, 0); !ok {
		t.Fatal("PopWait must return the pushed entry")
	}
	if _, ok := s.PopWait(key, 0); ok {
		t.Fatal("queue must be empty after the pop")
	}

	// The last-greeting cache must still serve entry1.
	got, ok := s.Last(key)
	if !ok {
		t.Fatal("Last must survive queue consumption — it is the lost-raw-leg fallback")
	}
	if len(got.RespPackets) != 1 || !bytes.Equal(got.RespPackets[0], []byte("greeting-1")) {
		t.Fatalf("Last returned wrong entry: %+v", got)
	}

	// A newer push overwrites the cache.
	entry2 := TLSHandshakeEntry{RespPackets: [][]byte{[]byte("greeting-2")}}
	s.Push(key, entry2)
	got, ok = s.Last(key)
	if !ok || !bytes.Equal(got.RespPackets[0], []byte("greeting-2")) {
		t.Fatalf("Last must return the most recent push, got %+v ok=%v", got, ok)
	}

	// Keys are independent.
	if _, ok := s.Last(HandshakeStoreKey("", 5432)); ok {
		t.Fatal("Last must be keyed — a different port must miss")
	}
}

// TestTLSHandshakeStore_LastOnlyCachesPortKeys pins the leak guard (finding A):
// `last` is never TTL/size-pruned, so it must only cache low-cardinality
// PORT-scoped keys. A conn-scoped key must NOT populate `last` (it would
// accumulate one unremovable entry per connection over a long session) — while
// still flowing normally through the consumable queue m.
func TestTLSHandshakeStore_LastOnlyCachesPortKeys(t *testing.T) {
	s := NewTLSHandshakeStore()
	connKey := HandshakeStoreKey("srcport:55123", 3306) // -> "conn:srcport:55123"
	if !strings.HasPrefix(connKey, "conn:") {
		t.Fatalf("precondition: expected a conn-scoped key, got %q", connKey)
	}

	entry := TLSHandshakeEntry{RespPackets: [][]byte{[]byte("greeting")}}
	s.Push(connKey, entry)

	// The conn-scoped push must be popable from the queue (unchanged behavior).
	if _, ok := s.PopWait(connKey, 0); !ok {
		t.Fatal("conn-scoped entry must still be queued/popable in m")
	}
	// ...but it must NOT have been cached in `last` (the unpruned map).
	if _, ok := s.Last(connKey); ok {
		t.Fatal("conn-scoped key must NOT populate the last-greeting cache — it would leak unbounded")
	}

	// A port-scoped push is cached as before.
	portKey := HandshakeStoreKey("", 3306)
	s.Push(portKey, entry)
	if _, ok := s.Last(portKey); !ok {
		t.Fatal("port-scoped key must populate the last-greeting cache")
	}
}

// TestTLSHandshakeStore_PopWaitWakesOnPush pins that a blocked PopWait is
// woken by a Push well before its timeout — the property that makes the
// recorder's long bounded wait free on the success path.
func TestTLSHandshakeStore_PopWaitWakesOnPush(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeStoreKey("", 3306)

	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Push(key, TLSHandshakeEntry{RespPackets: [][]byte{[]byte("g")}})
	}()

	start := time.Now()
	entry, ok := s.PopWait(key, 5*time.Second)
	if !ok {
		t.Fatal("PopWait must return the entry pushed during the wait")
	}
	if len(entry.RespPackets) != 1 {
		t.Fatalf("wrong entry: %+v", entry)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("PopWait took %v — it must wake on Push, not sleep out the timeout", elapsed)
	}
}
