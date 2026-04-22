package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockMemDb is a minimal mock of the MockMemDb interface for testing.
// updateUnFilteredMockOld / updateUnFilteredMockNew capture the most
// recent (old, new) pair handed to UpdateUnFilteredMock so tests can
// assert on the arguments.
// updateUnFilteredReturn is the value returned to the caller.
// deletedFiltered captures the argument to DeleteFilteredMock; its
// return value is driven by deleteFilteredReturn.
type mockMemDb struct {
	mocks                   []*models.Mock
	err                     error
	updateUnFilteredMockOld *models.Mock
	updateUnFilteredMockNew *models.Mock
	updateUnFilteredReturn  bool
	deletedFiltered         *models.Mock
	deleteFilteredReturn    bool
}

func (m *mockMemDb) GetUnFilteredMocks() ([]*models.Mock, error) { return m.mocks, m.err }
func (m *mockMemDb) GetFilteredMocks() ([]*models.Mock, error)   { return nil, nil }
func (m *mockMemDb) UpdateUnFilteredMock(oldMock *models.Mock, newMock *models.Mock) bool {
	m.updateUnFilteredMockOld = oldMock
	m.updateUnFilteredMockNew = newMock
	return m.updateUnFilteredReturn
}
func (m *mockMemDb) DeleteFilteredMock(mock models.Mock) bool {
	m.deletedFiltered = &mock
	return m.deleteFilteredReturn
}
func (m *mockMemDb) DeleteUnFilteredMock(_ models.Mock) bool             { return false }
func (m *mockMemDb) GetMySQLCounts() (total, config, data int)           { return 0, 0, 0 }
func (m *mockMemDb) MarkMockAsUsed(_ models.Mock) bool                   { return false }
func (m *mockMemDb) SetCurrentTestWindow(_, _ time.Time)                 {}
func (m *mockMemDb) IsTestWindowActive() bool                            { return false }
func (m *mockMemDb) GetFilteredMocksInWindow() ([]*models.Mock, error)   { return nil, m.err }
func (m *mockMemDb) GetPerTestMocksInWindow() ([]*models.Mock, error)    { return nil, m.err }
func (m *mockMemDb) GetSessionMocks() ([]*models.Mock, error)            { return m.mocks, m.err }
func (m *mockMemDb) GetStartupMocks() ([]*models.Mock, error)            { return nil, nil }
func (m *mockMemDb) GetSessionScopedMocks() ([]*models.Mock, error)      { return m.mocks, m.err }
func (m *mockMemDb) HasFirstTestFired() bool                             { return false }
func (m *mockMemDb) WindowSnapshot() models.WindowSnapshot                { return models.WindowSnapshot{} }
func (m *mockMemDb) GetConnectionMocks(_ string) ([]*models.Mock, error) { return nil, nil }
func (m *mockMemDb) SessionMockHitCounts() map[string]uint64             { return nil }

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

// --- Tests for noise-aware matching ---

// noiseFor builds exact-match anchored regex patterns for the given values,
// mirroring what the enterprise obfuscator produces for Mock.Noise.
func noiseFor(values ...string) []string {
	patterns := make([]string, len(values))
	for i, v := range values {
		patterns[i] = "^" + regexp.QuoteMeta(v) + "$"
	}
	return patterns
}

func TestJSONBodyMatchScore_NoNoise(t *testing.T) {
	mockData := map[string]interface{}{"name": "john", "age": float64(25)}
	reqData := map[string]interface{}{"name": "john", "age": float64(25)}
	nc := util.NewNoiseChecker(nil)
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 2 || total != 2 || noisy != 0 {
		t.Errorf("expected matched=2 total=2 noisy=0, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_AllNoisy(t *testing.T) {
	mockData := map[string]interface{}{
		"token":    "KEPLOYREDACTabc",
		"password": "KEPLOYREDACTdef",
	}
	reqData := map[string]interface{}{
		"token":    "real_token",
		"password": "real_password",
	}
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTabc", "KEPLOYREDACTdef"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 0 || total != 0 || noisy != 2 {
		t.Errorf("expected matched=0 total=0 noisy=2, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_MixedNoise(t *testing.T) {
	mockData := map[string]interface{}{
		"username": "john",
		"password": "KEPLOYREDACTabc123",
		"age":      float64(25),
	}
	reqData := map[string]interface{}{
		"username": "john",
		"password": "secret123",
		"age":      float64(25),
	}
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTabc123"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 2 || total != 2 || noisy != 1 {
		t.Errorf("expected matched=2 total=2 noisy=1, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_PartialMismatch(t *testing.T) {
	mockData := map[string]interface{}{
		"username": "john",
		"password": "KEPLOYREDACTabc123",
		"age":      float64(25),
	}
	reqData := map[string]interface{}{
		"username": "jane", // different
		"password": "secret123",
		"age":      float64(25),
	}
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTabc123"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 1 || total != 2 || noisy != 1 {
		t.Errorf("expected matched=1 total=2 noisy=1, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_NestedNoise(t *testing.T) {
	mockData := map[string]interface{}{
		"user": map[string]interface{}{
			"name":    "john",
			"api_key": "KEPLOYREDACTxyz789",
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
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTxyz789"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 2 || total != 2 || noisy != 1 {
		t.Errorf("expected matched=2 total=2 noisy=1, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_ArrayWithNoise(t *testing.T) {
	mockData := []interface{}{
		"public_value",
		"KEPLOYREDACTsecret",
		"another_public",
	}
	reqData := []interface{}{
		"public_value",
		"actual_secret",
		"another_public",
	}
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTsecret"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 2 || total != 2 || noisy != 1 {
		t.Errorf("expected matched=2 total=2 noisy=1, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_MissingKey(t *testing.T) {
	mockData := map[string]interface{}{
		"name":  "john",
		"email": "john@example.com",
	}
	reqData := map[string]interface{}{
		"name": "john",
	}
	nc := util.NewNoiseChecker(nil)
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 1 || total != 2 || noisy != 0 {
		t.Errorf("expected matched=1 total=2 noisy=0, got %d/%d/%d", matched, total, noisy)
	}
}

func TestJSONBodyMatchScore_DigitOnlyNoise(t *testing.T) {
	// Digit-only obfuscated values have no prefix — noise is the only way to detect them
	mockData := map[string]interface{}{
		"username": "john",
		"otp":      "317508",
	}
	reqData := map[string]interface{}{
		"username": "john",
		"otp":      "849372",
	}
	nc := util.NewNoiseChecker(noiseFor("317508"))
	matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)
	if matched != 1 || total != 1 || noisy != 1 {
		t.Errorf("expected matched=1 total=1 noisy=1, got %d/%d/%d", matched, total, noisy)
	}
}

func TestExactBodyMatch_NoisyFullMatch(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name:  "mock-noisy",
			Kind:  models.Kind(models.HTTP),
			Noise: noiseFor("KEPLOYREDACTabc123"),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"username":"john","password":"KEPLOYREDACTabc123","age":25}`,
				},
			},
		},
	}
	reqBody := []byte(`{"username":"john","password":"real_password","age":25}`)
	ok, match := h.ExactBodyMatch(reqBody, mocks)
	if !ok {
		t.Fatal("expected noise-aware match to succeed")
	}
	if match.Name != "mock-noisy" {
		t.Errorf("expected mock-noisy, got %s", match.Name)
	}
}

func TestExactBodyMatch_NoisyPartialMismatch(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name:  "mock-noisy",
			Kind:  models.Kind(models.HTTP),
			Noise: noiseFor("KEPLOYREDACTabc123"),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"username":"john","password":"KEPLOYREDACTabc123"}`,
				},
			},
		},
	}
	reqBody := []byte(`{"username":"jane","password":"real_password"}`)
	ok, _ := h.ExactBodyMatch(reqBody, mocks)
	if ok {
		t.Error("expected no match when non-noisy field differs")
	}
}

func TestExactBodyMatch_PreferExactOverNoisy(t *testing.T) {
	h := newHTTP()
	mocks := []*models.Mock{
		{
			Name:  "mock-noisy",
			Kind:  models.Kind(models.HTTP),
			Noise: noiseFor("KEPLOYREDACTabc"),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"name":"KEPLOYREDACTabc"}`,
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
	if match.Name != "mock-exact" {
		t.Errorf("expected mock-exact (exact match preferred), got %s", match.Name)
	}
}

func TestExactBodyMatch_FullyNoisyBody(t *testing.T) {
	h := newHTTP()
	noisyBody := "KEPLOYREDACTentire_body_redacted"
	mocks := []*models.Mock{
		{
			Name:  "mock-full-noisy",
			Kind:  models.Kind(models.HTTP),
			Noise: noiseFor(noisyBody),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: noisyBody,
				},
			},
		},
	}
	reqBody := []byte(`anything goes here`)
	ok, match := h.ExactBodyMatch(reqBody, mocks)
	if !ok {
		t.Fatal("expected fully noisy body to auto-match")
	}
	if match.Name != "mock-full-noisy" {
		t.Errorf("expected mock-full-noisy, got %s", match.Name)
	}
}

func TestExactBodyMatch_NoNoisePatterns(t *testing.T) {
	h := newHTTP()
	// Mock has no Noise patterns — second pass should skip it
	mocks := []*models.Mock{
		{
			Name: "mock-no-noise",
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Body: `{"name":"john"}`,
				},
			},
		},
	}
	reqBody := []byte(`{"name":"jane"}`)
	ok, _ := h.ExactBodyMatch(reqBody, mocks)
	if ok {
		t.Error("expected no match when bodies differ and no noise patterns")
	}
}

func TestStripNoisyJSON_RemovesNoisyFields(t *testing.T) {
	input := `{"name":"john","secret":"KEPLOYREDACTabc","age":25}`
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTabc"))
	result := util.StripNoisyJSON(input, nc)

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

func TestStripNoisyJSON_NoNoise(t *testing.T) {
	input := `{"name":"john","age":25}`
	nc := util.NewNoiseChecker(nil)
	result := util.StripNoisyJSON(input, nc)
	if result != input {
		t.Errorf("expected unchanged body, got %s", result)
	}
}

func TestStripNoisyJSON_NonJSON(t *testing.T) {
	input := "plain text body"
	nc := util.NewNoiseChecker(noiseFor("anything"))
	result := util.StripNoisyJSON(input, nc)
	if result != input {
		t.Errorf("expected unchanged non-JSON body, got %s", result)
	}
}

func TestStripNoisyJSON_Nested(t *testing.T) {
	input := `{"user":{"name":"john","token":"KEPLOYREDACTxyz"},"active":true}`
	nc := util.NewNoiseChecker(noiseFor("KEPLOYREDACTxyz"))
	result := util.StripNoisyJSON(input, nc)

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

// TestUpdateMock_DoesNotMutatePoolPointer guards the fix for the
// proxy-stress-test data race: updateMock must never mutate the
// shared *models.Mock pointer handed to it. It must clone the mock,
// mutate the clone, and pass (old=matchedMock, new=&clone) to
// MockMemDb.UpdateUnFilteredMock. If this invariant regresses,
// concurrent HTTP requests matching the same session-lifetime mock
// would again race on TestModeInfo.IsFiltered / SortOrder — exactly
// the race detector output that motivated match.go's rewrite.
func TestUpdateMock_DoesNotMutatePoolPointer(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{updateUnFilteredReturn: true}

	original := &models.Mock{
		Name: "session-mock",
		Kind: models.Kind(models.HTTP),
		TestModeInfo: models.TestModeInfo{
			IsFiltered: true,
			SortOrder:  42,
			Lifetime:   models.LifetimeSession,
		},
	}
	beforeIsFiltered := original.TestModeInfo.IsFiltered
	beforeSortOrder := original.TestModeInfo.SortOrder

	if ok := h.updateMock(context.TODO(), original, db); !ok {
		t.Fatalf("updateMock returned false; expected true from stubbed UpdateUnFilteredMock")
	}

	// The pool pointer must be untouched — a future reader of this
	// same *Mock from the shared pool must still see the original
	// TestModeInfo.
	if original.TestModeInfo.IsFiltered != beforeIsFiltered {
		t.Errorf("updateMock mutated original.TestModeInfo.IsFiltered: before=%v after=%v",
			beforeIsFiltered, original.TestModeInfo.IsFiltered)
	}
	if original.TestModeInfo.SortOrder != beforeSortOrder {
		t.Errorf("updateMock mutated original.TestModeInfo.SortOrder: before=%v after=%v",
			beforeSortOrder, original.TestModeInfo.SortOrder)
	}

	// UpdateUnFilteredMock must have received the untouched original
	// as "old" and a freshly-allocated clone with updated
	// TestModeInfo fields as "new".
	if db.updateUnFilteredMockOld != original {
		t.Errorf("expected old arg to be the original pool pointer, got a different pointer")
	}
	if db.updateUnFilteredMockNew == nil {
		t.Fatal("expected new arg to be non-nil")
	}
	if db.updateUnFilteredMockNew == original {
		t.Error("new arg must not alias the pool pointer; updateMock is expected to allocate a copy")
	}
	if db.updateUnFilteredMockNew.TestModeInfo.IsFiltered {
		t.Error("new.TestModeInfo.IsFiltered expected to be false after updateMock")
	}
	if db.updateUnFilteredMockNew.TestModeInfo.SortOrder == beforeSortOrder {
		t.Errorf("new.TestModeInfo.SortOrder expected to advance from %d via GetNextSortNum; got %d",
			beforeSortOrder, db.updateUnFilteredMockNew.TestModeInfo.SortOrder)
	}
}

// TestUpdateMock_PerTestPrefersDelete verifies the per-test routing:
// a per-test (non-config) mock is consumed via DeleteFilteredMock
// rather than UpdateUnFilteredMock, and the DeleteFilteredMock
// argument is a value-copy of the original (not the updated clone).
func TestUpdateMock_PerTestPrefersDelete(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{deleteFilteredReturn: true}

	original := &models.Mock{
		Name: "per-test-mock",
		Kind: models.Kind(models.HTTP),
		TestModeInfo: models.TestModeInfo{
			IsFiltered: true,
			SortOrder:  7,
			Lifetime:   models.LifetimePerTest,
		},
	}

	if ok := h.updateMock(context.TODO(), original, db); !ok {
		t.Fatalf("updateMock returned false; expected true from stubbed DeleteFilteredMock")
	}
	if db.deletedFiltered == nil {
		t.Fatal("expected DeleteFilteredMock to be called for per-test lifetime")
	}
	if db.deletedFiltered.Name != original.Name {
		t.Errorf("DeleteFilteredMock received wrong mock: got name=%q want %q",
			db.deletedFiltered.Name, original.Name)
	}
	// Per-test routing must NOT have reached UpdateUnFilteredMock.
	if db.updateUnFilteredMockOld != nil || db.updateUnFilteredMockNew != nil {
		t.Error("UpdateUnFilteredMock should not be called when DeleteFilteredMock succeeds")
	}
	// Pool pointer untouched.
	if !original.TestModeInfo.IsFiltered || original.TestModeInfo.SortOrder != 7 {
		t.Errorf("updateMock mutated the pool pointer on the per-test path: %+v",
			original.TestModeInfo)
	}
}
