package replayer

import (
	"fmt"
	"testing"
)

// TestQuerySigCacheIsBounded pins the retention contract on querySigCache.
//
// The cache key is the raw SQL text. A client that inlines literals mints a
// fresh key on every request, so an unbounded map here grows for as long as
// the agent process lives. The cache must therefore cap out instead of
// tracking the number of distinct inputs.
func TestQuerySigCacheIsBounded(t *testing.T) {
	querySigCache.Purge()
	t.Cleanup(querySigCache.Purge)

	// Every query below is a distinct string only because of the inlined
	// literal — exactly the shape that makes the key space infinite.
	const distinct = querySigCacheSize + 500
	for i := 0; i < distinct; i++ {
		if _, err := getQueryStructureCached(fmt.Sprintf("SELECT name FROM users WHERE id = %d", i)); err != nil {
			t.Fatalf("getQueryStructureCached(%d) returned an error: %v", i, err)
		}
	}

	if got := querySigCache.Len(); got > querySigCacheSize {
		t.Errorf("querySigCache holds %d entries after %d distinct queries, must never exceed the %d cap: the cache is tracking its input, not bounding it",
			got, distinct, querySigCacheSize)
	}
}

// TestQuerySigCacheSurvivesPoolScan pins the reason the memo is a 2Q cache and
// not a plain LRU. matchCommand rescans the whole candidate mock pool for each
// live command and looks the live query's signature up once per candidate. A
// pool holding more distinct SQL texts than the cap would, under plain-LRU
// recency, evict the live query between candidates and re-parse it on every
// iteration — strictly worse than the unbounded map this replaced. The
// repeatedly-touched query must survive a scan larger than the whole cache.
func TestQuerySigCacheSurvivesPoolScan(t *testing.T) {
	querySigCache.Purge()
	t.Cleanup(querySigCache.Purge)

	const liveQuery = "SELECT name FROM users WHERE id = 4242"

	// Two touches: 2Q promotes on the second access, which is what the
	// per-candidate lookups in a real pool scan provide.
	for i := 0; i < 2; i++ {
		if _, err := getQueryStructureCached(liveQuery); err != nil {
			t.Fatalf("warming the live query returned an error: %v", err)
		}
	}

	// Scan a candidate pool twice the size of the cache.
	for i := 0; i < querySigCacheSize*2; i++ {
		if _, err := getQueryStructureCached(fmt.Sprintf("SELECT total FROM orders WHERE customer = %d", i)); err != nil {
			t.Fatalf("scanning candidate %d returned an error: %v", i, err)
		}
	}

	if !querySigCache.Contains(liveQuery) {
		t.Error("the live query was evicted by a candidate-pool scan: every candidate now costs a re-parse of the same query")
	}
}

// TestQuerySigCacheMemoIsStable guards the other half of the bound: evicting
// an entry may cost a re-parse but must never change the answer, because
// getQueryStructure is a pure function of the SQL text. If that stopped
// holding, bounding the cache would silently change match verdicts.
func TestQuerySigCacheMemoIsStable(t *testing.T) {
	querySigCache.Purge()
	t.Cleanup(querySigCache.Purge)

	const sql = "SELECT name FROM users WHERE id = 7 AND status = 'active'"

	first, err := getQueryStructureCached(sql)
	if err != nil {
		t.Fatalf("first call returned an error: %v", err)
	}
	cached, err := getQueryStructureCached(sql)
	if err != nil {
		t.Fatalf("cached call returned an error: %v", err)
	}
	if cached != first {
		t.Errorf("cached signature %q != first signature %q", cached, first)
	}

	// Force the entry out the way an eviction would, then recompute.
	querySigCache.Purge()
	recomputed, err := getQueryStructureCached(sql)
	if err != nil {
		t.Fatalf("post-eviction call returned an error: %v", err)
	}
	if recomputed != first {
		t.Errorf("signature after eviction %q != original %q: eviction is not side-effect free", recomputed, first)
	}
}

// TestQuerySigCacheSkipsUnparseableSQL keeps the pre-existing behaviour that a
// parse failure is not memoized, so a bad key can never occupy a cache slot.
func TestQuerySigCacheSkipsUnparseableSQL(t *testing.T) {
	querySigCache.Purge()
	t.Cleanup(querySigCache.Purge)

	if _, err := getQueryStructureCached("%%% not sql %%%"); err == nil {
		t.Fatal("expected unparseable SQL to return an error")
	}
	if got := querySigCache.Len(); got != 0 {
		t.Errorf("querySigCache holds %d entries after a parse failure, want 0", got)
	}
}
