package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var isIngressRecordingPaused = memoryguard.IsRecordingPaused

type httpCaptureState struct {
	mu        sync.Mutex
	maxBytes  int
	usedBytes int
	aborted   bool
}

func newHTTPCaptureState(maxBytes int) *httpCaptureState {
	return &httpCaptureState{maxBytes: maxBytes}
}

func (s *httpCaptureState) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.aborted {
		return false
	}
	if isIngressRecordingPaused() {
		s.aborted = true
		return false
	}
	if s.maxBytes > 0 && s.usedBytes+n > s.maxBytes {
		s.aborted = true
		return false
	}

	s.usedBytes += n
	return true
}

func (s *httpCaptureState) isAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

type httpBodyCaptureBuffer struct {
	state *httpCaptureState
	buf   bytes.Buffer
}

func (b *httpBodyCaptureBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.state == nil || !b.state.reserve(len(p)) {
		b.buf = bytes.Buffer{} // release backing array for GC
		return len(p), nil
	}

	if _, err := b.buf.Write(p); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (b *httpBodyCaptureBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *httpBodyCaptureBuffer) Reset() {
	b.buf.Reset()
}

// asyncPipeFeeder is a non-blocking writer used in the forwarding path.
// It copies incoming bytes to a buffered channel; a separate bridge goroutine
// drains the channel into an io.PipeWriter. This guarantees the forwarding
// goroutine (io.Copy) is never blocked by capture — even if the parser is slow
// or CaptureHook blocks on a full channel.
//
// Graceful degradation: when memory pressure is detected, the max capture size
// is exceeded, or the channel fills up, the feeder silently stops capturing.
// The forwarding path is never affected.
type asyncPipeFeeder struct {
	ch      chan []byte
	pw      *io.PipeWriter
	closed  atomic.Bool
	written atomic.Int64
	maxSize int64
	logger  *zap.Logger
}

// newAsyncPipeFeeder creates a feeder and returns the pipe reader end for
// the streaming parser. The bridge goroutine is started automatically.
func newAsyncPipeFeeder(maxSize int, logger *zap.Logger) (*asyncPipeFeeder, *io.PipeReader) {
	pr, pw := io.Pipe()
	f := &asyncPipeFeeder{
		// 512 slots ≈ 16MB at 32KB/chunk. Must be large enough that
		// the channel never overflows during normal operation — a single
		// 5MB response body needs ~160 slots, and brief pipeline stalls
		// (parser transitioning between request/response reads) can
		// cause data to accumulate. Overflowing kills capture for the
		// rest of the connection because the HTTP stream becomes
		// unrecoverable.
		ch:      make(chan []byte, 512),
		pw:      pw,
		maxSize: int64(maxSize),
		logger:  logger,
	}
	go f.bridge()
	return f, pr
}

// bridge drains the buffered channel into the pipe writer. It runs in its
// own goroutine so the forwarding goroutine never blocks on pipe writes.
// Exits when the channel is closed (by Close) or on pipe write error.
func (f *asyncPipeFeeder) bridge() {
	defer f.pw.Close()
	for chunk := range f.ch {
		if _, err := f.pw.Write(chunk); err != nil {
			// Pipe reader closed (parser done). Mark closed so Write()
			// stops enqueuing, then drain remaining channel items to
			// prevent the forwarding goroutine from blocking on send.
			f.closed.Store(true)
			for range f.ch {
			}
			return
		}
	}
}

// Write copies p and enqueues it for the bridge goroutine. It never blocks
// the caller. Called only from the forwarding goroutine (via io.MultiWriter).
func (f *asyncPipeFeeder) Write(p []byte) (int, error) {
	if f.closed.Load() {
		return len(p), nil
	}
	if isIngressRecordingPaused() {
		f.closed.Store(true)
		return len(p), nil
	}
	newTotal := f.written.Add(int64(len(p)))
	if f.maxSize > 0 && newTotal > f.maxSize {
		f.closed.Store(true)
		return len(p), nil
	}
	// Copy data — the original slice belongs to io.Copy's reusable buffer.
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case f.ch <- buf:
	default:
		// Channel full — parser can't keep up. Stop capture for this
		// connection. Remaining test cases on this connection will be lost.
		f.closed.Store(true)
		if f.logger != nil {
			f.logger.Debug("Capture channel full — dropping remaining test cases on this connection. "+
				"This may indicate responses too large for the channel buffer.",
				zap.Int64("total_bytes_written", f.written.Load()),
				zap.Int("chunk_size", len(p)),
				zap.Int("channel_capacity", cap(f.ch)),
			)
		}
	}
	return len(p), nil
}

// Close signals the bridge goroutine to exit. Must be called after the
// forwarding goroutine finishes (after io.Copy returns). Write() is
// never called after Close() — they run on the same goroutine — so
// there is no send-on-closed-channel race.
//
// Important: we do NOT close f.pw here. The bridge goroutine must first
// drain all remaining channel items into the pipe so the parser sees
// every byte. The bridge's deferred f.pw.Close() runs after the drain
// completes, giving the parser a clean EOF only after all data is written.
func (f *asyncPipeFeeder) Close() {
	f.closed.Store(true)
	close(f.ch)
}

func serializeCapturedRequest(req *http.Request, body []byte) ([]byte, error) {
	clone := new(http.Request)
	*clone = *req
	clone.Header = req.Header.Clone()
	clone.Trailer = req.Trailer.Clone()
	clone.GetBody = nil
	if len(body) == 0 {
		clone.Body = http.NoBody
	} else {
		clone.Body = io.NopCloser(bytes.NewReader(body))
	}

	var buf bytes.Buffer
	if err := clone.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func serializeCapturedResponse(resp *http.Response, body []byte) ([]byte, error) {
	clone := new(http.Response)
	*clone = *resp
	clone.Header = resp.Header.Clone()
	clone.Trailer = resp.Trailer.Clone()
	if len(body) == 0 {
		clone.Body = http.NoBody
	} else {
		clone.Body = io.NopCloser(bytes.NewReader(body))
	}

	var buf bytes.Buffer
	if err := clone.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (pm *IngressProxyManager) handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	var acquiredLock bool

	// 1. Non-Blocking Concurrency Control
	if pm.synchronous {
		// Sync mode (Concurrency 1): Blocks until the slot is free
		select {
		case sem <- struct{}{}:
			acquiredLock = true
		case <-ctx.Done():
			return
		}
	} else if pm.sampling && pm.samplingSem != nil {
		// Sampling mode (Concurrency 5): Non-blocking lock attempt
		select {
		case pm.samplingSem <- struct{}{}:
			acquiredLock = true
		case <-ctx.Done():
			return
		default:
			// Non-blocking bypass: The 5 slots are FULL.
			// We do not block. We set acquiredLock to false.
			// This connection will be proxied normally and marked closed, but WILL NOT be captured.
			acquiredLock = false
		}
	}

	var releaseOnce sync.Once
	releaseLock := func() {
		if acquiredLock {
			releaseOnce.Do(func() {
				if pm.synchronous {
					<-sem
				} else if pm.sampling && pm.samplingSem != nil {
					<-pm.samplingSem
				}
			})
		}
	}
	// Ensure lock is released eventually if we exit early or finish normally
	defer releaseLock()

	// Get the actual destination address
	finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, logger)

	// Determine the correct port for the test case:
	// On Windows, getActualDestination resolves the real destination dynamically,
	// so we extract the port from the resolved address.
	// On non-Windows (Linux/Docker), getActualDestination returns the fallback (newAppAddr)
	// which contains the eBPF-redirected port, NOT the original app port.
	// In that case, we use the passed-in appPort which carries the correct OrigAppPort.
	actualPort := appPort
	if finalAppAddr != newAppAddr {
		// Destination was dynamically resolved (Windows) — extract port from resolved address
		actualPort = extractPortFromAddr(finalAppAddr, appPort)
	}

	// Dial Upstream
	upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
	if err != nil {
		logger.Error("Failed to connect to upstream application. Verify the application is listening on the resolved address.",
			zap.String("final_app_addr", finalAppAddr),
			zap.Error(err),
		)
		return
	}
	defer upConn.Close()

	// forceCloseMode: only sync mode needs the traditional HTTP parsing loop
	// (strict one-at-a-time ordering with forced close). Sampling mode now
	// uses the zero-copy path for both tracked and bypass connections.
	forceCloseMode := pm.synchronous

	if !forceCloseMode {
		// Sampling bypass: no lock, no capture — raw TCP passthrough.
		if pm.sampling && !acquiredLock {
			forwardRawTCP(ctx, clientConn, upConn)
			return
		}
		// Normal mode OR sampling-tracked: zero-copy TCP passthrough with
		// streaming capture. Forwarding runs at wire speed via io.Copy;
		// capture is fully decoupled via non-blocking asyncPipeFeeders.
		// For sampling-tracked, the semaphore stays held (via defer
		// releaseLock) until the connection closes, limiting concurrent
		// captures to the configured slot count.
		if !pm.sampling {
			releaseLock() // Normal mode has no sampling lock to hold
		}
		pm.handleHttp1ZeroCopy(ctx, clientConn, upConn, logger, t, actualPort)
		return
	}

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	for {
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Debug("Failed to read client request; ignoring this connection. Verify the client is sending valid HTTP if this persists.", zap.Error(err))
			}
			return
		}
		reqTimestamp := time.Now()

		var chunked bool = false
		// pressureCloseMode unifies forceCloseMode with memory pressure.
		// When true, expected close errors are handled gracefully (DEBUG level),
		// the sampling lock is released early, and the loop exits after one exchange.
		pressureCloseMode := forceCloseMode || isIngressRecordingPaused()
		captureEnabled := !isIngressRecordingPaused()

		// Request modifications for sync/sampling modes.
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				releaseLock()
				chunked = true
				captureEnabled = false
			} else if captureEnabled && pm.synchronous && acquiredLock {
				mgr := syncMock.Get()
				if !mgr.GetFirstReqSeen() {
					mgr.SetFirstRequestSignaled()
				}
			}

			// Mark the connection closed for keep-alive in these modes even if
			// we are only proxying and not capturing.
			req.Close = true
			req.Header.Set("Connection", "close")
		}
		// Note: the pressureCloseMode && !forceCloseMode branch was removed
		// because forceCloseMode is always true here (the !forceCloseMode
		// path returns early via handleHttp1ZeroCopy/forwardRawTCP).

		// Determine whether capture is eligible for this exchange.
		// Skip tee/buffering to avoid overhead when capture will be skipped anyway.
		captureEligible := captureEnabled && !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock)

		// Re-check memory pressure right before attaching capture buffers.
		if captureEligible && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEligible = false
			captureEnabled = false
			releaseLock()
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		reqCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && req.Body != nil && req.Body != http.NoBody {
			req.Body = newTeeReadCloser(req.Body, reqCapture)
		}

		if err := req.Write(upConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress request write ended during close-under-pressure path", zap.Error(err))
				req.Body.Close()
				return
			}
			logger.Error("Failed to forward request to upstream. Verify the upstream application is running and reachable at the resolved address.",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Bool("request_capture_truncated", reqCapture.Truncated()),
			)
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop.

		// Re-check memory pressure after forwarding the request.
		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
		}

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress upstream closed while finishing close-under-pressure path", zap.Error(err))
				return
			}
			logger.Error("Failed to read upstream response. Check upstream application health and network connectivity.",
				zap.Error(err),
				zap.Duration("time_since_request_received", time.Since(reqTimestamp)),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
			)
			return
		}

		// Response modifications for sync/sampling modes.
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				releaseLock()
				chunked = true
			}

			resp.Close = true
			resp.Header.Set("Connection", "close")
		}
		if pressureCloseMode && !forceCloseMode {
			resp.Close = true
			resp.Header.Set("Connection", "close")
			captureEnabled = false
		}

		respTimestamp := time.Now()

		// Re-evaluate capture eligibility after response headers (chunked may have changed).
		// Also re-check memory pressure in case it started mid-exchange.
		captureEligible = captureEnabled && !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock)
		if captureEligible && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEligible = false
			captureEnabled = false
			releaseLock()
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		respCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && resp.Body != nil && resp.Body != http.NoBody {
			resp.Body = newTeeReadCloser(resp.Body, respCapture)
		}

		if err := resp.Write(clientConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress client connection closed while finishing close-under-pressure path", zap.Error(err))
				resp.Body.Close()
				return
			}
			logger.Error("Failed to forward response to client. The client may have closed the connection before the response was fully written.",
				zap.Error(err),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Bool("response_capture_truncated", respCapture.Truncated()),
				zap.Duration("exchange_duration", time.Since(reqTimestamp)),
			)
			resp.Body.Close()
			return
		}
		resp.Body.Close() // Close explicitly.

		// Final memory pressure check before capture evaluation.
		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
		}

		shouldCapture := captureEnabled
		if forceCloseMode {
			if chunked {
				shouldCapture = false
				logger.Debug("Skipping testcase capture for streaming exchange",
					zap.Bool("synchronous_mode", pm.synchronous),
					zap.Bool("sampling_mode", pm.sampling),
					zap.Int64("request_bytes_seen", reqCapture.Total()),
					zap.Int64("response_bytes_seen", respCapture.Total()),
				)
			} else if pm.sampling && !acquiredLock {
				shouldCapture = false
			}
		}
		// Skip capture if memory pressure kicked in during the exchange
		if shouldCapture && isIngressRecordingPaused() {
			shouldCapture = false
		}

		if shouldCapture && (reqCapture.Truncated() || respCapture.Truncated()) {
			logger.Debug("Skipping HTTP capture because body exceeded capture budget while streaming",
				zap.Int("capture_budget_bytes", maxHTTPBodyCaptureBytes),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.String("url", req.URL.String()),
				zap.String("method", req.Method),
				zap.Int("status_code", resp.StatusCode),
				zap.String("response_content_type", resp.Header.Get("Content-Type")),
			)
			shouldCapture = false
		}

		if !shouldCapture {
			if forceCloseMode || pressureCloseMode {
				return
			}
			continue
		}

		// Move ALL capture parsing to a background goroutine so the
		// sampling semaphore is released immediately (via defer releaseLock
		// when this function returns). For large payloads the dump+parse
		// cycle allocates 8MB+ of intermediate copies; doing that while
		// holding the semaphore starves other connections of capture slots
		// and drives up p95 latency under load.
		//
		// Snapshot the capture buffer bytes — the goroutine will own them.
		reqBodyBytes := reqCapture.Bytes()
		respBodyBytes := respCapture.Bytes()
		reqBytesTotal := reqCapture.Total()
		respBytesTotal := respCapture.Total()

		go func() {
			exchangeCaptureSize, err := capturedExchangeSize(req, resp, reqBodyBytes, respBodyBytes)
			if err != nil {
				logger.Error("Failed to estimate combined captured exchange size. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int64("response_bytes_seen", respBytesTotal),
				)
				return
			}
			if exchangeCaptureSize > maxHTTPCombinedCaptureBytes {
				logger.Debug("Skipping HTTP capture because combined request and response exceeded capture budget",
					zap.Int("capture_budget_bytes", maxHTTPCombinedCaptureBytes),
					zap.Int("captured_exchange_bytes", exchangeCaptureSize),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.String("url", req.URL.String()),
					zap.String("method", req.Method),
					zap.Int("status_code", resp.StatusCode),
				)
				return
			}

			// Capture parsing is best-effort: the exchange has already been
			// proxied successfully, so parse failures just skip the test case.
			reqData, err := dumpCapturedRequest(req, reqBodyBytes)
			if err != nil {
				logger.Error("Failed to dump captured request. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int("captured_request_bytes", len(reqBodyBytes)),
				)
				return
			}

			parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
			if err != nil {
				logger.Error("Failed to parse captured request for testcase. Verify the client is sending valid HTTP if this persists.",
					zap.Error(err),
					zap.Int("captured_request_dump_bytes", len(reqData)),
					zap.Int64("request_bytes_seen", reqBytesTotal),
				)
				return
			}

			respData, err := dumpCapturedResponse(resp, parsedHTTPReq, respBodyBytes)
			if err != nil {
				logger.Error("Failed to dump captured response. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int("status_code", resp.StatusCode),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.Int("captured_response_bytes", len(respBodyBytes)),
				)
				return
			}
			parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
			if err != nil {
				logger.Error("Failed to parse captured response for testcase. Verify the upstream application is returning valid HTTP if this persists.",
					zap.Error(err),
					zap.Int("captured_response_dump_bytes", len(respData)),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.Int("status_code", resp.StatusCode),
				)
				return
			}

			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
		}()

		// Exit the loop in sync/sampling mode or when memory pressure requires closing.
		if pressureCloseMode {
			return
		}
	}
}

// handleHttp1ZeroCopy handles HTTP/1.x connections in normal (non-sync,
// non-sampling) mode. It forwards raw TCP bytes bidirectionally between client
// and upstream with zero HTTP parsing overhead on the critical path.
//
// Capture is fully decoupled from forwarding: byte streams are piped through
// non-blocking asyncPipeFeeders to a streaming parser (parseStreamingHTTP)
// that emits test cases as soon as each HTTP exchange completes — without
// waiting for the connection to close. When memory pressure is detected or
// the capture size limit is reached, the feeders silently stop capturing
// while forwarding continues unimpacted.
func (pm *IngressProxyManager) handleHttp1ZeroCopy(ctx context.Context, clientConn net.Conn, upConn net.Conn, logger *zap.Logger, t chan *models.TestCase, appPort uint16) {
	logger.Debug("Using zero-copy TCP passthrough with streaming capture")

	captureEnabled := !isIngressRecordingPaused()
	var reqFeeder, respFeeder *asyncPipeFeeder
	if captureEnabled {
		var reqPipeR, respPipeR *io.PipeReader
		// maxSize=0 disables the per-feeder total-bytes limit. With
		// streaming capture the parser consumes data incrementally, so
		// in-flight memory is bounded by the channel buffer plus one
		// request/response body in the parser. The per-test-case size
		// limit is enforced downstream in CaptureHook.
		reqFeeder, reqPipeR = newAsyncPipeFeeder(0, logger)
		respFeeder, respPipeR = newAsyncPipeFeeder(0, logger)
		go pm.parseStreamingHTTP(ctx, logger, reqPipeR, respPipeR, t, appPort)
	}

	// Close connections on context cancellation (shutdown) to unblock
	// the io.Copy goroutines. Without this, the function hangs until
	// the remote side (e.g., upstream app's keep-alive) closes naturally.
	// Same pattern used by the gRPC handler.
	go func() {
		<-ctx.Done()
		clientConn.Close()
		upConn.Close()
	}()

	done := make(chan struct{}, 2)

	// client → upstream (with optional non-blocking side-copy for capture)
	go func() {
		var dst io.Writer = upConn
		if reqFeeder != nil {
			dst = io.MultiWriter(upConn, reqFeeder)
		}
		_, _ = io.Copy(dst, clientConn)
		if reqFeeder != nil {
			reqFeeder.Close()
		}
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// upstream → client (with optional non-blocking side-copy for capture)
	go func() {
		var dst io.Writer = clientConn
		if respFeeder != nil {
			dst = io.MultiWriter(clientConn, respFeeder)
		}
		_, _ = io.Copy(dst, upConn)
		if respFeeder != nil {
			respFeeder.Close()
		}
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	// No post-hoc parsing needed — test cases were emitted incrementally
	// by parseStreamingHTTP as each HTTP exchange completed.
}

// parseStreamingHTTP reads from request and response pipes concurrently with
// live forwarding, emitting test cases as soon as each HTTP exchange completes.
// This avoids waiting for the connection to close before capturing test cases.
//
// http.ReadRequest / http.ReadResponse act as natural delimiters — HTTP/1.1 is
// self-framing (headers end with \r\n\r\n, body length from Content-Length or
// chunked encoding), so no custom delimiter detection is needed.
//
// Timestamps are taken at parse time which closely tracks when bytes actually
// flowed through the forwarding path, since the pipe feeds data as fast as the
// network delivers it.
func (pm *IngressProxyManager) parseStreamingHTTP(ctx context.Context, logger *zap.Logger,
	reqR *io.PipeReader, respR *io.PipeReader, t chan *models.TestCase, appPort uint16) {
	defer reqR.Close()
	defer respR.Close()

	reqReader := bufio.NewReader(reqR)
	respReader := bufio.NewReader(respR)

	for {
		if isIngressRecordingPaused() {
			return
		}

		// Timestamp taken just before reading — approximates when the
		// first byte of this request arrived from the client.
		reqTimestamp := time.Now()

		req, err := http.ReadRequest(reqReader)
		if err != nil {
			return
		}

		// Set Host header to match pkg.ParseHTTPRequest behavior
		req.Header.Set("Host", req.Host)

		// Read request body with a size cap to avoid unbounded allocations.
		// We must consume the full body to advance the pipe reader past it
		// (HTTP framing), but only keep up to MaxTestCaseSize in memory.
		reqBody, err := io.ReadAll(io.LimitReader(req.Body, int64(hooksUtils.MaxTestCaseSize)+1))
		// Drain any remainder to keep the stream aligned.
		_, _ = io.Copy(io.Discard, req.Body)
		req.Body.Close()
		if err != nil {
			return
		}
		if len(reqBody) > hooksUtils.MaxTestCaseSize {
			// Body exceeds capture budget — skip this exchange.
			// Read and discard the response to keep the stream aligned.
			resp, rerr := http.ReadResponse(respReader, req)
			if rerr != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))

		resp, err := http.ReadResponse(respReader, req)
		if err != nil {
			return
		}

		// Same bounded read for the response body.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(hooksUtils.MaxTestCaseSize)+1))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}
		if len(respBody) > hooksUtils.MaxTestCaseSize {
			continue // Skip oversized response
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		// Timestamp taken right after reading the full response body —
		// approximates when the last byte of the response was forwarded.
		respTimestamp := time.Now()
		if respTimestamp.Before(reqTimestamp) {
			respTimestamp = reqTimestamp
		}

		// Emit in a goroutine so the parser loop is never blocked by
		// CaptureHook (e.g. if the test case channel is temporarily full).
		go hooksUtils.CaptureHook(ctx, logger, t, req, resp, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, appPort)
	}
}

// forwardRawTCP does bidirectional TCP forwarding between two connections
// with zero HTTP parsing overhead. Used for sampling-bypass connections
// where capture is not needed — bytes flow straight through via io.Copy.
// Keep-alive is preserved (no Connection: close injected), so the client
// can reuse the connection for multiple requests.
func forwardRawTCP(ctx context.Context, clientConn, upConn net.Conn) {
	// Close connections on context cancellation (shutdown).
	go func() {
		<-ctx.Done()
		clientConn.Close()
		upConn.Close()
	}()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upConn, clientConn)
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func isChunked(te []string) bool {
	for _, s := range te {
		if strings.EqualFold(s, "chunked") {
			return true
		}
	}
	return false
}

func isIngressExpectedCloseErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "unexpected EOF") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "wsarecv") ||
		strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host")
}
