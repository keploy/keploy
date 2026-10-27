package http

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// requestNoiseCases are options requestNoiseFor builds the request noise of:
// none, each bucket, case variants of one header, a bucket present but empty,
// value patterns on request-body noise, and auto header noise turned off.
func requestNoiseCases() map[string]models.OutgoingOptions {
	return map[string]models.OutgoingOptions{
		"no configuration":      {},
		"auto header noise off": {DisableAutoHeaderNoise: true},
		"header noise": {NoiseConfig: map[string]map[string][]string{
			"header": {"X-Request-Id": {}, "x-request-id": {"^r-"}, "Date": nil}}},
		"header noise, auto off": {DisableAutoHeaderNoise: true, NoiseConfig: map[string]map[string][]string{
			"header": {"X-Trace": {}}}},
		"empty header bucket, auto off": {DisableAutoHeaderNoise: true, NoiseConfig: map[string]map[string][]string{
			"header": {}}},
		"nil header bucket, auto off": {DisableAutoHeaderNoise: true, NoiseConfig: map[string]map[string][]string{
			"header": nil}},
		"request-body noise": {NoiseConfig: map[string]map[string][]string{
			"requestbody": {"Order.ID": {}, "ts": {"^[0-9]+$"}}}},
		"url noise": {NoiseConfig: map[string]map[string][]string{
			"url": {"/orders/[0-9]+": {}, "/users/.*": {}}}},
		"every bucket": {NoiseConfig: map[string]map[string][]string{
			"header":      {"X-A": {}},
			"requestbody": {"a.b": {}},
			"url":         {"/v[0-9]+/": {}},
			"body":        {"resp": {}}}},
	}
}

// sortedRequestNoise is n with its URL patterns and each header's patterns
// sorted: NewRequestNoise lists them in its maps' iteration order.
func sortedRequestNoise(n RequestNoise) RequestNoise {
	out := RequestNoise{Body: n.Body}
	if n.URL != nil {
		out.URL = append([]string{}, n.URL...)
		sort.Strings(out.URL)
	}
	if n.Header != nil {
		out.Header = make(map[string][]string, len(n.Header))
		for k, v := range n.Header {
			if v != nil {
				v = append([]string{}, v...)
				sort.Strings(v)
			}
			out.Header[k] = v
		}
	}
	return out
}

// The request noise built once per options value is the noise built for each
// call (NewRequestNoise), for every options value, with the proven switch off
// and on, the second time it is asked for as the first, and again once the
// options' configuration was changed in place.
func TestRequestNoiseForIsTheNoiseOfItsOptions(t *testing.T) {
	nop := zap.NewNop()
	for _, proven := range []string{"0", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			for name, opts := range requestNoiseCases() {
				want := sortedRequestNoise(NewRequestNoise(nop, opts))
				for i := 0; i < 2; i++ {
					if got := sortedRequestNoise(requestNoiseFor(nop, opts)); !reflect.DeepEqual(got, want) {
						t.Errorf("%s, call %d: requestNoiseFor = %#v, NewRequestNoise = %#v", name, i+1, got, want)
					}
				}
			}
		})
	}

	opts := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"X-A": {}}}}
	_ = requestNoiseFor(nop, opts)
	opts.NoiseConfig["header"]["X-B"] = []string{}
	if _, ok := requestNoiseFor(nop, opts).Header["x-b"]; !ok {
		t.Error("a header added to the options in place is not noise")
	}
	opts.NoiseConfig["header"]["X-B"] = []string{"^v"}
	if got := requestNoiseFor(nop, opts).Header["x-b"]; !reflect.DeepEqual(got, []string{"^v"}) {
		t.Errorf("a header's patterns changed in place: noise %q, want [^v]", got)
	}
	opts.NoiseConfig["header"]["X-B"][0] = "^w"
	if got := requestNoiseFor(nop, opts).Header["x-b"]; !reflect.DeepEqual(got, []string{"^w"}) {
		t.Errorf("a header's pattern changed in place, the same number of them: noise %q, want [^w]", got)
	}
	opts.NoiseConfig["header"]["X-N"] = nil
	_ = requestNoiseFor(nop, opts)
	delete(opts.NoiseConfig["header"], "X-N")
	opts.NoiseConfig["header"]["X-M"] = nil
	n := requestNoiseFor(nop, opts).Header
	_, renamed := n["x-m"]
	if _, stale := n["x-n"]; !renamed || stale {
		t.Errorf("a header with no patterns renamed in place: noise %v, want x-m and not x-n", n)
	}
	delete(opts.NoiseConfig, "header")
	if _, ok := requestNoiseFor(nop, opts).Header["x-a"]; ok {
		t.Error("a header bucket removed from the options in place is still noise")
	}
}

// The calls of one options value build their request noise once and allocate
// nothing for it. The switch is pinned, as the agent pins it from its flag:
// unpinned, every call reads KEPLOY_PROVEN, and on Windows that read allocates
// (syscall.Getenv converts the key to UTF-16 and makes a buffer), so the row
// would count the test process's env read and not the memo.
func TestRequestNoiseForBuildsOncePerOptionsValue(t *testing.T) {
	provenswitch.Pin(false)
	t.Cleanup(provenswitch.Unpin)
	nop := zap.NewNop()
	opts := requestNoiseCases()["every bucket"]
	lastRequestNoise.Store(nil)
	before := requestNoiseBuilds.Load()
	allocs := testing.AllocsPerRun(100, func() { _ = requestNoiseFor(nop, opts) })
	if builds := requestNoiseBuilds.Load() - before; builds != 1 {
		t.Errorf("101 calls of one options value built the request noise %d times, want once", builds)
	}
	if allocs != 0 {
		t.Errorf("a call of an options value already built allocated %v times, want 0", allocs)
	}
}

// With debug on, the request a call came with is logged by its shape only:
// its method, its path without the query, its header names and its body's
// length. Its credentials, cookies, query and body are customer content and
// none of their values is logged. Main logged the complete request, its
// headers and its body.
func TestTheRequestIsLoggedByItsShapeOnly(t *testing.T) {
	secrets := []string{"Bearer au7h-v4lue", "s1d=c00k1e-v4lue", "q-t0ken-v4lue", "b0dy-v4lue"}
	body := `{"note":"` + secrets[3] + `"}`
	req := "POST /u?token=" + secrets[2] + " HTTP/1.1\r\nHost: api.example\r\nAuthorization: " + secrets[0] +
		"\r\nCookie: " + secrets[1] + "\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"
	core, logs := observer.New(zapcore.DebugLevel)
	h := &HTTP{Logger: zap.New(core)}
	db := serveHTTPMock(t, h, req, resp)
	app, proxy := tcpPair(t)
	runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(req), proxy, nil, db, models.OutgoingOptions{})
	})
	if got := readServed(t, bufio.NewReader(app)); got != "pong" {
		t.Fatalf("served %q, want the mock's answer", got)
	}
	input := logs.FilterMessage("decodeHTTP debug logs for input").All()
	if len(input) != 1 {
		t.Fatalf("logged the request %d times, want once", len(input))
	}
	want := map[string]interface{}{
		"method":       "POST",
		"path":         "/u",
		"header names": []interface{}{"Authorization", "Content-Length", "Content-Type", "Cookie", "Host"},
		"body bytes":   int64(len(body)),
	}
	if got := input[0].ContextMap(); !reflect.DeepEqual(got, want) {
		t.Errorf("logged the request as %#v, want %#v", got, want)
	}
	// Every entry, as the agent's JSON log writes it. The matched recording
	// and the answer sent are the recording's content (the test set holds it
	// as it is), so those two entries are not the request's.
	recording := map[string]bool{"after matching the http request": true}
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	for _, e := range logs.All() {
		if recording[e.Message] || strings.HasPrefix(e.Message, "Mock Response sending back to client:") {
			continue
		}
		buf, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			t.Fatalf("encode %q: %v", e.Message, err)
		}
		for _, v := range secrets {
			if strings.Contains(buf.String(), v) {
				t.Errorf("the entry %q carries the request's %q", e.Message, v)
			}
		}
		buf.Free()
	}
}

// serveHTTPMock records req and resp as a mock and returns a database that
// serves it, again and again.
func serveHTTPMock(t *testing.T, h *HTTP, req, resp string) *mockMemDb {
	t.Helper()
	m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(req), Resp: []byte(resp), ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || m == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", m, err)
	}
	return &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true, deleteFilteredReturn: true}
}

// readServed reads one answer from app and returns its body.
func readServed(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("read the served answer: %v", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read the served body: %v", err)
	}
	return body.String()
}

// The calls of two connections with the same options are served with the
// request noise built once (requestNoiseFor), not once per call.
func TestServedCallsBuildTheirRequestNoiseOncePerOptionsValue(t *testing.T) {
	const req = "GET /u HTTP/1.1\r\nHost: api.example\r\n\r\n"
	h := &HTTP{Logger: zaptest.NewLogger(t, zaptest.Level(zapcore.InfoLevel))}
	db := serveHTTPMock(t, h, req, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	opts := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"X-Request-Id": {}}}}
	lastRequestNoise.Store(nil)
	before := requestNoiseBuilds.Load()
	for c := 0; c < 2; c++ {
		app, proxy := tcpPair(t)
		runDecode(t, proxy, func(ctx context.Context) error {
			return h.decodeHTTP(ctx, []byte(req), proxy, nil, db, opts)
		})
		r := bufio.NewReader(app)
		for i := 0; i < 3; i++ {
			if i > 0 {
				if _, err := app.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
			}
			if got := readServed(t, r); got != "ok" {
				t.Fatalf("connection %d, call %d: served %q, want the mock's answer", c+1, i+1, got)
			}
		}
	}
	if builds := requestNoiseBuilds.Load() - before; builds != 1 {
		t.Errorf("6 calls of 2 connections with one options value built the request noise %d times, want once", builds)
	}
}

// servedCallCost serves a POST with a body of size bytes, recorded with an
// answer of size bytes, over one kept-alive connection with debug off, and
// returns what one call allocated: how many times (testing.AllocsPerRun) and
// how many bytes. The app's side reads the answer into a reused buffer, so what
// is counted is the serving. The switch is pinned off, as the agent pins it
// from its flag, so the count is the serving's and not an env read's per call.
func servedCallCost(t *testing.T, size int) (allocs float64, bytesPerCall uint64) {
	t.Helper()
	provenswitch.Pin(false)
	t.Cleanup(provenswitch.Unpin)
	body := strings.Repeat("a", size)
	req := "POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(size) + "\r\n\r\n" + body
	// One header: a served answer's headers come in its map's order.
	resp := "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(size) + "\r\n\r\n" + body
	h := &HTTP{Logger: zap.NewNop()}
	db := serveHTTPMock(t, h, req, resp)
	app, proxy := tcpPair(t)
	runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(req), proxy, nil, db, models.OutgoingOptions{})
	})
	wire, answer := []byte(req), make([]byte, len(resp))
	read := func() {
		if _, err := io.ReadFull(app, answer); err != nil {
			t.Fatalf("read the served answer: %v", err)
		}
		if string(answer) != resp {
			t.Fatalf("served %q, want the recorded answer", answer)
		}
	}
	read()
	call := func() {
		if _, err := app.Write(wire); err != nil {
			t.Fatal(err)
		}
		read()
	}
	const runs = 50
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	allocs = testing.AllocsPerRun(runs, call)
	runtime.ReadMemStats(&after)
	return allocs, (after.TotalAlloc - before.TotalAlloc) / (runs + 1)
}

// A served call with debug off builds none of its debug entries (debugOn,
// logger.Check) and no request noise (requestNoiseFor). They were built on
// every call and dropped: the copies of the request and of the answer the
// entries held, the names of the pool's mocks, the boxed values and the noise
// maps. Main allocated 159 times for a call with a 16 B body and answer, and
// 24.7 times the body for one with a 64 KB body and answer (linux/arm64).
// The budgets are Linux's, without -race: sync.Pool drops what it is given at
// random under -race, and another OS's network calls allocate differently.
// The 16 B budget is each GOARCH's count exactly, so one more allocation,
// a single guard reverted, fails the row.
func TestAServedCallBuildsNoDebugEntryWithDebugOff(t *testing.T) {
	if raceOn || runtime.GOOS != "linux" {
		t.Skip("the allocation budgets are Linux's, without -race")
	}
	// Allocations of a call with a 16 B body and answer.
	smallBudget, ok := map[string]float64{"amd64": 105, "arm64": 105}[runtime.GOARCH]
	if !ok {
		t.Skipf("no allocation budget for %s", runtime.GOARCH)
	}
	const largeBudget = 15 << 6 // KB a call with a 64 KB body and answer allocates: 15 times the body
	small, _ := servedCallCost(t, 16)
	_, large := servedCallCost(t, 64<<10)
	t.Logf("a 16 B call allocates %v times; a 64 KB call %d bytes (%.1f times its body)", small, large, float64(large)/(64<<10))
	if small > smallBudget {
		t.Errorf("a served call with a 16 B body and answer allocated %v times, budget %v (%s)", small, smallBudget, runtime.GOARCH)
	}
	if large > largeBudget<<10 {
		t.Errorf("a served call with a 64 KB body and answer allocated %d bytes (%.1f times the body), budget %d KB",
			large, float64(large)/(64<<10), largeBudget)
	}
}

// With debug on, a served call still logs its debug entries: the request's
// shape, the noise, the match and the answer sent.
func TestAServedCallLogsItsDebugEntriesWithDebugOn(t *testing.T) {
	const req = "POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Type: text/plain\r\nContent-Length: 4\r\n\r\nping"
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"
	core, logs := observer.New(zapcore.DebugLevel)
	h := &HTTP{Logger: zap.New(core)}
	db := serveHTTPMock(t, h, req, resp)
	app, proxy := tcpPair(t)
	runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(req), proxy, nil, db, models.OutgoingOptions{})
	})
	if got := readServed(t, bufio.NewReader(app)); got != "pong" {
		t.Fatalf("served %q, want the mock's answer", got)
	}
	input := logs.FilterMessage("decodeHTTP debug logs for input").All()
	if len(input) != 1 || input[0].ContextMap()["path"] != "/u" || input[0].ContextMap()["body bytes"] != int64(4) {
		t.Errorf("logged the input as %v, want its path and body length", input)
	}
	for _, msg := range []string{"header noise", "body noise", "url noise", "after matching the http request",
		"mocks under consideration for match function", "http mock schema match results", "exact body match found"} {
		if n := logs.FilterMessage(msg).Len(); n != 1 {
			t.Errorf("logged %q %d times, want once", msg, n)
		}
	}
	served := logs.FilterMessageSnippet("Mock Response sending back to client:\n").All()
	if len(served) != 1 || !strings.HasSuffix(served[0].Message, "\r\n\r\npong") {
		t.Errorf("logged the answer sent as %v, want it whole", served)
	}
}
