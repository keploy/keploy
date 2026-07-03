package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func h2MockForTest(name, authority string) *models.Mock {
	mk := newMockForTest(name, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), models.LifetimeSession)
	mk.Kind = models.HTTP2
	mk.Spec.HTTP2Req = &models.HTTP2Req{Authority: authority, Scheme: "https"}
	return mk
}

// alwaysMatch matches any Http2 mock (used to assert presence-by-kind only).
func alwaysMatch(*models.Mock) bool { return true }

// TestHasMocksByKind_FreshNotCached is the regression guard for the two
// staleness bugs a manager-pointer cache introduced:
//
//  1. Pinned-empty: a query BEFORE any Http2 mock loads must not freeze the
//     answer to false — once Http2 mocks are set on the same manager, the
//     check must see them.
//  2. Stale carryover: after the mock set is REPLACED with one that no longer
//     has Http2 mocks (e.g. the next test-set is HTTP/1.1-only), the check must
//     go back to false and not carry the old h2 preference forward.
func TestHasMocksByKind_FreshNotCached(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// (1) No Http2 mocks yet.
	if mm.HasMocksByKind(models.HTTP2, alwaysMatch) {
		t.Fatal("empty manager must report no Http2 mocks")
	}

	// Http2 mocks load later on the SAME manager -> must now be seen.
	mm.SetUnFilteredMocks([]*models.Mock{h2MockForTest("h2-a", "vault.svc:8200")})
	if !mm.HasMocksByKind(models.HTTP2, alwaysMatch) {
		t.Fatal("must see Http2 mocks loaded after the first (empty) query")
	}

	// (2) Replace with an Http2-free set -> must go back to false.
	http1 := newMockForTest("http1-b", time.Date(2024, 1, 1, 12, 1, 0, 0, time.UTC), models.LifetimeSession)
	http1.Kind = models.HTTP
	mm.SetUnFilteredMocks([]*models.Mock{http1})
	if mm.HasMocksByKind(models.HTTP2, alwaysMatch) {
		t.Fatal("after replacing with an Http2-free set, must report no Http2 mocks (no stale carryover)")
	}
}

// TestHasMocksByKind_Tiers confirms all three tiers are consulted and the
// destination predicate scopes the match.
func TestHasMocksByKind_Tiers(t *testing.T) {
	destPred := func(host string, port uint32) func(*models.Mock) bool {
		return func(mk *models.Mock) bool {
			if mk.Spec.HTTP2Req == nil {
				return false
			}
			h, p := hostPortFromAuthority(mk.Spec.HTTP2Req.Authority, mk.Spec.HTTP2Req.Scheme)
			return http2DestMatches(h, p, host, port)
		}
	}

	// Startup tier.
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{h2MockForTest("boot", "vault.svc:8200")}, models.BaseTime, time.Now())
	if !mm.HasMocksByKind(models.HTTP2, destPred("vault.svc", 8200)) {
		t.Fatal("startup-tier Http2 mock for vault.svc:8200 must match")
	}
	// A different destination must NOT match (the mixed-target guard).
	if mm.HasMocksByKind(models.HTTP2, destPred("other.svc", 443)) {
		t.Fatal("Http2 mock for vault.svc:8200 must not match other.svc:443")
	}

	// Filtered tier.
	mm2 := NewMockManager(nil, nil, zap.NewNop())
	defer mm2.Close()
	mm2.SetFilteredMocks([]*models.Mock{h2MockForTest("flt", "api.example.com:9000")})
	if !mm2.HasMocksByKind(models.HTTP2, destPred("api.example.com", 9000)) {
		t.Fatal("filtered-tier Http2 mock must match")
	}
}
