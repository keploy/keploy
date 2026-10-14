package conn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
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
