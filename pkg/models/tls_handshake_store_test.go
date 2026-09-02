package models

import (
	"bytes"
	"fmt"
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
	// The cache is recorded explicitly now: a queue key is a port or a
	// connection, neither of which names a server, so Push no longer populates
	// it as a side effect.
	dstKey := HandshakeLastKey("scope-a", &ConditionalDstCfg{Addr: "10.0.0.5:3306"})
	s.RememberLast(dstKey, entry1)

	// Consume the queue the way another connection's PopWait would.
	if _, ok := s.PopWait(key, 0); !ok {
		t.Fatal("PopWait must return the pushed entry")
	}
	if _, ok := s.PopWait(key, 0); ok {
		t.Fatal("queue must be empty after the pop")
	}

	// The last-greeting cache must still serve entry1.
	got, ok := s.Last(dstKey)
	if !ok {
		t.Fatal("Last must survive queue consumption — it is the lost-raw-leg fallback")
	}
	if len(got.RespPackets) != 1 || !bytes.Equal(got.RespPackets[0], []byte("greeting-1")) {
		t.Fatalf("Last returned wrong entry: %+v", got)
	}

	// A newer record overwrites the cache.
	entry2 := TLSHandshakeEntry{RespPackets: [][]byte{[]byte("greeting-2")}}
	s.RememberLast(dstKey, entry2)
	got, ok = s.Last(dstKey)
	if !ok || !bytes.Equal(got.RespPackets[0], []byte("greeting-2")) {
		t.Fatalf("Last must return the most recent record, got %+v ok=%v", got, ok)
	}

	// Keys are independent.
	if _, ok := s.Last(HandshakeStoreKey("", 5432)); ok {
		t.Fatal("Last must be keyed — a different port must miss")
	}
}

// TestTLSHandshakeStore_CacheIsExplicitAndScopeIsolated pins that `last` is
// populated ONLY by an explicit RememberLast/RememberLastForPort call, never as
// a side effect of Push. A conn-scoped key must not reach it: that would add one
// entry per connection, which even with the TTL and size cap would churn the
// cache and evict the low-cardinality entries the fallback depends on — while
// still flowing normally through the consumable queue m.
func TestTLSHandshakeStore_CacheIsExplicitAndScopeIsolated(t *testing.T) {
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
		t.Fatal("conn-scoped key must NOT populate the last-greeting cache — one entry per connection would churn it")
	}

	// Push never populates the cache now, whatever the key shape.
	portKey := HandshakeStoreKey("", 3306)
	s.Push(portKey, entry)
	if _, ok := s.Last(portKey); ok {
		t.Fatal("Push must not populate the last-greeting cache: a port names no server, so an " +
			"entry there is only safe when tagged via RememberLastForPort")
	}

	// Only an explicit, scope+destination keyed record does.
	dstKey := HandshakeLastKey("ns/app-a/ts0", &ConditionalDstCfg{Addr: "10.0.0.5:3306"})
	s.RememberLast(dstKey, entry)
	if _, ok := s.Last(dstKey); !ok {
		t.Fatal("RememberLast must populate the cache under the destination key")
	}
	// A second app on the same node, same address, must not see it.
	otherKey := HandshakeLastKey("ns/app-b/ts0", &ConditionalDstCfg{Addr: "10.0.0.5:3306"})
	if _, ok := s.Last(otherKey); ok {
		t.Fatal("a different app/session scope must not read another's cached greeting")
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

// TestRememberLastForPort_LatchesAmbiguousAcrossServers pins the guard that
// makes a port-scoped greeting cache safe. A port key does not name a server,
// so the moment two DIFFERENT destinations are seen under one it must stop
// serving: handing server B's greeting to a connection talking to server A
// would stitch A's capability flags and auth plugin into B's config mock —
// silently wrong data, which is worse than the missing mock it replaces.
func TestRememberLastForPort_LatchesAmbiguousAcrossServers(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastPortKey("ns/app/ts0", 3306)
	if key == "" {
		t.Fatal("expected a non-empty port key for a scoped caller")
	}

	s.RememberLastForPort(key, "v10|8.4.0|caps=1|cs=255|caching_sha2_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	if got, ok := s.Last(key); !ok || string(got.RespPackets[0]) != "\x0aA" {
		t.Fatalf("single server should be served from the port key; ok=%v", ok)
	}

	// A second, different server appears on the same port.
	s.RememberLastForPort(key, "v10|5.7.44|caps=2|cs=33|mysql_native_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	if _, ok := s.Last(key); ok {
		t.Error("port key kept serving after two distinct servers were seen — cross-server contamination")
	}

	// The latch is permanent: a later write from the original server must not
	// resurrect a key already proven not to identify one server.
	s.RememberLastForPort(key, "v10|8.4.0|caps=1|cs=255|caching_sha2_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	if _, ok := s.Last(key); ok {
		t.Error("ambiguity latch was cleared by a later write; it must never unlatch")
	}
}

// TestHandshakeLastPortKey_AllowsEmptyScopeRejectsZeroPort pins the key's
// contract.
func TestHandshakeLastPortKey_AllowsEmptyScopeRejectsZeroPort(t *testing.T) {
	// An unscoped caller is a classic sidecar serving one session, so it still
	// gets a key — matching HandshakeLastKey, which accepts an empty scope for
	// the same reason. Cross-server safety comes from the ambiguity latch, not
	// from scope; scope is the CROSS-APP guard the DaemonSet path sets.
	if got := HandshakeLastPortKey("", 3306); got == "" {
		t.Error("an unscoped caller got no port key; the sidecar path would lose the fallback entirely")
	}
	// A port is the one thing the key must have.
	if got := HandshakeLastPortKey("ns/app/ts0", 0); got != "" {
		t.Errorf("HandshakeLastPortKey(_, 0) = %q, want \"\"", got)
	}
	// Scope must still separate apps when it IS set.
	if HandshakeLastPortKey("ns/app-a/ts0", 3306) == HandshakeLastPortKey("ns/app-b/ts0", 3306) {
		t.Error("two scoped apps share a port key — cross-app greeting contamination")
	}
}

// TestLastGreetingCacheIsBounded pins that the never-consumed cache cannot grow
// without limit in a long-lived DaemonSet agent, which accumulates one entry
// per app x session x destination.
func TestLastGreetingCacheIsBounded(t *testing.T) {
	s := NewTLSHandshakeStore()
	for i := 0; i < maxLastGreetings*2; i++ {
		s.RememberLast(HandshakeLastKey("scope", &ConditionalDstCfg{Addr: fmt.Sprintf("10.0.%d.%d:3306", i/256, i%256)}),
			TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
	}
	s.mu.Lock()
	n := len(s.last)
	s.mu.Unlock()
	if n > maxLastGreetings {
		t.Errorf("last-greeting cache holds %d entries, want <= %d — unbounded growth", n, maxLastGreetings)
	}
}

// TestLastGreetingExpires pins the TTL: a greeting cached long ago may describe
// a server that has since restarted with different capabilities.
func TestLastGreetingExpires(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastKey("scope", &ConditionalDstCfg{Addr: "10.0.0.5:3306"})
	s.RememberLast(key, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
	s.mu.Lock()
	v := s.last[key]
	v.seen = time.Now().Add(-2 * lastGreetingTTL)
	s.last[key] = v
	s.mu.Unlock()
	if _, ok := s.Last(key); ok {
		t.Error("Last served an entry older than lastGreetingTTL")
	}
}

// TestAmbiguityLatchSurvivesTTL pins that a proven-unsafe key stays unsafe. The
// tombstone's timestamp must be refreshed by suppressed writes and exempt from
// the TTL sweep — otherwise the latch simply expires and the next single-server
// write recreates a clean, serving entry, reopening cross-server contamination
// on a repeating 30-minute cycle.
func TestAmbiguityLatchSurvivesTTL(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastPortKey("ns/app/ts0", 3306)
	s.RememberLastForPort(key, "v10|8.4.0|caps=1|cs=255|caching_sha2_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	s.RememberLastForPort(key, "v10|5.7.44|caps=2|cs=33|mysql_native_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	if _, ok := s.Last(key); ok {
		t.Fatal("precondition: key should be latched ambiguous")
	}

	// Age the tombstone well past the TTL, then drive a sweep with an unrelated
	// write, exactly as a busy agent would.
	s.mu.Lock()
	v := s.last[key]
	v.seen = time.Now().Add(-4 * lastGreetingTTL)
	s.last[key] = v
	// Also backdate lastPruned: pruneLastLocked has a fast path that skips the
	// sweep entirely while the map is under budget and was swept recently, so
	// without this the "unrelated write" below returns before sweeping and the
	// test asserts nothing. (That fast path silently disarmed this guard once.)
	s.lastPruned = time.Now().Add(-4 * lastGreetingTTL)
	s.mu.Unlock()
	s.RememberLast(HandshakeLastKey("ns/app/ts0", &ConditionalDstCfg{Addr: "10.0.0.7:5432"}),
		TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})

	s.mu.Lock()
	_, stillThere := s.last[key]
	s.mu.Unlock()
	if !stillThere {
		t.Error("ambiguity tombstone was collected by the TTL sweep; it must age with use, not with time")
	}
	// And a later single-server write must not resurrect it.
	s.RememberLastForPort(key, "v10|8.4.0|caps=1|cs=255|caching_sha2_password", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	if _, ok := s.Last(key); ok {
		t.Error("port key serves again after the TTL window — cross-server contamination reopened")
	}
}

// TestTombstonesDoNotStarveTheCache pins that ambiguous keys cannot disable the
// whole fallback. Sharing one budget with live entries lets tombstones fill the
// map, after which the only evictable entry is whichever was just inserted — so
// every write is undone and the cache goes dead store-wide, silently recreating
// the capture-loss bug it exists to prevent.
func TestTombstonesDoNotStarveTheCache(t *testing.T) {
	s := NewTLSHandshakeStore()
	// Live greetings recorded first, so they are the OLDEST entries in the map.
	// Half the budget, so the result is unambiguous: with SEPARATE budgets every
	// one of these survives; with a shared budget the refreshed tombstones push
	// the total past the cap and evict these, the oldest, first.
	nLive := maxLastGreetings / 2
	var live []string
	for i := 0; i < nLive; i++ {
		k := HandshakeLastKey(fmt.Sprintf("ns/live-%d/ts0", i), &ConditionalDstCfg{Addr: "10.0.0.5:3306"})
		s.RememberLast(k, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'L'}}})
		live = append(live, k)
	}
	// Then a busy set of latched keys. Every suppressed write refreshes their
	// timestamps, so on a SHARED budget they stay newest and the live entries
	// above become the eviction victims.
	for i := 0; i < maxLastGreetings; i++ {
		k := HandshakeLastPortKey(fmt.Sprintf("ns/amb-%d/ts0", i), 3306)
		s.RememberLastForPort(k, "server-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
		s.RememberLastForPort(k, "server-B", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
		s.RememberLastForPort(k, "server-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}}) // refresh
	}

	surviving := 0
	for _, k := range live {
		if _, ok := s.Last(k); ok {
			surviving++
		}
	}
	if surviving == 0 {
		t.Fatalf("all %d live greetings were evicted by refreshed tombstones — the fallback is dead "+
			"for destinations that were never ambiguous", len(live))
	}
	if surviving != nLive {
		t.Errorf("live greetings surviving = %d, want %d: tombstones must not compete with them "+
			"for the same budget", surviving, nLive)
	}
}

// TestRememberLastRefusesPortKeys pins that the untagged setter cannot be used on
// a port key. A port key is only safe while every write carries a server
// identity; an untagged write would blank that tag and permanently disable the
// ambiguity latch, turning the guarded key into an unguarded one.
func TestRememberLastRefusesPortKeys(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastPortKey("ns/app/ts0", 3306)
	s.RememberLast(key, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'X'}}})
	if _, ok := s.Last(key); ok {
		t.Error("RememberLast populated a port key; an untagged entry there disables the latch")
	}
	// And it must not be able to clear an existing latch either.
	s.RememberLastForPort(key, "server-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	s.RememberLastForPort(key, "server-B", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	s.RememberLast(key, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'X'}}})
	if _, ok := s.Last(key); ok {
		t.Error("an untagged RememberLast cleared a latched port key")
	}
}

// TestRememberLastForPort_SameServerNewAddressDoesNotLatch is the Kubernetes
// case that decides whether this fallback survives in production at all.
//
// The latch identifies a server by the caller-supplied serverID, NOT by address,
// precisely because addresses are unstable in k8s: a StatefulSet rollout gives
// the same logical MySQL a new pod IP, and a Service with several endpoints
// hands out a different IP per connection. An address-tagged latch fires on two
// connections to the SAME server, and because tombstones are TTL-exempt it would
// then disable the fallback permanently for that scope+port — the fix would go
// dead after the first rollout, silently, while every test stayed green.
func TestRememberLastForPort_SameServerNewAddressDoesNotLatch(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastPortKey("ns/app/ts0", 3306)
	const sameServer = "v10|8.4.0|caps=123|cs=255|caching_sha2_password"

	// Pod 10.244.0.24 before the rollout, 10.244.0.31 after — one server.
	s.RememberLastForPort(key, sameServer, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	s.RememberLastForPort(key, sameServer, TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})

	got, ok := s.Last(key)
	if !ok {
		t.Fatal("the same server on a new pod IP latched the key ambiguous; the fallback is now " +
			"permanently dead for this scope+port, which is the environment it exists for")
	}
	if len(got.RespPackets) == 0 || got.RespPackets[0][1] != 'A' {
		t.Errorf("wrong entry returned: %q", got.RespPackets)
	}
	if s.IsAmbiguous(key) {
		t.Error("IsAmbiguous reports a latch for a single server")
	}

	// A genuinely different server — different capability flags — must still latch.
	s.RememberLastForPort(key, "v10|5.7.44|caps=999|cs=33|mysql_native_password",
		TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	if _, ok := s.Last(key); ok {
		t.Error("two genuinely different servers did not latch the key")
	}
	if !s.IsAmbiguous(key) {
		t.Error("IsAmbiguous did not report the latch")
	}
}

// TestTombstoneRefreshSurvivesEvictionPressure pins the `cur.seen = now` refresh
// on a suppressed write.
//
// Tombstones are exempt from the TTL sweep but NOT from their own eviction
// budget. Without the refresh a tombstone's timestamp is frozen at the moment of
// latching, so a key that keeps taking traffic becomes the OLDEST entry and is
// evicted first — after which the next single-server write recreates a clean,
// SERVING entry for a key already proven to have two servers behind it. The
// refresh makes a tombstone age with USE rather than with time-since-latching.
func TestTombstoneRefreshSurvivesEvictionPressure(t *testing.T) {
	s := NewTLSHandshakeStore()
	victim := HandshakeLastPortKey("ns/victim/ts0", 3306)

	// Latch the victim FIRST, so without a refresh it is the oldest tombstone.
	s.RememberLastForPort(victim, "srv-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	s.RememberLastForPort(victim, "srv-B", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	if !s.IsAmbiguous(victim) {
		t.Fatal("precondition: victim should be latched")
	}

	// Fill the tombstone budget with other latched keys, refreshing the victim
	// along the way exactly as continuing traffic on that destination would.
	for i := 0; i < maxLastGreetings+16; i++ {
		k := HandshakeLastPortKey(fmt.Sprintf("ns/filler-%d/ts0", i), 3306)
		s.RememberLastForPort(k, "srv-X", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
		s.RememberLastForPort(k, "srv-Y", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
		// Traffic keeps arriving on the victim's destination.
		s.RememberLastForPort(victim, "srv-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	}

	if !s.IsAmbiguous(victim) {
		t.Error("a still-busy latched key was evicted under tombstone pressure; the next " +
			"single-server write would recreate it CLEAN and cross-server reuse reopens")
	}
	if _, ok := s.Last(victim); ok {
		t.Error("the victim key is serving again after eviction pressure")
	}
}

// TestRememberLastForPortRefusesEmptyIdentity pins the guard the CI mutation
// job's own reasoning depends on: mutant 2 blanks the server identity and
// expects the port key to go unwritten. If an untagged entry could reach a port
// key, the latch would be permanently disabled for it — a port key carries no
// server identity of its own, so an entry with no identity can never be shown
// to be unsafe.
func TestRememberLastForPortRefusesEmptyIdentity(t *testing.T) {
	s := NewTLSHandshakeStore()
	key := HandshakeLastPortKey("ns/app/ts0", 3306)
	s.RememberLastForPort(key, "", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
	if _, ok := s.Last(key); ok {
		t.Error("an untagged entry reached a port key; the ambiguity latch is permanently " +
			"disabled for it, and the CI guard's blank-identity mutant would stop being a kill")
	}
	// And it must not be able to clear an existing latch.
	s.RememberLastForPort(key, "srv-A", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	s.RememberLastForPort(key, "srv-B", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	s.RememberLastForPort(key, "", TLSHandshakeEntry{RespPackets: [][]byte{{0x0a}}})
	if !s.IsAmbiguous(key) {
		t.Error("an untagged write cleared a latched port key")
	}
}
