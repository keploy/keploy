package http

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestBuildReplayResponse_Chunked verifies that a mock recorded with
// Transfer-Encoding: chunked replays as a valid chunked HTTP response: the wire
// bytes parse back with the correct de-chunked body, chunked framing intact,
// and no Content-Length (the two framings are mutually exclusive).
func TestBuildReplayResponse_Chunked(t *testing.T) {
	statusLine := "HTTP/1.1 200 OK\r\n"
	header := http.Header{
		"Content-Type":      []string{"application/json"},
		"Transfer-Encoding": []string{"chunked"},
	}
	body := `{"changed":true,"version":117}`

	raw := buildReplayResponse(statusLine, header, body)

	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed on replayed bytes: %v\nraw:\n%s", err, raw)
	}
	defer resp.Body.Close()

	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v, want [chunked]", resp.TransferEncoding)
	}
	// Assert on the parsed response (not a raw substring — a body could itself
	// contain "content-length"): chunked responses must not carry Content-Length.
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("chunked replay must not carry Content-Length, got %q", cl)
	}
	if resp.ContentLength != -1 {
		t.Errorf("chunked ContentLength = %d, want -1 (unknown)", resp.ContentLength)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != body {
		t.Errorf("de-chunked body = %q, want %q", got, body)
	}
}

// TestBuildReplayResponse_ContentLength verifies the non-chunked path recomputes
// Content-Length from the actual body (ignoring any stale recorded value).
func TestBuildReplayResponse_ContentLength(t *testing.T) {
	statusLine := "HTTP/1.1 200 OK\r\n"
	header := http.Header{
		"Content-Type":   []string{"application/json"},
		"Content-Length": []string{"999"}, // stale; must be recomputed
	}
	body := "hello"

	raw := buildReplayResponse(statusLine, header, body)

	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v\nraw:\n%s", err, raw)
	}
	defer resp.Body.Close()

	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestBuildReplayResponse_ChunkedLowercaseKey ensures chunked detection is
// case-insensitive. pkg.ToHTTPHeader preserves the recorded key casing, so a
// mock header map may carry a lowercase "transfer-encoding" key; replay must
// still chunk the body rather than emit a chunked header over an unframed body.
func TestBuildReplayResponse_ChunkedLowercaseKey(t *testing.T) {
	statusLine := "HTTP/1.1 200 OK\r\n"
	header := http.Header{
		"content-type":      []string{"application/json"}, // non-canonical
		"transfer-encoding": []string{"chunked"},          // non-canonical
	}
	body := "hello world"

	raw := buildReplayResponse(statusLine, header, body)

	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed on replayed bytes (body likely sent unchunked under a chunked header): %v\nraw:\n%s", err, raw)
	}
	defer resp.Body.Close()

	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v, want [chunked]", resp.TransferEncoding)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != body {
		t.Errorf("de-chunked body = %q, want %q", got, body)
	}
}

// TestBuildReplayResponse_MissingContentLength ensures a non-chunked response
// always gets exactly one Content-Length derived from the body, even when the
// recorded header map omits it — otherwise the client waits for EOF to delimit
// the body and can hang on a keepalive connection.
func TestBuildReplayResponse_MissingContentLength(t *testing.T) {
	statusLine := "HTTP/1.1 200 OK\r\n"
	header := http.Header{
		"Content-Type": []string{"application/json"}, // deliberately no Content-Length
	}
	body := "hello world"

	raw := buildReplayResponse(statusLine, header, body)

	if n := strings.Count(raw, "Content-Length:"); n != 1 {
		t.Errorf("Content-Length header count = %d, want exactly 1\nraw:\n%s", n, raw)
	}

	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v\nraw:\n%s", err, raw)
	}
	defer resp.Body.Close()

	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}
