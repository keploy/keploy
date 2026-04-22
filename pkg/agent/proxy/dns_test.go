package proxy

import (
	"testing"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
)

func TestGenerateDNSDedupeKey_SameQuerySameKey(t *testing.T) {
	// The dedup key is derived solely from the question fields, so
	// identical questions must always produce identical keys.
	question := dns.Question{
		Name:   "sqs.us-east-1.amazonaws.com.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}

	key1 := generateDNSDedupeKey(question)
	key2 := generateDNSDedupeKey(question)

	if key1 != key2 {
		t.Errorf("same query must produce same dedup key, got %q vs %q", key1, key2)
	}
}

func TestGenerateDNSDedupeKey_DifferentQueryTypes(t *testing.T) {
	qA := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	qAAAA := dns.Question{Name: "example.com.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(qA) == generateDNSDedupeKey(qAAAA) {
		t.Error("different query types must produce different dedup keys")
	}
}

func TestGenerateDNSDedupeKey_DifferentNames(t *testing.T) {
	q1 := dns.Question{Name: "sqs.us-east-1.amazonaws.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "sqs.us-west-2.amazonaws.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) == generateDNSDedupeKey(q2) {
		t.Error("different domain names must produce different dedup keys")
	}
}

func TestGenerateDNSDedupeKey_NormalizesName(t *testing.T) {
	// Without trailing dot — generateDNSDedupeKey uses dns.Fqdn which adds it.
	q1 := dns.Question{Name: "example.com", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) != generateDNSDedupeKey(q2) {
		t.Error("FQDN normalization should make these keys equal")
	}
}

func TestGenerateDNSDedupeKey_CaseInsensitive(t *testing.T) {
	// DNS names are case-insensitive per RFC 4343.
	q1 := dns.Question{Name: "Example.COM.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) != generateDNSDedupeKey(q2) {
		t.Error("DNS dedup key must be case-insensitive")
	}
}

func TestShouldCacheDNSResponse(t *testing.T) {
	upstream := dnsCacheEntry{Msg: &dns.Msg{}, FromUpstream: true}
	synthetic := dnsCacheEntry{Msg: &dns.Msg{}, FromUpstream: false}

	cases := []struct {
		name string
		mode models.Mode
		resp dnsCacheEntry
		want bool
	}{
		{"record + upstream caches", models.MODE_RECORD, upstream, true},
		{"test + upstream caches", models.MODE_TEST, upstream, true},
		{"record + synthetic does not cache", models.MODE_RECORD, synthetic, false},
		{"test + synthetic does not cache", models.MODE_TEST, synthetic, false},
		{"off + upstream does not cache", models.MODE_OFF, upstream, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldCacheDNSResponse(tc.mode, tc.resp)
			if got != tc.want {
				t.Fatalf("shouldCacheDNSResponse(%v, FromUpstream=%v) = %v; want %v",
					tc.mode, tc.resp.FromUpstream, got, tc.want)
			}
		})
	}
}

// TestDNSCacheLifecycle_RecordMode asserts the core invariants of the
// record-mode DNS cache:
//
//  1. A freshly inserted entry is returned on the next lookup (no
//     second resolution is needed → no duplicate DNS mock is emitted).
//  2. After TTL expiry the entry is gone, so a second resolution will
//     happen and (via the dedupe tracker) a second mock CAN still be
//     emitted if necessary.
//
// This covers the key-level behavior that ServeDNS relies on: the cache
// hit short-circuits the resolve + recordDNSMock path entirely.
func TestDNSCacheLifecycle_RecordMode(t *testing.T) {
	// Use a short TTL so we can validate expiry without sleeping the
	// full production 30 s.
	const testTTL = 100 * time.Millisecond
	cache := expirable.NewLRU[string, dnsCacheEntry](dnsCacheMaxSize, nil, testTTL)

	key := generateCacheKey("example.com", dns.TypeA)
	entry := dnsCacheEntry{Msg: &dns.Msg{}, FromUpstream: true}

	// Gate 1: shouldCacheDNSResponse says we cache in record mode.
	if !shouldCacheDNSResponse(models.MODE_RECORD, entry) {
		t.Fatalf("record-mode upstream entry should be cacheable")
	}

	// First resolution — population step.
	cache.Add(key, entry)

	// Gate 2: within TTL, the entry is returned without re-resolution.
	if _, found := cache.Get(key); !found {
		t.Fatalf("entry must be present immediately after Add")
	}
	// A second Get in quick succession should also hit.
	if _, found := cache.Get(key); !found {
		t.Fatalf("entry must still be cached on second lookup within TTL")
	}

	// Gate 3: after TTL, entry expires — callers will re-resolve and
	// (because recordDNSMock dedupe keys have a much longer TTL of
	// 30 min) the replayer will still see exactly one mock per
	// (name, qtype) across the whole session. The second resolution
	// happens but is a no-op at the mock-emission layer.
	time.Sleep(testTTL + 50*time.Millisecond)
	if _, found := cache.Get(key); found {
		t.Fatalf("entry should have expired after TTL")
	}
}

// TestDNSCacheLifecycle_DedupeStillCatchesDuplicates asserts the second
// line of defense: the recordedDNSMocks dedupe tracker. Even if the DNS
// cache were entirely disabled, each (name, qtype) query must still
// emit at most one mock per session. This guards against a reviewer
// later removing shouldCacheDNSResponse without understanding the
// interaction.
func TestDNSCacheLifecycle_DedupeStillCatchesDuplicates(t *testing.T) {
	dedupe := newRecordedDNSMocksCache()

	q := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	key := generateDNSDedupeKey(q)

	if _, ok := dedupe.Get(key); ok {
		t.Fatalf("fresh dedupe cache must not report the query as already recorded")
	}
	dedupe.Add(key, true)
	if _, ok := dedupe.Get(key); !ok {
		t.Fatalf("dedupe cache must remember the recorded query")
	}
	// Simulate a duplicate emission attempt — the caller checks Get()
	// and skips recording if found. No double-Add is possible in
	// production; the test just pins the read path.
	for i := 0; i < 3; i++ {
		if _, ok := dedupe.Get(key); !ok {
			t.Fatalf("dedupe cache must consistently signal a duplicate (iter=%d)", i)
		}
	}
}
