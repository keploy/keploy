package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
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

// --- Tests for obfuscation-aware matching ---

func TestJsonBodyMatchScore_NoObfuscation(t *testing.T) {
	mockData := map[string]interface{}{"name": "john", "age": float64(25)}
	reqData := map[string]interface{}{"name": "john", "age": float64(25)}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	if matched != 2 || total != 2 || obfuscated != 0 {
		t.Errorf("expected matched=2 total=2 obfuscated=0, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_AllObfuscated(t *testing.T) {
	mockData := map[string]interface{}{
		"token":    util.ObfuscationPrefix + "abc",
		"password": util.ObfuscationPrefix + "def",
	}
	reqData := map[string]interface{}{
		"token":    "real_token",
		"password": "real_password",
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// All fields obfuscated — excluded from matched/total entirely
	if matched != 0 || total != 0 || obfuscated != 2 {
		t.Errorf("expected matched=0 total=0 obfuscated=2, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_MixedObfuscation(t *testing.T) {
	mockData := map[string]interface{}{
		"username": "john",
		"password": util.ObfuscationPrefix + "abc123",
		"age":      float64(25),
	}
	reqData := map[string]interface{}{
		"username": "john",
		"password": "secret123",
		"age":      float64(25),
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// username: match (1/1), password: obfuscated (skipped), age: match (1/1)
	// → matched=2, total=2, obfuscated=1 → 100%
	if matched != 2 || total != 2 || obfuscated != 1 {
		t.Errorf("expected matched=2 total=2 obfuscated=1, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_PartialMismatch(t *testing.T) {
	mockData := map[string]interface{}{
		"username": "john",
		"password": util.ObfuscationPrefix + "abc123",
		"age":      float64(25),
	}
	reqData := map[string]interface{}{
		"username": "jane", // different
		"password": "secret123",
		"age":      float64(25),
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// username: no match (0/1), password: obfuscated (skipped), age: match (1/1)
	// → matched=1, total=2, obfuscated=1 → 50%
	if matched != 1 || total != 2 || obfuscated != 1 {
		t.Errorf("expected matched=1 total=2 obfuscated=1, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_NestedObfuscation(t *testing.T) {
	mockData := map[string]interface{}{
		"user": map[string]interface{}{
			"name":    "john",
			"api_key": util.ObfuscationPrefix + "xyz789",
		},
		"active": true,
	}
	reqData := map[string]interface{}{
		"user": map[string]interface{}{
			"name":    "john",
			"api_key": "real_key_456",
		},
		"active": true,
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// user.name: match (1/1), user.api_key: obfuscated (skipped), active: match (1/1)
	// → matched=2, total=2, obfuscated=1 → 100%
	if matched != 2 || total != 2 || obfuscated != 1 {
		t.Errorf("expected matched=2 total=2 obfuscated=1, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_ArrayWithObfuscation(t *testing.T) {
	mockData := []interface{}{
		"public_value",
		util.ObfuscationPrefix + "secret",
		"another_public",
	}
	reqData := []interface{}{
		"public_value",
		"actual_secret",
		"another_public",
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// index 0: match (1/1), index 1: obfuscated (skipped), index 2: match (1/1)
	// → matched=2, total=2, obfuscated=1 → 100%
	if matched != 2 || total != 2 || obfuscated != 1 {
		t.Errorf("expected matched=2 total=2 obfuscated=1, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestJsonBodyMatchScore_MissingKey(t *testing.T) {
	mockData := map[string]interface{}{
		"name":  "john",
		"email": "john@example.com",
	}
	reqData := map[string]interface{}{
		"name": "john",
		// email missing
	}
	matched, total, obfuscated := jsonBodyMatchScore(mockData, reqData)
	// name: match, email: missing (not matched)
	if matched != 1 || total != 2 || obfuscated != 0 {
		t.Errorf("expected matched=1 total=2 obfuscated=0, got %d/%d/%d", matched, total, obfuscated)
	}
}

func TestExactBodyMatch_ObfuscatedFullMatch(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name: "mock-obf",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"username":"john","password":"` + util.ObfuscationPrefix + `abc123","age":25}`,
				},
			},
		},
	}
	reqBody := []byte(`{"username":"john","password":"real_password","age":25}`)
	ok, match := h.ExactBodyMatch(reqBody, mocks)
	if !ok {
		t.Fatal("expected obfuscation-aware match to succeed")
	}
	if match.Name != "mock-obf" {
		t.Errorf("expected mock-obf, got %s", match.Name)
	}
}

func TestExactBodyMatch_ObfuscatedPartialMismatch(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name: "mock-obf",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"username":"john","password":"` + util.ObfuscationPrefix + `abc123"}`,
				},
			},
		},
	}
	// username differs — should NOT match even though password is obfuscated
	reqBody := []byte(`{"username":"jane","password":"real_password"}`)
	ok, _ := h.ExactBodyMatch(reqBody, mocks)
	if ok {
		t.Error("expected no match when non-obfuscated field differs")
	}
}

func TestExactBodyMatch_PreferExactOverObfuscated(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name: "mock-obf",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"name":"` + util.ObfuscationPrefix + `abc"}`,
				},
			},
		},
		{
			Name: "mock-exact",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"name":"john"}`,
				},
			},
		},
	}
	reqBody := []byte(`{"name":"john"}`)
	ok, match := h.ExactBodyMatch(reqBody, mocks)
	if !ok {
		t.Fatal("expected match")
	}
	// Exact match should be preferred over obfuscation-aware match
	if match.Name != "mock-exact" {
		t.Errorf("expected mock-exact (exact match preferred), got %s", match.Name)
	}
}

func TestExactBodyMatch_FullyObfuscatedBody(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name: "mock-full-obf",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: util.ObfuscationPrefix + "entire_body_redacted",
				},
			},
		},
	}
	reqBody := []byte(`anything goes here`)
	ok, match := h.ExactBodyMatch(reqBody, mocks)
	if !ok {
		t.Fatal("expected fully obfuscated body to auto-match")
	}
	if match.Name != "mock-full-obf" {
		t.Errorf("expected mock-full-obf, got %s", match.Name)
	}
}

func TestStripObfuscatedJSON_RemovesRedactedFields(t *testing.T) {
	input := `{"name":"john","secret":"` + util.ObfuscationPrefix + `abc","age":25}`
	result := stripObfuscatedJSON(input)

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(result), &data); err != nil {
		t.Fatal(err)
	}
	if _, exists := data["secret"]; exists {
		t.Error("expected 'secret' key to be stripped")
	}
	if data["name"] != "john" {
		t.Errorf("expected name=john, got %v", data["name"])
	}
	if data["age"] != float64(25) {
		t.Errorf("expected age=25, got %v", data["age"])
	}
}

func TestStripObfuscatedJSON_NoObfuscation(t *testing.T) {
	input := `{"name":"john","age":25}`
	result := stripObfuscatedJSON(input)
	if result != input {
		t.Errorf("expected unchanged body, got %s", result)
	}
}

func TestStripObfuscatedJSON_NonJSON(t *testing.T) {
	input := "plain text body with " + util.ObfuscationPrefix + "value"
	result := stripObfuscatedJSON(input)
	if result != input {
		t.Errorf("expected unchanged non-JSON body, got %s", result)
	}
}

func TestStripObfuscatedJSON_Nested(t *testing.T) {
	input := `{"user":{"name":"john","token":"` + util.ObfuscationPrefix + `xyz"},"active":true}`
	result := stripObfuscatedJSON(input)

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(result), &data); err != nil {
		t.Fatal(err)
	}
	user, ok := data["user"].(map[string]interface{})
	if !ok {
		t.Fatal("expected user to be a map")
	}
	if _, exists := user["token"]; exists {
		t.Error("expected nested 'token' to be stripped")
	}
	if user["name"] != "john" {
		t.Errorf("expected name=john, got %v", user["name"])
	}
	if data["active"] != true {
		t.Errorf("expected active=true, got %v", data["active"])
	}
}
