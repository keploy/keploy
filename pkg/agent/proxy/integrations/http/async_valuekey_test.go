package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func httpRespMock(status int, body string) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		HTTPResp: &models.HTTPResp{StatusCode: status, Body: body},
	}}
}

func TestResponseValueKeyStableForSameValue(t *testing.T) {
	h := &HTTP{}
	lane := models.AsyncLane{Name: "L", Type: "httpPoll"}
	a := h.ResponseValueKey(httpRespMock(200, `{"type":"NOT_MODIFIED"}`), lane)
	b := h.ResponseValueKey(httpRespMock(200, `{"type":"NOT_MODIFIED"}`), lane)
	if a != b {
		t.Fatalf("same value must yield same key: %q vs %q", a, b)
	}
}

func TestResponseValueKeyDiffersOnChange(t *testing.T) {
	h := &HTTP{}
	lane := models.AsyncLane{Name: "L", Type: "httpPoll"}
	unchanged := h.ResponseValueKey(httpRespMock(200, `{"type":"NOT_MODIFIED"}`), lane)
	changed := h.ResponseValueKey(httpRespMock(200, `{"version":39,"keys":{"flag":true}}`), lane)
	if unchanged == changed {
		t.Fatal("a changed body must yield a different key")
	}
}
