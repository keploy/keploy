package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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

// TestMatch_NoCorrelationMetadataServesStale: an identical mock WITHOUT the
// correlation annotation does not render — the annotation is what drives it.
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
