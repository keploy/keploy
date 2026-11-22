package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// goJSONWithoutContentType serves what a Go handler sends that writes a JSON
// document of over net/http's 2 KB buffer and sets no Content-Type: net/http
// sniffs "text/plain; charset=utf-8" for it and, having no Content-Length,
// sends it chunked. It is no stream: one document, written at once.
func goJSONWithoutContentType(t *testing.T) (url, body string) {
	t.Helper()
	items := make([]map[string]string, 0, 120)
	for i := 0; i < 120; i++ {
		items = append(items, map[string]string{"id": fmt.Sprintf("cluster-%03d", i), "name": "a cluster of the list"})
	}
	doc, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, string(doc)
}

// storedAs is a response as a recording stores it and a replay reads it
// back: through its YAML form, which keeps a stream's body as its frames.
func storedAs(t *testing.T, resp models.HTTPResp) models.HTTPResp {
	t.Helper()
	out, err := yaml.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got models.HTTPResp
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// getGoJSON is the app's answer as keploy's client reads it, with the
// preconditions the cases below stand on: text/plain, chunked, no length.
func getGoJSON(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("precondition: net/http sniffed %q for JSON written with no Content-Type; want text/plain; charset=utf-8", ct)
	}
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" || resp.ContentLength != -1 {
		t.Fatalf("precondition: the answer is not chunked with no length (TransferEncoding %v, ContentLength %d)", resp.TransferEncoding, resp.ContentLength)
	}
	return resp
}

// A chunked text/plain answer, Go's JSON written with no Content-Type, is no
// stream: how the transport framed its length is no evidence of one. It used
// to be one (isLikelyStreamingHTTPResponse), so the test case was replayed
// after every other, out of its recorded order, with a stream's mock window,
// and with no mocks in its report row.
func TestAChunkedTextPlainJSONAnswerIsNoStream(t *testing.T) {
	url, body := goJSONWithoutContentType(t)
	resp := getGoJSON(t, url)

	for _, header := range []map[string]string{
		// As keploy's own capture records it (Go keeps the transfer
		// coding out of the header): no Content-Length.
		{"Content-Type": "text/plain; charset=utf-8"},
		// As a capture that keeps the header line records it.
		{"Content-Type": "text/plain; charset=utf-8", "Transfer-Encoding": "chunked"},
	} {
		t.Run(fmt.Sprint(header), func(t *testing.T) {
			// Recorded, and read back as a replay reads it.
			tc := &models.TestCase{Name: "get-cluster-list", Kind: models.HTTP,
				HTTPReq:  models.HTTPReq{Method: "GET", URL: url},
				HTTPResp: storedAs(t, models.HTTPResp{StatusCode: 200, Header: header, Body: body})}
			if len(tc.HTTPResp.StreamBody) != 0 || tc.HTTPResp.Body != body {
				t.Errorf("the recording kept the JSON as %d frames (body %d of %d bytes); want it whole", len(tc.HTTPResp.StreamBody), len(tc.HTTPResp.Body), len(body))
			}
			if IsHTTPStreamingTestCase(tc) {
				t.Errorf("a chunked text/plain JSON test case is taken for a stream")
			}
			if got := DetectHTTPStreamConfig(tc, resp).Mode; got != HTTPStreamModeNone {
				t.Errorf("the app's chunked text/plain JSON answer is read as a %q stream; want none", got)
			}
		})
	}
}

// A recorded stream the recording kept no frames of (multipart, which is
// never framed) answered with chunked text/plain (an error page, say) is read
// whole and compared as a normal response: the answer's transfer coding made
// it a text stream, compared against no recorded line, and passed.
func TestAChunkedTextPlainAnswerToAStreamIsReadWhole(t *testing.T) {
	tc := &models.TestCase{Name: "camera", Kind: models.HTTP,
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "--frame\r\n\r\none\r\n--frame--\r\n",
			Header: map[string]string{"Content-Type": "multipart/x-mixed-replace; boundary=frame"}}}
	if !IsHTTPStreamingTestCase(tc) {
		t.Fatal("precondition: a multipart/x-mixed-replace test case is not a stream")
	}
	live := &http.Response{Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, TransferEncoding: []string{"chunked"}, ContentLength: -1}
	if got := DetectHTTPStreamConfig(tc, live).Mode; got != HTTPStreamModeNone {
		t.Errorf("a chunked text/plain answer to a multipart stream is read as a %q stream; want it read whole", got)
	}
}

// The same goes for a chunked octet stream with no frames kept: a download
// whose length the server did not know is no stream either.
func TestAChunkedOctetStreamWithNoFramesIsNoStream(t *testing.T) {
	tc := &models.TestCase{Name: "download", Kind: models.HTTP,
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "PK\x03\x04",
			Header: map[string]string{"Content-Type": "application/octet-stream", "Transfer-Encoding": "chunked"}}}
	if IsHTTPStreamingTestCase(tc) {
		t.Errorf("a chunked octet stream the recording kept whole is taken for a stream")
	}
	live := &http.Response{Header: http.Header{"Content-Type": {"application/octet-stream"}}, TransferEncoding: []string{"chunked"}, ContentLength: -1}
	if got := DetectHTTPStreamConfig(tc, live).Mode; got != HTTPStreamModeNone {
		t.Errorf("a chunked octet-stream answer is read as a %q stream; want none", got)
	}
}

// Real evidence still makes a stream: a stream's Content-Type, or frames the
// recording kept, whatever the transport did with the length.
func TestAStreamIsKnownByItsContentTypeOrItsRecordedFrames(t *testing.T) {
	frames := []models.HTTPStreamChunk{
		{Data: []models.HTTPStreamDataField{{Key: "raw", Value: "[INFO] booting"}}},
		{Data: []models.HTTPStreamDataField{{Key: "raw", Value: "[INFO] ready"}}},
	}
	for _, c := range []struct {
		name   string
		header map[string]string
		frames []models.HTTPStreamChunk
		want   HTTPStreamMode
	}{
		{"server-sent events", map[string]string{"Content-Type": "text/event-stream"}, nil, HTTPStreamModeSSE},
		{"server-sent events with a charset", map[string]string{"Content-Type": "text/event-stream; charset=utf-8", "Content-Length": "40"}, nil, HTTPStreamModeSSE},
		{"a Content-Type sent twice, as replay has always read it", map[string]string{"Content-Type": "text/event-stream,text/event-stream"}, nil, HTTPStreamModeNone},
		{"ndjson", map[string]string{"Content-Type": "application/x-ndjson"}, nil, HTTPStreamModeNDJSON},
		{"ndjson by its other name", map[string]string{"Content-Type": "application/ndjson"}, nil, HTTPStreamModeNDJSON},
		{"multipart/x-mixed-replace", map[string]string{"Content-Type": "multipart/x-mixed-replace; boundary=frame"}, nil, HTTPStreamModeMultipart},
		{"multipart/mixed", map[string]string{"Content-Type": "multipart/mixed; boundary=part"}, nil, HTTPStreamModeMultipart},
		{"text the recording kept as frames", map[string]string{"Content-Type": "text/plain"}, frames, HTTPStreamModePlainText},
		{"an octet stream the recording kept as a frame", map[string]string{"Content-Type": "application/octet-stream"}, frames[:1], HTTPStreamModeBinary},
		{"chunked JSON", map[string]string{"Content-Type": "application/json", "Transfer-Encoding": "chunked"}, nil, HTTPStreamModeNone},
		{"multipart with no boundary", map[string]string{"Content-Type": "multipart/mixed"}, nil, HTTPStreamModeNone},
		{"a form", map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, nil, HTTPStreamModeNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: c.name, Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200, Header: c.header, StreamBody: c.frames}}
			if got := DetectHTTPStreamConfig(tc, nil).Mode; got != c.want {
				t.Errorf("mode %q; want %q", got, c.want)
			}
			if got := IsHTTPStreamingTestCase(tc); got != (c.want != HTTPStreamModeNone) {
				t.Errorf("IsHTTPStreamingTestCase %v; want %v", got, c.want != HTTPStreamModeNone)
			}
		})
	}
}

// application/stream+json (NDJSON by its older name) is, under the proven
// switch, a stream by its Content-Type. A recording keeps its body whole, and
// the stream is compared by that body split into its lines: a recording made
// without the switch, or a test case carried as JSON (which keeps no frames),
// reads the same. Without the switch it is read whole, as it always has been.
func TestStreamJSONIsAStreamByItsContentTypeUnderTheProvenSwitch(t *testing.T) {
	const lines = "{\"n\":1}\n{\"n\":2}\n"
	answer := lines
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	recorded := models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/stream+json"}, Body: lines}

	t.Setenv(provenswitch.Env, "")
	tc := &models.TestCase{Name: "flux", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: srv.URL}, HTTPResp: storedAs(t, recorded)}
	if IsHTTPStreamingTestCase(tc) {
		t.Errorf("without the proven switch an application/stream+json test case is taken for a stream; want it read whole, as before")
	}

	t.Setenv(provenswitch.Env, "1")
	tc.HTTPResp = storedAs(t, recorded)
	if len(tc.HTTPResp.StreamBody) != 0 || tc.HTTPResp.Body != lines {
		t.Fatalf("under the proven switch the recording kept application/stream+json as %d frames (body %q); want it whole", len(tc.HTTPResp.StreamBody), tc.HTTPResp.Body)
	}
	if !IsHTTPStreamingTestCase(tc) {
		t.Fatal("under the proven switch an application/stream+json test case is not a stream")
	}
	compare := func() (bool, *StreamMismatchInfo) {
		t.Helper()
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "test-set", zap.NewNop(), SimulationConfig{APITimeout: 10})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Reader.Close()
		if resp.StreamConfig.Mode != HTTPStreamModeNDJSON {
			t.Fatalf("the application/stream+json answer is read as %q; want ndjson", resp.StreamConfig.Mode)
		}
		ok, _, info, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, nil, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		return ok, info
	}
	if ok, info := compare(); !ok {
		t.Errorf("the app's application/stream+json answer, as recorded, does not match its recording: %+v", info)
	}
	answer = "{\"n\":1}\n{\"n\":3}\n"
	if ok, _ := compare(); ok {
		t.Errorf("an application/stream+json answer with a line of its own matches the recording")
	}
}

// Under the proven switch a stream whose recording kept no frames (a test
// case carried as JSON keeps none) is compared by its body split as a
// recording splits a stream, not as a recording of no frame, past which
// every frame of the answer would be.
func TestAStreamRecordedWithoutFramesIsComparedByItsBodyUnderTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		contentType, recorded, same, other string
		mode                               HTTPStreamMode
	}{
		{"text/event-stream", "data: one\n\ndata: two\n\n", "data: one\n\ndata: two\n\n", "data: one\n\ndata: six\n\n", HTTPStreamModeSSE},
		{"application/x-ndjson", "{\"n\":1}\n{\"n\":2}\n", "{\"n\":1}\n{\"n\":2}\n", "{\"n\":1}\n{\"n\":6}\n", HTTPStreamModeNDJSON},
		// A body that is not UTF-8, which a recording keeps whole.
		{"application/stream+json", "{\"s\":\"caf\xe9\"}\n{\"n\":2}\n", "{\"s\":\"caf\xe9\"}\n{\"n\":2}\n", "{\"s\":\"caf\xe9\"}\n{\"n\":6}\n", HTTPStreamModeNDJSON},
	} {
		expected := models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": c.contentType}, Body: c.recorded}
		cfg := HTTPStreamConfig{Mode: c.mode}
		if ok, _, info, err := CompareHTTPStream(expected, strings.NewReader(c.same), cfg, nil, zap.NewNop()); err != nil || !ok {
			t.Errorf("%s: the answer as recorded does not match a recording of no frames: ok=%v err=%v info=%+v", c.contentType, ok, err, info)
		}
		if ok, _, _, _ := CompareHTTPStream(expected, strings.NewReader(c.other), cfg, nil, zap.NewNop()); ok {
			t.Errorf("%s: an answer with a frame of its own matches a recording of no frames", c.contentType)
		}
	}
}

// A chunked text/plain JSON test case is sent and read as any other: whole,
// by SimulateHTTP, and an NDJSON one still as a stream. SimulateHTTPStreaming
// reads the live answer by the same rule: the recorded stream's frames say
// how, not the transfer coding of the answer.
func TestSimulateHTTPStreamingReadsTheAnswerByTheStreamRule(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, "{\"n\":1}\n")
		f.Flush()
		_, _ = io.WriteString(w, "{\"n\":2}\n")
	}))
	defer srv.Close()
	tc := &models.TestCase{Name: "ndjson", Kind: models.HTTP,
		HTTPReq:  models.HTTPReq{Method: "GET", URL: srv.URL},
		HTTPResp: storedAs(t, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: "{\"n\":1}\n{\"n\":2}\n"})}
	if len(tc.HTTPResp.StreamBody) != 2 {
		t.Fatalf("precondition: the recording kept the NDJSON body as %d frames; want its 2 lines", len(tc.HTTPResp.StreamBody))
	}
	if !IsHTTPStreamingTestCase(tc) {
		t.Fatal("an NDJSON test case is no longer a stream")
	}
	resp, err := SimulateHTTPStreaming(ctx, tc, "test-set", zap.NewNop(), SimulationConfig{APITimeout: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Reader.Close()
	if resp.StreamConfig.Mode != HTTPStreamModeNDJSON {
		t.Fatalf("the NDJSON answer is read as %q; want ndjson", resp.StreamConfig.Mode)
	}
	ok, got, info, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, nil, zap.NewNop())
	if err != nil || !ok {
		t.Fatalf("the NDJSON stream did not match: ok=%v err=%v info=%+v body=%q", ok, err, info, got)
	}
	if !strings.Contains(got, `{"n":2}`) {
		t.Fatalf("the stream comparison did not read the second frame: %q", got)
	}
}
