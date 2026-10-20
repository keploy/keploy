package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
// stream recorded empty, or with keepalives alone, is compared with what it
// holds: it passes when the app streams that and ends, and fails when the app
// streams an event it does not hold. A recording that saved nothing of its
// stream (a body over 1 MB) is refused, its stream unread (the result's body,
// which would say why, the matcher empties for such a recording; the log
// says it). With the switch off a
// changed stream passed: it was compared with no recorded frame, and one after
// a recorded keepalive passed once the keepalive had come.
func TestRunTestSetComparesAStreamTestCarriedAsJSONByItsBody(t *testing.T) {
	const (
		recorded = "id: 1\nevent: price\ndata: {\"sku\":\"a\",\"price\":10}\n\nid: 2\nevent: price\ndata: {\"sku\":\"b\",\"price\":20}\n\n"
		changed  = "id: 8\nevent: stock\ndata: {\"sku\":\"x\",\"price\":99}\n\nid: 9\nevent: stock\ndata: {\"sku\":\"y\",\"price\":98}\n\n"
	)
	sse := map[string]string{"Content-Type": "text/event-stream"}
	for _, c := range []struct {
		name     string
		body     string // what the recording saw
		skipped  bool   // the recording did not save its body: over 1 MB
		answer   string // what the app streams
		wantPass bool
		wantBody string // in the result's body
		// wantUnread: refused before the app's stream is read.
		wantUnread bool
	}{
		{name: "the recorded stream passes", body: recorded, answer: recorded, wantPass: true, wantBody: `"sku":"b"`},
		{name: "a changed stream fails", body: recorded, answer: changed, wantBody: `"sku":"x"`},
		{name: "a stream recorded empty passes when the app streams nothing", body: "", answer: "", wantPass: true},
		{name: "a stream recorded empty fails when the app streams", body: "", answer: recorded, wantBody: `"sku":"a"`},
		{name: "a stream of keepalives alone passes when the app streams them and ends", body: ": ping\n\n", answer: ": ping\n\n", wantPass: true},
		{name: "a stream of keepalives alone fails when the app streams events after them", body: ": ping\n\n", answer: ": ping\n\n" + recorded, wantBody: `"sku":"a"`},
		{name: "a stream not saved is refused", skipped: true, answer: recorded, wantUnread: true},
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
				if c.skipped {
					stream.HTTPResp.BodySkipped, stream.HTTPResp.BodySize = true, 2<<20
				}
				carryAsJSON(t, stream)
				if stream.HTTPResp.StreamBody != nil {
					t.Fatalf("JSON carried the frames: %+v", stream.HTTPResp.StreamBody)
				}
				// read counts the bytes of the app's stream read.
				var read atomic.Int64
				h.replayer.hookImpl = nonStreamAnswerHooks{answers: map[string]func(*models.TestCase) *pkg.StreamingHTTPResponse{
					stream.Name: func(tc *models.TestCase) *pkg.StreamingHTTPResponse {
						return &pkg.StreamingHTTPResponse{StatusCode: 200, StatusMessage: http.StatusText(200), Header: sse,
							Reader:       io.NopCloser(&readCount{r: strings.NewReader(c.answer), n: &read}),
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
				if unread := read.Load() == 0; c.wantUnread && !unread {
					t.Errorf("%d bytes of the app's stream read; want it refused before its stream is read", read.Load())
				} else if !c.wantUnread && unread && c.answer != "" {
					t.Errorf("the app's stream was not read; want it compared")
				}
			})
		}
	}
}

// readCount counts, in n, the bytes read through it.
type readCount struct {
	r io.Reader
	n *atomic.Int64
}

func (c *readCount) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
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
// TestAResponseWithNoBodyIsNoStream's), whatever its Content-Type says, and
// RunTestSet compares it as any response, by its status and headers: an SSE or
// NDJSON endpoint's answer to a HEAD, or its 204 (how the SSE spec tells an
// EventSource to stop reconnecting), passes when answered alike, YAML or
// carried as JSON, under either switch. A HEAD test is no stream test; a
// recorded 204 is sent as a stream test, and its answer says whether it has a
// body. An answer that differs fails.
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
					if streaming := pkg.IsHTTPStreamingTestCase(tc); streaming != (c.method != http.MethodHead) {
						t.Fatalf("IsHTTPStreamingTestCase = %v for a recorded %s answered %d; want a stream test unless its request is a HEAD", streaming, c.method, c.status)
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

// A recorded 204 of an SSE endpoint (no body, a stream Content-Type) that the
// app now answers with a stream it never ends, keepalive after keepalive or
// event after event, is a test that ends, and fails, under either switch, with
// the real hooks: its answer is read as a stream is, which ends on its first
// keepalive (nothing of it to compare) or its first frame past the recording.
// Read whole, as a response with no body by its recording was read, the
// answer never ended: under the switch no clock bounds that read, and every
// byte the app sends is progress to the stall rule. The app ends the stream
// only once it wrote far more than its connection's buffers hold, which it
// could only have done with keploy reading on.
func TestRunTestSetEndsARecordedNoContentAnsweredByAnEndlessStream(t *testing.T) {
	const (
		frameSize = 16 << 10
		// endless is far more than the connection's buffers hold, both
		// sides' together: an app that wrote it all had keploy read on.
		endless = 64 << 20
	)
	for _, c := range []struct{ name, frame string }{
		{"keepalives", ": " + strings.Repeat("k", frameSize) + "\n\n"},
		{"events", "data: " + strings.Repeat("x", frameSize) + "\n\n"},
	} {
		for _, on := range []bool{false, true} {
			sw := map[bool]string{false: "switch-off", true: "switch-on"}[on]
			t.Run(c.name+"/"+sw, func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				var readOn atomic.Bool
				app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header()["Date"] = nil
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					flusher := w.(http.Flusher)
					for written := 0; written < endless; written += len(c.frame) {
						if _, err := io.WriteString(w, c.frame); err != nil {
							return // keploy stopped reading and closed the connection
						}
						flusher.Flush()
					}
					readOn.Store(true)
				}))
				t.Cleanup(func() {
					app.CloseClientConnections()
					app.Close()
				})

				h := newPartialRunHarness(t, 2, 0)
				tc := h.cases[1]
				tc.HTTPReq.Method = http.MethodGet
				tc.HTTPReq.URL = app.URL + "/events"
				tc.HTTPResp = models.HTTPResp{StatusCode: http.StatusNoContent, Header: map[string]string{"Content-Type": "text/event-stream"}, Timestamp: tc.HTTPResp.Timestamp}
				keepAsYAML(t, tc)
				h.replayer.hookImpl = newHooks(prLogger(), h.replayer.config, h.replayer.instrumentation, false)
				_ = h.run(t)

				if readOn.Load() {
					t.Fatalf("keploy read the app's endless stream on, %d bytes of it: a stream that never ends is a read that never ends", endless)
				}
				var got *models.TestResult
				for i := range h.report.results {
					if h.report.results[i].TestCaseID == tc.Name {
						got = &h.report.results[i]
					}
				}
				if got == nil {
					t.Fatalf("no result for the test; results: %+v", h.report.results)
				}
				if got.Status == models.TestStatusPassed {
					t.Fatalf("a recorded 204 answered with a 200 stream passed: %+v", got.Result)
				}
			})
		}
	}
}
