package http

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
		{"a JSONPath-style root", "$.data.version", `{"data":{"version":"1"}}`, `{"data":{"version":"2"}}`, true},
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
		{"", "/files/a%20b", "GET", "http://api/files/a%20b", true},
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

func TestEndpointRulesKeepTheRegexShapedNoiseWarning(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	tc := endpointCase("GET", "http://api/version", `{"ts":"a"}`)
	tc.Noise = map[string][]string{"body": {fmt.Sprintf("^endpointRuleTs%d.*", time.Now().UnixNano())}}
	noise := AddEndpointNoise(nil, []config.EndpointNoise{{Path: "/version", Body: config.Noise{"version": {}}}}, tc)
	Match(tc, &models.HTTPResp{StatusCode: 200, Body: `{"ts":"a"}`}, noise, false, false, zap.New(core), false)
	if logs.FilterMessageSnippet("looks like a regex").Len() != 1 {
		t.Fatal("the warning for a regex-shaped test-case noise value was not said")
	}
}

func TestATestCaseKeyThatMatchesNothingLeavesTheRuleOn(t *testing.T) {
	for _, own := range []string{"body.$.data.version", "body.data[].version"} {
		rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"data.version": {}}}}
		tc := endpointCase("GET", "http://api/version", `{"data":{"version":"1"}}`)
		tc.Noise = map[string][]string{own: {}}
		if !matchWith(t, tc, `{"data":{"version":"2"}}`, nil, rules) {
			t.Fatalf("the test case's key %q, which ignores nothing, turned the rule off", own)
		}
	}
}

func TestTheFirstRuleThatNamesAFieldDecides(t *testing.T) {
	tc := endpointCase("GET", "http://api/apps/a9/version", `{"items":[{"version":"v1.2"}]}`)
	for _, rules := range [][]config.EndpointNoise{
		{{Path: "/apps/{id}/version", Body: config.Noise{"items[].version": {`^v1\.`}}}, {Method: "GET", Path: "/apps/{appId}/version", Body: config.Noise{"items.version": {}}}},
		{{Method: "get", Path: "/apps/{id}/version/", Body: config.Noise{"$.items.version": {`^v1\.`}}}, {Path: "/apps/{id}/version", Body: config.Noise{"Items.Version": {}}}},
	} {
		if !matchWith(t, tc, `{"items":[{"version":"v1.3"}]}`, nil, rules) {
			t.Fatal("a value inside the first rule's pattern failed")
		}
		if matchWith(t, tc, `{"items":[{"version":"v2.0"}]}`, nil, rules) {
			t.Fatalf("a later rule widened the first one's pattern: %+v", rules)
		}
	}
}

func TestAnEndpointRuleNeverNarrowsOtherNoise(t *testing.T) {
	headerRule := []config.EndpointNoise{{Path: "/version", Header: config.Noise{"X-Build": {"^b-"}}}}
	for _, key := range []string{"x-build", "X-Build", "build"} {
		tc := endpointCase("GET", "http://api/version", `{}`)
		tc.HTTPResp.Header = map[string]string{"X-Build": "b-1"}
		live := &models.HTTPResp{StatusCode: 200, Body: `{}`, Header: map[string]string{"X-Build": "c-2"}}
		global := map[string]map[string][]string{"header": {key: {}}}
		if pass, _ := Match(tc, live, AddEndpointNoise(global, headerRule, tc), false, false, zap.NewNop(), false); !pass {
			t.Errorf("the rule's pattern narrowed the header entry %q, which ignores any value", key)
		}
	}
	bodyRule := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"data.info.version": {`^v1\.`}}}}
	for _, key := range []string{"version", "data.info.version", "Data.Info.Version", "info.version"} {
		tc := endpointCase("GET", "http://api/version", `{"data":{"info":{"version":"v1.0"}}}`)
		global := map[string]map[string][]string{"body": {key: {}}}
		if !matchWith(t, tc, `{"data":{"info":{"version":"v2.0"}}}`, global, bodyRule) {
			t.Errorf("the rule's pattern narrowed the body entry %q, which ignores any value", key)
		}
	}
}

func TestAnEndpointRuleAddsToOtherNoisesPatterns(t *testing.T) {
	global := map[string]map[string][]string{"body": {"data.version": {`^v2\.`}}, "header": {"x-build": {"^c-"}}}
	cases := []struct {
		name                       string
		rec, live, recHdr, liveHdr string
		rules                      []config.EndpointNoise
		want                       bool
	}{
		{"values the rule lets vary", "v1.0", "v1.1", "b-1", "b-2", nil, true},
		{"values the other noise lets vary", "v2.0", "v2.1", "c-1", "c-2", nil, true},
		{"a value either lets vary into one the other does", "v1.0", "v2.0", "b-1", "c-2", nil, true},
		{"a body value outside both", "v1.0", "x", "b-1", "b-2", nil, false},
		{"a header value outside both", "v1.0", "v1.1", "b-1", "x", nil, false},
		{"a rule with no patterns", "v1.0", "x", "b-1", "x", []config.EndpointNoise{{Path: "/version", Body: config.Noise{"data.version": {}}, Header: config.Noise{"X-Build": {}}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules := c.rules
			if rules == nil {
				rules = []config.EndpointNoise{{Path: "/version", Body: config.Noise{"data.version": {`^v1\.`}}, Header: config.Noise{"X-Build": {"^b-"}}}}
			}
			tc := endpointCase("GET", "http://api/version", `{"data":{"version":"`+c.rec+`"}}`)
			tc.HTTPResp.Header = map[string]string{"X-Build": c.recHdr}
			live := &models.HTTPResp{StatusCode: 200, Body: `{"data":{"version":"` + c.live + `"}}`, Header: map[string]string{"X-Build": c.liveHdr}}
			if pass, _ := Match(tc, live, AddEndpointNoise(global, rules, tc), false, false, zap.NewNop(), false); pass != c.want {
				t.Fatalf("pass=%v, want %v", pass, c.want)
			}
		})
	}
}

func TestATestCaseKeyTheComparisonNeverAppliesOnAnArrayLeavesTheRuleOn(t *testing.T) {
	rules := []config.EndpointNoise{{Path: "/version", Body: config.Noise{"items.v": {}}}}
	tc := endpointCase("GET", "http://api/version", `{"items":[{"v":"a1"}]}`)
	tc.Noise = map[string][]string{"body.items[].v": {"^a"}}
	if matchWith(t, tc, `{"items":[{"v":"a2"}]}`, nil, nil) {
		t.Fatal("the test case's key items[].v now applies to items.v; the comparison walks an array's elements under the array's own key")
	}
	if !matchWith(t, tc, `{"items":[{"v":"b2"}]}`, nil, rules) {
		t.Fatal("the test case's key items[].v, which ignores nothing, turned the rule on items.v off")
	}
	tc.Noise = map[string][]string{"body.items.v": {"^a"}}
	if matchWith(t, tc, `{"items":[{"v":"b2"}]}`, nil, rules) {
		t.Fatal("the rule widened the test case's own items.v")
	}
}

func TestBodyNoiseCoveringNamesOtherNoiseBeforeAnEndpointRule(t *testing.T) {
	keys := []string{"data", "version"}
	global := map[string]map[string][]string{"body": {"data.version": {}, matcherUtils.ExactKey("data.version"): {`^v1\.`}}}
	if got, want := matcherUtils.BodyNoiseCovering(keys, nil, global, zap.NewNop()), "body.data.version (globalNoise)"; got != want {
		t.Fatalf("covering %q, want %q", got, want)
	}
	delete(global["body"], "data.version")
	if got, want := matcherUtils.BodyNoiseCovering(keys, nil, global, zap.NewNop()), "body.data.version (globalNoise.endpoints)"; got != want {
		t.Fatalf("covering %q, want %q", got, want)
	}
}

func TestARuleThatIgnoresNothingIsSaid(t *testing.T) {
	version := config.Noise{"version": {}}
	cases := []struct {
		rule   config.EndpointNoise
		reason string
	}{
		{config.EndpointNoise{Method: "GET", Body: version}, "it has no path"},
		{config.EndpointNoise{Path: "/version?x=1", Body: version}, "its path holds"},
		{config.EndpointNoise{Path: "/version#top", Body: version}, "its path holds"},
		{config.EndpointNoise{Path: "https://api.example.com/version", Body: version}, "its path holds"},
		{config.EndpointNoise{Path: "api.example.com/version", Body: version}, "its path holds"},
		{config.EndpointNoise{Path: "GET /version", Body: version}, "its path holds"},
		{config.EndpointNoise{Path: "/version", Body: config.Noise{"items[0].id": {}}}, "body field items[0].id names one element"},
		{config.EndpointNoise{Path: "/version"}, "it names no body field or header"},
	}
	for _, c := range cases {
		core, logs := observer.New(zap.WarnLevel)
		WarnEndpointNoiseProblems(zap.New(core), []config.EndpointNoise{c.rule})
		said := logs.FilterMessage("an endpoint noise rule ignores nothing").All()
		if len(said) != 1 || !strings.HasPrefix(said[0].ContextMap()["reason"].(string), c.reason) {
			t.Errorf("rule %+v: said %v, want one warning whose reason starts %q", c.rule, said, c.reason)
		}
	}
	core, logs := observer.New(zap.WarnLevel)
	WarnEndpointNoiseProblems(zap.New(core), []config.EndpointNoise{
		{Method: "GET", Path: "/version", Body: version},
		{Path: "version", Header: config.Noise{"X-Build": {}}},
		{Path: "/apps/{id}/files/a.json", Body: config.Noise{"items[].at": {}}},
	})
	if logs.Len() != 0 {
		t.Fatalf("rules that match were warned about: %v", logs.All())
	}
}
