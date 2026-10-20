package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A stream test case carried as JSON, as a cloud replay gets it, keeps its
// recorded body and none of its frames (HTTPResp.StreamBody is not JSON).
// RunTestSet compares the app's stream with that body split into frames: the
// recorded stream passes and a changed one fails, under either switch. With
// nothing recorded the test is refused, and says why. With the switch off a
// changed stream passed: it was compared with no recorded frame.
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
		{name: "a stream with nothing recorded is refused", body: "", answer: recorded, wantBody: "keploy refuses this stream test"},
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
