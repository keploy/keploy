package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

// trackCloser wraps a response body and records whether Close was called.
type trackCloser struct {
	io.Reader
	closed bool
}

func (tc *trackCloser) Close() error { tc.closed = true; return nil }

// gzipBomb returns a gzip stream that inflates to MaxTestCaseSize+1 bytes.
func gzipBomb(t *testing.T) []byte {
	t.Helper()
	data, err := pkg.Compress(zap.NewNop(), "gzip", make([]byte, MaxTestCaseSize+1))
	if err != nil {
		t.Fatalf("compress bomb: %v", err)
	}
	return data
}

// TestCapture_DecompressOverLimit pins the behavior when a body inflates
// past MaxTestCaseSize during capture (#3867): the exchange is dropped with
// the regular size-limit message (not a corrupt-stream error), no testcase
// is emitted, and resp.Body is closed even on the request-side early return.
func TestCapture_DecompressOverLimit(t *testing.T) {
	newCapture := func(t *testing.T, reqBody []byte, reqEncoding string, respBody []byte, respEncoding string) (*observer.ObservedLogs, *trackCloser, chan *models.TestCase) {
		t.Helper()
		core, logs := observer.New(zap.ErrorLevel)
		logger := zap.New(core)

		req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/upload", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if reqEncoding != "" {
			req.Header.Set("Content-Encoding", reqEncoding)
		}

		body := &trackCloser{Reader: bytes.NewReader(respBody)}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       body,
		}
		if respEncoding != "" {
			resp.Header.Set("Content-Encoding", respEncoding)
		}

		tcChan := make(chan *models.TestCase, 1)
		Capture(context.Background(), logger, tcChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, true, false, 8080)
		return logs, body, tcChan
	}

	assertDropped := func(t *testing.T, logs *observer.ObservedLogs, body *trackCloser, tcChan chan *models.TestCase) {
		t.Helper()
		select {
		case tc := <-tcChan:
			t.Fatalf("expected capture to be dropped, got testcase %q", tc.Name)
		default:
		}
		if !body.closed {
			t.Error("resp.Body must be closed when capture returns early")
		}
		// Exactly ONE Error-level log — the size-limit drop. Anything else
		// (Capture's decode-failure messages, or Decompress's internal
		// "failed to read the ... compressed data") is the oversized body
		// being misreported as a corrupt stream.
		if n := logs.Len(); n != 1 {
			t.Errorf("want exactly 1 error log (the size-limit drop), got %d: %v", n, logs.All())
		}
		if n := logs.FilterMessageSnippet("exceeds 5MB limit").Len(); n != 1 {
			t.Errorf("want 1 size-limit log, got %d (all: %v)", n, logs.All())
		}
	}

	t.Run("RequestBody", func(t *testing.T) {
		logs, body, tcChan := newCapture(t, gzipBomb(t), "gzip", []byte("ok"), "")
		assertDropped(t, logs, body, tcChan)
	})

	t.Run("ResponseBody", func(t *testing.T) {
		logs, body, tcChan := newCapture(t, []byte("hi"), "", gzipBomb(t), "gzip")
		assertDropped(t, logs, body, tcChan)
	})
}

func TestIsFiltered_FilterPolicy(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name     string
		filters  []models.Filter
		method   string
		path     string
		expected bool
	}{
		{
			name: "Only Exclude - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
		{
			name: "Only Exclude - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/data",
			expected: false,
		},
		{
			name: "Only Include - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // NOT filtered
		},
		{
			name: "Only Include - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true, // Filtered because it's not in the whitelist
		},
		{
			name: "Mixed - Include Match and Exclude Match (Exclude Wins)",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/admin",
			expected: true, // Filtered because Exclude takes priority
		},
		{
			name: "Mixed - Include Match and Exclude No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // Not filtered
		},
		{
			name: "Multiple Includes - One Matches",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/v1/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/v2/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/v2/users",
			expected: false, // Not filtered
		},
		{
			name: "Backward Compatibility - Default to Exclude",
			filters: []models.Filter{
				{
					BypassRule: models.BypassRule{Path: "/health"},
					// FilterPolicy is missing (defaults to empty string, which is Exclude logic)
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, "http://localhost"+tt.path, nil)
			opts := models.IncomingOptions{
				Filters: tt.filters,
			}
			got := IsFiltered(logger, req, opts)
			if got != tt.expected {
				t.Errorf("IsFiltered() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestCaptureGRPC_SchemaKeyStamping pins the GRPCCaptureHook contract: the
// hook's schemaKey return is stamped onto the kept test case (so the control
// plane can mark cross-pod duplicates), a duplicate verdict still drops the
// capture, and with no hook installed the schema key stays empty.
func TestCaptureGRPC_SchemaKeyStamping(t *testing.T) {
	origHook := GRPCCaptureHook
	defer func() { GRPCCaptureHook = origHook }()

	newStream := func() *pkg.HTTP2Stream {
		return &pkg.HTTP2Stream{
			GRPCReq:  &models.GrpcReq{},
			GRPCResp: &models.GrpcResp{},
		}
	}
	const key = "GRPC|/svc.Users/Get|0|11|22"

	t.Run("hook stamps schema key on kept test case", func(t *testing.T) {
		GRPCCaptureHook = func(_ *models.GrpcReq, _ *models.GrpcResp) (bool, string) {
			return false, key
		}
		tChan := make(chan *models.TestCase, 1)
		CaptureGRPC(context.Background(), zap.NewNop(), tChan, newStream(), 9090, false, false)
		select {
		case tc := <-tChan:
			if tc.SchemaKey != key {
				t.Fatalf("SchemaKey = %q, want %q", tc.SchemaKey, key)
			}
		default:
			t.Fatal("expected a captured test case")
		}
	})

	t.Run("duplicate verdict drops the capture", func(t *testing.T) {
		GRPCCaptureHook = func(_ *models.GrpcReq, _ *models.GrpcResp) (bool, string) {
			return true, key
		}
		tChan := make(chan *models.TestCase, 1)
		CaptureGRPC(context.Background(), zap.NewNop(), tChan, newStream(), 9090, false, false)
		if len(tChan) != 0 {
			t.Fatal("duplicate must not emit a test case")
		}
	})

	t.Run("nil hook leaves schema key empty", func(t *testing.T) {
		GRPCCaptureHook = nil
		tChan := make(chan *models.TestCase, 1)
		CaptureGRPC(context.Background(), zap.NewNop(), tChan, newStream(), 9090, false, false)
		select {
		case tc := <-tChan:
			if tc.SchemaKey != "" {
				t.Fatalf("SchemaKey = %q, want empty with no hook", tc.SchemaKey)
			}
		default:
			t.Fatal("expected a captured test case")
		}
	})
}

// A request header that arrived on several lines is replayed on those lines,
// in their order — recorded, written to disk, read back and sent. Folding them
// into one comma-joined line is not the same request: an app that reads the
// first value of `X_tenant: acme` twice gets "acme", while
// `X_tenant: acme,acme` gets "acme,acme" and rejects it, as a production
// service's header validation did for every such recorded test.
//
// A single line that carries commas stays one line: splitting `Accept: a, b`
// into two lines breaks servers that map headers into a dict.
func TestARepeatedRequestHeaderReplaysOnItsRecordedLines(t *testing.T) {
	type seen struct{ tenant, accept, cookie []string }
	var (
		mu  sync.Mutex
		got seen
	)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = seen{r.Header.Values("X_tenant"), r.Header.Values("Accept"), r.Header.Values("Cookie")}
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer app.Close()
	appURL, err := url.Parse(app.URL)
	if err != nil {
		t.Fatal(err)
	}
	var appPort uint16
	if _, err := fmt.Sscanf(appURL.Port(), "%d", &appPort); err != nil {
		t.Fatal(err)
	}

	// Lines as the wire carried them. The two Cookie values carry commas of
	// their own, which a length-free split could not put back.
	wire := "GET /v1/session HTTP/1.1\r\n" +
		"Host: " + appURL.Host + "\r\n" +
		"X_tenant: acme\r\n" +
		"Accept: a, b\r\n" +
		"Cookie: a=1,2\r\n" +
		"X_tenant: acme\r\n" +
		"Cookie: b=3\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}

	tcChan := make(chan *models.TestCase, 1)
	Capture(context.Background(), zap.NewNop(), tcChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, appPort)
	var recorded *models.TestCase
	select {
	case recorded = <-tcChan:
	default:
		t.Fatal("Capture emitted no test case")
	}

	// The recording as the test file stores it, and as replay reads it back.
	doc, err := testdb.EncodeTestcase(*recorded, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var readBack yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
		t.Fatal(err)
	}
	tc, err := testdb.Decode(&readBack, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pkg.SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), pkg.SimulationConfig{APITimeout: 5}); err != nil {
		t.Fatalf("SimulateHTTP: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := seen{tenant: []string{"acme", "acme"}, accept: []string{"a, b"}, cookie: []string{"a=1,2", "b=3"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the app got X_tenant %q, Accept %q, Cookie %q; recorded %q, %q, %q\n%s",
			got.tenant, got.accept, got.cookie, want.tenant, want.accept, want.cookie, onDisk)
	}
}

// A recording with no header on more than one line is stored exactly as it
// always was: nothing new appears on disk for the common case.
func TestARecordingWithoutRepeatedHeadersGainsNothingOnDisk(t *testing.T) {
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(
		"GET /v1/session HTTP/1.1\r\nHost: localhost:8080\r\nAccept: a, b\r\nX_tenant: acme\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Set-Cookie": {"a=1", "b=2"}}, Body: io.NopCloser(strings.NewReader("ok"))}
	tcChan := make(chan *models.TestCase, 1)
	Capture(context.Background(), zap.NewNop(), tcChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
	tc := <-tcChan
	doc, err := testdb.EncodeTestcase(*tc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// The response is never re-sent, so its repeated Set-Cookie changes
	// nothing either: a test case's response is only compared.
	if bytes.Contains(onDisk, []byte("header_line_lengths")) {
		t.Errorf("a recording without a repeated request header stores something new:\n%s", onDisk)
	}
}

// A multipart file part is captured into a temp file of the capturing process
// (dsmon F19). Saved by that process (sidecar, local) the persister copies it
// from there, as it always has. Saved by another one -- the DaemonSet agent
// streams the test case as JSON to k8s-proxy's control plane, where that temp
// file does not exist -- the bytes ride the test case (InlineFormFiles). Either
// way the asset is written byte for byte, binary content included, and replay
// sends the file.
func TestAMultipartFileReachesReplayWhereverItsTestCaseIsPersisted(t *testing.T) {
	file := []byte{'P', 'K', 0x03, 0x04, 0xff, 0xfe, 0x00, 0x80, 0xc3, 0x28, '\n'} // not valid UTF-8
	type sent struct {
		fields, files, fileNames map[string]string
	}
	var got sent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = sent{fields: map[string]string{}, files: map[string]string{}, fileNames: map[string]string{}}
		mr, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				got.files[part.FormName()] = string(data)
				got.fileNames[part.FormName()] = part.FileName()
				continue
			}
			got.fields[part.FormName()] = string(data)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, remote := range []bool{false, true} {
		name := "persisted by the capturing process"
		if remote {
			name = "persisted by another process"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if err := mw.WriteField("namespace", "ns"); err != nil {
				t.Fatal(err)
			}
			fw, err := mw.CreateFormFile("classes", "classes.zip")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write(file); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/dedup/bytecode", &body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":"stored"}`))}
			tcChan := make(chan *models.TestCase, 1)
			// App port 0: replay sends the request to the recorded URL, srv.
			Capture(ctx, zap.NewNop(), tcChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, 0)
			tc := <-tcChan

			var tempPath string
			for _, form := range tc.HTTPReq.Form {
				if form.Key == "classes" && len(form.Paths) == 1 {
					tempPath = form.Paths[0]
				}
			}
			if tempPath == "" {
				t.Fatalf("the capture left no temp file for the file part: %+v", tc.HTTPReq.Form)
			}
			t.Cleanup(func() { _ = os.Remove(tempPath) })

			if remote {
				InlineFormFiles(zap.NewNop(), tc)
				if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
					t.Fatalf("temp file %s left behind after its bytes were inlined (stat err %v)", tempPath, err)
				}
				// What the DaemonSet stream carries to the control plane.
				payload, err := json.Marshal(tc)
				if err != nil {
					t.Fatal(err)
				}
				var streamed models.TestCase
				if err := json.Unmarshal(payload, &streamed); err != nil {
					t.Fatal(err)
				}
				tc = &streamed
			}

			keployPath := t.TempDir()
			db := testdb.New(zap.NewNop(), keployPath)
			if err := db.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
				t.Fatalf("insert test case: %v", err)
			}
			stored, err := db.GetTestCases(ctx, "test-set-0")
			if err != nil || len(stored) != 1 {
				t.Fatalf("stored test cases = %d (err %v), want 1", len(stored), err)
			}
			var asset string
			for _, form := range stored[0].HTTPReq.Form {
				if form.Key == "classes" && len(form.Paths) == 1 {
					asset = form.Paths[0]
				}
			}
			if asset == "" {
				t.Fatalf("the stored test case has no file for the file part: %+v", stored[0].HTTPReq.Form)
			}
			onDisk, err := os.ReadFile(filepath.Join(keployPath, asset))
			if err != nil || !bytes.Equal(onDisk, file) {
				t.Fatalf("asset %s = %q (err %v), want %q", asset, onDisk, err, file)
			}

			got = sent{}
			replayed, err := pkg.SimulateHTTP(ctx, stored[0], "test-set-0", zap.NewNop(), pkg.SimulationConfig{APITimeout: 10, KeployPath: keployPath})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if replayed.StatusCode != http.StatusOK {
				t.Fatalf("replay status = %d, want 200", replayed.StatusCode)
			}
			if got.files["classes"] != string(file) || got.fileNames["classes"] != "classes.zip" || got.fields["namespace"] != "ns" {
				t.Fatalf("replay sent files %q (names %v), fields %v; want classes.zip = %q and namespace = ns",
					got.files, got.fileNames, got.fields, file)
			}
		})
	}
}

// While the memory guard has paused recording, a capture hook's skip closes
// the exchange's bodies and counts the request as not recorded, on the ingress
// connection its context names: a request that was in flight as the pause
// began is not recorded, and the recording says so. Recording running, it
// skips nothing and counts nothing.
func TestSkipUnderPressureClosesAndCountsOnTheIngressConnection(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	ctx := syncMock.WithIngressConn(syncMock.NewContext(context.Background(), mgr), "10.0.0.1:5000 -> 127.0.0.1:8080")
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	reqBody := &trackCloser{Reader: strings.NewReader("in")}
	req.Body = reqBody
	respBody := &trackCloser{Reader: strings.NewReader("out")}
	resp := &http.Response{StatusCode: http.StatusOK, Body: respBody}

	if SkipUnderPressure(ctx, req, resp) || reqBody.closed || respBody.closed || mgr.NotRecordedUnderPressure().Total != 0 {
		t.Fatal("recording running: skipped, closed a body or counted")
	}
	restore := memoryguard.SetRecordingPausedForTest(true)
	skipped := SkipUnderPressure(ctx, req, resp)
	restore()
	if !skipped || !reqBody.closed || !respBody.closed {
		t.Fatalf("recording paused: skipped %v, request body closed %v, response body closed %v; want all", skipped, reqBody.closed, respBody.closed)
	}
	got := mgr.NotRecordedUnderPressure()
	if got.Total != 1 || got.ByConn["10.0.0.1:5000 -> 127.0.0.1:8080"] != 1 {
		t.Fatalf("not recorded under pressure: %+v, want the request on its connection", got)
	}
}

// A gRPC call is left out for a pause of the memory guard when the guard has
// paused recording now, or had as the forwarder read the call's request, and
// each one left out is counted on the ingress connection its context names. A
// call whose request was read while recording was not paused is not, however
// close to a pause its request's time is: the gRPC ingress tees a
// connection's whole stream through a pause, and only the calls the pause took
// part in are its cost. A request read just after a pause, in the tick its
// span ended in, is not in it: on a clock as coarse as Windows' the request's
// time cannot tell, and the first call after every pause was left out.
func TestSkipGRPCUnderPressureDecidesByThePauseAsTheRequestWasRead(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	ctx := syncMock.WithIngressConn(syncMock.NewContext(context.Background(), mgr), "10.0.0.1:5000 -> 127.0.0.1:50051")
	// A span that holds every instant: the request's time must not decide.
	mgr.SetMemoryPressure(true)
	defer mgr.SetMemoryPressure(false)

	for _, c := range []struct {
		what            string
		pausedAtRequest bool
		now             bool // the guard has paused recording now
		want            bool
	}{
		{what: "a request read while recording was not paused, its time in a span"},
		{what: "a request read during the pause, decided after it", pausedAtRequest: true, want: true},
		{what: "a request read after the pause, decided during the next", now: true, want: true},
		{what: "a request read during a pause, decided during it", pausedAtRequest: true, now: true, want: true},
	} {
		restore := memoryguard.SetRecordingPausedForTest(c.now)
		got := SkipGRPCUnderPressure(ctx, c.pausedAtRequest)
		restore()
		if got != c.want {
			t.Errorf("%s: left out %v, want %v", c.what, got, c.want)
		}
	}
	if got := mgr.NotRecordedUnderPressure(); got.Total != 3 || got.ByConn["10.0.0.1:5000 -> 127.0.0.1:50051"] != 3 {
		t.Fatalf("not recorded under pressure: %+v, want the three calls left out, on their connection", got)
	}
}

// failingBody yields data, then fails with err: a request body the client
// stopped sending partway through.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *failingBody) Close() error { return nil }

// Capture's log lines name a request by its method, host, path and query
// parameter names, and a body by its length and content type: no stretch of
// a body or of a query value appears in the debug log in any encoding. A
// body that failed partway through its read was logged whole, as base64, and
// a request over a size limit by its URL whole, query values and all. The
// log is rendered as production renders it, JSON and console.
func TestCapture_LogsNoBodyAndNoQueryValue(t *testing.T) {
	const token, email = "sk_live_9fQ2xLp7Rw", "ada.lovelace@example.com"
	target := "http://localhost:8080/pay?token=" + token + "&email=" + url.QueryEscape(email)
	// The request body ends in bytes no text holds. They are not looked for
	// themselves (logleak.Unplantable: their hex.Dump text is dots, as any
	// log's dotted text is); the body's base64 and hex, from any alignment,
	// start with its printable stretch's, which is.
	printable := []byte("card=4111111111111111&cvv=737&holder=Ada+Lovelace")
	reqBody := append(append([]byte(nil), printable...), 0x00, 0xff, 0x10, 0x80, 0xfe, 0x7f, 0xc3, 0x28, 0x1b, 0x9d)
	respUnit := []byte(`{"balance":"98765.43","iban":"DE89370400440532013000"}`)
	planted := map[string][]byte{"the token": []byte(token), "the e-mail": []byte(email), "the request body": printable, "the response body": respUnit}

	for _, c := range []struct {
		name, msg string
		req       func() io.ReadCloser
		resp      []byte
		level     zapcore.Level
	}{
		{"a body that fails partway", "failed to read the http request body",
			func() io.ReadCloser {
				return &failingBody{data: append([]byte(nil), reqBody...), err: errors.New("client reset the connection")}
			}, respUnit, zapcore.DebugLevel},
		{"a response over 1 MB", "response body exceeds 1MB during recording, storing only body size",
			func() io.ReadCloser { return io.NopCloser(bytes.NewReader(reqBody)) },
			bytes.Repeat(respUnit, LargeBodyThreshold/len(respUnit)+1), zapcore.DebugLevel},
		{"a test case over 5 MB", "HTTP test case data exceeds 5MB limit, skipping capture",
			func() io.ReadCloser { return io.NopCloser(bytes.NewReader(reqBody)) },
			bytes.Repeat(respUnit, MaxTestCaseSize/len(respUnit)+1), zapcore.ErrorLevel},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			req, err := http.NewRequest(http.MethodPost, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Body = c.req()
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(c.resp))}
			Capture(context.Background(), logger, make(chan *models.TestCase, 1), req, resp, time.Now(), time.Now(), models.IncomingOptions{}, true, false, 8080)

			lines := logs.FilterMessage(c.msg).All()
			if len(lines) != 1 || lines[0].Level != c.level {
				t.Fatalf("want the line %q once at %v, got %d: %v", c.msg, c.level, len(lines), logs.All())
			}
			fields := lines[0].ContextMap()
			meta, _ := fields["metadata"].(map[string]string)
			if meta["method"] != http.MethodPost || meta["path"] != "/pay" || meta["query keys"] != "email,token" {
				t.Errorf("the line names the request by its method, path and query parameter names; got %v", fields)
			}
			if c.msg == "failed to read the http request body" {
				if fields["bytesRead"] != int64(len(reqBody)) || fields["contentType"] != "application/x-www-form-urlencoded" {
					t.Errorf("the line gives how much of the body was read and its content type; got %v", fields)
				}
			}
			log := out.String()
			if !strings.Contains(log, c.msg) {
				t.Fatalf("the rendered log is not the one checked: %s", log)
			}
			for what, content := range planted {
				if found := logleak.Find(t, log, content); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}

// errorLinesPast is a core that closes the returned channel once more than n
// lines at Error or above were written through it: the event a test waits
// on, with no clock, for a loop that logs on every turn.
func errorLinesPast(core zapcore.Core, n int64) (zapcore.Core, <-chan struct{}) {
	past := make(chan struct{})
	var seen atomic.Int64
	return zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if e.Level >= zapcore.ErrorLevel && seen.Add(1) == n+1 {
			close(past)
		}
		return nil
	}), past
}

// mime/multipart's error for a part of a multipart/form-data request body
// it cannot read quotes a line of the body (a part header line without its
// colon). ExtractFormData logs it at Error by the body's length, with no
// byte of the body, and stops on it, so the line after (which the reader
// would quote as a stray line) is never read.
func TestExtractFormDataLogsAPartThatDoesNotParseByNoByteOfTheBody(t *testing.T) {
	const secret = "Zq7FORMsecret41"
	ct := "multipart/form-data; boundary=XYZ"
	for _, c := range []struct{ name, body string }{
		{"part header line without its colon", "--XYZ\r\npassword " + secret + "\r\n\r\nv\r\n--XYZ--\r\n"},
		{"a value after a header line that does not parse", "--XYZ\r\nX-Bad-Header-Line\r\n\r\n" + secret + " value\r\n--XYZ--\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			core, spins := errorLinesPast(obs, 1)
			logger, out := logleak.Logger(core)
			done := make(chan []models.FormData)
			go func() { done <- ExtractFormData(logger, []byte(c.body), ct) }()
			select {
			case <-done:
			case <-spins:
				t.Fatalf("ExtractFormData logged more than one Error line on one part that does not parse:\n%s", out.String())
			}
			lines := logs.FilterMessage("Error reading part").All()
			if len(lines) != 1 {
				t.Fatalf("precondition: want one %q line, got %d:\n%s", "Error reading part", len(lines), out.String())
			}
			want := fmt.Sprintf("the multipart/form-data body (%d bytes) has a part that does not parse", len(c.body))
			if !strings.Contains(out.String(), want) {
				t.Errorf("the Error line does not name the part by the body's length (%q):\n%s", want, out.String())
			}
			if found := logleak.Find(t, out.String(), []byte(secret)); len(found) > 0 {
				t.Errorf("the capture log holds a byte of the request body: %v\n%s", found, out.String())
			}
		})
	}
}

// A multipart/form-data body cut before its closing boundary (a client that
// aborted its upload; the capture keeps a body that fails partway) makes
// multipart.Reader return "NextPart: EOF", io.EOF wrapped, on every call.
// ExtractFormData returns on it with the parts read whole kept; it once
// compared err == io.EOF and logged at Error on every turn, forever.
func TestExtractFormDataReturnsOnABodyCutBeforeItsClosingBoundary(t *testing.T) {
	ct := "multipart/form-data; boundary=XYZ"
	body := "--XYZ\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nfirst\r\n" +
		"--XYZ\r\nContent-Disposition: form-data; name=\"b\"\r\n\r\nvalue with no closing boundary"
	obs, logs := observer.New(zapcore.DebugLevel)
	core, spins := errorLinesPast(obs, 2)
	logger, _ := logleak.Logger(core)
	done := make(chan []models.FormData)
	go func() { done <- ExtractFormData(logger, []byte(body), ct) }()
	var got []models.FormData
	select {
	case got = <-done:
	case <-spins:
		t.Fatalf("ExtractFormData loops on a body cut before its closing boundary: %d Error lines and counting", logs.FilterLevelExact(zapcore.ErrorLevel).Len())
	}
	if len(got) != 1 || got[0].Key != "a" || !reflect.DeepEqual(got[0].Values, []string{"first"}) {
		t.Errorf("ExtractFormData = %+v, want the whole part a=first kept", got)
	}
	// The cut is the end of the parts, not a part that does not parse.
	if n := logs.FilterMessage("Error reading part").Len(); n != 0 {
		t.Errorf("a body cut before its closing boundary logged %d %q lines, want none", n, "Error reading part")
	}
}
