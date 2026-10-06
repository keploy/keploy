package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
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
	wait  <-chan struct{}
}

func (s *scriptedReadCloser) Read(p []byte) (int, error) {
	if s.idx >= len(s.steps) {
		return 0, io.EOF
	}
	step := s.steps[s.idx]
	s.idx++
	if step.wait != nil {
		<-step.wait
	}
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

	secondChunkRelease := make(chan struct{})
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
				{data: "B", wait: secondChunkRelease},
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

	// Read past the HTTP headers (up through "\r\n\r\n") to reach the body.
	headerBuf := make([]byte, 0, 512)
	oneByte := make([]byte, 1)
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline(header) error = %v", err)
	}
	for {
		if _, err := clientConn.Read(oneByte); err != nil {
			t.Fatalf("Read(header) error = %v", err)
		}
		headerBuf = append(headerBuf, oneByte[0])
		if len(headerBuf) >= 4 && string(headerBuf[len(headerBuf)-4:]) == "\r\n\r\n" {
			break
		}
	}

	// Read the first body byte before releasing the second chunk. If body streaming were buffered
	// until the full response was available, this read would time out while the second chunk remains blocked.
	firstBodyByte := make([]byte, 1)
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline(body) error = %v", err)
	}
	if _, err := clientConn.Read(firstBodyByte); err != nil {
		t.Fatalf("Read(body) error = %v; ensure the response writer is streaming body data before the next chunk is released", err)
	}
	if got := string(firstBodyByte); got != "A" {
		t.Fatalf("first body byte = %q, want %q", got, "A")
	}

	select {
	case err := <-writeDone:
		t.Fatalf("resp.Write() completed before releasing the second chunk: %v", err)
	default:
	}

	close(secondChunkRelease)

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

// A recorded response carries the headers the app sent, as replay's HTTP
// client sees them from the same app: the header whether the connection
// closes after it is Go's verdict on reusing it, which it writes into a
// serialized HTTP/1.0 response on its own, and so into the recording, where
// the app's next answer, read at replay, never has it.
func TestResponseCaptureRecordsTheHeadersTheAppSent(t *testing.T) {
	t.Parallel()

	for name, wire := range map[string]string{
		"HTTP/1.0":                    "HTTP/1.0 200 OK\r\nServer: BaseHTTP/0.6\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.0, Connection: close": "HTTP/1.0 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.0, keep-alive":        "HTTP/1.0 200 OK\r\nConnection: keep-alive\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.0, no length":         "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nok",
		"HTTP/1.1":                    "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.1, Connection: close": "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.1, no length":         "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req, err := pkg.ParseHTTPRequest([]byte("GET / HTTP/1.1\r\nHost: app\r\n\r\n"))
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), req)
			if err != nil {
				t.Fatal(err)
			}
			proxied, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(proxied.Body)
			if err != nil {
				t.Fatal(err)
			}
			dump, err := dumpCapturedResponse(proxied, req, body)
			if err != nil {
				t.Fatal(err)
			}
			recorded, err := pkg.ParseHTTPResponse(dump, req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recorded.Header, replayed.Header) {
				t.Fatalf("recorded header %v, want %v, as replay sees the app's response", recorded.Header, replayed.Header)
			}
			got, err := io.ReadAll(recorded.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "ok" {
				t.Fatalf("recorded body %q, want %q", got, "ok")
			}
		})
	}
}
