package http

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockMemDb is a minimal mock of the MockMemDb interface for testing.
type mockMemDb struct {
	mocks []*models.Mock
	err   error
}

func (m *mockMemDb) GetUnFilteredMocks() ([]*models.Mock, error)              { return m.mocks, m.err }
func (m *mockMemDb) GetFilteredMocks() ([]*models.Mock, error)                { return nil, nil }
func (m *mockMemDb) UpdateUnFilteredMock(_ *models.Mock, _ *models.Mock) bool { return false }
func (m *mockMemDb) DeleteFilteredMock(_ models.Mock) bool                    { return false }
func (m *mockMemDb) DeleteUnFilteredMock(_ models.Mock) bool                  { return false }
func (m *mockMemDb) GetMySQLCounts() (total, config, data int)                { return 0, 0, 0 }
func (m *mockMemDb) MarkMockAsUsed(_ models.Mock) bool                        { return false }

func newHTTP() *HTTP {
	return &HTTP{Logger: zap.NewNop()}
}

func makeReq(method, path string) *http.Request {
	return &http.Request{
		Method: method,
		URL:    &url.URL{Path: path},
	}
}

func httpMock(name, method, rawURL string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    rawURL,
			},
		},
	}
}

func TestBuildHTTPMismatchReport_NoMocks(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: nil}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/users"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.Protocol != "HTTP" {
		t.Errorf("expected protocol HTTP, got %s", report.Protocol)
	}
	if report.ClosestMock != "" {
		t.Errorf("expected empty ClosestMock, got %s", report.ClosestMock)
	}
	if report.ActualSummary != "GET /api/users" {
		t.Errorf("expected 'GET /api/users', got %s", report.ActualSummary)
	}
}

func TestBuildHTTPMismatchReport_DbError(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{err: fmt.Errorf("db error")}
	report := h.buildHTTPMismatchReport(makeReq("POST", "/api/items"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.ClosestMock != "" {
		t.Errorf("expected empty ClosestMock on db error, got %s", report.ClosestMock)
	}
	if report.NextSteps != "Failed to read mock database. Check logs for errors and retry." {
		t.Errorf("expected db error next steps, got %s", report.NextSteps)
	}
}

func TestBuildHTTPMismatchReport_NoHTTPMocks(t *testing.T) {
	h := newHTTP()
	// Non-HTTP mock (different Kind)
	db := &mockMemDb{mocks: []*models.Mock{
		{Name: "dns-mock", Kind: "DNS", Spec: models.MockSpec{}},
	}}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/data"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	// No HTTP mocks after filtering, so no closest match
	if report.ClosestMock != "" {
		t.Errorf("expected empty ClosestMock, got %s", report.ClosestMock)
	}
}

func TestBuildHTTPMismatchReport_SameMethodMatch(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{
		httpMock("mock-get-users", "GET", "http://localhost:8080/api/users"),
		httpMock("mock-post-users", "POST", "http://localhost:8080/api/users"),
	}}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/items"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	// Should prefer mock-get-users (same method) over mock-post-users
	if report.ClosestMock != "mock-get-users" {
		t.Errorf("expected closest mock 'mock-get-users', got %s", report.ClosestMock)
	}
	if report.Protocol != "HTTP" {
		t.Errorf("expected protocol HTTP, got %s", report.Protocol)
	}
}

func TestBuildHTTPMismatchReport_DiffMethodFallback(t *testing.T) {
	h := newHTTP()
	// Only POST mocks, request is GET
	db := &mockMemDb{mocks: []*models.Mock{
		httpMock("mock-post-items", "POST", "http://localhost:8080/api/items"),
	}}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/items"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	// Should fall back to different-method mock
	if report.ClosestMock != "mock-post-items" {
		t.Errorf("expected closest mock 'mock-post-items', got %s", report.ClosestMock)
	}
	// Diff should mention method mismatch
	if report.Diff == "" {
		t.Error("expected non-empty diff for method mismatch")
	}
}

func TestBuildHTTPMismatchReport_PathDiff(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{
		httpMock("mock-get-users", "GET", "http://localhost:8080/api/users"),
	}}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/orders"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.ClosestMock != "mock-get-users" {
		t.Errorf("expected 'mock-get-users', got %s", report.ClosestMock)
	}
	// Diff should mention path difference
	if report.Diff == "" || report.Diff == "method and path match but headers or body differ" {
		t.Errorf("expected path diff, got %q", report.Diff)
	}
}

func TestBuildHTTPMismatchReport_ExactPathMethodMatch(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{
		httpMock("mock-get-users", "GET", "http://localhost:8080/api/users"),
	}}
	// Same method and path — diff should indicate headers/body differ
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/users"), db, nil)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.Diff != "method and path match but headers or body differ" {
		t.Errorf("expected headers/body diff message, got %q", report.Diff)
	}
}

// --- Tests for flaky header noise behaviour ---

func TestHeadersContainKeys_FlakyHeaderSkippedByNoise(t *testing.T) {
	// Mock recorded with X-Amz-Security-Token (IRSA on k8s), but the replay
	// request does NOT have it (static creds). With the header in noise, the
	// presence check should be skipped and matching should succeed.
	h := newHTTP()
	mockHeaders := map[string]string{
		"Content-Type":         "application/x-amz-json-1.1",
		"X-Amz-Target":         "secretsmanager.GetSecretValue",
		"Authorization":        "AWS4-HMAC-SHA256 ...",
		"X-Amz-Security-Token": "FwoGZXIvYXdzEBY...",
	}
	actualHeaders := http.Header{
		"Content-Type":  {"application/x-amz-json-1.1"},
		"X-Amz-Target":  {"secretsmanager.GetSecretValue"},
		"Authorization": {"AWS4-HMAC-SHA256 different-sig"},
		// Note: X-Amz-Security-Token is absent
	}

	// Build noise map containing the flaky headers (simulates what decode.go injects)
	noise := make(map[string][]string)
	for _, hdr := range flakyHeaders {
		noise[hdr] = []string{}
	}

	if !h.HeadersContainKeys(mockHeaders, actualHeaders, noise) {
		t.Error("expected match to succeed when flaky headers are in noise, but it failed")
	}
}

func TestHeadersContainKeys_StrictMode_FlakyHeaderCausesMismatch(t *testing.T) {
	// Same setup as above but with empty noise (simulates --disableAutoHeaderNoise).
	// Mock has X-Amz-Security-Token but request doesn't → should fail.
	h := newHTTP()
	mockHeaders := map[string]string{
		"Content-Type":         "application/x-amz-json-1.1",
		"X-Amz-Security-Token": "FwoGZXIvYXdzEBY...",
	}
	actualHeaders := http.Header{
		"Content-Type": {"application/x-amz-json-1.1"},
		// X-Amz-Security-Token absent
	}

	// No noise — strict matching
	if h.HeadersContainKeys(mockHeaders, actualHeaders, nil) {
		t.Error("expected match to fail in strict mode when X-Amz-Security-Token is missing, but it succeeded")
	}
}

func TestHeadersContainKeys_NonFlakyHeaderStillRequired(t *testing.T) {
	// Even with flaky header noise, non-flaky headers must still be present.
	h := newHTTP()
	mockHeaders := map[string]string{
		"Content-Type":  "application/json",
		"X-Custom-App":  "myapp",
		"Authorization": "AWS4-HMAC-SHA256 ...",
	}
	actualHeaders := http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"AWS4-HMAC-SHA256 different"},
		// X-Custom-App is missing — not a flaky header
	}

	noise := make(map[string][]string)
	for _, hdr := range flakyHeaders {
		noise[hdr] = []string{}
	}

	if h.HeadersContainKeys(mockHeaders, actualHeaders, noise) {
		t.Error("expected match to fail when non-flaky header X-Custom-App is missing, but it succeeded")
	}
}

func TestHeadersContainKeys_UserNoisePreserved(t *testing.T) {
	// User-configured noise entries should not be overwritten by flaky header injection.
	h := newHTTP()
	mockHeaders := map[string]string{
		"Content-Type":   "application/json",
		"X-Custom-Nonce": "abc123",
	}
	actualHeaders := http.Header{
		"Content-Type": {"application/json"},
		// X-Custom-Nonce absent — but it's in user noise
	}

	noise := map[string][]string{
		"x-custom-nonce": {}, // user-configured noise
	}
	// Also inject flaky headers on top — should not clobber user entry
	for _, hdr := range flakyHeaders {
		if _, exists := noise[hdr]; !exists {
			noise[hdr] = []string{}
		}
	}

	if !h.HeadersContainKeys(mockHeaders, actualHeaders, noise) {
		t.Error("expected match to succeed when X-Custom-Nonce is in user noise, but it failed")
	}
}

func TestFlakyHeaders_AllLowercase(t *testing.T) {
	// Verify all entries in flakyHeaders are lowercase, since HeadersContainKeys
	// lowercases keys before lookup.
	for _, hdr := range flakyHeaders {
		for _, c := range hdr {
			if c >= 'A' && c <= 'Z' {
				t.Errorf("flakyHeaders entry %q contains uppercase character; all entries must be lowercase", hdr)
				break
			}
		}
	}
}
