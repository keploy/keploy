package http

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// httpMockWithReq builds an HTTP mock with full request details for
// field-diff assertions.
func httpMockWithReq(name, method, rawURL, body string, header map[string]string, reqBodyNoise map[string][]string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method:       models.Method(method),
				URL:          rawURL,
				Body:         body,
				Header:       header,
				ReqBodyNoise: reqBodyNoise,
			},
		},
	}
}

func makeReqWithQuery(method, path, rawQuery string) *http.Request {
	return &http.Request{
		Method: method,
		URL:    &url.URL{Path: path, RawQuery: rawQuery},
		Header: http.Header{},
	}
}

// A schema-match survivor should be preferred for the diff and its body/query
// drift reported field-by-field with noise-vocabulary paths.
func TestBuildHTTPMismatchReport_FieldDiffsAgainstSchemaSurvivor(t *testing.T) {
	h := newHTTP()
	mock := httpMockWithReq("mock-1", "POST", "http://localhost:8080/api/orders?page=1",
		`{"order_id":"o-1","ts":"111"}`, map[string]string{"Content-Type": "application/json"}, nil)
	db := &mockMemDb{mocks: []*models.Mock{mock}}

	request := makeReqWithQuery("POST", "/api/orders", "page=2")
	liveBody := []byte(`{"order_id":"o-2","ts":"111"}`)
	diag := &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{mock}}

	report := h.buildHTTPMismatchReport(request, liveBody, db, nil, nil, nil, diag)
	if report.ClosestMock != "mock-1" {
		t.Fatalf("expected schema survivor as closest, got %q", report.ClosestMock)
	}
	if report.MatchPhase != models.MatchPhaseBody {
		t.Errorf("expected phase %q, got %q", models.MatchPhaseBody, report.MatchPhase)
	}

	paths := map[string]models.MockFieldDiff{}
	for _, d := range report.FieldDiffs {
		paths[d.Path] = d
	}
	if d, ok := paths["body.order_id"]; !ok || d.Expected != "o-1" || d.Actual != "o-2" {
		t.Errorf("expected body.order_id value diff, got %+v", report.FieldDiffs)
	}
	if d, ok := paths["query.page"]; !ok || d.Expected != "1" || d.Actual != "2" {
		t.Errorf("expected query.page value diff, got %+v", report.FieldDiffs)
	}
	if _, ok := paths["body.ts"]; ok {
		t.Errorf("unchanged body.ts must not be reported: %+v", report.FieldDiffs)
	}
}

// The report must record WHICH upstream the missed call targeted (Host first,
// URL authority as fallback) so the log and report can disambiguate the same
// method+path hitting different hosts.
func TestBuildHTTPMismatchReport_RecordsDestination(t *testing.T) {
	h := newHTTP()
	mock := httpMockWithReq("mock-1", "GET", "http://localhost:8080/api/orders", "", nil, nil)
	db := &mockMemDb{mocks: []*models.Mock{mock}}
	diag := &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{mock}}

	// Host header wins.
	req := makeReqWithQuery("GET", "/api/orders", "")
	req.Host = "api.payments.svc:8443"
	if got := h.buildHTTPMismatchReport(req, nil, db, nil, nil, nil, diag).Destination; got != "api.payments.svc:8443" {
		t.Fatalf("expected destination from Host header, got %q", got)
	}

	// Falls back to the URL authority when Host is unset.
	req = makeReqWithQuery("GET", "/api/orders", "")
	req.URL.Host = "inventory.internal:9000"
	if got := h.buildHTTPMismatchReport(req, nil, db, nil, nil, nil, diag).Destination; got != "inventory.internal:9000" {
		t.Fatalf("expected destination from URL authority fallback, got %q", got)
	}

	// A nil URL (hand-built request reaching the error path) must not panic; the
	// destination still comes from the Host header.
	nilURLReq := &http.Request{Method: "GET", Host: "api.example.com", Header: http.Header{}}
	if got := h.buildHTTPMismatchReport(nilURLReq, nil, db, nil, nil, nil, diag).Destination; got != "api.example.com" {
		t.Fatalf("nil-URL request: expected destination from Host, got %q", got)
	}
}

// Fields covered by learned req_body_noise or user body noise must not be
// flagged in the report — the report should never tell the user to fix a
// field the matcher already ignores.
func TestBuildHTTPMismatchReport_RespectsLearnedAndUserNoise(t *testing.T) {
	h := newHTTP()
	mock := httpMockWithReq("mock-1", "POST", "http://localhost:8080/api/orders",
		`{"request_id":"r-1","ts":"111","real":"x"}`, nil,
		map[string][]string{"body.request_id": {}})
	db := &mockMemDb{mocks: []*models.Mock{mock}}

	request := makeReqWithQuery("POST", "/api/orders", "")
	liveBody := []byte(`{"request_id":"r-2","ts":"222","real":"y"}`)
	userBodyNoise := map[string][]string{"ts": {}}
	diag := &matchDiag{phase: models.MatchPhaseStrict, candidates: 1, schemaMatched: []*models.Mock{mock}}

	report := h.buildHTTPMismatchReport(request, liveBody, db, nil, nil, userBodyNoise, diag)
	var gotPaths []string
	for _, d := range report.FieldDiffs {
		gotPaths = append(gotPaths, d.Path)
	}
	joined := strings.Join(gotPaths, ",")
	if strings.Contains(joined, "request_id") {
		t.Errorf("learned-noise field reported: %v", gotPaths)
	}
	if strings.Contains(joined, "body.ts") {
		t.Errorf("user-noise field reported: %v", gotPaths)
	}
	if !strings.Contains(joined, "body.real") {
		t.Errorf("genuinely drifted field body.real missing: %v", gotPaths)
	}
}

// User-configured body noise must thread into detectReqBodyNoise so manual
// noise config and learned noise share one vocabulary.
func TestDetectReqBodyNoise_UserNoiseExcluded(t *testing.T) {
	h := newHTTP()
	mock := httpMockWithReq("mock-1", "POST", "http://x/y",
		`{"ts":"111","real":"a"}`, map[string]string{"Content-Type": "application/json"}, nil)

	got := h.detectReqBodyNoise(true, mock, []byte(`{"ts":"222","real":"b"}`), map[string][]string{"ts": {}})
	if _, ok := got["body.ts"]; ok {
		t.Errorf("user-noised field must not be re-learned as noise: %+v", got)
	}
	if _, ok := got["body.real"]; !ok {
		t.Errorf("drifting non-noise field should be detected: %+v", got)
	}
}

// Strict filtering must treat user-configured body noise as allowed drift.
func TestFilterStrictNoiseMatches_UserNoiseAllowsDrift(t *testing.T) {
	h := newHTTP()
	// Mock carries learned noise (so strict applies) on another field.
	mock := httpMockWithReq("mock-1", "POST", "http://x/y",
		`{"request_id":"r-1","ts":"111"}`, map[string]string{"Content-Type": "application/json"},
		map[string][]string{"body.request_id": {}})

	// ts drifted; without user noise the candidate must be rejected.
	live := []byte(`{"request_id":"r-9","ts":"222"}`)
	if kept := h.filterStrictNoiseMatches([]*models.Mock{mock}, live, nil); len(kept) != 0 {
		t.Fatalf("expected strict rejection without user noise, kept %d", len(kept))
	}
	// With user noise on ts the drift is allowed.
	if kept := h.filterStrictNoiseMatches([]*models.Mock{mock}, live, map[string][]string{"ts": {}}); len(kept) != 1 {
		t.Fatalf("expected candidate kept with user noise, kept %d", len(kept))
	}
}
