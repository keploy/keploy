package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TestEarlierKindPreservedAcrossStartUpAndRanCalls verifies that Earlier.Kind
// survives both start-up retention and earlier-test ranCalls summarization
// (even when ranCalls strips non-drifted path leaves and keeps only the
// changed body leaf), while remaining empty when unset.
func TestEarlierKindPreservedAcrossStartUpAndRanCalls(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tr := NewTracker()
	tr.NewSet()

	// Start-up HTTP call (before first Begin).
	tr.Served(tr.Epoch(), Earlier{
		Mock: "mock-startup-http",
		Kind: models.HTTP,
		At:   time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC),
		Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "url.path.0", Type: String, Value: "v1"}, Live: "v1", Place: "url.path[0]"},
			{Side: Request, Leaf: Leaf{Path: "body.data.lastUsedAt", Type: Number, Value: "1795447495"}, Live: "1795447579", Place: "body.data.lastUsedAt"},
		},
	})

	// Earlier test's own HTTP call with stable path and drifted body leaf.
	tr.Begin()
	tr.Served(tr.Epoch(), Earlier{
		Mock: "mock-earlier-http",
		Kind: models.HTTP,
		At:   time.Date(2026, 11, 24, 4, 0, 1, 0, time.UTC),
		Own:  true,
		Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "url.path.0", Type: String, Value: "v1"}, Live: "v1", Place: "url.path[0]"},
			{Side: Request, Leaf: Leaf{Path: "body.data.lastUsedAt", Type: Number, Value: "1795447495"}, Live: "1795447579", Place: "body.data.lastUsedAt"},
		},
	})
	tr.Served(tr.Epoch(), Earlier{
		Mock: "mock-earlier-unset",
		At:   time.Date(2026, 11, 24, 4, 0, 2, 0, time.UTC),
		Own:  true,
		Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.authTime", Type: Number, Value: "1795447495"}, Live: "1795447579", Place: "body.authTime"},
		},
	})
	tr.End()

	// Next test opens: earlier test's calls are now summarized via ranCalls.
	tr.Begin()
	defer tr.End()
	got := map[string]Earlier{}
	for _, e := range tr.Placed().Earlier {
		got[e.Mock] = e
	}

	st, ok := got["mock-startup-http"]
	if !ok || !st.Run || !st.StartUp || st.Kind != models.HTTP {
		t.Fatalf("startup HTTP earlier = %+v (ok=%v); want Run=true StartUp=true Kind=Http", st, ok)
	}
	eh, ok := got["mock-earlier-http"]
	if !ok || !eh.Run || eh.Own || eh.Kind != models.HTTP {
		t.Fatalf("earlier-test HTTP earlier = %+v (ok=%v); want Run=true Own=false Kind=Http", eh, ok)
	}
	if len(eh.Seen) != 1 || eh.Seen[0].Leaf.Path != "body.data.lastUsedAt" {
		t.Fatalf("ranCalls should keep only drifted body leaf while preserving Kind; got Seen=%+v", eh.Seen)
	}
	eu, ok := got["mock-earlier-unset"]
	if !ok || eu.Kind != "" {
		t.Fatalf("earlier-test unset-kind earlier = %+v (ok=%v); want Kind empty", eu, ok)
	}
}
