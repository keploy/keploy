package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func stubIngressPaused(t *testing.T, fn func() bool) {
	t.Helper()
	prev := isIngressRecordingPaused
	isIngressRecordingPaused = fn
	t.Cleanup(func() {
		isIngressRecordingPaused = prev
	})
}

// TestAsyncPipeFeederLastReadTimeMatchesConsumedChunk locks in the contract
// that LastReadTime, when called after a successful Read, returns the readAt
// of the chunk whose data the most recent Read returned bytes from — never
// a chunk that is queued but not yet consumed.
//
// This is the exact race that broke listmonk-postgres on PR #4130's first
// pass: the previous bridge+pipe design stored chunk N+1's ts BEFORE
// pipe.Write blocked, so a parser that finished consuming chunk N and
// queried LastReadTime before the bridge advanced past pipe.Write would
// observe the next chunk's ts as the current request's reqTimestamp,
// pushing per-test postgres mocks out of the window. Empty-body GETs
// hit it deterministically because the parser never re-entered Read
// between ReadRequest and LastReadTime.
func TestAsyncPipeFeederLastReadTimeMatchesConsumedChunk(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	f := newAsyncPipeFeeder(0, zap.NewNop())

	// Enqueue three chunks at distinct times.
	t1 := time.Now()
	t2 := t1.Add(15 * time.Millisecond)
	t3 := t2.Add(15 * time.Millisecond)
	f.ch <- timedChunk{data: []byte("AAA"), readAt: t1}
	f.ch <- timedChunk{data: []byte("BBB"), readAt: t2}
	f.ch <- timedChunk{data: []byte("CCC"), readAt: t3}
	close(f.ch)

	// Before any Read, LastReadTime is zero.
	if !f.LastReadTime().IsZero() {
		t.Fatalf("expected zero LastReadTime before any Read, got %v", f.LastReadTime())
	}

	read := func(n int) string {
		buf := make([]byte, n)
		got, err := f.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected Read error: %v", err)
		}
		return string(buf[:got])
	}

	// Consume chunk 1 fully. LastReadTime should equal t1, NOT t2 — even
	// though chunk 2 is already queued.
	if got := read(3); got != "AAA" {
		t.Fatalf("first read: want AAA, got %q", got)
	}
	if !f.LastReadTime().Equal(t1) {
		t.Fatalf("after chunk 1: want %v, got %v (overshoot to next chunk)", t1, f.LastReadTime())
	}

	// Consume chunk 2 fully. LastReadTime should equal t2.
	if got := read(3); got != "BBB" {
		t.Fatalf("second read: want BBB, got %q", got)
	}
	if !f.LastReadTime().Equal(t2) {
		t.Fatalf("after chunk 2: want %v, got %v", t2, f.LastReadTime())
	}

	// Partial read of chunk 3 still updates LastReadTime to t3 (the
	// chunk was popped on the partial read; subsequent reads draw
	// from the same chunk).
	if got := read(2); got != "CC" {
		t.Fatalf("third read partial: want CC, got %q", got)
	}
	if !f.LastReadTime().Equal(t3) {
		t.Fatalf("after partial chunk 3: want %v, got %v", t3, f.LastReadTime())
	}
	// Drain the rest from the same chunk.
	if got := read(8); got != "C" {
		t.Fatalf("third read drain: want C, got %q", got)
	}
	if !f.LastReadTime().Equal(t3) {
		t.Fatalf("after full chunk 3: want %v, got %v", t3, f.LastReadTime())
	}

	// Channel closed, no more chunks. EOF.
	buf := make([]byte, 4)
	if _, err := f.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF after drain, got %v", err)
	}
}

// TestAsyncPipeFeederShutdownDrainWaitsForParser guards the regression
// fixed alongside python-schema-match's missing /edge/nested_null capture:
// shutdown's residual-chunk drain MUST wait for the parser goroutine to
// exit before it consumes anything from the channel. Otherwise the drain
// races the parser's Read for closed-channel data and silently steals
// chunks the parser has not yet seen.
//
// Repro shape: short-lived HTTP/1.0 + Connection: close exchange where the
// upstream socket EOFs immediately after the response, triggering Close
// (and thus shutdown) on the feeder before the parser goroutine has been
// scheduled. The parser then reads from a channel the drain has already
// emptied, returns EOF, and emits nothing.
func TestAsyncPipeFeederShutdownDrainWaitsForParser(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	f := newAsyncPipeFeeder(0, zap.NewNop())

	// Enqueue a chunk, mimicking io.Copy's Write of a single response.
	if _, err := f.Write([]byte("HELLO")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Forwarder finished — close the feeder. This used to spawn a drain
	// goroutine that started consuming f.ch immediately, racing with the
	// not-yet-scheduled parser.
	f.Close()

	// Give the (pre-fix) drain goroutine ample time to win the race and
	// empty the channel. With the fix in place the drain blocks on the
	// parserExited signal and stays out of the channel.
	time.Sleep(20 * time.Millisecond)

	// Now play parser: read the chunk. With the fix this must succeed —
	// the chunk is still in the channel waiting for us. Pre-fix the
	// channel is empty and Read returns EOF.
	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read: unexpected error %v", err)
	}
	if string(buf[:n]) != "HELLO" {
		t.Fatalf("parser was supposed to consume HELLO before drain — got %q (drain raced and stole the chunk)", string(buf[:n]))
	}

	// Signal parser exit so shutdown's drain can proceed and the goroutine
	// doesn't leak past test end.
	f.signalParserExit()

	// EOF after drain.
	if _, err := f.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF after drain, got %v", err)
	}
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

func TestNewTeeReadCloserStreamsAndCopies(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	capture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
	body := io.NopCloser(strings.NewReader("payload"))
	wrapped := newTeeReadCloser(body, capture)
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

// stubCaptureHook replaces the package-level CaptureHook for the duration
// of a test so the test can observe capture calls without running the
// full parser + yaml-persist stack.
func stubCaptureHook(t *testing.T, fn hooksUtils.CaptureFunc) {
	t.Helper()
	prev := hooksUtils.CaptureHook
	hooksUtils.CaptureHook = fn
	t.Cleanup(func() { hooksUtils.CaptureHook = prev })
}

// TestHandleHttp1Connection_ChunkedExchangeIsCaptured is a regression test
// for the record-time "skip chunked capture" bug that dropped 24 of every
// 25 tests when the upstream returned Transfer-Encoding: chunked (the Go
// net/http default when no Content-Length is set).
//
// The pre-fix handleHttp1Connection logged "Skipping testcase capture for
// streaming exchange" and never called CaptureHook. After the fix, chunked
// exchanges under the per-body capture budget are persisted normally.
func TestHandleHttp1Connection_ChunkedExchangeIsCaptured(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	// Upstream httptest server that returns a chunked response
	// (no Content-Length => net/http picks Transfer-Encoding: chunked).
	const upstreamBody = `{"ok":true,"id":42}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Explicitly flush before body to force chunked framing in
		// addition to the missing Content-Length.
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(upstreamBody[:5]))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(upstreamBody[5:]))
	}))
	t.Cleanup(upstream.Close)
	upstreamAddr := strings.TrimPrefix(upstream.URL, "http://")

	// Capture observed test cases (CaptureHook replaces the real
	// dump+parse+persist pipeline for this unit test).
	var (
		captured   []*capturedExchange
		capturedMu sync.Mutex
		captureWG  sync.WaitGroup
	)
	captureWG.Add(1)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, synchronous bool, mapping bool, appPort uint16) {
		defer captureWG.Done()
		// Read request+response bodies here so the test can assert them.
		reqBody, _ := io.ReadAll(req.Body)
		respBody, _ := io.ReadAll(resp.Body)
		capturedMu.Lock()
		captured = append(captured, &capturedExchange{
			method:   req.Method,
			url:      req.URL.String(),
			reqBody:  string(reqBody),
			status:   resp.StatusCode,
			respBody: string(respBody),
		})
		capturedMu.Unlock()
	})

	// Build an IngressProxyManager configured for synchronous record mode.
	pm := &IngressProxyManager{
		logger:       zap.NewNop(),
		tcChan:       make(chan *models.TestCase, 4),
		synchronous:  true,
		samplingSem:  make(chan struct{}, 1),
		incomingOpts: models.IncomingOptions{},
	}

	// Dial the client side via a TCP pipe. handleHttp1Connection dials
	// upstream itself (so it needs a real TCP listener), but the client
	// side is a loopback TCP connection we drive from the test.
	clientListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { _ = clientListener.Close() })

	connCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := clientListener.Accept()
		if aerr != nil {
			t.Logf("accept err: %v", aerr)
			return
		}
		connCh <- c
	}()

	clientConn, err := net.Dial("tcp4", clientListener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial client: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for accept goroutine to deliver serverConn")
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	// Run handleHttp1Connection in a goroutine — it reads the request off
	// serverConn, forwards to upstream, writes response back, and fires
	// the capture goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sem := make(chan struct{}, 1)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		pm.handleHttp1Connection(ctx, serverConn, upstreamAddr, pm.logger, pm.tcChan, sem, 8080)
	}()

	// Send a plain HTTP/1.1 GET from the test "client" side. We don't
	// need a chunked request body for this regression — the response
	// is chunked, which is the common case. (A chunked request variant
	// is covered by the second test below.)
	req := "GET /resource HTTP/1.1\r\nHost: example.local\r\nConnection: close\r\n\r\n"
	if _, werr := clientConn.Write([]byte(req)); werr != nil {
		t.Fatalf("failed to write request: %v", werr)
	}

	// Read the full response back (handleHttp1Connection writes it).
	respReader := bufio.NewReader(clientConn)
	respMsg, err := http.ReadResponse(respReader, nil)
	if err != nil {
		t.Fatalf("failed to read response from handler: %v", err)
	}
	gotBody, _ := io.ReadAll(respMsg.Body)
	respMsg.Body.Close()
	if string(gotBody) != upstreamBody {
		t.Fatalf("forwarded response body mismatch: want %q got %q", upstreamBody, string(gotBody))
	}

	// Wait for the capture goroutine to run.
	captureDone := make(chan struct{})
	go func() {
		captureWG.Wait()
		close(captureDone)
	}()
	select {
	case <-captureDone:
	case <-time.After(3 * time.Second):
		t.Fatal("CaptureHook was never invoked for chunked exchange — the skip bug has regressed")
	}

	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handleHttp1Connection did not return within 3s; inspect the handler's ctx/EOF handling")
	}

	capturedMu.Lock()
	defer capturedMu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 captured test case for chunked exchange, got %d", len(captured))
	}
	got := captured[0]
	if got.method != http.MethodGet {
		t.Fatalf("captured method = %q, want GET", got.method)
	}
	if got.status != http.StatusOK {
		t.Fatalf("captured status = %d, want 200", got.status)
	}
	if got.respBody != upstreamBody {
		t.Fatalf("captured response body = %q, want %q", got.respBody, upstreamBody)
	}
}

// TestHandleHttp1Connection_ChunkedRequestIsCaptured covers the
// Transfer-Encoding: chunked request side (less common than chunked
// responses, but also dropped by the pre-fix skip branch).
func TestHandleHttp1Connection_ChunkedRequestIsCaptured(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })

	const reqBody = "hello-chunked-body"
	var gotUpstreamBody string
	var gotUpstreamMu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotUpstreamMu.Lock()
		gotUpstreamBody = string(b)
		gotUpstreamMu.Unlock()
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	upstreamAddr := strings.TrimPrefix(upstream.URL, "http://")

	var (
		captured  []*capturedExchange
		mu        sync.Mutex
		captureWG sync.WaitGroup
	)
	captureWG.Add(1)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, synchronous bool, mapping bool, appPort uint16) {
		defer captureWG.Done()
		rb, _ := io.ReadAll(req.Body)
		rsb, _ := io.ReadAll(resp.Body)
		mu.Lock()
		captured = append(captured, &capturedExchange{
			method:   req.Method,
			url:      req.URL.String(),
			reqBody:  string(rb),
			status:   resp.StatusCode,
			respBody: string(rsb),
		})
		mu.Unlock()
	})

	pm := &IngressProxyManager{
		logger:       zap.NewNop(),
		tcChan:       make(chan *models.TestCase, 4),
		synchronous:  true,
		samplingSem:  make(chan struct{}, 1),
		incomingOpts: models.IncomingOptions{},
	}

	clientListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { _ = clientListener.Close() })

	connCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := clientListener.Accept()
		if aerr != nil {
			return
		}
		connCh <- c
	}()

	clientConn, err := net.Dial("tcp4", clientListener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for accept goroutine to deliver serverConn")
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sem := make(chan struct{}, 1)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		pm.handleHttp1Connection(ctx, serverConn, upstreamAddr, pm.logger, pm.tcChan, sem, 8080)
	}()

	// Construct a chunked-encoded POST request manually.
	chunkHex := fmt.Sprintf("%x", len(reqBody))
	raw := "POST /upload HTTP/1.1\r\n" +
		"Host: example.local\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		chunkHex + "\r\n" + reqBody + "\r\n" +
		"0\r\n\r\n"
	if _, werr := clientConn.Write([]byte(raw)); werr != nil {
		t.Fatalf("failed to write chunked request: %v", werr)
	}

	respReader := bufio.NewReader(clientConn)
	respMsg, err := http.ReadResponse(respReader, nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, respMsg.Body)
	respMsg.Body.Close()

	gotUpstreamMu.Lock()
	if gotUpstreamBody != reqBody {
		t.Fatalf("upstream received %q, want %q", gotUpstreamBody, reqBody)
	}
	gotUpstreamMu.Unlock()

	captureDone := make(chan struct{})
	go func() {
		captureWG.Wait()
		close(captureDone)
	}()
	select {
	case <-captureDone:
	case <-time.After(3 * time.Second):
		t.Fatal("CaptureHook was never invoked for chunked-request exchange — the skip bug has regressed")
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not finish after the chunked-request exchange; inspect the handler shutdown path")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 captured test case for chunked request, got %d", len(captured))
	}
	if captured[0].reqBody != reqBody {
		t.Fatalf("captured request body = %q, want %q", captured[0].reqBody, reqBody)
	}
}

type capturedExchange struct {
	method, url, reqBody string
	status               int
	respBody             string
}
