package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg"
)

func stubIngressPaused(t *testing.T, fn func() bool) {
	t.Helper()
	prev := isIngressRecordingPaused
	isIngressRecordingPaused = fn
	t.Cleanup(func() {
		isIngressRecordingPaused = prev
	})
}

func TestHTTPBodyCaptureBufferStopsWhenPaused(t *testing.T) {
	paused := false
	stubIngressPaused(t, func() bool { return paused })

	state := newHTTPCaptureState(32)
	capture := &httpBodyCaptureBuffer{state: state}

	n, err := capture.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected write error before pause: %v", err)
	}
	if n != len("hello") {
		t.Fatalf("expected to report %d bytes written, got %d", len("hello"), n)
	}
	if got := string(capture.Bytes()); got != "hello" {
		t.Fatalf("expected capture buffer to contain %q, got %q", "hello", got)
	}

	paused = true
	n, err = capture.Write([]byte(" world"))
	if err != nil {
		t.Fatalf("unexpected write error after pause: %v", err)
	}
	if n != len(" world") {
		t.Fatalf("expected to report %d bytes written after pause, got %d", len(" world"), n)
	}
	if !state.isAborted() {
		t.Fatal("expected capture state to abort after pause")
	}
	if got := len(capture.Bytes()); got != 0 {
		t.Fatalf("expected paused capture buffer to be cleared, got %d bytes", got)
	}
}

func TestHTTPBodyCaptureBufferStopsAtBudget(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	state := newHTTPCaptureState(5)
	capture := &httpBodyCaptureBuffer{state: state}

	n, err := capture.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected write error before limit: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected to report 5 bytes written, got %d", n)
	}

	n, err = capture.Write([]byte("!"))
	if err != nil {
		t.Fatalf("unexpected write error after limit: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected to report 1 byte written after limit, got %d", n)
	}
	if !state.isAborted() {
		t.Fatal("expected capture state to abort once the budget is exceeded")
	}
	if got := len(capture.Bytes()); got != 0 {
		t.Fatalf("expected over-budget capture buffer to be cleared, got %d bytes", got)
	}
}

func TestWrapBodyForCaptureStreamsAndCopies(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	state := newHTTPCaptureState(32)
	capture := &httpBodyCaptureBuffer{state: state}
	body := io.NopCloser(strings.NewReader("payload"))
	wrapped := wrapBodyForCapture(body, capture)
	defer wrapped.Close()

	data, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("expected wrapped body to stream %q, got %q", "payload", string(data))
	}
	if got := string(capture.Bytes()); got != "payload" {
		t.Fatalf("expected capture buffer to mirror body, got %q", got)
	}
}

func TestSerializeCapturedRequestRoundTrip(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	body := []byte(`{"name":"demo"}`)
	raw := fmt.Sprintf(
		"POST /orders?status=paid HTTP/1.1\r\nHost: api:8080\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		len(body),
		body,
	)

	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("failed to parse original request: %v", err)
	}

	serialized, err := serializeCapturedRequest(req, body)
	if err != nil {
		t.Fatalf("failed to serialize request: %v", err)
	}

	parsed, err := pkg.ParseHTTPRequest(serialized)
	if err != nil {
		t.Fatalf("failed to parse serialized request: %v", err)
	}
	defer parsed.Body.Close()

	parsedBody, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("failed to read parsed request body: %v", err)
	}
	if parsed.Method != http.MethodPost {
		t.Fatalf("expected method %q, got %q", http.MethodPost, parsed.Method)
	}
	if parsed.URL.RequestURI() != "/orders?status=paid" {
		t.Fatalf("expected request URI %q, got %q", "/orders?status=paid", parsed.URL.RequestURI())
	}
	if parsed.Host != "api:8080" {
		t.Fatalf("expected host %q, got %q", "api:8080", parsed.Host)
	}
	if !bytes.Equal(parsedBody, body) {
		t.Fatalf("expected request body %q, got %q", string(body), string(parsedBody))
	}
}

func TestSerializeCapturedResponseRoundTrip(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	reqBody := []byte(`{"name":"demo"}`)
	rawReq := fmt.Sprintf(
		"POST /orders HTTP/1.1\r\nHost: api:8080\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		len(reqBody),
		reqBody,
	)
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(rawReq)))
	if err != nil {
		t.Fatalf("failed to parse original request: %v", err)
	}

	respBody := []byte(`{"ok":true}`)
	rawResp := fmt.Sprintf(
		"HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		len(respBody),
		respBody,
	)
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(rawResp)), req)
	if err != nil {
		t.Fatalf("failed to parse original response: %v", err)
	}

	serialized, err := serializeCapturedResponse(resp, respBody)
	if err != nil {
		t.Fatalf("failed to serialize response: %v", err)
	}

	parsed, err := pkg.ParseHTTPResponse(serialized, req)
	if err != nil {
		t.Fatalf("failed to parse serialized response: %v", err)
	}
	defer parsed.Body.Close()

	parsedBody, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("failed to read parsed response body: %v", err)
	}
	if parsed.StatusCode != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, parsed.StatusCode)
	}
	if !bytes.Equal(parsedBody, respBody) {
		t.Fatalf("expected response body %q, got %q", string(respBody), string(parsedBody))
	}
}
