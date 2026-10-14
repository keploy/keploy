package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// onHost is a recorded GET of path on host, recorded as the recorder writes
// it: a path-only URL, the authority in the Host header.
func onHost(name, path, host string) *models.Mock {
	m := httpMock(name, "GET", path)
	m.Spec.HTTPReq.Header = map[string]string{"Host": host, "Accept": "application/json"}
	return m
}

// getTo is a live GET of path to host.
func getTo(path, host string) *req {
	h := http.Header{}
	h.Set("Host", host)
	h.Set("Accept", "application/json")
	return &req{method: "GET", url: &url.URL{Path: path}, header: h}
}

func matchOnce(t *testing.T, ctx context.Context, input *req, mocks ...*models.Mock) (*models.Mock, *matchDiag) {
	t.Helper()
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	ok, stub, diag, err := newHTTP().match(ctx, input, db, nil, nil, nil, true, false, false, true, true)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !ok {
		return nil, diag
	}
	return stub, nil
}

// Under the switch a recorded request answers only a request to the same
// authority: the same path on another host is another dependency. Without
// the switch the host is not compared, as before.
func TestStrictMatchesOnlyARequestToTheRecordedHost(t *testing.T) {
	a := onHost("on-a", "/v1/items", "a.example")
	b := onHost("on-b", "/v1/items", "B.example") // the authority is compared case-insensitively

	t.Setenv("KEPLOY_PROVEN", "1")
	if got, _ := matchOnce(t, context.Background(), getTo("/v1/items", "b.example"), a, b); got == nil || got.Name != "on-b" {
		t.Fatalf("served %v, want on-b", got)
	}
	got, diag := matchOnce(t, context.Background(), getTo("/v1/items", "c.example"), a, b)
	if got != nil {
		t.Fatalf("a request to c.example was served %s", got.Name)
	}
	if diag == nil || diag.phase != models.MatchPhaseSchema {
		t.Fatalf("diag = %+v, want the schema phase", diag)
	}

	t.Setenv("KEPLOY_PROVEN", "")
	if got, _ := matchOnce(t, context.Background(), getTo("/v1/items", "c.example"), a, b); got == nil || got.Name != "on-a" {
		t.Fatalf("without the switch served %v, want on-a (the host is not compared)", got)
	}
}

// The container key is recorded but not compared yet (strict.go): a
// recording reached on another published port misses, though it is the same
// container, and one recorded at the live authority is served, whatever
// container it names. The key that names a container by its own
// configuration replaces both.
func TestStrictComparesTheAuthorityNotTheContainerKey(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	keyed := func(m *models.Mock, key string) *models.Mock {
		m.Spec.Metadata = map[string]string{"containerKey": key}
		return m
	}
	rec := keyed(onHost("ls", "/queue", "localhost:49153"), "container:localstack/localstack:3.8:4566#1")
	if got, _ := matchOnce(t, context.Background(), getTo("/queue", "localhost:50001"), rec); got != nil {
		t.Fatalf("a recording on another published port was served: %s", got.Name)
	}
	other := keyed(onHost("other", "/queue", "localhost:49153"), "container:postgres:16:5432#2")
	if got, _ := matchOnce(t, context.Background(), getTo("/queue", "localhost:49153"), other); got == nil {
		t.Fatal("a recording at the live authority was not served")
	}
}

// The miss report names the destination that differs, and diffs against a
// recorded request to the live destination when there is one.
func TestStrictMissReportNamesTheHost(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	other := onHost("elsewhere", "/v1/items", "a.example")
	got, diag := matchOnce(t, context.Background(), getTo("/v1/items", "c.example"), other)
	if got != nil {
		t.Fatalf("served %s", got.Name)
	}
	diag.live = &destination{host: "c.example"}
	r := newHTTP().buildHTTPMismatchReport(makeReq("GET", "/v1/items"), nil, &mockMemDb{mocks: []*models.Mock{other}}, nil, nil, nil, diag)
	found := false
	for _, d := range r.FieldDiffs {
		if d.Path == "host" && d.Expected == "a.example" && d.Actual == "c.example" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the report's field diffs %+v do not name the host", r.FieldDiffs)
	}

	near := onHost("same-host", "/v1/itemz", "c.example")
	r = newHTTP().buildHTTPMismatchReport(makeReq("GET", "/v1/items"), nil, &mockMemDb{mocks: []*models.Mock{other, near}}, nil, nil, nil,
		&matchDiag{phase: models.MatchPhaseSchema, candidates: 2, live: &destination{host: "c.example"}})
	if r.ClosestMock != "same-host" {
		t.Fatalf("closest = %q, want the recording to the live destination", r.ClosestMock)
	}
}

// Under the switch no path segment or query value is relaxed for its shape:
// /users/123 recorded does not answer /users/456. Without the switch the
// auto-detected dynamic pass still answers it.
func TestStrictDropsTheAutoDynamicPass(t *testing.T) {
	path := onHost("user-123", "/users/123?since=1700000000000", "api.example")
	path.Spec.HTTPReq.URLParams = map[string]string{"since": "1700000000000"}
	live := getTo("/users/456", "api.example")
	live.url.RawQuery = "since=1700000099999"

	t.Setenv("KEPLOY_PROVEN", "")
	if got, _ := matchOnce(t, context.Background(), live, path); got == nil {
		t.Fatal("without the switch the auto-dynamic pass did not answer")
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got, _ := matchOnce(t, context.Background(), live, path); got != nil {
		t.Fatalf("under the switch an id-shaped segment was relaxed: served %s", got.Name)
	}
}
