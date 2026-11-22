package http

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The fuzzy-identity goldens of keploy #4761: requests main's fuzzy fallback
// answered with the nearest recording of another call, which under S1 must
// miss. A drifted argument misses too, unless the mock learned it.

func identityMock(name, body string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: models.Method("POST"), URL: "/query", Body: body},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"data":{}}`},
		},
	}
}

const (
	createAppReq = `{"operationName":"CreateATGApp","variables":{"name":"app-1767232949464"},"query":"mutation CreateATGApp($name: String!) { createATGApp(name: $name) }"}`
	reportsBody  = `{"operationName":"GetTestReports","variables":{"appId":"259f9ac2","page":1},"query":"query GetTestReports($appId: String!) { getTestReports(appId: $appId) { reports } }"}`
)

func identityReq(body string) *req {
	return &req{method: "POST", url: &url.URL{Path: "/query"}, header: http.Header{}, body: []byte(body)}
}

func TestStrictFuzzyIdentityGoldensMiss(t *testing.T) {
	recordedCreate := `{"operationName":"CreateATGApp","variables":{"name":"app-1767227201284"},"query":"mutation CreateATGApp($name: String!) { createATGApp(name: $name) }"}`
	loadTest := `{"query":"mutation($id: String!) { startLoadTest(id: $id) { loadJobId } }","variables":{"id":"y"}}`
	anonOld := `{"query":"mutation($name: String!) { createATGApp(name: $name) }","variables":{"name":"e2e-atgconfig-OLD"}}`
	for _, tc := range []struct {
		name string
		pool []*models.Mock
		body string
	}{
		{"a different operation (CreateATGApp answered with GetTestReports)", []*models.Mock{identityMock("reports", reportsBody)}, createAppReq},
		{"the same operation with a drifted argument", []*models.Mock{identityMock("reports", reportsBody), identityMock("createApp", recordedCreate)}, createAppReq},
		{"a body with no identity key", []*models.Mock{identityMock("plain", `{"user":"alice","role":"admin"}`)}, `{"user":"bob","role":"admin"}`},
		{"a mock with no identity", []*models.Mock{identityMock("legacy", `{"name":"app-1767227201284"}`)}, createAppReq},
		{"an anonymous mutation of another root field", []*models.Mock{identityMock("loadtest", loadTest)}, `{"query":"mutation($name: String!) { createATGApp(name: $name) }","variables":{"name":"e2e-atgconfig-1"}}`},
		{"an anonymous mutation with a drifted argument", []*models.Mock{identityMock("loadtest", loadTest), identityMock("createApp", anonOld)}, `{"query":"mutation($name: String!) { createATGApp(name: $name) }","variables":{"name":"e2e-atgconfig-NEW"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := strictMatch(t, identityReq(tc.body), nil, nil, nil, tc.pool...); got != nil {
				t.Fatalf("served %q: under S1 a call no recording is answers nothing", got.Name)
			}
		})
	}

	// The drifted argument the mock learned (record's full round saw it
	// change) is excused, and only it.
	learned := identityMock("createApp", recordedCreate)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.variables.name": {}}
	if got, _ := strictMatch(t, identityReq(createAppReq), nil, nil, nil, identityMock("reports", reportsBody), learned); got == nil || got.Name != "createApp" {
		t.Fatalf("served %v, want createApp with its learned argument", got)
	}
	if got, _ := strictMatch(t, identityReq(strings.Replace(createAppReq, "CreateATGApp", "DeleteATGApp", 1)), nil, nil, nil, learned); got != nil {
		t.Fatal("a learned argument excused another operation")
	}
}

// The two-way Origin case of keploy #4761: a browser XHR and a server-side
// call of the same endpoint are not interchangeable (Origin makes the server
// attach CORS headers), so under S1 neither recording answers the other.
var (
	serverSideRecording = map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate",
		"Accept-Language": "*",
		"Connection":      "keep-alive",
		"Cookie":          "session=abc",
		"Host":            "localhost:8083",
		"Sec-Fetch-Mode":  "cors",
		"User-Agent":      "node",
	}

	browserRecording = map[string]string{
		"Accept":             "*/*",
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Accept-Language":    "en-US,en;q=0.9",
		"Connection":         "keep-alive",
		"Cookie":             "session=abc",
		"Host":               "localhost:8083",
		"Origin":             "http://localhost:3000",
		"Referer":            "http://localhost:3000/",
		"Sec-Ch-Ua":          `"Chromium";v="141"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-site",
		"User-Agent":         "Mozilla/5.0",
	}

	browserRequest = http.Header{
		"Accept":             {"*/*"},
		"Accept-Encoding":    {"gzip, deflate, br, zstd"},
		"Accept-Language":    {"en-US,en;q=0.9"},
		"Connection":         {"keep-alive"},
		"Cookie":             {"session=abc"},
		"Host":               {"localhost:8083"},
		"Origin":             {"http://localhost:3000"},
		"Referer":            {"http://localhost:3000/"},
		"Sec-Ch-Ua":          {`"Chromium";v="141"`},
		"Sec-Ch-Ua-Mobile":   {"?0"},
		"Sec-Ch-Ua-Platform": {`"Windows"`},
		"Sec-Fetch-Dest":     {"empty"},
		"Sec-Fetch-Mode":     {"cors"},
		"Sec-Fetch-Site":     {"same-site"},
		"User-Agent":         {"Mozilla/5.0"},
	}

	serverSideRequest = http.Header{
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"*"},
		"Connection":      {"keep-alive"},
		"Cookie":          {"session=abc"},
		"Host":            {"localhost:8083"},
		"Sec-Fetch-Mode":  {"cors"},
		"User-Agent":      {"node"},
	}
)

func TestStrictOriginPresenceMustAgree(t *testing.T) {
	mockOf := func(name string, h map[string]string) *models.Mock {
		m := httpMock(name, "GET", "/cluster/proxy-origins")
		m.Spec.HTTPReq.Header = h
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200}
		return m
	}
	reqOf := func(h http.Header) *req {
		return &req{method: "GET", url: &url.URL{Path: "/cluster/proxy-origins"}, header: h.Clone()}
	}
	for _, tc := range []struct {
		name   string
		mock   map[string]string
		req    http.Header
		served bool
	}{
		{"server-side recording must NOT serve a browser request", serverSideRecording, browserRequest, false},
		{"browser recording must NOT serve a server-side request", browserRecording, serverSideRequest, false},
		{"browser recording serves a browser request", browserRecording, browserRequest, true},
		{"server-side recording serves a server-side request", serverSideRecording, serverSideRequest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diag := strictMatch(t, reqOf(tc.req), nil, nil, nil, mockOf("m", tc.mock))
			if (got != nil) != tc.served {
				t.Fatalf("served %v, want %v (differs %v)", got != nil, tc.served, differs(diag))
			}
		})
	}
}
