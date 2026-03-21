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
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/users"), db)

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
	report := h.buildHTTPMismatchReport(makeReq("POST", "/api/items"), db)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.ClosestMock != "" {
		t.Errorf("expected empty ClosestMock on db error, got %s", report.ClosestMock)
	}
}

func TestBuildHTTPMismatchReport_NoHTTPMocks(t *testing.T) {
	h := newHTTP()
	// Non-HTTP mock (different Kind)
	db := &mockMemDb{mocks: []*models.Mock{
		{Name: "dns-mock", Kind: "DNS", Spec: models.MockSpec{}},
	}}
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/data"), db)

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
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/items"), db)

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
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/items"), db)

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
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/orders"), db)

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
	report := h.buildHTTPMismatchReport(makeReq("GET", "/api/users"), db)

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.Diff != "method and path match but headers or body differ" {
		t.Errorf("expected headers/body diff message, got %q", report.Diff)
	}
}
