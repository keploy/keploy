package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// uuidRe matches a v4-style UUID segment, e.g. the S3 receipt key the
// orderflow-producer sample regenerates per request.
const uuidRe = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// TestRepro_NonDeterministicURLPathDefeatsMatch reproduces the atg-with-mocks
// "502 Bad Gateway" at the matcher level: the app PUTs receipts/<user>/<uuid>.txt
// to S3, regenerates a different uuid on replay, and SchemaMatch — which matched
// the URL path by EXACT equality with no noise hook — rejected the recorded mock,
// returning 0 candidates so decode.go writes "no matching mock -> 502".
func TestRepro_NonDeterministicURLPathDefeatsMatch(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()

	recorded := httpMock("s3-put-receipt", "PUT",
		"http://minio:9000/order-receipts/receipts/amit/2d0fca4a-4739-c436-3352-da4a97dcb1b1.txt")
	replay := &req{
		method: "PUT",
		url:    &url.URL{Path: "/order-receipts/receipts/amit/9f7c1e22-0000-1111-2222-aaaabbbbcccc.txt"},
		header: http.Header{},
	}

	// Baseline: with NO url noise the exact path mismatch yields 0 candidates
	// (the 502 cause). This must stay true — the fix is opt-in, not a blanket
	// loosening of URL matching.
	matched, err := h.SchemaMatch(ctx, replay, []*models.Mock{recorded}, nil, nil)
	if err != nil {
		t.Fatalf("SchemaMatch error: %v", err)
	}
	if len(matched) != 0 {
		t.Fatalf("baseline: expected 0 candidates (exact URL mismatch), got %d", len(matched))
	}
	t.Logf("REPRODUCED: a non-deterministic URL segment yields 0 schema candidates -> no-matching-mock 502")
}

// TestSchemaMatch_URLPathNoiseMatches is the fix: configuring the variable
// segment as url noise lets the same logical call match despite the drifted uuid.
func TestSchemaMatch_URLPathNoiseMatches(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()

	recorded := httpMock("s3-put-receipt", "PUT",
		"http://minio:9000/order-receipts/receipts/amit/2d0fca4a-4739-c436-3352-da4a97dcb1b1.txt")
	replay := &req{
		method: "PUT",
		url:    &url.URL{Path: "/order-receipts/receipts/amit/9f7c1e22-0000-1111-2222-aaaabbbbcccc.txt"},
		header: http.Header{},
	}

	matched, err := h.SchemaMatch(ctx, replay, []*models.Mock{recorded}, nil, []string{uuidRe})
	if err != nil {
		t.Fatalf("SchemaMatch error: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("with url noise: expected the mock to match (1 candidate), got %d", len(matched))
	}
}

// TestMatchURLPath_NoiseScoping guards that url noise wildcards ONLY the variable
// segment — a genuinely different path (different resource) must still NOT match,
// so the fix doesn't silently serve the wrong mock.
func TestMatchURLPath_NoiseScoping(t *testing.T) {
	h := newHTTP()
	mock := "http://minio:9000/order-receipts/receipts/amit/2d0fca4a-4739-c436-3352-da4a97dcb1b1.txt"

	// same template, drifted uuid -> matches under noise
	if !h.MatchURLPath(mock, "/order-receipts/receipts/amit/9f7c1e22-0000-1111-2222-aaaabbbbcccc.txt", []string{uuidRe}) {
		t.Fatal("expected drifted-uuid path to match under url noise")
	}
	// different user (a real, non-noise difference) -> must NOT match
	if h.MatchURLPath(mock, "/order-receipts/receipts/BOB/9f7c1e22-0000-1111-2222-aaaabbbbcccc.txt", []string{uuidRe}) {
		t.Fatal("url noise must not wildcard a non-noise segment difference (different user)")
	}
	// no noise -> exact behaviour unchanged
	if h.MatchURLPath(mock, "/order-receipts/receipts/amit/different.txt", nil) {
		t.Fatal("with no url noise, a different path must not match (backward compatibility)")
	}
}

// TestMatchURLPath_NumericIDScoping documents how a simple, changing numeric id
// (e.g. /users/55) is handled, and why patterns must be anchored to their path
// context. A bare value pattern over-matches; an anchored one is precise.
func TestMatchURLPath_NumericIDScoping(t *testing.T) {
	h := newHTTP()
	mock := "http://api/users/55/orders/100"

	// ANCHORED pattern: wildcards ONLY the user-id segment.
	anchored := []string{`/users/[0-9]+`}
	if !h.MatchURLPath(mock, "/users/56/orders/100", anchored) {
		t.Fatal("anchored: a different user-id should match")
	}
	if h.MatchURLPath(mock, "/users/55/orders/999", anchored) {
		t.Fatal("anchored: a different ORDER id must NOT match (order-id stays strict)")
	}
	if h.MatchURLPath("http://api/v1/users/55", "/v2/users/56", anchored) {
		t.Fatal("anchored: a different API version must NOT match (version stays strict)")
	}

	// BARE value pattern: over-matches every numeric run — captured here so the
	// footgun is explicit and a future change to this behaviour is caught. Users
	// should anchor instead (see MatchURLPath doc + the anchored cases above).
	bare := []string{`[0-9]+`}
	if !h.MatchURLPath(mock, "/users/55/orders/999", bare) {
		t.Fatal("bare [0-9]+ is expected to (over-)match a different order — documents the footgun")
	}
	if !h.MatchURLPath("http://api/v1/users/55", "/v2/users/56", bare) {
		t.Fatal("bare [0-9]+ is expected to (over-)match across /v1 vs /v2 — documents the footgun")
	}
}
