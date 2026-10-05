package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
)

func chargeMock() *models.Mock {
	m := httpMock("charge", "POST", "http://payments/charge")
	m.Spec.HTTPReq.Header = map[string]string{"Content-Type": "application/json"}
	m.Spec.HTTPReq.Body = `{"amount":100,"currency":"USD","ts":1}`
	m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func chargeReq(body string) *req {
	return &req{
		method: "POST",
		url:    &url.URL{Path: "/charge"},
		header: http.Header{"Content-Type": {"application/json"}},
		body:   []byte(body),
	}
}

// A mock served although the request changed carries the drift to the mock
// manager (the consumed copy), so the replay can report it as a DriftedCall.
func TestMatch_ServedMockCarriesItsRequestDrift(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{chargeMock()}, deleteFilteredReturn: true}
	ok, _, _, err := h.match(context.Background(), chargeReq(`{"amount":250,"currency":"USD","ts":2}`), db, nil, nil, nil, true, false, false)
	if err != nil || !ok {
		t.Fatalf("lenient matching serves the drifted mock: ok=%v err=%v", ok, err)
	}
	if db.deletedFiltered == nil {
		t.Fatal("the per-test mock was not consumed")
	}
	got := db.deletedFiltered.ServedRequestDrift
	if len(got) != 1 || got[0].Path != "body.amount" || got[0].Expected != "100" || got[0].Actual != "250" {
		t.Fatalf("consumed copy should carry body.amount 100 -> 250; got %+v", got)
	}
}

// With detection on (the auto-replay that learns noise) nothing is reported:
// the drift becomes learned noise instead.
func TestMatch_NoServedDriftWhileDetectionLearns(t *testing.T) {
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{chargeMock()}, deleteFilteredReturn: true}
	ok, _, _, err := h.match(context.Background(), chargeReq(`{"amount":250,"currency":"USD","ts":2}`), db, nil, nil, nil, true, true, false)
	if err != nil || !ok || db.deletedFiltered == nil {
		t.Fatalf("detection still serves the mock: ok=%v err=%v", ok, err)
	}
	if d := db.deletedFiltered.ServedRequestDrift; d != nil {
		t.Fatalf("detection on must not report served drift; got %+v", d)
	}
}

// Secret-shaped values never reach the report.
func TestServedRequestDrift_RedactsSecrets(t *testing.T) {
	m := chargeMock()
	m.Spec.HTTPReq.Body = `{"amount":100,"password":"hunter2-old"}`
	eng := schemanoise.New(httpNoiseAdapter{}, false, false)
	got := servedRequestDrift(eng, m, []byte(`{"amount":100,"password":"hunter2-new"}`), nil)
	if len(got) != 1 || got[0].Expected == "hunter2-old" || got[0].Actual == "hunter2-new" {
		t.Fatalf("a password value must be redacted; got %+v", got)
	}
}

// A multipart body's boundary differs on every request; it is never reported
// as a changed request.
func TestServedRequestDrift_SkipsMultipart(t *testing.T) {
	m := chargeMock()
	m.Spec.HTTPReq.Header = map[string]string{"Content-Type": "multipart/form-data; boundary=aaa"}
	m.Spec.HTTPReq.Body = "--aaa\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\n1\r\n--aaa--"
	eng := schemanoise.New(httpNoiseAdapter{}, false, false)
	if got := servedRequestDrift(eng, m, []byte("--bbb\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\n1\r\n--bbb--"), nil); got != nil {
		t.Fatalf("multipart must not be reported; got %+v", got)
	}
}
