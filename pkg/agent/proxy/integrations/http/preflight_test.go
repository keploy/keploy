package http

import (
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func preflightMock(name, requestHeaders string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "OPTIONS", URL: "/query", Header: map[string]string{
				"Origin":                         "http://localhost:3000",
				"Access-Control-Request-Method":  "POST",
				"Access-Control-Request-Headers": requestHeaders,
			}},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		},
	}
}

func livePreflight(requestHeaders string) *req {
	u, _ := url.Parse("http://localhost:8083/query")
	return &req{method: http.MethodOptions, url: u, header: http.Header{
		"Origin":                         {"http://localhost:3000"},
		"Access-Control-Request-Method":  {"POST"},
		"Access-Control-Request-Headers": {requestHeaders},
	}}
}

// A preflight is answered by a recording of the same preflight: the page's
// Content-Type-only preflight must not answer the test's Authorization one.
func TestPreflightCandidates_KeepsTheSamePreflight(t *testing.T) {
	pool := []*models.Mock{
		preflightMock("page", "content-type"),
		preflightMock("test", "authorization,content-type"),
		preflightMock("page-2", "content-type"),
	}
	got := names(preflightCandidates(livePreflight("authorization,content-type"), pool))
	if len(got) != 1 || got[0] != "test" {
		t.Fatalf("authorization preflight kept %v, want [test]", got)
	}
	got = names(preflightCandidates(livePreflight("content-type"), pool))
	if len(got) != 2 || got[0] != "page" || got[1] != "page-2" {
		t.Fatalf("content-type preflight kept %v, want [page page-2]", got)
	}
}

// The requested headers are a set: order, case and spacing do not matter.
func TestPreflightCandidates_HeaderListIsASet(t *testing.T) {
	pool := []*models.Mock{preflightMock("test", "authorization,content-type")}
	if got := preflightCandidates(livePreflight("Content-Type, Authorization"), pool); len(got) != 1 {
		t.Fatalf("reordered header list matched %d recordings, want 1", len(got))
	}
}

// With no recording of the same preflight, the candidates stay as they were.
func TestPreflightCandidates_NoSamePreflightKeepsAll(t *testing.T) {
	pool := []*models.Mock{preflightMock("page", "content-type")}
	if got := preflightCandidates(livePreflight("authorization,content-type,x-new"), pool); len(got) != 1 {
		t.Fatalf("got %d candidates, want the original 1", len(got))
	}
}

// Anything that is not a preflight is untouched.
func TestPreflightCandidates_OnlyPreflights(t *testing.T) {
	pool := []*models.Mock{preflightMock("page", "content-type"), preflightMock("test", "authorization,content-type")}
	plain := livePreflight("authorization")
	plain.header.Del("Access-Control-Request-Method")
	if got := preflightCandidates(plain, pool); len(got) != 2 {
		t.Fatalf("an OPTIONS without Access-Control-Request-Method was narrowed to %d", len(got))
	}
	get := livePreflight("authorization")
	get.method = http.MethodGet
	if got := preflightCandidates(get, pool); len(got) != 2 {
		t.Fatalf("a GET was narrowed to %d", len(got))
	}
}
