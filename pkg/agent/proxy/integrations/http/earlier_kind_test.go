package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// TestEarlierOfSetsHTTPKind verifies that the HTTP adapter stamps models.HTTP
// on every learn.Earlier exchange it builds.
func TestEarlierOfSetsHTTPKind(t *testing.T) {
	m := learnRecorded("mock-229", 0, "PUT", "/v1/secret/data/keploy/personal-access-tokens/keploy.io/e2e-go-key",
		`{"data":{"lastUsedAt":1795447495}}`, nil, `{"data":{"version":1}}`)
	e := earlierOf(learn.NewDriftCache(), m, m, learn.Shape{}, learn.Shape{}, m, true)
	if e.Kind != models.HTTP {
		t.Fatalf("earlierOf Kind = %q, want %q", e.Kind, models.HTTP)
	}
}
