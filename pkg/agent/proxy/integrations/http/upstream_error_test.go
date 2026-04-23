package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	nhttp "net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// fakeTimeoutErr satisfies net.Error with Timeout() == true. Used to simulate
// what SetReadDeadline-backed reads return when the upstream never replies —
// this is the exact error class that caused the sap-demo-java test-16/17
// Class B diffs (pipeline 384 step 38) before the fix.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout (simulated)" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

// TestClassifyUpstreamError verifies the error-class → (status, reason, marker)
// mapping used by the recorder when synthesizing mocks for dropped upstream
// calls.
func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantReason string
		wantMarker string
	}{
		{
			name:       "context deadline exceeded becomes 504",
			err:        context.DeadlineExceeded,
			wantStatus: 504,
			wantReason: "Gateway Timeout",
			wantMarker: UpstreamTimeoutMarker,
		},
		{
			name:       "net.Error Timeout() becomes 504",
			err:        fakeTimeoutErr{},
			wantStatus: 504,
			wantReason: "Gateway Timeout",
			wantMarker: UpstreamTimeoutMarker,
		},
		{
			name:       "io.EOF becomes 502 with eof marker",
			err:        io.EOF,
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-eof: true",
		},
		{
			name:       "unexpected EOF becomes 502 with eof marker",
			err:        io.ErrUnexpectedEOF,
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-eof: true",
		},
		{
			name:       "connection refused becomes 502 with unreachable marker",
			err:        errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-unreachable: true",
		},
		{
			name:       "connection reset becomes 502 with unreachable marker",
			err:        errors.New("read tcp 127.0.0.1:443: connection reset by peer"),
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-unreachable: true",
		},
		{
			name:       "no route to host becomes 502 with unreachable marker",
			err:        errors.New("dial tcp: no route to host"),
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-unreachable: true",
		},
		{
			name:       "generic error still persists as 502",
			err:        errors.New("totally unknown bang"),
			wantStatus: 502,
			wantReason: "Bad Gateway",
			wantMarker: "keploy-recorded-upstream-error: true",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			status, reason, marker := classifyUpstreamError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status: got %d, want %d", status, tc.wantStatus)
			}
			if reason != tc.wantReason {
				t.Errorf("reason: got %q, want %q", reason, tc.wantReason)
			}
			if marker != tc.wantMarker {
				t.Errorf("marker: got %q, want %q", marker, tc.wantMarker)
			}
		})
	}
}

// TestSynthesizeUpstreamErrorResponseShape asserts the synthesized bytes
// parse as a valid HTTP response via net/http.ReadResponse — this is the
// exact path parseFinalHTTP takes, so if this breaks the mock would be
// corrupt when consumed downstream.
func TestSynthesizeUpstreamErrorResponseShape(t *testing.T) {
	raw := synthesizeUpstreamErrorResponse("GET", "/foo", context.DeadlineExceeded)

	req, _ := nhttp.NewRequest("GET", "/foo", nil)
	resp, err := nhttp.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), req)
	if err != nil {
		t.Fatalf("synthesized response did not parse as HTTP: %v\nraw:\n%s", err, raw)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 504 {
		t.Errorf("status code: got %d, want 504", resp.StatusCode)
	}
	if got := resp.Header.Get(UpstreamErrorMarker); got != UpstreamTimeoutMarker {
		t.Errorf("marker header: got %q, want %q", got, UpstreamTimeoutMarker)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, UpstreamTimeoutMarker) {
		t.Errorf("body missing timeout marker; got: %q", body)
	}
	if !strings.Contains(body, "context deadline exceeded") {
		t.Errorf("body missing underlying error text; got: %q", body)
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Errorf("Content-Length header missing — parseFinalHTTP relies on it")
	}
}

// TestSynthesizeUpstreamErrorResponseAllClasses sanity-checks every error
// class we surface produces a parseable HTTP response.
func TestSynthesizeUpstreamErrorResponseAllClasses(t *testing.T) {
	errs := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"deadline-exceeded", context.DeadlineExceeded},
		{"net-timeout", fakeTimeoutErr{}},
		{"eof", io.EOF},
		{"unexpected-eof", io.ErrUnexpectedEOF},
		{"connection-refused", errors.New("connect: connection refused")},
		{"connection-reset", errors.New("connection reset by peer")},
		{"broken-pipe", errors.New("write: broken pipe")},
		{"generic", errors.New("something weird")},
	}

	for _, tc := range errs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw := synthesizeUpstreamErrorResponse("GET", "/foo", tc.err)
			req, _ := nhttp.NewRequest("GET", "/foo", nil)
			resp, err := nhttp.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), req)
			if err != nil {
				t.Fatalf("class %q: synthesized response did not parse as HTTP: %v\nraw:\n%s",
					tc.name, err, raw)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 500 || resp.StatusCode > 504 {
				t.Errorf("class %q: expected a 5xx status, got %d", tc.name, resp.StatusCode)
			}
			if got := resp.Header.Get(UpstreamErrorMarker); got == "" {
				t.Errorf("class %q: %s header missing", tc.name, UpstreamErrorMarker)
			}
		})
	}
}

// TestParseFinalHTTPAcceptsSynthesizedTimeoutResponse is the regression test
// for pipeline 384 step 38 Class B (sap-demo-java test-16/test-17). It drives
// parseFinalHTTP with the exact (req, synthesized-resp) pair the recorder
// produces on an upstream timeout — proving the synthesized response is a
// well-formed HTTP message that the parser accepts without error. Before
// the fix, this path was never exercised: the recorder dropped the mock
// silently on ReadBytes error.
func TestParseFinalHTTPAcceptsSynthesizedTimeoutResponse(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}

	// The same request shape the SAP aggregator issues during record.
	req := []byte("GET /A_BusinessPartner('202')/to_BusinessPartnerAddress HTTP/1.1\r\n" +
		"Host: sandbox.api.sap.com\r\n" +
		"Connection: close\r\n" +
		"\r\n")

	// Simulate the recorder's synthesized timeout response.
	resp := synthesizeUpstreamErrorResponse("GET", "/A_BusinessPartner('202')/to_BusinessPartnerAddress",
		context.DeadlineExceeded)

	mock := &FinalHTTP{
		Req:              req,
		Resp:             resp,
		ReqTimestampMock: time.Now().Add(-26 * time.Second), // mimics 26s SAP timeout
		ResTimestampMock: time.Now(),
	}

	mocks := make(chan *models.Mock, 2)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "test-conn")

	err := h.parseFinalHTTP(ctx, mock, 443, mocks, models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("parseFinalHTTP returned error on synthesized timeout response: %v", err)
	}

	// The mock may be routed to the global syncMock buffer (when no output
	// channel is bound) or directly onto our `mocks` channel. If we do
	// receive one, assert its shape matches the timeout contract. If the
	// syncMock manager intercepts, the shape is still validated by the
	// upstream-synthesis unit tests above.
	select {
	case m := <-mocks:
		if m.Kind != models.HTTP {
			t.Errorf("kind: got %v, want %v", m.Kind, models.HTTP)
		}
		if m.Spec.HTTPResp == nil {
			t.Fatalf("HTTPResp is nil")
		}
		if m.Spec.HTTPResp.StatusCode != 504 {
			t.Errorf("status: got %d, want 504", m.Spec.HTTPResp.StatusCode)
		}
		if !strings.Contains(m.Spec.HTTPResp.Body, UpstreamTimeoutMarker) {
			t.Errorf("mock body missing timeout marker; got:\n%s", m.Spec.HTTPResp.Body)
		}
		if m.Spec.HTTPReq == nil || !strings.Contains(m.Spec.HTTPReq.URL, "A_BusinessPartner") {
			t.Errorf("request URL missing from persisted mock: %+v", m.Spec.HTTPReq)
		}
	case <-time.After(200 * time.Millisecond):
		// Routed through syncMock buffer; shape already validated above.
	}
}

// TestParseFinalHTTPAcceptsSynthesizedConnResetResponse verifies the same
// path works for connection-reset-style errors — covering the full spread
// of upstream error classes the recorder now persists.
func TestParseFinalHTTPAcceptsSynthesizedConnResetResponse(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}

	req := []byte("POST /submit HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n")
	resetErr := errors.New("read tcp 10.0.0.1:443: connection reset by peer")
	resp := synthesizeUpstreamErrorResponse("POST", "/submit", resetErr)

	mock := &FinalHTTP{
		Req:              req,
		Resp:             resp,
		ReqTimestampMock: time.Now(),
		ResTimestampMock: time.Now(),
	}

	mocks := make(chan *models.Mock, 2)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "test-conn")

	if err := h.parseFinalHTTP(ctx, mock, 443, mocks, models.OutgoingOptions{}); err != nil {
		t.Fatalf("parseFinalHTTP returned error on synthesized conn-reset response: %v", err)
	}
}

// TestParseFinalHTTPAcceptsSynthesizedEOFResponse covers the mid-stream EOF
// class (io.ErrUnexpectedEOF) — upstream closed the socket mid-body.
func TestParseFinalHTTPAcceptsSynthesizedEOFResponse(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}

	req := []byte("GET /stream HTTP/1.1\r\nHost: example.com\r\n\r\n")
	resp := synthesizeUpstreamErrorResponse("GET", "/stream", io.ErrUnexpectedEOF)

	mock := &FinalHTTP{
		Req:              req,
		Resp:             resp,
		ReqTimestampMock: time.Now(),
		ResTimestampMock: time.Now(),
	}

	mocks := make(chan *models.Mock, 2)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "test-conn")

	if err := h.parseFinalHTTP(ctx, mock, 443, mocks, models.OutgoingOptions{}); err != nil {
		t.Fatalf("parseFinalHTTP returned error on synthesized eof response: %v", err)
	}
}
