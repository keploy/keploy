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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
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

// TestCaptureHookSemaphoreMatchesConcurrencyConstant guards the link
// between captureHookConcurrency and captureHookSem's buffer size. If
// the constant is bumped without re-allocating the channel (or vice
// versa), the parser would silently lose its concurrency cap and
// regress the go-memory-load CI guard. Cap is asserted on the live
// channel so a future refactor that swaps the type still has to keep
// the invariant true.
func TestCaptureHookSemaphoreMatchesConcurrencyConstant(t *testing.T) {
	if got := cap(captureHookSem); got != captureHookConcurrency {
		t.Fatalf("captureHookSem cap=%d, want %d (must equal captureHookConcurrency)", got, captureHookConcurrency)
	}
}

// TestCaptureHookSemaphoreBackpressuresParser verifies that once
// captureHookConcurrency slots are held, a further takeCaptureSlot waits:
// a handler waits there for a slot before it starts another CaptureHook
// goroutine, and takes the next slot given back. A take whose context ends
// first gives up then, so a handler is not pinned to a saturated semaphore
// through shutdown. Without this backpressure the unbounded `go
// hooksUtils.CaptureHook(...)` call piled goroutines (each holding ~10MB
// in body buffers) past the 250 MiB go-memory-load CI threshold.
func TestCaptureHookSemaphoreBackpressuresParser(t *testing.T) {
	// Save and restore the global so this test doesn't poison sibling
	// tests. No capture of another test is running to read it: each test
	// that starts captures waits for them (pm.captures) before it returns.
	saved := captureHookSem
	t.Cleanup(func() { captureHookSem = saved })
	sem := make(chan struct{}, captureHookConcurrency)
	captureHookSem = sem

	// Bounds every wait below: a take that never returns fails the test in
	// seconds instead of hanging it until go test's timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	releases := make([]func(), 0, captureHookConcurrency)
	for i := 0; i < captureHookConcurrency; i++ {
		release, ok := takeCaptureSlot(ctx)
		if !ok {
			t.Fatalf("take %d failed before reaching capacity", i)
		}
		releases = append(releases, release)
	}

	type take struct {
		release func()
		ok      bool
	}
	// A take still running when the test fails has read captureHookSem, and
	// the restore above would race with it. This cleanup runs first and lets
	// every such take end.
	var takes sync.WaitGroup
	t.Cleanup(func() {
		cancel()           // a take that honours its context gives up,
		for len(sem) > 0 { // and one that does not gets a slot.
			<-sem
		}
		ended := make(chan struct{})
		go func() { takes.Wait(); close(ended) }()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
		}
	})
	takeInBackground := func(ctx context.Context) <-chan take {
		got := make(chan take, 1)
		takes.Add(1)
		go func() {
			defer takes.Done()
			release, ok := takeCaptureSlot(ctx)
			got <- take{release, ok}
		}()
		return got
	}

	// A take past capacity waits for a slot: that's the backpressure the
	// handler relies on.
	waiting := takeInBackground(ctx)
	select {
	case got := <-waiting:
		if got.ok {
			got.release()
		}
		t.Fatalf("a take past captureHookConcurrency returned (ok=%v) while every slot was held; it must wait for one", got.ok)
	case <-time.After(50 * time.Millisecond):
	}
	// It takes the next slot given back.
	releases[0]()
	select {
	case got := <-waiting:
		if !got.ok {
			t.Fatal("the waiting take gave up instead of taking the slot given back")
		}
		releases[0] = got.release
	case <-ctx.Done():
		t.Fatal("the waiting take did not take the slot given back")
	}

	// A take past capacity whose context ends gives up then, and not before.
	ending, cancelEnding := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelEnding()
	select {
	case got := <-takeInBackground(ending):
		if got.ok {
			got.release()
			t.Fatal("a take past captureHookConcurrency succeeded; the semaphore is not backpressuring")
		}
		if ending.Err() == nil {
			t.Fatal("a take past captureHookConcurrency gave up before its context ended; it must wait for a slot until then")
		}
	case <-ctx.Done():
		t.Fatal("a take past captureHookConcurrency did not give up when its context ended")
	}

	for _, release := range releases {
		release()
	}
	if n := len(sem); n != 0 {
		t.Fatalf("%d slots still held after every slot taken was given back", n)
	}
}

// A capture gives its slot back to the semaphore it took it from. Read again
// when the capture ended, captureHookSem freed a slot of whatever had replaced
// it by then (TestCaptureHookSemaphoreBackpressuresParser replaces it): the
// slot taken stayed held for good, and a slot its holder still had was freed.
// A capture that outlived its test also raced with that replacement.
func TestHandleHttp1ZeroCopy_CaptureGivesBackTheSlotItTook(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	upstream := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	entered, proceed := make(chan struct{}), make(chan struct{})
	var enterOnce, proceedOnce sync.Once
	letCaptureEnd := func() { proceedOnce.Do(func() { close(proceed) }) }
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase,
		*http.Request, *http.Response, time.Time, time.Time,
		models.IncomingOptions, bool, bool, uint16) {
		enterOnce.Do(func() { close(entered) })
		<-proceed
	})
	// Registered before ingressPipe's pm.captures.Wait, so it runs after it.
	saved := captureHookSem
	t.Cleanup(func() { captureHookSem = saved })
	took := make(chan struct{}, captureHookConcurrency)
	captureHookSem = took

	// Neither synchronous nor sampled: the zero-copy path.
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())), 10*time.Second)
	defer cancel()
	client, done := ingressPipe(t, ctx, pm, upstream)
	t.Cleanup(letCaptureEnd) // runs before ingressPipe's cleanups: a failure still lets the capture end

	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the exchange was not captured")
	}
	if n := len(took); n != 1 {
		t.Fatalf("captureHookSem holds %d slots while the capture runs, want the 1 it took", n)
	}

	// While the capture holds its slot, captureHookSem is replaced by a
	// semaphore one slot of which its holder still has.
	replacement := make(chan struct{}, captureHookConcurrency)
	replacement <- struct{}{}
	captureHookSem = replacement
	letCaptureEnd()

	captured := make(chan struct{})
	go func() { pm.captures.Wait(); close(captured) }()
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal("the capture did not end")
	}
	if n := len(took); n != 0 {
		t.Errorf("the semaphore the capture took its slot from still holds %d, want it given back", n)
	}
	if n := len(replacement); n != 1 {
		t.Errorf("the semaphore that replaced it holds %d slots, want its holder's 1: the capture gave back a slot it never took there", n)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handleHttp1Connection did not return")
	}
}

// TestWireTimeConn_LastReadTimePrecedesBufioParseCompletion locks in the
// contract that wireTimeConn.LastReadTime returns the time of the
// most recent socket Read, which by construction is BEFORE the call
// site that consumes the buffered bytes (here: an http.ReadRequest
// that goes through a bufio.Reader on top of the wireTimeConn).
//
// This is the wire-arrival timestamp the sync-path HTTP capture loop
// stamps onto tc.HTTPReq.Timestamp. The whole point of the wrapper is
// that this timestamp is on-or-before the real arrival of the bytes,
// so downstream parser captures (postgres v3 reqTimestampMock) sampled
// at decode time — which is necessarily AFTER the SUT receives and
// processes the HTTP request — never fall outside the per-test window's
// left edge.
func TestWireTimeConn_LastReadTimePrecedesBufioParseCompletion(t *testing.T) {
	srvSide, clientSide := net.Pipe()
	defer srvSide.Close()
	defer clientSide.Close()

	wire := &wireTimeConn{Conn: srvSide}
	if !wire.LastReadTime().IsZero() {
		t.Fatalf("expected zero LastReadTime before any Read, got %v", wire.LastReadTime())
	}

	rawReq := "GET /probe HTTP/1.1\r\nHost: x\r\n\r\n"
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		if _, err := clientSide.Write([]byte(rawReq)); err != nil {
			t.Errorf("client Write: %v", err)
		}
	}()

	br := bufio.NewReader(wire)
	beforeParse := time.Now()
	req, err := http.ReadRequest(br)
	if err != nil {
		t.Fatalf("http.ReadRequest: %v", err)
	}
	afterParse := time.Now()
	<-writeDone

	if req.URL.Path != "/probe" {
		t.Fatalf("parsed unexpected request: %+v", req.URL)
	}

	got := wire.LastReadTime()
	if got.IsZero() {
		t.Fatalf("LastReadTime is zero after a successful ReadRequest")
	}
	// LastReadTime is sampled inside Read AFTER the syscall returns,
	// so it must be no earlier than the moment we entered ReadRequest
	// and no later than the moment ReadRequest returned. The whole
	// point of the wrapper is that it is also on-or-before the call
	// site of "after parse" — i.e. on-or-before the time the sync-path
	// loop would have sampled time.Now() under the old behaviour.
	if got.Before(beforeParse) {
		t.Fatalf("LastReadTime %v is before the call to ReadRequest %v", got, beforeParse)
	}
	if got.After(afterParse) {
		t.Fatalf("LastReadTime %v is AFTER ReadRequest returned %v — wrapper sampled too late", got, afterParse)
	}
}

// TestReqTimestampClampedAtIterStartUnderBufioPrefetch is the regression
// test for the gin-mongo `record_build_replay_build` failure on
// keploy/keploy#4147 review iteration: HTTP/1.1 keepalive can have
// bufio.Reader serve request N entirely from bytes prefetched during
// the prior iteration's socket Read (which was consuming request N-1's
// tail). When that happens, wireConn.LastReadTime is from request N-1's
// read window — DURING the previous test's handler — and using it raw
// as request N's reqTimestamp pushes the per-test mock-window left
// edge backwards across the previous test boundary, contaminating
// request N's mock pool with mocks captured during request N-1.
//
// The capture loop's clamp `if !lastRead.After(iterStart) { ts =
// iterStart }` defends against this. iterStart is captured AFTER the
// previous iteration finished (i.e. after the previous response was
// written), so the previous test's mocks are guaranteed to be outside
// this test's window.
//
// The test simulates the prefetch scenario by:
//  1. Pre-stamping wireTimeConn.lastReadNano to a moment in the past
//     (representing a Read that fired during the prior iteration).
//  2. Capturing iterStart = time.Now().
//  3. Driving a ReadRequest entirely from bytes already in bufio's
//     buffer (no underlying socket Read this iteration).
//  4. Asserting the clamp falls back to iterStart, NOT the stale
//     prior-iter lastReadNano.
func TestReqTimestampClampedAtIterStartUnderBufioPrefetch(t *testing.T) {
	srvSide, clientSide := net.Pipe()
	defer srvSide.Close()
	defer clientSide.Close()

	wire := &wireTimeConn{Conn: srvSide}

	// Step 1: pre-fill bufio's internal buffer in a way that emulates
	// prefetch: write a full HTTP request to the pipe BEFORE the
	// capture loop's iterStart is taken, then drive the bufio Read
	// once so the bytes land in the buffer with lastReadNano stamped
	// to the prior-iteration time.
	rawReq := "GET /probe HTTP/1.1\r\nHost: x\r\n\r\n"
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		if _, err := clientSide.Write([]byte(rawReq)); err != nil {
			t.Errorf("client Write: %v", err)
		}
	}()

	br := bufio.NewReader(wire)
	if _, err := br.Peek(1); err != nil {
		t.Fatalf("priming bufio buffer: %v", err)
	}
	<-writeDone

	priorRead := wire.LastReadTime()
	if priorRead.IsZero() {
		t.Fatalf("priming Peek did not stamp lastReadNano")
	}

	// Step 2: simulate the next iteration starting AFTER the prior
	// request's response was written. Sleep long enough that
	// iterStart is strictly later than priorRead even on a low-res
	// monotonic clock.
	time.Sleep(2 * time.Millisecond)
	iterStart := time.Now()

	// Step 3: ReadRequest serves entirely from the prefetched buffer
	// — no underlying socket Read happens during this iteration.
	req, err := http.ReadRequest(br)
	if err != nil {
		t.Fatalf("http.ReadRequest: %v", err)
	}
	if req.URL.Path != "/probe" {
		t.Fatalf("parsed unexpected request: %+v", req.URL)
	}

	// Step 4: lastReadNano is unchanged from priorRead because no
	// new Read fired this iteration. The capture loop's clamp must
	// detect that and fall back to iterStart.
	lastRead := wire.LastReadTime()
	if !lastRead.Equal(priorRead) {
		t.Fatalf("lastReadNano changed during a buffer-served ReadRequest: prior=%v now=%v", priorRead, lastRead)
	}

	// Replay the capture loop's clamp logic.
	reqTimestamp := lastRead
	if !reqTimestamp.After(iterStart) {
		reqTimestamp = iterStart
	}

	if reqTimestamp.Before(iterStart) {
		t.Fatalf("reqTimestamp %v fell BEFORE iterStart %v — clamp failed and per-test window left edge would bleed into the prior iteration", reqTimestamp, iterStart)
	}
	if !reqTimestamp.Equal(iterStart) {
		t.Fatalf("expected reqTimestamp == iterStart under prefetch (no fresh Read this iter); got reqTimestamp=%v iterStart=%v", reqTimestamp, iterStart)
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
		logger:      zap.NewNop(),
		tcChan:      make(chan *models.TestCase, 4),
		synchronous: true,
		samplingSem: make(chan struct{}, 1),
	}
	t.Cleanup(pm.captures.Wait) // every capture it started is over before the next test swaps a hook or a semaphore

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
		logger:      zap.NewNop(),
		tcChan:      make(chan *models.TestCase, 4),
		synchronous: true,
		samplingSem: make(chan struct{}, 1),
	}
	t.Cleanup(pm.captures.Wait) // every capture it started is over before the next test swaps a hook or a semaphore

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

// oneShotUpstream answers each connection with one response, then closes it.
func oneShotUpstream(t *testing.T, response string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				_, _ = io.WriteString(c, response)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// Sync and sampled recording close the client's connection after each
// exchange, to free their slot, and say so in the response they forward. The
// recording keeps the response as the app sent it: the app's answer at replay
// does not say so either, and a test recorded with it failed every replay.
func TestHandleHttp1Connection_ForcedCloseIsNotRecorded(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	for _, tc := range []struct {
		mode, app, response string
		recorded            []string // the Connection header recorded, as the app sent it
	}{
		{"sync", "HTTP/1.0", "HTTP/1.0 200 OK\r\nServer: BaseHTTP/0.6\r\nContent-Length: 2\r\n\r\nok", nil},
		{"sampled", "HTTP/1.0", "HTTP/1.0 200 OK\r\nServer: BaseHTTP/0.6\r\nContent-Length: 2\r\n\r\nok", nil},
		{"sync", "HTTP/1.1 keep-alive", "HTTP/1.1 200 OK\r\nServer: BaseHTTP/0.6\r\nConnection: keep-alive\r\nContent-Length: 2\r\n\r\nok", []string{"keep-alive"}},
		{"sampled", "HTTP/1.1 keep-alive", "HTTP/1.1 200 OK\r\nServer: BaseHTTP/0.6\r\nConnection: keep-alive\r\nContent-Length: 2\r\n\r\nok", []string{"keep-alive"}},
	} {
		mode := tc.mode
		t.Run(mode+", "+tc.app, func(t *testing.T) {
			upstream := oneShotUpstream(t, tc.response)
			recorded := make(chan http.Header, 1)
			stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
				_ *http.Request, resp *http.Response, _, _ time.Time,
				_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
				recorded <- resp.Header.Clone()
			})
			pm := &IngressProxyManager{
				logger:      zap.NewNop(),
				tcChan:      make(chan *models.TestCase, 4),
				synchronous: mode == "sync",
				sampling:    mode == "sampled",
				samplingSem: make(chan struct{}, 1),
			}
			t.Cleanup(pm.captures.Wait) // every capture it started is over before the next test swaps a hook or a semaphore

			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- c
				}
			}()
			clientConn, err := net.Dial("tcp4", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = clientConn.Close() })
			serverConn := <-accepted
			t.Cleanup(func() { _ = serverConn.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				pm.handleHttp1Connection(ctx, serverConn, upstream, pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
			}()

			if _, err := io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: app\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			// The bytes on the wire: Go's parser drops an HTTP/1.1
			// response's Connection: close from its header.
			var wire bytes.Buffer
			forwarded, err := http.ReadResponse(bufio.NewReader(io.TeeReader(clientConn, &wire)), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(forwarded.Body)
			_ = forwarded.Body.Close()
			head, _, _ := strings.Cut(wire.String(), "\r\n\r\n")
			if !strings.Contains(head+"\r\n", "\r\nConnection: close\r\n") {
				t.Fatalf("forwarded %q, want Connection: close: the client's connection is closed after the exchange", head)
			}

			select {
			case header := <-recorded:
				if got := header["Connection"]; !slices.Equal(got, tc.recorded) {
					t.Fatalf("recorded Connection %q, want %q as the app sent it; recorded header %v", got, tc.recorded, header)
				}
				if got := header.Get("Server"); got != "BaseHTTP/0.6" {
					t.Fatalf("recorded Server %q, want the app's", got)
				}
			case <-ctx.Done():
				t.Fatal("the exchange was not recorded")
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("handleHttp1Connection did not return")
			}
		})
	}
}

// pausedExchange sends one GET for path on client (closing the connection
// after it when last) and reads its response whole.
func pausedExchange(t *testing.T, client net.Conn, br *bufio.Reader, path string, last bool) {
	t.Helper()
	hdr := ""
	if last {
		hdr = "Connection: close\r\n"
	}
	if _, err := client.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: app.local\r\n" + hdr + "\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
}

// A pause of the memory guard costs the exchanges that come while it lasts and
// no more: the keep-alive connection's next exchange after it is captured, and
// the one that came during it is counted, on its connection, as not recorded.
// Latched for the connection's life (captureEnabled cleared at the loop's top
// and never set again), one half-second pause stopped the capture of every
// keep-alive connection open through it, a load generator's included, for the
// rest of the recording, and the recording did not say so.
func TestHandleHttp1ZeroCopy_AKeepAliveConnectionIsCapturedAgainAfterAPause(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	mgr := syncMock.New(zap.NewNop())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	captured := make(chan string, 4)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		captured <- req.URL.Path
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	app := strings.TrimPrefix(upstream.URL, "http://")
	client, done := ingressPipe(t, ctx, pm, app)
	br := bufio.NewReader(client)
	next := func(what string) string {
		t.Helper()
		select {
		case p := <-captured:
			return p
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not captured", what)
			return ""
		}
	}

	pausedExchange(t, client, br, "/before", false)
	if p := next("the exchange before the pause"); p != "/before" {
		t.Fatalf("captured %s, want /before", p)
	}
	paused.Store(true)
	pausedExchange(t, client, br, "/during", false)
	paused.Store(false)
	pausedExchange(t, client, br, "/after", true)
	if p := next("the exchange after the pause, on the same keep-alive connection"); p != "/after" {
		t.Fatalf("captured %s, want /after", p)
	}
	<-done
	pm.captures.Wait()
	if len(captured) != 0 {
		t.Fatalf("captured %s too: the exchange that came during the pause is not recorded", <-captured)
	}

	got := mgr.NotRecordedUnderPressure()
	if got.Total != 1 || len(got.ByConn) != 1 {
		t.Fatalf("not recorded under pressure: %+v, want the one exchange that came during the pause, on its connection", got)
	}
	for conn := range got.ByConn {
		if !strings.HasSuffix(conn, " -> "+app) || !strings.HasPrefix(conn, "127.0.0.1:") {
			t.Fatalf("the connection is named %q, want <client address> -> %s", conn, app)
		}
	}
}

// In the synchronous loop an exchange whose capture a pause decided against is
// counted as not recorded, once, whichever of the loop's checks saw the pause;
// one that came while recording ran is captured and counts nothing.
func TestHandleHttp1Connection_SyncCountsAnExchangeAPauseCost(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	mgr := syncMock.New(zap.NewNop())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	captured := make(chan string, 4)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		captured <- req.URL.Path
	})
	app := strings.TrimPrefix(upstream.URL, "http://")
	for _, during := range []bool{true, false} {
		paused.Store(during)
		pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: true}
		ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
		client, done := ingressPipeOn(t, ctx, pm, app, make(chan struct{}, 1), nil)
		pausedExchange(t, client, bufio.NewReader(client), "/x", true)
		<-done
		pm.captures.Wait()
		cancel()
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d exchanges, want the one that came while recording ran", len(captured))
	}
	if got := mgr.NotRecordedUnderPressure(); got.Total != 1 || got.Connections() != 1 {
		t.Fatalf("not recorded under pressure: %+v, want the one exchange that came during the pause", got)
	}
}

// The debug knob that forces a pause by request count
// (memoryguard.ForcePauseEnv) is driven by both ingress loops: each counts the
// request it has read before it reads the pause for it, so with "2:3" the
// second request comes during the pause and is counted as not recorded, and
// the first and third are captured.
func TestBothIngressLoopsDriveTheForcedPause(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		name := "zero-copy"
		if synchronous {
			name = "synchronous"
		}
		t.Run(name, func(t *testing.T) {
			stubIngressPaused(t, memoryguard.IsRecordingPaused)
			t.Cleanup(memoryguard.SetForcedPauseForTest("2:3"))
			mgr := syncMock.New(zap.NewNop())
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(upstream.Close)
			captured := make(chan string, 4)
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
				captured <- req.URL.Path
			})
			app := strings.TrimPrefix(upstream.URL, "http://")
			for _, path := range []string{"/1", "/2", "/3"} {
				pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous}
				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
				client, done := ingressPipeOn(t, ctx, pm, app, make(chan struct{}, 1), nil)
				pausedExchange(t, client, bufio.NewReader(client), path, true)
				<-done
				pm.captures.Wait()
				cancel()
			}
			close(captured)
			var got []string
			for p := range captured {
				got = append(got, p)
			}
			if !slices.Equal(got, []string{"/1", "/3"}) {
				t.Fatalf("captured %v, want /1 and /3: the second request came during the forced pause", got)
			}
			if n := mgr.NotRecordedUnderPressure(); n.Total != 1 {
				t.Fatalf("not recorded under pressure: %+v, want the second request", n)
			}
			if memoryguard.IsRecordingPaused() {
				t.Fatal("recording is still paused after the third request")
			}
		})
	}
}

// firstReadConn is a connection that says when it was first read.
type firstReadConn struct {
	net.Conn
	read *atomic.Bool
}

func (c firstReadConn) Read(p []byte) (int, error) {
	c.read.Store(true)
	return c.Conn.Read(p)
}

// A sampled connection (one the handler closes after its exchange) is decided
// as its request comes, not as it opens: a pause that was on when it opened and
// is over before its request is read costs it nothing. Read at the open, the
// pause demoted the connection out of the sample, and its request was neither
// recorded nor counted.
func TestHandleHttp1ZeroCopy_ASampledConnectionReadsThePauseAsItsRequestComes(t *testing.T) {
	var read atomic.Bool
	stubIngressPaused(t, func() bool { return !read.Load() }) // paused until the client's side is first read
	mgr := syncMock.New(zap.NewNop())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	captured := make(chan string, 2)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		captured <- req.URL.Path
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 2)}
	t.Cleanup(pm.captures.Wait)
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	upConn, err := net.Dial("tcp4", strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	server, client := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleHttp1ZeroCopy(ctx, firstReadConn{Conn: server, read: &read}, upConn, pm.logger, pm.tcChan, 8080, true, func() {})
	}()
	pausedExchange(t, client, bufio.NewReader(client), "/sampled", true)
	<-done
	pm.captures.Wait()
	select {
	case p := <-captured:
		if p != "/sampled" {
			t.Fatalf("captured %s, want /sampled", p)
		}
	default:
		t.Fatal("the sampled exchange was not captured: the pause over before its request came cost it")
	}
	if n := mgr.NotRecordedUnderPressure().Total; n != 0 {
		t.Fatalf("counted %d requests not recorded under pressure, want none", n)
	}
}

// A pause that starts while a body is being forwarded and ends before its
// exchange does cuts that body's capture: the exchange is not recorded, and it
// is counted as not recorded under pressure, in both loops. Every check the
// loops make of the pause itself comes before the pause (the zero-copy loop
// reads it once, as the request comes) or after it, so only the cut capture
// says the pause cost the exchange; read as the size budget, the exchange was
// dropped and nothing said so.
func TestBothIngressLoopsCountAnExchangeAPauseCutMidBody(t *testing.T) {
	parts := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for _, synchronous := range []bool{false, true} {
		for _, side := range []string{"request", "response"} {
			name := "zero-copy/" + side
			if synchronous {
				name = "synchronous/" + side
			}
			t.Run(name, func(t *testing.T) {
				var paused atomic.Bool
				stubIngressPaused(t, paused.Load)
				mgr := syncMock.New(zap.NewNop())
				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
				defer cancel()
				// step hands each body part on: the app takes the next one
				// (request) or writes it (response) only once the test says.
				step, took := make(chan struct{}), make(chan struct{})
				wait := func(ch chan struct{}, what string) {
					t.Helper()
					select {
					case <-ch:
					case <-ctx.Done():
						t.Fatalf("%s: %v", what, ctx.Err())
					}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if side == "request" {
						buf := make([]byte, len(parts[0]))
						for range parts {
							if _, err := io.ReadFull(r.Body, buf); err != nil {
								return
							}
							select {
							case took <- struct{}{}:
							case <-ctx.Done():
								return
							}
						}
						_, _ = w.Write([]byte("ok"))
						return
					}
					for i, part := range parts {
						if i > 0 {
							select {
							case <-step:
							case <-ctx.Done():
								return
							}
						}
						_, _ = w.Write([]byte(part))
						w.(http.Flusher).Flush()
					}
				}))
				t.Cleanup(upstream.Close)
				captured := make(chan string, 4)
				stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
					req *http.Request, resp *http.Response, reqTS, respTS time.Time,
					opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
					captured <- req.URL.Path
				})
				pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous}
				client, done := ingressPipeOn(t, ctx, pm, strings.TrimPrefix(upstream.URL, "http://"), make(chan struct{}, 1), nil)
				br := bufio.NewReader(client)

				if side == "request" {
					head := fmt.Sprintf("POST /cut HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", 3*len(parts[0]))
					for i, part := range parts {
						if i == 0 {
							part = head + part
						}
						if _, err := client.Write([]byte(part)); err != nil {
							t.Fatal(err)
						}
						// The app has the part, so its capture has seen it:
						// the pause covers the second part only.
						wait(took, "the app did not get part "+fmt.Sprint(i+1))
						paused.Store(i == 0)
					}
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.ReadAll(resp.Body)
					resp.Body.Close()
				} else {
					if _, err := client.Write([]byte("GET /cut HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
						t.Fatal(err)
					}
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatal(err)
					}
					buf := make([]byte, len(parts[0]))
					for i := range parts {
						if i > 0 {
							select {
							case step <- struct{}{}:
							case <-ctx.Done():
								t.Fatal(ctx.Err())
							}
						}
						// The client has the part, so its capture has seen
						// it: the pause covers the second part only.
						if _, err := io.ReadFull(resp.Body, buf); err != nil {
							t.Fatalf("part %d: %v", i+1, err)
						}
						paused.Store(i == 0)
					}
					_, _ = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				wait(done, "the ingress loop did not return")
				pm.captures.Wait()
				if len(captured) != 0 {
					t.Fatalf("captured %s: a pause cut its %s body's capture", <-captured, side)
				}
				if got := mgr.NotRecordedUnderPressure(); got.Total != 1 || got.Connections() != 1 {
					t.Fatalf("not recorded under pressure: %+v, want the exchange whose %s body the pause cut", got, side)
				}
			})
		}
	}
}
