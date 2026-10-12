package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func contentTypeMock(name, method, path, contentType string) *models.Mock {
	header := map[string]string{"Host": "localhost:8083", "Origin": "http://localhost:3000"}
	if contentType != "" {
		header["Content-Type"] = contentType
	}
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: models.Method(method), URL: path, Header: header},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		},
	}
}

func liveWithContentType(method, path, contentType string) *req {
	u, _ := url.Parse("http://localhost:8083" + path)
	header := http.Header{"Host": {"localhost:8083"}, "Origin": {"http://localhost:3000"}}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &req{method: method, url: u, header: header}
}

// A request that omits Content-Type matches a recording that sent one: the
// app sends GET /cluster/getApps both ways, and a bodyless GET without it
// found no candidate among 805 recordings that all carried it.
func TestSchemaMatch_RequestWithoutContentTypeMatchesRecordingWithIt(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	pool := []*models.Mock{contentTypeMock("getApps", "GET", "/cluster/getApps", "application/json")}

	got, err := h.SchemaMatch(context.Background(), liveWithContentType("GET", "/cluster/getApps", ""), pool, nil, nil, false)
	if err != nil {
		t.Fatalf("SchemaMatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d candidates, want the recording that sent Content-Type", len(got))
	}
}

// Content-Type still decides when both sides send one: a form post is not a
// JSON recording.
func TestSchemaMatch_DifferentContentTypesStillDoNotMatch(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	pool := []*models.Mock{contentTypeMock("json", "POST", "/auth/login", "application/json")}

	got, err := h.SchemaMatch(context.Background(), liveWithContentType("POST", "/auth/login", "application/x-www-form-urlencoded"), pool, nil, nil, false)
	if err != nil {
		t.Fatalf("SchemaMatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a form request matched a JSON recording")
	}
}

// Any other recorded header is still required as a key.
func TestHeadersContainKeys_ContentTypeExemptOthersRequired(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	recorded := map[string]string{"Host": "x", "Content-Type": "application/json", "Authorization": "Bearer x"}
	if !h.HeadersContainKeys(recorded, http.Header{"Host": {"x"}, "Authorization": {"y"}}, nil) {
		t.Error("a live request without Content-Type was rejected")
	}
	if h.HeadersContainKeys(recorded, http.Header{"Host": {"x"}}, nil) {
		t.Error("a live request without Authorization was accepted")
	}
}
