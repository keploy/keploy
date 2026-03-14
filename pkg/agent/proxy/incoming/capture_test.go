package proxy

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
)

type scriptedReadCloser struct {
	steps []readStep
	idx   int
}

type readStep struct {
	data  string
	delay time.Duration
}

func (s *scriptedReadCloser) Read(p []byte) (int, error) {
	if s.idx >= len(s.steps) {
		return 0, io.EOF
	}
	step := s.steps[s.idx]
	s.idx++
	if step.delay > 0 {
		time.Sleep(step.delay)
	}
	return copy(p, step.data), nil
}

func (s *scriptedReadCloser) Close() error {
	return nil
}

func TestCaptureBufferTruncatesWithoutBackpressure(t *testing.T) {
	t.Parallel()

	buf := newCaptureBuffer(4)
	n, err := buf.Write([]byte("abcdef"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 6 {
		t.Fatalf("Write() bytes = %d, want %d", n, 6)
	}
	if got := string(buf.Bytes()); got != "abcd" {
		t.Fatalf("captured bytes = %q, want %q", got, "abcd")
	}
	if !buf.Truncated() {
		t.Fatal("expected capture buffer to report truncation")
	}
	if got := buf.Total(); got != 6 {
		t.Fatalf("total bytes seen = %d, want %d", got, 6)
	}
}

func TestResponseCaptureStreamsToClient(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodGet, "http://example.com/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	resp := &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"text/plain"}},
		ContentLength: 2,
		Body: &scriptedReadCloser{
			steps: []readStep{
				{data: "A"},
				{data: "B", delay: 200 * time.Millisecond},
			},
		},
		Request: req,
	}

	capture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
	resp.Body = newTeeReadCloser(resp.Body, capture)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- resp.Write(serverConn)
		_ = serverConn.Close()
	}()

	firstByte := make([]byte, 1)
	start := time.Now()
	if err := clientConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if _, err := clientConn.Read(firstByte); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("first response byte arrived after %s, expected streaming before 100ms", elapsed)
	}

	if err := clientConn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline(reset) error = %v", err)
	}
	if _, err := io.Copy(io.Discard, clientConn); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("resp.Write() error = %v", err)
	}

	if got := string(capture.Bytes()); got != "AB" {
		t.Fatalf("captured body = %q, want %q", got, "AB")
	}

	rawResp, err := dumpCapturedResponse(resp, req, capture.Bytes())
	if err != nil {
		t.Fatalf("dumpCapturedResponse() error = %v", err)
	}
	parsedResp, err := pkg.ParseHTTPResponse(rawResp, req)
	if err != nil {
		t.Fatalf("ParseHTTPResponse() error = %v", err)
	}
	defer parsedResp.Body.Close()

	body, err := io.ReadAll(parsedResp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if got := string(body); got != "AB" {
		t.Fatalf("parsed response body = %q, want %q", got, "AB")
	}
}

func TestRequestCaptureRoundTrip(t *testing.T) {
	t.Parallel()

	req, err := pkg.ParseHTTPRequest([]byte("POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello"))
	if err != nil {
		t.Fatalf("ParseHTTPRequest(seed) error = %v", err)
	}
	defer req.Body.Close()

	rawReq, err := dumpCapturedRequest(req, []byte("hello"))
	if err != nil {
		t.Fatalf("dumpCapturedRequest() error = %v", err)
	}
	parsedReq, err := pkg.ParseHTTPRequest(rawReq)
	if err != nil {
		t.Fatalf("ParseHTTPRequest() error = %v", err)
	}
	defer parsedReq.Body.Close()

	body, err := io.ReadAll(parsedReq.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if got := string(body); got != "hello" {
		t.Fatalf("parsed request body = %q, want %q", got, "hello")
	}
}
