package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A stream test case carried as JSON, as a cloud replay gets it, keeps its
// recorded body and none of its frames (HTTPResp.StreamBody is not JSON).
// RunTestSet compares the app's stream with that body split into frames: the
// recorded stream passes and a changed one fails, under either switch. A
// stream recorded empty is compared with its empty stream: it passes when the
// app streams nothing, and fails when the app streams. With keepalives alone
// recorded the test is refused, and says why. With the switch off a changed
// stream passed: it was compared with no recorded frame.
func TestRunTestSetComparesAStreamTestCarriedAsJSONByItsBody(t *testing.T) {
	const (
		recorded = "id: 1\nevent: price\ndata: {\"sku\":\"a\",\"price\":10}\n\nid: 2\nevent: price\ndata: {\"sku\":\"b\",\"price\":20}\n\n"
		changed  = "id: 8\nevent: stock\ndata: {\"sku\":\"x\",\"price\":99}\n\nid: 9\nevent: stock\ndata: {\"sku\":\"y\",\"price\":98}\n\n"
	)
	sse := map[string]string{"Content-Type": "text/event-stream"}
	for _, c := range []struct {
		name     string
		body     string // what the recording saw
		answer   string // what the app streams
		wantPass bool
		wantBody string // in the result's body
	}{
		{name: "the recorded stream passes", body: recorded, answer: recorded, wantPass: true, wantBody: `"sku":"b"`},
		{name: "a changed stream fails", body: recorded, answer: changed, wantBody: `"sku":"x"`},
		{name: "a stream recorded empty passes when the app streams nothing", body: "", answer: "", wantPass: true},
		{name: "a stream recorded empty fails when the app streams", body: "", answer: recorded, wantBody: `"sku":"a"`},
		{name: "a stream of keepalives alone is refused", body: ": ping\n\n", answer: recorded, wantBody: "keploy refuses this stream test"},
	} {
		for _, on := range []bool{false, true} {
			sw := map[bool]string{false: "switch-off", true: "switch-on"}[on]
			t.Run(c.name+"/"+sw, func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				h := newPartialRunHarness(t, 2, 0)
				stream := h.cases[1]
				stream.HTTPResp.Header = sse
				stream.HTTPResp.Body = c.body
				carryAsJSON(t, stream)
				if stream.HTTPResp.StreamBody != nil {
					t.Fatalf("JSON carried the frames: %+v", stream.HTTPResp.StreamBody)
				}
				h.replayer.hookImpl = nonStreamAnswerHooks{answers: map[string]func(*models.TestCase) *pkg.StreamingHTTPResponse{
					stream.Name: func(tc *models.TestCase) *pkg.StreamingHTTPResponse {
						return &pkg.StreamingHTTPResponse{StatusCode: 200, StatusMessage: http.StatusText(200), Header: sse,
							Reader:       io.NopCloser(strings.NewReader(c.answer)),
							StreamConfig: pkg.DetectHTTPStreamConfig(tc, &http.Response{Header: http.Header{"Content-Type": {sse["Content-Type"]}}})}
					},
				}}
				_ = h.run(t)

				var got *models.TestResult
				for i := range h.report.results {
					if h.report.results[i].TestCaseID == stream.Name {
						got = &h.report.results[i]
					}
				}
				if got == nil {
					t.Fatalf("no result for the stream test; results: %+v", h.report.results)
				}
				if pass := got.Status == models.TestStatusPassed; pass != c.wantPass {
					t.Errorf("status %s; want passed=%v (result body %q)", got.Status, c.wantPass, got.Res.Body)
				}
				if !strings.Contains(got.Res.Body, c.wantBody) {
					t.Errorf("result body %q; want it to hold %q", got.Res.Body, c.wantBody)
				}
			})
		}
	}
}

// carryAsJSON makes tc what a replay gets from JSON once its recording kept
// it: written as YAML and read back (a stream's body kept as its frames), then
// sent through JSON.
func carryAsJSON(t *testing.T, tc *models.TestCase) {
	t.Helper()
	out, err := yamlLib.Marshal(tc.HTTPResp)
	if err != nil {
		t.Fatal(err)
	}
	var kept models.HTTPResp
	if err := yamlLib.Unmarshal(out, &kept); err != nil {
		t.Fatal(err)
	}
	if tc.HTTPResp.Body != "" && len(kept.StreamBody) == 0 {
		t.Fatalf("the recording kept no frame of %q", tc.HTTPResp.Body)
	}
	tc.HTTPResp = kept
	wire, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	var carried models.TestCase
	if err := json.Unmarshal(wire, &carried); err != nil {
		t.Fatal(err)
	}
	*tc = carried
}

// A response HTTP says has no body (the answer to a HEAD, a 204 or 304) is no
// stream (a 304's, which net/http's server sends with no Content-Type, is
// TestAResponseWithNoBodyIsNoStream's), whatever its Content-Type says, and RunTestSet compares it as any
// response, by its status and headers: an SSE or NDJSON endpoint's answer to a
// HEAD, or its 204 (how the SSE spec tells an EventSource to stop
// reconnecting), passes when answered alike, YAML or carried as JSON, under
// either switch. Read as a stream it had nothing recorded to compare, and
// re-recording cannot give it a body. An answer that differs fails.
func TestRunTestSetComparesAResponseWithNoBodyByItsStatusAndHeaders(t *testing.T) {
	type answer struct {
		status      int
		contentType string
		body        string
	}
	var mu sync.Mutex
	answers := map[string]answer{}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a := answers[r.URL.Path]
		mu.Unlock()
		// No Date: the recording below has none, and noise is not
		// what is under test.
		w.Header()["Date"] = nil
		w.Header().Set("Content-Type", a.contentType)
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(app.Close)
	for _, c := range []struct {
		name        string
		method      string
		status      int // recorded, and answered unless differs
		contentType string
		differs     *answer // what the app answers now, when it changed
	}{
		{name: "HEAD of an SSE stream", method: http.MethodHead, status: 200, contentType: "text/event-stream"},
		{name: "204 of an SSE stream", method: http.MethodGet, status: 204, contentType: "text/event-stream"},
		{name: "HEAD of an NDJSON stream", method: http.MethodHead, status: 200, contentType: "application/x-ndjson"},
		{name: "204 of an NDJSON stream", method: http.MethodGet, status: 204, contentType: "application/x-ndjson"},
		{name: "a 204 now a stream", method: http.MethodGet, status: 204, contentType: "text/event-stream",
			differs: &answer{status: 200, contentType: "text/event-stream", body: "data: a\n\n"}},
	} {
		for _, carried := range []string{"yaml", "json"} {
			for _, on := range []bool{false, true} {
				sw := map[bool]string{false: "switch-off", true: "switch-on"}[on]
				t.Run(c.name+"/"+carried+"/"+sw, func(t *testing.T) {
					if on {
						t.Setenv(provenswitch.Env, "1")
					} else {
						t.Setenv(provenswitch.Env, "")
					}
					h := newPartialRunHarness(t, 2, 0)
					tc := h.cases[1]
					path := "/" + strings.NewReplacer(" ", "-").Replace(c.name) + "-" + carried + "-" + sw
					tc.HTTPReq.Method = models.Method(c.method)
					tc.HTTPReq.URL = app.URL + path
					tc.HTTPResp = models.HTTPResp{StatusCode: c.status, Header: map[string]string{"Content-Type": c.contentType}, Timestamp: tc.HTTPResp.Timestamp}
					now := answer{status: c.status, contentType: c.contentType}
					if c.differs != nil {
						now = *c.differs
					}
					mu.Lock()
					answers[path] = now
					mu.Unlock()
					keepAsYAML(t, tc)
					if carried == "json" {
						carryAsJSON(t, tc)
					}
					if pkg.IsHTTPStreamingTestCase(tc) {
						t.Fatalf("a response with no body is a stream test: %+v", tc.HTTPResp)
					}
					h.replayer.hookImpl = newHooks(prLogger(), h.replayer.config, h.replayer.instrumentation, false)
					_ = h.run(t)

					var got *models.TestResult
					for i := range h.report.results {
						if h.report.results[i].TestCaseID == tc.Name {
							got = &h.report.results[i]
						}
					}
					if got == nil {
						t.Fatalf("no result for the test; results: %+v", h.report.results)
					}
					wantPass := c.differs == nil
					if pass := got.Status == models.TestStatusPassed; pass != wantPass {
						t.Fatalf("status %s; want passed=%v (result %+v)", got.Status, wantPass, got.Result)
					}
					if strings.Contains(got.Res.Body, "keploy refuses this stream test") {
						t.Fatalf("refused as a stream: %q", got.Res.Body)
					}
				})
			}
		}
	}
}

// keepAsYAML makes tc's response what its recording keeps: written as YAML
// and read back.
func keepAsYAML(t *testing.T, tc *models.TestCase) {
	t.Helper()
	out, err := yamlLib.Marshal(tc.HTTPResp)
	if err != nil {
		t.Fatal(err)
	}
	var kept models.HTTPResp
	if err := yamlLib.Unmarshal(out, &kept); err != nil {
		t.Fatal(err)
	}
	tc.HTTPResp = kept
}
