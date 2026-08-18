package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

const uuidRe = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

func putReq(path string) *req {
	return &req{method: "PUT", url: &url.URL{Path: path}, header: http.Header{}}
}

// TestURLMatch_S3Receipt is the atg "no matching mock -> 502" reproduced and
// fixed. The app PUTs receipts/<user>/<uuid>.txt to S3; on replay BOTH the
// <user> token (carries a timestamp) and the <uuid>.txt segment drift.
func TestURLMatch_S3Receipt(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	recorded := httpMock("s3", "PUT",
		"http://minio:9000/order-receipts/receipts/amit_1781794443438_47ona3/2d0fca4a-4739-c436-3352-da4a97dcb1b1.txt")
	replay := putReq("/order-receipts/receipts/zara_1781999999999_q9zzz1/9f7c1e22-0000-1111-2222-aaaabbbbcccc.txt")

	// autoDynamic OFF (old behaviour / DisableAutoURLDynamic): exact mismatch ->
	// 0 candidates -> the 502.
	if got, _ := h.SchemaMatch(ctx, replay, []*models.Mock{recorded}, nil, nil, false); len(got) != 0 {
		t.Fatalf("autoDynamic=false: expected 0 candidates, got %d", len(got))
	}
	// DEFAULT (auto-detect): the <user> token and <uuid>.txt segments are
	// recognised as machine ids and the mock matches with ZERO config.
	if got, _ := h.SchemaMatch(ctx, replay, []*models.Mock{recorded}, nil, nil, true); len(got) != 1 {
		t.Fatalf("autoDynamic=true: expected the mock to auto-match, got %d candidates", len(got))
	}
}

// TestLooksDynamicSegment pins the conservative heuristic: obvious machine ids
// are dynamic; plain words and short composite-but-static tokens are not (those
// are left to explicit url noise).
func TestLooksDynamicSegment(t *testing.T) {
	dynamic := []string{
		"55", "1781794443438", // numeric id / timestamp
		"2d0fca4a-4739-c436-3352-da4a97dcb1b1",     // uuid
		"507f1f77bcf86cd799439011",                 // mongo objectid (24 hex)
		"0123456789abcdef0123",                     // long hex
		"amit_1781794443438_47ona3",                // long mixed token
		"2d0fca4a-4739-c436-3352-da4a97dcb1b1.txt", // uuid + extension
	}
	static := []string{
		"users", "profile", "orders", "settings", "report", "api", // plain words
		"v1", "v1alpha1", "oauth2", "base64", // short composite-but-static
		"", // empty
	}
	for _, s := range dynamic {
		if !looksDynamicSegment(s) {
			t.Errorf("expected %q to be detected as a dynamic id", s)
		}
	}
	for _, s := range static {
		if looksDynamicSegment(s) {
			t.Errorf("expected %q to be treated as static (not auto-wildcarded)", s)
		}
	}
}

// TestAutoDynamic_DefaultAndCorners covers the default auto-match plus the safety
// rails and the corner case that falls back to url noise.
func TestAutoDynamic_DefaultAndCorners(t *testing.T) {
	h := newHTTP()

	// DEFAULT covers numeric and uuid ids with no config:
	if !h.MatchURLPath("http://api/users/55/profile", "/users/56/profile", nil, true) {
		t.Fatal("numeric id should auto-match")
	}
	if !h.MatchURLPath("http://api/items/2d0fca4a-4739-c436-3352-da4a97dcb1b1", "/items/9f7c1e22-0000-1111-2222-aaaabbbbcccc", nil, true) {
		t.Fatal("uuid should auto-match")
	}

	// CORNER CASE: a word-like variable slug is ambiguous with a static segment,
	// so auto does NOT match it — and the url-noise mechanism covers it.
	if h.MatchURLPath("http://api/users/alice", "/users/bob", nil, true) {
		t.Fatal("plain-word variable segment must NOT auto-match")
	}
	if !h.MatchURLPath("http://api/users/alice", "/users/bob", []string{`/users/[a-z]+`}, false) {
		t.Fatal("url noise must cover the word-slug corner case")
	}

	// SAFETY rails — none of these may match even with auto-detect on:
	if h.MatchURLPath("http://api/users/55", "/orders/55", nil, true) {
		t.Fatal("different static segment (users vs orders) must not match")
	}
	if h.MatchURLPath("http://api/users/55", "/users/me", nil, true) {
		t.Fatal("a numeric id vs a non-id word must not match")
	}
	if h.MatchURLPath("http://api/users/55", "/users/55/extra", nil, true) {
		t.Fatal("different segment count must not match")
	}
	if h.MatchURLPath("http://api/v1/users/55", "/v2/users/56", nil, true) {
		t.Fatal("v1 vs v2 (short static composite) must not match — only the numeric id varies")
	}
}

// TestURLNoise_CornerCaseStillWorks confirms the explicit url-noise layer still
// works (for segments the auto-heuristic intentionally leaves alone), with the
// anchored-vs-bare scoping behaviour. autoDynamic is off here to isolate noise.
func TestURLNoise_CornerCaseStillWorks(t *testing.T) {
	h := newHTTP()
	mock := "http://api/users/55/orders/100"

	// anchored pattern wildcards ONLY the user-id segment
	if !h.MatchURLPath(mock, "/users/56/orders/100", []string{`/users/[0-9]+`}, false) {
		t.Fatal("anchored noise: different user-id should match")
	}
	if h.MatchURLPath(mock, "/users/55/orders/999", []string{`/users/[0-9]+`}, false) {
		t.Fatal("anchored noise: different order-id must NOT match")
	}
	// bare value pattern over-matches (documented footgun) — anchor instead
	if !h.MatchURLPath(mock, "/users/55/orders/999", []string{`[0-9]+`}, false) {
		t.Fatal("bare [0-9]+ is expected to over-match (footgun) — documents why patterns should be anchored")
	}
	// no noise + no auto => exact only
	if h.MatchURLPath(mock, "/users/56/orders/100", nil, false) {
		t.Fatal("no noise + no auto: a different path must not match")
	}
}

// TestMatch_ExactPreferredOverAutoDynamic proves the two-pass: when an EXACT mock
// exists it wins, and a dynamic-looking sibling is not wrongly served.
func TestMatch_ExactPreferredOverAutoDynamic(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	db := &mockMemDb{
		mocks: []*models.Mock{
			httpMock("mock-55", "GET", "http://api/users/55"),
			httpMock("mock-77", "GET", "http://api/users/77"),
		},
		updateUnFilteredReturn: true,
	}

	// Deterministic request for an EXACT recorded id -> exact mock wins (pass 1),
	// the numeric sibling is never considered.
	ok, stub, _, err := h.match(ctx, putGet("/users/55"), db, nil, nil, nil, true, false, false)
	if err != nil || !ok || stub == nil || stub.Name != "mock-55" {
		t.Fatalf("exact should win: ok=%v stub=%v err=%v", ok, stub, err)
	}

	// Non-deterministic id with no exact mock -> auto-match falls back (pass 2).
	ok2, stub2, _, err2 := h.match(ctx, putGet("/users/99"), db, nil, nil, nil, true, false, false)
	if err2 != nil || !ok2 || stub2 == nil {
		t.Fatalf("auto-dynamic fallback should match: ok=%v stub=%v err=%v", ok2, stub2, err2)
	}
}

func putGet(path string) *req {
	return &req{method: "GET", url: &url.URL{Path: path}, header: http.Header{}}
}
