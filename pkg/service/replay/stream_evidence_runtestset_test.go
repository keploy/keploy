package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// A chunked text/plain JSON test case (Go's JSON written with no
// Content-Type, which net/http sends chunked once it is over 2 KB) is no
// stream: it runs in its recorded order, with its recorded mock window, and
// its report row carries the mocks it consumed, as every other test case's
// does. An NDJSON and an SSE test case are still streams: run after the
// others, with a stream's window, as before.
//
// The chunked one used to be taken for a stream on its transfer coding alone
// (isLikelyStreamingHTTPResponse): it ran at the end of its set, so its
// frozen clock went back past the test cases run before it, its window was
// its request time plus the stream timeout, and its row had no mocks.
func TestRunTestSetRunsAChunkedTextPlainJSONTestInItsRecordedOrder(t *testing.T) {
	h := newPartialRunHarness(t, 5, 0)
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i, tc := range h.cases {
		tc.HTTPReq.Timestamp = base.Add(time.Duration(i) * time.Second)
		tc.HTTPResp.Timestamp = tc.HTTPReq.Timestamp.Add(100 * time.Millisecond)
	}
	chunked := h.cases[1] // test-2
	chunked.HTTPResp.Header = map[string]string{"Content-Type": "text/plain; charset=utf-8", "Transfer-Encoding": "chunked"}
	chunked.HTTPResp.Body = `[{"id":"cluster-1"},{"id":"cluster-2"}]`
	h.streaming("test-3") // NDJSON
	ndjson := h.cases[2]
	sse := h.cases[3] // test-4
	sse.HTTPResp.Header = map[string]string{"Content-Type": "text/event-stream"}
	sse.HTTPResp.Body = "data: one\n\ndata: two\n\n"
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "mock-1", Kind: models.Mongo}}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the set reported %q; want PASSED", status)
	}

	// The order the test cases ran in is the order of their mock windows:
	// each is sent to the agent just before its request. (The report's rows
	// are not in it: the main loop inserts its rows once the loop is done.)
	var ran []string
	for _, p := range h.instr.allParams {
		for _, tc := range h.cases {
			if p.AfterTime.Equal(tc.HTTPReq.Timestamp) {
				ran = append(ran, tc.Name)
			}
		}
	}
	if got, want := strings.Join(ran, ","), "test-1,test-2,test-5,test-3,test-4"; got != want {
		t.Errorf("the test cases ran in the order %s; want %s: the chunked text/plain JSON test in its recorded place, the NDJSON and SSE streams after the rest", got, want)
	}
	byName := map[string]models.TestResult{}
	for _, res := range h.report.results {
		byName[res.TestCaseID] = res
	}
	if len(byName) != len(h.cases) {
		t.Errorf("the report has rows for %d test cases; want %d", len(byName), len(h.cases))
	}

	windowOf := func(tc *models.TestCase) (models.MockFilterParams, bool) {
		for _, p := range h.instr.allParams {
			if p.AfterTime.Equal(tc.HTTPReq.Timestamp) {
				return p, true
			}
		}
		return models.MockFilterParams{}, false
	}
	if p, ok := windowOf(chunked); !ok {
		t.Errorf("no mock window was sent for the chunked text/plain JSON test")
	} else if !p.BeforeTime.Equal(chunked.HTTPResp.Timestamp) {
		t.Errorf("the chunked text/plain JSON test's mock window ends at %s, %s past its recorded response; want its recorded window",
			p.BeforeTime.Format(time.RFC3339Nano), p.BeforeTime.Sub(chunked.HTTPResp.Timestamp))
	}
	for _, stream := range []*models.TestCase{ndjson, sse} {
		if p, ok := windowOf(stream); !ok {
			t.Errorf("no mock window was sent for the %s stream test", stream.Name)
		} else if !p.BeforeTime.After(stream.HTTPResp.Timestamp) {
			t.Errorf("the %s stream test's mock window ends at its recorded response; want a stream's window, past it", stream.Name)
		}
	}

	if res := byName["test-2"]; res.MockMismatches == nil || len(res.MockMismatches.ActualMocks) == 0 {
		t.Errorf("the chunked text/plain JSON test's row has no mocks (MockMismatches %+v); want the mock it consumed, as test-1's row has (%+v)",
			res.MockMismatches, byName["test-1"].MockMismatches)
	}
	if res := byName["test-1"]; res.MockMismatches == nil {
		t.Fatalf("precondition: a regular test's row has no MockMismatches, so the check above proves nothing")
	}
}

// keploy test sends a chunked text/plain JSON test case as any other and
// reads the app's answer whole (SimulateHTTP), as recorded and read back; an
// SSE one it still sends as a stream (SimulateHTTPStreaming), read frame by
// frame.
func TestHooksSendAChunkedTextPlainJSONTestAndReadItWhole(t *testing.T) {
	items := make([]map[string]string, 0, 120)
	for i := 0; i < 120; i++ {
		items = append(items, map[string]string{"id": fmt.Sprintf("cluster-%03d", i), "name": "a cluster of the list"})
	}
	doc, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	const events = "data: one\n\ndata: two\n\n"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, events)
			return
		}
		// No Content-Type: net/http sniffs text/plain and, the body being
		// over its 2 KB buffer, sends it chunked with no Content-Length.
		_, _ = w.Write(doc)
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Test.BasePath = app.URL
	cfg.Test.APITimeout = 10

	// recorded is a test case as a recording stores it and a replay reads
	// it back: through its YAML form.
	recorded := func(name string, resp models.HTTPResp) *models.TestCase {
		t.Helper()
		tc := bpTestCase(name, 1)
		out, err := yaml.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		tc.HTTPResp = models.HTTPResp{}
		if err := yaml.Unmarshal(out, &tc.HTTPResp); err != nil {
			t.Fatal(err)
		}
		if tc.HTTPReq.URL, err = ReplaceBaseURL(app.URL, tc.HTTPReq.URL); err != nil {
			t.Fatal(err)
		}
		return tc
	}
	hooks := NewHooks(zap.NewNop(), cfg, &bpNoAgent{})

	list := recorded("list", models.HTTPResp{StatusCode: 200, Body: string(doc), Timestamp: time.Now(),
		Header: map[string]string{"Content-Type": "text/plain; charset=utf-8", "Transfer-Encoding": "chunked"}})
	got, err := hooks.SimulateRequest(context.Background(), list, "test-set-0")
	if err != nil {
		t.Fatal(err)
	}
	switch resp := got.(type) {
	case *models.HTTPResp:
		if resp.Body != string(doc) {
			t.Errorf("the app's chunked text/plain JSON answer was read as %d bytes; want its %d", len(resp.Body), len(doc))
		}
	case *pkg.StreamingHTTPResponse:
		_ = resp.Reader.Close()
		t.Errorf("a chunked text/plain JSON test case was sent as a %q stream; want it sent and read whole", resp.StreamConfig.Mode)
	default:
		t.Fatalf("SimulateRequest answered %T", got)
	}

	stream := recorded("events", models.HTTPResp{StatusCode: 200, Body: events, Timestamp: time.Now(),
		Header: map[string]string{"Content-Type": "text/event-stream"}})
	got, err = hooks.SimulateRequest(context.Background(), stream, "test-set-0")
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := got.(*pkg.StreamingHTTPResponse)
	if !ok {
		t.Fatalf("an SSE test case was answered with a %T; want it sent as a stream", got)
	}
	defer resp.Reader.Close()
	if resp.StreamConfig.Mode != pkg.HTTPStreamModeSSE {
		t.Fatalf("the SSE answer is read as %q; want sse", resp.StreamConfig.Mode)
	}
	if ok, body, info, err := pkg.CompareHTTPStream(stream.HTTPResp, resp.Reader, resp.StreamConfig, nil, zap.NewNop()); err != nil || !ok {
		t.Fatalf("the SSE stream did not match: ok=%v err=%v info=%+v body=%q", ok, err, info, body)
	}
}
