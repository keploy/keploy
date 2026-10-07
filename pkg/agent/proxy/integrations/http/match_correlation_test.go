package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

const (
	recUUID = "550e8400-e29b-41d4-a716-446655440000"
	newUUID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
)

func jsonReq(method, path, body string) *req {
	return &req{
		method: method,
		url:    &url.URL{Path: path},
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   []byte(body),
		raw:    []byte(body),
	}
}

// correlationMock: a dependency mock whose request carries an app-minted uuid
// (idempotencyKey) that its response echoes, annotated with the correlation.
func correlationMock(recorded string, withCorrelation bool) *models.Mock {
	m := &models.Mock{
		Name: "pay",
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method("POST"),
				URL:    "http://api/pay",
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   `{"amount":10,"idempotencyKey":"` + recorded + `"}`,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       `{"status":"ok","idempotencyKey":"` + recorded + `"}`,
			},
		},
	}
	if withCorrelation {
		m.Spec.Correlations = []models.FieldCorrelation{{
			RequestPath:   "body.idempotencyKey",
			ResponsePaths: []string{"body.idempotencyKey"},
			RecordedValue: recorded,
			ValueClass:    "uuid",
		}}
	}
	return m
}

// TestMatch_CorrelationHonorsLiveValue: a correlated mock matches a live request
// that carries a NEW uuid, and the served response echoes the NEW uuid, not the
// recorded one.
func TestMatch_CorrelationHonorsLiveValue(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{correlationMock(recUUID, true)}, updateUnFilteredReturn: true}
	live := jsonReq("POST", "/pay", `{"amount":10,"idempotencyKey":"`+newUUID+`"}`)

	ok, stub, _, err := h.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub == nil {
		t.Fatalf("correlated mock should match a new-uuid request: ok=%v stub=%v err=%v", ok, stub, err)
	}
	if !strings.Contains(stub.Spec.HTTPResp.Body, newUUID) {
		t.Fatalf("served response must carry the LIVE uuid %q; got %q", newUUID, stub.Spec.HTTPResp.Body)
	}
	if strings.Contains(stub.Spec.HTTPResp.Body, recUUID) {
		t.Fatalf("served response must NOT carry the recorded uuid %q; got %q", recUUID, stub.Spec.HTTPResp.Body)
	}
}

// TestMatch_CorrelationDisabledServesStale (load-bearing): with correlation off,
// the same mock must not render the live value — proving the honor path is what
// does the work.
func TestMatch_CorrelationDisabledServesStale(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{correlationMock(recUUID, true)}, updateUnFilteredReturn: true}
	live := jsonReq("POST", "/pay", `{"amount":10,"idempotencyKey":"`+newUUID+`"}`)

	// Last arg mockCorrelation=false.
	ok, stub, _, err := h.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok && stub != nil && strings.Contains(stub.Spec.HTTPResp.Body, newUUID) {
		t.Fatalf("with correlation disabled the response must not carry the live uuid; got %q", stub.Spec.HTTPResp.Body)
	}
}

// TestMatch_DetectThenHonor: a mock with only raw bodies (no correlation
// annotation) is run through the replay-ingest detector (MaterializeCorrelations),
// then honored — tying the detector to the matcher end-to-end.
func TestMatch_DetectThenHonor(t *testing.T) {
	h := newHTTP()
	m := correlationMock(recUUID, false) // no Correlations annotation
	mocknoise.MaterializeCorrelations(m) // the replay-ingest step populates it
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
	live := jsonReq("POST", "/pay", `{"amount":10,"idempotencyKey":"`+newUUID+`"}`)

	ok, stub, _, err := h.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub == nil {
		t.Fatalf("detect+honor should match: ok=%v stub=%v err=%v", ok, stub, err)
	}
	if !strings.Contains(stub.Spec.HTTPResp.Body, newUUID) || strings.Contains(stub.Spec.HTTPResp.Body, recUUID) {
		t.Fatalf("detect+honor must render the live uuid; got %q", stub.Spec.HTTPResp.Body)
	}
}

// corrMockUser: a correlated mock whose request has a non-correlated userId and
// a correlated id echoed in the response.
func corrMockUser(id, user string) *models.Mock {
	return &models.Mock{
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: models.Method("POST"), URL: "http://api/u", Body: `{"id":"` + id + `","userId":"` + user + `"}`},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"id":"` + id + `","user":"` + user + `"}`},
			Correlations: []models.FieldCorrelation{{
				RequestPath: "body.id", ResponsePaths: []string{"body.id"}, RecordedValue: id, ValueClass: "uuid",
			}},
		},
	}
}

// TestCorrelationMatch_ExactPathNotSubstring is the regression for the substring
// poisoning blocker: a correlated field named "id" must NOT swallow "userId".
// Two mocks differ only on userId (alice/bob); a bob request must select the bob
// mock, never alice's.
func TestCorrelationMatch_ExactPathNotSubstring(t *testing.T) {
	h := newHTTP()
	const bobRec = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
	alice := corrMockUser(recUUID, "alice")
	bob := corrMockUser(bobRec, "bob")
	live := []byte(`{"id":"` + newUUID + `","userId":"bob"}`)

	ok, m, bindings := h.correlationMatch(live, []*models.Mock{alice, bob}, true)
	if !ok || m != bob {
		t.Fatalf("a bob request must select the bob mock, not alice (substring-poisoning blocker): ok=%v selected=%p bob=%p", ok, m, bob)
	}
	if bindings["body.id"] != newUUID {
		t.Fatalf("captured id = %q, want %q", bindings["body.id"], newUUID)
	}
}

// TestCorrelationMatch_RejectsExtraField is the regression for addition-blindness:
// a live request with an extra non-correlated field must NOT match a correlated
// mock that lacks it.
func TestCorrelationMatch_RejectsExtraField(t *testing.T) {
	h := newHTTP()
	m := correlationMock(recUUID, true) // req {"amount":10,"idempotencyKey":<uuid>}
	liveExtra := []byte(`{"amount":10,"idempotencyKey":"` + newUUID + `","refund":true}`)
	if ok, _, _ := h.correlationMatch(liveExtra, []*models.Mock{m}, true); ok {
		t.Fatalf("a live request with an extra field must not match a correlated mock that lacks it")
	}
}

// TestCorrelationMatch_RejectsBareScalar: a bare-scalar body (flattens to the ""
// path) must not be correlated (ignoring "" would exclude the whole body).
func TestCorrelationMatch_RejectsBareScalar(t *testing.T) {
	h := newHTTP()
	m := &models.Mock{
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:      &models.HTTPReq{Method: models.Method("POST"), URL: "http://api/x", Body: `"` + recUUID + `"`},
			HTTPResp:     &models.HTTPResp{StatusCode: 200, Body: `{"echo":"` + recUUID + `"}`},
			Correlations: []models.FieldCorrelation{{RequestPath: "body.", RecordedValue: recUUID, ValueClass: "uuid"}},
		},
	}
	if ok, _, _ := h.correlationMatch([]byte(`"`+newUUID+`"`), []*models.Mock{m}, true); ok {
		t.Fatalf("a bare-scalar correlated body must not match (would exclude the whole body)")
	}
}

// TestMatch_NoCorrelationMetadataServesStale: an identical mock WITHOUT the
// correlation annotation AND without materialization does not render — the
// annotation is what drives it.
func TestMatch_NoCorrelationMetadataServesStale(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{correlationMock(recUUID, false)}, updateUnFilteredReturn: true}
	live := jsonReq("POST", "/pay", `{"amount":10,"idempotencyKey":"`+newUUID+`"}`)

	ok, stub, _, err := h.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok && stub != nil && strings.Contains(stub.Spec.HTTPResp.Body, newUUID) {
		t.Fatalf("without correlation metadata the response must not carry the live uuid; got %q", stub.Spec.HTTPResp.Body)
	}
}
