package agent

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func specHTTPMock(name, method, url, body string, status int, respBody string, at int) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: url, Body: body, Header: map[string]string{"Host": "localhost:8083"}},
			HTTPResp:         &models.HTTPResp{StatusCode: status, Body: respBody},
			ReqTimestampMock: time.Unix(int64(1000+at), 0),
		},
	}
}

func sortedReps(sp *specPool) []string {
	r := append([]string(nil), sp.reps...)
	sort.Strings(r)
	return r
}

func TestBuildSpecPool(t *testing.T) {
	const myApps = `{"operationName":"MyApps","query":"query MyApps { apps { id } }"}`
	const branches = `{"operationName":"GetBranches","query":"query GetBranches { branches { id } }"}`
	const create = `{"operationName":"CreateBranch","query":"mutation CreateBranch { createBranch }"}`
	const validate = `{"operationName":"ValidateSchema","query":"mutation ValidateSchema { validateSchema }"}`
	mocks := []*models.Mock{
		// identical across the spec → shared, one representative (the earliest)
		specHTTPMock("suite-1", "POST", "/query", myApps, 200, `{"apps":[1]}`, 3),
		specHTTPMock("t1-1", "POST", "/query", myApps, 200, `{"apps":[1]}`, 1),
		specHTTPMock("t2-1", "POST", "/query", myApps, 200, `{"apps":[1]}`, 5),
		// changes within the spec → not shared
		specHTTPMock("t1-2", "POST", "/query", branches, 200, `{"branches":[]}`, 2),
		specHTTPMock("t1-3", "POST", "/query", branches, 200, `{"branches":["feat"]}`, 4),
		// a single write → never shared
		specHTTPMock("t1-4", "POST", "/query", create, 200, `{"id":"b1"}`, 6),
		// a write repeated with the same response → shared
		specHTTPMock("t2-2", "POST", "/query", validate, 200, `{"valid":true}`, 7),
		specHTTPMock("t2-3", "POST", "/query", validate, 200, `{"valid":true}`, 8),
		// REST heartbeat POST, same response every time → shared
		specHTTPMock("s-2", "POST", "/auth/session/activity", "", 404, "", 9),
		specHTTPMock("t1-5", "POST", "/auth/session/activity", "", 404, "", 10),
		// same path, different status → not shared
		specHTTPMock("s-3", "GET", "/cluster/list", "", 200, `[]`, 11),
		specHTTPMock("t2-4", "GET", "/cluster/list", "", 403, ``, 12),
		// non-HTTP is ignored
		{Name: "dns-1", Kind: models.DNS},
	}
	sp, st := buildSpecPool(mocks)

	want := []string{"s-2", "t1-1", "t2-2"}
	if got := sortedReps(sp); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("reps = %v, want %v", got, want)
	}
	for _, n := range []string{"suite-1", "t1-1", "t2-1"} {
		if sp.members[n] != "t1-1" {
			t.Errorf("member %s -> %q, want t1-1 (earliest copy)", n, sp.members[n])
		}
	}
	for _, n := range []string{"t1-2", "t1-3", "t1-4", "s-3", "t2-4", "dns-1"} {
		if _, ok := sp.members[n]; ok {
			t.Errorf("%s must not be shared", n)
		}
	}
	if st.sharedWrites != 2 || st.sharedGroups != 3 || st.mocks != 12 {
		t.Errorf("stats = %+v", st)
	}
}

func TestIsWriteRequest(t *testing.T) {
	cases := []struct {
		method, body string
		want         bool
	}{
		{"POST", `{"query":"query MyApps { x }"}`, false},
		{"POST", `{"query":"  mutation CreateBranch { x }"}`, true},
		{"POST", `{"query":"mutation($n: String!) { createATGApp(name: $n) }"}`, true},
		{"GET", ``, false},
		{"POST", ``, true},
		{"DELETE", ``, true},
	}
	for _, c := range cases {
		m := specHTTPMock("x", c.method, "/p", c.body, 200, "", 0)
		if got := isWriteRequest(m); got != c.want {
			t.Errorf("%s %q: got %v want %v", c.method, c.body, got, c.want)
		}
	}
}

func TestSpecOfScope(t *testing.T) {
	for in, want := range map[string]string{
		"projects/a/x.spec.ts > Describe > title": "projects/a/x.spec.ts",
		"__suite__:projects/a/x.spec.ts":          "projects/a/x.spec.ts",
	} {
		if got := specOfScope(in); got != want {
			t.Errorf("specOfScope(%q) = %q, want %q", in, got, want)
		}
	}
}

// The case that broke apps/page.spec.ts on the first spec-shared run: the app's
// server-side helper and the browser both GET /auth/check with the same
// response body. They must not be merged — the matcher refuses a mock without
// Origin for a request with Origin, so keeping only one copy starves the other
// caller.
func TestBuildSpecPoolKeepsServerAndBrowserCallsApart(t *testing.T) {
	helper := specHTTPMock("h-0", "GET", "/auth/check", "", 200, `{"user":"u"}`, 1)
	helper.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8083", "Accept": "*/*", "Cookie": "a=1", "User-Agent": "node", "Connection": "keep-alive"}
	helper.Spec.HTTPResp.Header = map[string]string{"Set-Cookie": "jwt=x"}

	browser := specHTTPMock("b-3", "GET", "/auth/check", "", 200, `{"user":"u"}`, 2)
	browser.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8083", "Accept": "*/*", "Cookie": "a=1; _ga=2", "User-Agent": "Chrome", "Connection": "keep-alive", "Origin": "http://localhost:3000", "Referer": "http://localhost:3000/"}
	browser.Spec.HTTPResp.Header = map[string]string{"Access-Control-Allow-Origin": "http://localhost:3000"}

	browser2 := specHTTPMock("b-7", "GET", "/auth/check", "", 200, `{"user":"u"}`, 3)
	browser2.Spec.HTTPReq.Header = browser.Spec.HTTPReq.Header
	browser2.Spec.HTTPResp.Header = browser.Spec.HTTPResp.Header

	sp, _ := buildSpecPool([]*models.Mock{helper, browser, browser2})
	if got := sortedReps(sp); len(got) != 2 || got[0] != "b-3" || got[1] != "h-0" {
		t.Fatalf("reps = %v, want [b-3 h-0]: helper and browser calls are separate requests", got)
	}
	if sp.members["b-7"] != "b-3" {
		t.Errorf("the two identical browser copies should share one representative, got %q", sp.members["b-7"])
	}
	if sp.members["h-0"] != "h-0" {
		t.Errorf("the helper copy must represent only itself, got %q", sp.members["h-0"])
	}
}

// vscode-platform: GET /auth/check before sign-in has no Cookie and answers 401;
// after sign-in it has one and answers 200. The matcher's header check is a
// subset test, so the cookie-less copy also matches the signed-in call. Split by
// header names, both became single-response groups and both were shared; the
// never-consumed 401 then answered every signed-in call. They are one request
// with two answers, consumed in order.
func TestBuildSpecPoolHeaderSubsetIsOneRequest(t *testing.T) {
	out := specHTTPMock("o-0", "GET", "/auth/check", "", 401, `{"error":"unauthorized"}`, 1)
	out.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8083", "Origin": "http://localhost:3000", "Accept": "*/*"}
	out2 := specHTTPMock("o-1", "GET", "/auth/check", "", 401, `{"error":"unauthorized"}`, 2)
	out2.Spec.HTTPReq.Header = out.Spec.HTTPReq.Header
	in := specHTTPMock("i-3", "GET", "/auth/check", "", 200, `{"user":"u"}`, 3)
	in.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8083", "Origin": "http://localhost:3000", "Accept": "*/*", "Cookie": "jwt=x"}

	sp, _ := buildSpecPool([]*models.Mock{out, out2, in})
	if len(sp.reps) != 0 {
		t.Fatalf("reps = %v: a request whose answer changes within the spec must not be shared", sp.reps)
	}
}

// Copies identical except for Set-Cookie are one answer (no mock ever checks the
// cookie value) and are shared; the representative is the cookie-bearing copy,
// because a caller may read the session cookie out of the response.
func TestBuildSpecPoolSetCookieDoesNotSplitAndIsKept(t *testing.T) {
	hdr := map[string]string{"Host": "localhost:8083", "Origin": "http://localhost:3000"}
	a := specHTTPMock("a-0", "GET", "/auth/check", "", 200, `{"user":"u"}`, 1)
	b := specHTTPMock("b-0", "GET", "/auth/check", "", 200, `{"user":"u"}`, 2)
	b.Spec.HTTPResp.Header = map[string]string{"Set-Cookie": "jwt=x"}
	c := specHTTPMock("c-0", "GET", "/auth/check", "", 200, `{"user":"u"}`, 3)
	for _, m := range []*models.Mock{a, b, c} {
		m.Spec.HTTPReq.Header = hdr
	}
	sp, _ := buildSpecPool([]*models.Mock{a, b, c})
	if len(sp.reps) != 1 || sp.reps[0] != "b-0" {
		t.Fatalf("reps = %v, want [b-0]: one shared request, answered by the copy that sets the cookie", sp.reps)
	}
	for _, n := range []string{"a-0", "b-0", "c-0"} {
		if sp.members[n] != "b-0" {
			t.Errorf("member %s -> %q, want b-0", n, sp.members[n])
		}
	}
}

func TestIsGraphQLMutation(t *testing.T) {
	for body, want := range map[string]bool{
		`{"query":"mutation($id: String!) { requestBranchReview(branchId: $id) }"}`:       true,
		`{"operationName":"GetBranches","query":"query GetBranches { branches { id } }"}`: false,
		`{"url":"http://x"}`: false,
		``:                   false,
	} {
		if got := isGraphQLMutation(specHTTPMock("x", "POST", "/query", body, 200, "", 0)); got != want {
			t.Errorf("%.40q: got %v want %v", body, got, want)
		}
	}
}
