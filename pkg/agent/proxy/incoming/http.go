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

// captureTimestamp records the wall-clock time at which a chunk of data
// was captured at a given byte offset in the capture buffer.
type captureTimestamp struct {
	offset int
	ts     time.Time
}

// captureWriter is a non-blocking writer that buffers data for async test case
// capture. It silently stops capturing (without returning errors) when memory
// pressure is detected or the buffer exceeds maxSize, so it never blocks or
// slows the primary forwarding path.
//
// It also records a timestamp for each Write call so that parseCapturedHTTP
// can assign accurate request/response timestamps instead of using time.Now()
// at parse time (which runs after the connection closes).
type captureWriter struct {
	buf     bytes.Buffer
	stopped bool
	maxSize int
	times   []captureTimestamp
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.stopped {
		return len(p), nil
	}
	if isIngressRecordingPaused() || (w.maxSize > 0 && w.buf.Len()+len(p) > w.maxSize) {
		w.stopped = true
		// Replace with zero-value buffer to immediately release the underlying
		// allocated memory to the GC. bytes.Buffer.Reset() only resets the
		// length but keeps the capacity, so a 5MB buffer would still hold 5MB.
		w.buf = bytes.Buffer{}
		w.times = nil
		return len(p), nil
	}
	w.times = append(w.times, captureTimestamp{offset: w.buf.Len(), ts: time.Now()})
	w.buf.Write(p)
	return len(p), nil
}

// free releases the capture buffer's underlying memory to the GC.
// Must only be called after forwarding goroutines have finished (after done channels).
func (w *captureWriter) free() {
	w.buf = bytes.Buffer{}
	w.times = nil
}

// timestampAtOffset returns the approximate wall-clock time at which the byte
// at the given offset was written to the capture buffer. It searches the
// recorded timestamps in reverse to find the last chunk that starts at or
// before the requested offset.
func timestampAtOffset(times []captureTimestamp, offset int) time.Time {
	for i := len(times) - 1; i >= 0; i-- {
		if times[i].offset <= offset {
			return times[i].ts
		}
	}
	if len(times) > 0 {
		return times[0].ts
	}
	return time.Now()
}

// countingReader wraps an io.Reader and counts the total bytes read from it.
// Used together with bufio.Reader.Buffered() to compute the byte offset of
// the next unconsumed message in the original capture buffer.
type countingReader struct {
	r     io.Reader
	count int
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.count += n
	return n, err
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

	// forceCloseMode is true if we are running in sync or sampling mode.
	// In these modes, disable keep-alive and drop the loop after one request.
	forceCloseMode := pm.synchronous || pm.sampling

	if !forceCloseMode {
		// Normal mode: transparent TCP passthrough with async test case capture.
		// Raw bytes are forwarded between client and upstream with zero HTTP
		// parsing overhead. A copy is captured in side buffers and parsed
		// asynchronously after the connection closes to create test cases.
		// When memory pressure is detected, the side-copy stops but forwarding
		// continues unimpacted.
		releaseLock()
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

		// Request modifications for sync/sampling modes.
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				releaseLock()
				chunked = true
			} else if pm.synchronous && acquiredLock {
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

		// Determine whether capture is already known to be disabled for this exchange.
		// Skip tee/buffering to avoid overhead when capture will be skipped anyway.
		// Also skip capture when memory pressure is active.
		captureEligible := !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock) && !isIngressRecordingPaused()

		reqCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && req.Body != nil && req.Body != http.NoBody {
			req.Body = newTeeReadCloser(req.Body, reqCapture)
		}

		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream. Verify the upstream application is running and reachable at the resolved address.",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Bool("request_capture_truncated", reqCapture.Truncated()),
			)
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop.

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
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

		respTimestamp := time.Now()
		// Re-evaluate capture eligibility after response headers (chunked may have changed).
		// Also re-check memory pressure in case it started mid-exchange.
		captureEligible = !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock) && !isIngressRecordingPaused()
		respCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && resp.Body != nil && resp.Body != http.NoBody {
			resp.Body = newTeeReadCloser(resp.Body, respCapture)
		}

		if err := resp.Write(clientConn); err != nil {
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

		shouldCapture := true
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

		if reqCapture.Truncated() || respCapture.Truncated() {
			logger.Debug("Skipping HTTP capture because body exceeded capture budget while streaming",
				zap.Int("capture_budget_bytes", maxHTTPBodyCaptureBytes),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.String("url", req.URL.String()),
				zap.String("method", req.Method),
				zap.Int("status_code", resp.StatusCode),
				zap.String("response_content_type", resp.Header.Get("Content-Type")),
			)
			if forceCloseMode {
				return
			}
			continue
		}

		if !shouldCapture {
			if forceCloseMode {
				return
			}
			continue
		}

		exchangeCaptureSize, err := capturedExchangeSize(req, resp, reqCapture.Bytes(), respCapture.Bytes())
		if err != nil {
			logger.Error("Failed to estimate combined captured exchange size. This indicates an internal capture error; report it if it persists.",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int64("response_bytes_seen", respCapture.Total()),
			)
			if forceCloseMode {
				return
			}
			continue
		}
		if exchangeCaptureSize > maxHTTPCombinedCaptureBytes {
			logger.Debug("Skipping HTTP capture because combined request and response exceeded capture budget",
				zap.Int("capture_budget_bytes", maxHTTPCombinedCaptureBytes),
				zap.Int("captured_exchange_bytes", exchangeCaptureSize),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.String("url", req.URL.String()),
				zap.String("method", req.Method),
				zap.Int("status_code", resp.StatusCode),
			)
			if forceCloseMode {
				return
			}
			continue
		}

		// Capture parsing is best-effort: the exchange has already been proxied
		// successfully, so parse failures should not terminate the connection.
		reqData, err := dumpCapturedRequest(req, reqCapture.Bytes())
		if err != nil {
			logger.Error("Failed to dump captured request. This indicates an internal capture error; report it if it persists.",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int("captured_request_bytes", len(reqCapture.Bytes())),
			)
			if forceCloseMode {
				return
			}
			continue
		}

		parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
		if err != nil {
			logger.Error("Failed to parse captured request for testcase. Verify the client is sending valid HTTP if this persists.",
				zap.Error(err),
				zap.Int("captured_request_dump_bytes", len(reqData)),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
			)
			if forceCloseMode {
				return
			}
			continue
		}

		respData, err := dumpCapturedResponse(resp, parsedHTTPReq, respCapture.Bytes())
		if err != nil {
			logger.Error("Failed to dump captured response. This indicates an internal capture error; report it if it persists.",
				zap.Error(err),
				zap.Int("status_code", resp.StatusCode),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Int("captured_response_bytes", len(respCapture.Bytes())),
			)
			if forceCloseMode {
				return
			}
			continue
		}
		parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
		if err != nil {
			logger.Error("Failed to parse captured response for testcase. Verify the upstream application is returning valid HTTP if this persists.",
				zap.Error(err),
				zap.Int("captured_response_dump_bytes", len(respData)),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Int("status_code", resp.StatusCode),
			)
			if forceCloseMode {
				return
			}
			continue
		}

		go func() {
			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
		}()

		if forceCloseMode {
			return
		}
	}
}

// handleHttp1ZeroCopy handles HTTP/1.x connections in normal (non-sync,
// non-sampling) mode. It forwards raw TCP bytes bidirectionally between client
// and upstream with zero HTTP parsing overhead on the critical path. A copy of
// the bytes is captured in side buffers. After the connection closes, the
// captured data is parsed asynchronously to create test cases. When memory
// pressure is detected, the side-copy stops but forwarding continues unimpacted.
func (pm *IngressProxyManager) handleHttp1ZeroCopy(ctx context.Context, clientConn net.Conn, upConn net.Conn, logger *zap.Logger, t chan *models.TestCase, appPort uint16) {
	logger.Debug("Using zero-copy TCP passthrough with async capture")

	captureEnabled := !isIngressRecordingPaused()
	var reqCapture, respCapture *captureWriter
	if captureEnabled {
		reqCapture = &captureWriter{maxSize: hooksUtils.MaxTestCaseSize}
		respCapture = &captureWriter{maxSize: hooksUtils.MaxTestCaseSize}
	}

	done := make(chan struct{}, 2)

	// client → upstream (with optional side-copy for capture)
	go func() {
		var dst io.Writer = upConn
		if reqCapture != nil {
			dst = io.MultiWriter(upConn, reqCapture)
		}
		_, _ = io.Copy(dst, clientConn)
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// upstream → client (with optional side-copy for capture)
	go func() {
		var dst io.Writer = clientConn
		if respCapture != nil {
			dst = io.MultiWriter(clientConn, respCapture)
		}
		_, _ = io.Copy(dst, upConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done

	// Immediately free capture buffers to release memory back to GC.
	// This is critical: we must not hold onto multi-MB buffers any longer
	// than necessary, especially when memory may be near the container limit.
	// Copy the data out first (for async parsing), then free immediately.
	var reqBytes, respBytes []byte
	var reqTimes, respTimes []captureTimestamp
	if reqCapture != nil && respCapture != nil && !reqCapture.stopped && !respCapture.stopped {
		if reqCapture.buf.Len() > 0 && respCapture.buf.Len() > 0 {
			reqBytes = make([]byte, reqCapture.buf.Len())
			copy(reqBytes, reqCapture.buf.Bytes())
			respBytes = make([]byte, respCapture.buf.Len())
			copy(respBytes, respCapture.buf.Bytes())
			// Preserve timestamp slices before freeing — the background
			// goroutine references the same underlying arrays.
			reqTimes = reqCapture.times
			respTimes = respCapture.times
		}
	}
	// Free regardless of whether capture succeeded or was stopped
	if reqCapture != nil {
		reqCapture.free()
	}
	if respCapture != nil {
		respCapture.free()
	}

	if len(reqBytes) > 0 && len(respBytes) > 0 {
		go pm.parseCapturedHTTP(ctx, logger, reqBytes, respBytes, reqTimes, respTimes, t, appPort)
	}
}

// parseCapturedHTTP parses raw HTTP request/response bytes captured during
// zero-copy passthrough and creates test cases. It handles multiple
// request/response pairs from keep-alive connections. This runs in a background
// goroutine after the connection closes, with zero impact on the client-server
// communication.
//
// reqTimes/respTimes carry the wall-clock timestamps recorded by captureWriter
// during live forwarding, so test cases get accurate timestamps that align with
// the mocks recorded by the outgoing proxy. Without these, all test cases would
// get post-connection-close timestamps and the mock-to-test mapping would break.
func (pm *IngressProxyManager) parseCapturedHTTP(ctx context.Context, logger *zap.Logger, reqData, respData []byte, reqTimes, respTimes []captureTimestamp, t chan *models.TestCase, appPort uint16) {
	reqCounting := &countingReader{r: bytes.NewReader(reqData)}
	respCounting := &countingReader{r: bytes.NewReader(respData)}
	reqReader := bufio.NewReader(reqCounting)
	respReader := bufio.NewReader(respCounting)

	for {
		if isIngressRecordingPaused() {
			return
		}

		// Byte offset of the start of this request in the original capture
		// buffer = total bytes read from underlying reader minus bytes still
		// buffered by bufio.Reader (read-ahead that hasn't been consumed yet).
		reqOffset := reqCounting.count - reqReader.Buffered()
		reqTimestamp := timestampAtOffset(reqTimes, reqOffset)

		req, err := http.ReadRequest(reqReader)
		if err != nil {
			return
		}

		// Set Host header to match pkg.ParseHTTPRequest behavior
		req.Header.Set("Host", req.Host)

		// Read the full request body to advance the reader past it
		reqBody, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return
		}
		// Re-wrap body so CaptureHook can read it
		req.Body = io.NopCloser(bytes.NewReader(reqBody))

		resp, err := http.ReadResponse(respReader, req)
		if err != nil {
			return
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		// Byte offset right after this response — the timestamp at this
		// position approximates when the last byte of the response arrived.
		respEndOffset := respCounting.count - respReader.Buffered()
		respTimestamp := timestampAtOffset(respTimes, respEndOffset)
		if respTimestamp.Before(reqTimestamp) {
			respTimestamp = reqTimestamp
		}

		hooksUtils.CaptureHook(ctx, logger, t, req, resp, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, appPort)
	}
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
