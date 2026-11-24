package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func endpointCase(method, url, recorded string) *models.TestCase {
	return &models.TestCase{
		Name:     method + " " + url,
		HTTPReq:  models.HTTPReq{Method: models.Method(method), URL: url},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: recorded},
	}
}

func matchWith(t *testing.T, tc *models.TestCase, live string, global map[string]map[string][]string, rules []config.EndpointNoise) bool {
	t.Helper()
	noise := AddEndpointNoise(global, rules, tc)
	pass, _ := Match(tc, &models.HTTPResp{StatusCode: 200, Body: live}, noise, false, false, zap.NewNop(), false)
	return pass
}

func TestEndpointNoiseIgnoresTheFieldOnlyOnItsEndpoint(t *testing.T) {
	rules := []config.EndpointNoise{{Method: "GET", Path: "/version", Body: config.Noise{"version": {}}}}
	cases := []struct {
		name           string
		tc             *models.TestCase
		live           string
		endpoint, glob bool
	}{
		{"its own endpoint", endpointCase("GET", "http://api:8080/version", `{"version":"a1b2c3d4"}`), `{"version":"pr-42"}`, true, true},
		{"the same top-level field on another endpoint", endpointCase("GET", "http://api:8080/charts/details", `{"version":"3.8.18","chart":"web"}`), `{"version":"3.8.19","chart":"web"}`, false, true},
		{"a nested field of that name on another endpoint", endpointCase("GET", "http://api:8080/clusters", `[{"name":"c1","heartbeats":[{"version":"3.8.18"}]}]`), `[{"name":"c1","heartbeats":[{"version":"3.8.19"}]}]`, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchWith(t, c.tc, c.live, nil, rules); got != c.endpoint {
				t.Fatalf("with the endpoint rule: pass=%v, want %v", got, c.endpoint)
			}
			global := map[string]map[string][]string{"body": {"version": {}}}
			if got := matchWith(t, c.tc, c.live, global, nil); got != c.glob {
				t.Fatalf("with global noise: pass=%v, want %v", got, c.glob)
			}
		})
	}
}

func TestEndpointNoiseNamesAnExactPathFromTheTop(t *testing.T) {
	get := func(body string) []config.EndpointNoise {
		return []config.EndpointNoise{{Path: "/apps/{id}", Body: config.Noise{body: {}}}}
	}
	cases := []struct {
		name, field, recorded, live string
		pass                        bool
	}{
		{"top level only", "version", `{"version":"a","data":{"version":"1"}}`, `{"version":"b","data":{"version":"2"}}`, false},
		{"top level", "version", `{"version":"a","data":{"version":"1"}}`, `{"version":"b","data":{"version":"1"}}`, true},
		{"a nested path", "data.version", `{"version":"a","data":{"version":"1"}}`, `{"version":"a","data":{"version":"2"}}`, true},
		{"never a substring", "data.id", `{"metadata":{"id":"1"},"data":{"identifier":"x"}}`, `{"metadata":{"id":"2"},"data":{"identifier":"y"}}`, false},
		{"array items without []", "data.heartbeats.at", `{"data":{"heartbeats":[{"at":1,"v":"a"},{"at":2,"v":"b"}]}}`, `{"data":{"heartbeats":[{"at":3,"v":"a"},{"at":4,"v":"b"}]}}`, true},
		{"array items with []", "data.heartbeats[].at", `{"data":{"heartbeats":[{"at":1,"v":"a"},{"at":2,"v":"b"}]}}`, `{"data":{"heartbeats":[{"at":3,"v":"a"},{"at":4,"v":"b"}]}}`, true},
		{"array items still check other fields", "data.heartbeats[].at", `{"data":{"heartbeats":[{"at":1,"v":"a"}]}}`, `{"data":{"heartbeats":[{"at":3,"v":"z"}]}}`, false},
		{"a top-level array", "[].heartbeats.version", `[{"heartbeats":[{"version":"1"}]}]`, `[{"heartbeats":[{"version":"2"}]}]`, true},
		{"a whole subtree", "data.app", `{"data":{"app":{"id":"a1","updatedAt":1}},"n":1}`, `{"data":{"app":{"id":"a2","updatedAt":2}},"n":1}`, true},
		{"a sibling with a longer name", "data.ver", `{"data":{"version":"1"}}`, `{"data":{"version":"2"}}`, false},
		{"case does not matter", "Data.Version", `{"data":{"version":"1"}}`, `{"data":{"version":"2"}}`, true},
		{"a map keyed by ids", "results.{id}.traceId", `{"results":{"0b7c5d9e-1c1f-4a55-9a8e-7f3c2e1d4b6a":{"traceId":"t1","n":1}}}`, `{"results":{"0b7c5d9e-1c1f-4a55-9a8e-7f3c2e1d4b6a":{"traceId":"t2","n":1}}}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := endpointCase("GET", "http://api/apps/a9e6", c.recorded)
			if got := matchWith(t, tc, c.live, nil, get(c.field)); got != c.pass {
				t.Fatalf("rule %q: pass=%v, want %v", c.field, got, c.pass)
			}
		})
	}
}

func TestEndpointNoisePatternsLimitTheValues(t *testing.T) {
	rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"version": {"^[0-9a-f]{8}$"}}}}
	tc := endpointCase("GET", "http://api/version", `{"version":"a1b2c3d4"}`)
	if !matchWith(t, tc, `{"version":"e5f6a7b8"}`, nil, rules) {
		t.Fatal("a value inside the pattern failed")
	}
	if matchWith(t, tc, `{"version":"pr-42"}`, nil, rules) {
		t.Fatal("a value outside the pattern passed")
	}
}

func TestEndpointNoiseNamesAHeaderExactly(t *testing.T) {
	rules := []config.EndpointNoise{{Path: "/version", Header: config.Noise{"x-build": {}}}}
	tc := endpointCase("GET", "http://api/version", `{}`)
	tc.HTTPResp.Header = map[string]string{"X-Build": "a", "X-Build-Id": "1"}
	noise := AddEndpointNoise(nil, rules, tc)
	live := &models.HTTPResp{StatusCode: 200, Body: `{}`, Header: map[string]string{"X-Build": "b", "X-Build-Id": "1"}}
	if pass, _ := Match(tc, live, noise, false, false, zap.NewNop(), false); !pass {
		t.Fatal("the named header failed")
	}
	live.Header = map[string]string{"X-Build": "a", "X-Build-Id": "2"}
	if pass, _ := Match(tc, live, noise, false, false, zap.NewNop(), false); pass {
		t.Fatal("a header whose name only contains the rule's passed")
	}
}

func TestEndpointNoiseKeepsTestCaseNoise(t *testing.T) {
	rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"version": {}}}}
	tc := endpointCase("GET", "http://api/version", `{"version":"a","at":1}`)
	tc.Noise = map[string][]string{"body.at": {}}
	if !matchWith(t, tc, `{"version":"b","at":2}`, nil, rules) {
		t.Fatal("endpoint and test-case noise together did not cover both fields")
	}
}

func TestEndpointMatches(t *testing.T) {
	cases := []struct {
		method, path, reqMethod, url string
		want                         bool
	}{
		{"GET", "/version", "GET", "http://api:8080/version", true},
		{"get", "/version", "GET", "http://api:8080/version?x=1", true},
		{"", "/version", "POST", "http://api/version", true},
		{"GET", "/version", "POST", "http://api/version", false},
		{"GET", "/version", "GET", "http://api/version/", true},
		{"GET", "/version", "GET", "http://api/versions", false},
		{"GET", "/version", "GET", "http://api/v1/version", false},
		{"", "/apps/{id}/config", "GET", "http://api/apps/a9e6/config", true},
		{"", "/apps/{id}/config", "GET", "http://api/apps/config", false},
		{"", "/apps/{id}/config", "GET", "http://api/apps/a/b/config", false},
		{"", "/", "GET", "http://api/", true},
		{"", "/", "GET", "http://api/version", false},
		{"", "", "GET", "http://api/version", false},
		{"", "/Version", "GET", "http://api/version", false},
		{"", "/files/{name}", "GET", "http://api/files/a%2Fb", true},
		{"", "/files/a b", "GET", "http://api/files/a%20b", true},
	}
	for _, c := range cases {
		r := config.EndpointNoise{Method: c.method, Path: c.path, Body: config.Noise{"v": {}}}
		req := models.HTTPReq{Method: models.Method(c.reqMethod), URL: c.url}
		_, got := AddEndpointNoise(nil, []config.EndpointNoise{r}, &models.TestCase{HTTPReq: req})["body"][matcherUtils.ExactKey("v")]
		if got != c.want {
			t.Errorf("rule %s %q on %s %s: %v, want %v", c.method, c.path, c.reqMethod, c.url, got, c.want)
		}
	}
}

func TestAddEndpointNoiseLeavesTheConfigAlone(t *testing.T) {
	global := map[string]map[string][]string{"body": {"id": {}}}
	rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"version": {}}}}
	got := AddEndpointNoise(global, rules, &models.TestCase{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://api/version"}})
	if _, ok := got["body"][matcherUtils.ExactKey("version")]; !ok {
		t.Fatalf("the rule's field is missing: %v", got)
	}
	if _, ok := global["body"][matcherUtils.ExactKey("version")]; ok || len(global["body"]) != 1 {
		t.Fatalf("the shared config was changed: %v", global)
	}
	if other := AddEndpointNoise(global, rules, &models.TestCase{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://api/charts"}}); len(other["body"]) != 1 {
		t.Fatalf("a rule for another endpoint added fields: %v", other)
	}
}

func TestTheTestCasesOwnNoiseWinsOverAnEndpointRule(t *testing.T) {
	rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"data.version": {`^\d+$`}}, Header: config.Noise{"X-Build": {"^b-"}}}}
	tc := endpointCase("GET", "http://api/version", `{"data":{"version":"1"}}`)
	tc.HTTPResp.Header = map[string]string{"X-Build": "b-1"}
	live := &models.HTTPResp{StatusCode: 200, Body: `{"data":{"version":"dev-1"}}`, Header: map[string]string{"X-Build": "dev"}}
	if pass, _ := Match(tc, live, AddEndpointNoise(nil, rules, tc), false, false, zap.NewNop(), false); pass {
		t.Fatal("values outside the rule's patterns passed")
	}
	tc.Noise = map[string][]string{"body.data.version": {}, "header.x-build": {}}
	if pass, _ := Match(tc, live, AddEndpointNoise(nil, rules, tc), false, false, zap.NewNop(), false); !pass {
		t.Fatal("the test case's own noise, which ignores any value, lost to the endpoint rule's patterns")
	}
}

func TestAKeyThatLooksLikeAPathFromTheRootKeepsItsMeaning(t *testing.T) {
	tc := endpointCase("GET", "http://api/version", `{"data":{"token":"a"}}`)
	noise := map[string]map[string][]string{"body": {"$.data.token": {}}}
	if pass, _ := Match(tc, &models.HTTPResp{StatusCode: 200, Body: `{"data":{"token":"b"}}`}, noise, false, false, zap.NewNop(), false); pass {
		t.Fatal("a global noise key written as $.data.token now ignores data.token; it never matched a field before")
	}
}

func TestEndpointNoiseUnderARootArrayIsLeftOutOfTheAssessment(t *testing.T) {
	noise := map[string][]string{matcherUtils.ExactKey("heartbeats"): {}}
	got, err := matcherUtils.ComputeFailureAssessmentJSON(`[{"heartbeats":[{"at":1}]}]`, `[{"heartbeats":[{"v":"x"}],"region":"eu"}]`, noise, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(append(append([]string{}, got.RemovedFields...), got.AddedFields...), got.ValueChanges...) {
		if strings.Contains(f, "heartbeats") {
			t.Fatalf("an ignored field is graded as a change: %+v", got)
		}
	}
	if len(got.AddedFields) != 1 {
		t.Fatalf("want only region added, got %+v", got)
	}
}

func TestBodyNoiseCoveringNamesAnEndpointRuleWithAnIDKey(t *testing.T) {
	global := map[string]map[string][]string{"body": {matcherUtils.ExactKey("results.{id}.traceId"): {}}}
	keys := []string{"results", "0b7c5d9e-1c1f-4a55-9a8e-7f3c2e1d4b6a", "traceId"}
	if got, want := matcherUtils.BodyNoiseCovering(keys, nil, global, zap.NewNop()), "body.results.{id}.traceId (globalNoise.endpoints)"; got != want {
		t.Fatalf("covering %q, want %q", got, want)
	}
	if got := matcherUtils.BodyNoiseCovering([]string{"results", "0b7c5d9e-1c1f-4a55-9a8e-7f3c2e1d4b6a", "n"}, nil, global, zap.NewNop()); got != "" {
		t.Fatalf("a sibling field is covered by %q", got)
	}
}
