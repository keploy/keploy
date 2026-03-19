package proxy

import (
	"bufio"
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
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

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
			logger.Debug("Acquired 1 of 5 sampling slots for capture")
		case <-ctx.Done():
			return
		default:
			// Non-blocking bypass: The 5 slots are FULL.
			// We do not block. We set acquiredLock to false.
			// This connection will be proxied normally and marked closed, but WILL NOT be captured.
			acquiredLock = false
			logger.Debug("Sampling limit reached (5/5). Ignoring request for capture.")
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
				logger.Debug("Lock released")
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
	sessionStart := time.Now()
	requestSeq := 0
	logger.Debug("Starting HTTP/1 ingress proxy session",
		zap.String("resolved_app_addr", finalAppAddr),
		zap.String("fallback_app_addr", newAppAddr),
		zap.Uint16("actual_app_port", actualPort),
		zap.Bool("synchronous_mode", pm.synchronous),
	)
	defer func() {
		logger.Debug("HTTP/1 ingress proxy session finished",
			zap.Int("requests_seen", requestSeq),
			zap.Duration("session_duration", time.Since(sessionStart)),
		)
	}()

	// Dial Upstream
	upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream app port", zap.String("Final_App_Port", finalAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	// forceCloseMode is true if we are running in sync or sampling mode.
	// In these modes, disable keep-alive and drop the loop after one request.
	forceCloseMode := pm.synchronous || pm.sampling

	for {
		requestSeq++
		requestReadStart := time.Now()
		exchangeLogger := logger.With(zap.Int("ingress_request_seq", requestSeq))

		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				exchangeLogger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				exchangeLogger.Warn("Failed to read client request", zap.Error(err))
			}
			return
		}
		reqTimestamp := time.Now()
		reqStreaming, reqStreamingReasons := requestLooksStreaming(req)
		exchangeLogger.Debug("Ingress HTTP request received",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
			zap.String("host", req.Host),
			zap.String("proto", req.Proto),
			zap.Int64("content_length", req.ContentLength),
			zap.Strings("transfer_encoding", req.TransferEncoding),
			zap.String("content_type", req.Header.Get("Content-Type")),
			zap.String("accept", req.Header.Get("Accept")),
			zap.Bool("looks_streaming", reqStreaming),
			zap.Strings("streaming_reasons", reqStreamingReasons),
			zap.Duration("request_header_read_duration", reqTimestamp.Sub(requestReadStart)),
		)

		var chunked bool = false

		// Request modifications for sync/sampling modes.
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				exchangeLogger.Debug("Detected request body that may stream in force-close mode. Releasing lock.",
					zap.Bool("synchronous_mode", pm.synchronous),
					zap.Bool("sampling_mode", pm.sampling),
					zap.Bool("acquired_lock", acquiredLock),
					zap.Int64("content_length", req.ContentLength),
					zap.Strings("transfer_encoding", req.TransferEncoding),
				)
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

		reqCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if req.Body != nil && req.Body != http.NoBody {
			req.Body = newTeeReadCloser(req.Body, reqCapture)
		}

		requestForwardStart := time.Now()
		if err := req.Write(upConn); err != nil {
			exchangeLogger.Error("Failed to forward request to upstream",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Bool("request_capture_truncated", reqCapture.Truncated()),
			)
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop.
		exchangeLogger.Debug("Forwarded request upstream",
			zap.Duration("forward_request_duration", time.Since(requestForwardStart)),
			zap.Int64("request_bytes_seen", reqCapture.Total()),
			zap.Bool("request_capture_truncated", reqCapture.Truncated()),
		)

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			exchangeLogger.Error("Failed to read upstream response",
				zap.Error(err),
				zap.Duration("time_since_request_received", time.Since(reqTimestamp)),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
			)
			return
		}

		respHeaderReadAt := time.Now()
		respStreaming, respStreamingReasons := responseLooksStreaming(resp)
		exchangeLogger.Debug("Received upstream response headers",
			zap.Int("status_code", resp.StatusCode),
			zap.String("status", resp.Status),
			zap.String("proto", resp.Proto),
			zap.Int64("content_length", resp.ContentLength),
			zap.Strings("transfer_encoding", resp.TransferEncoding),
			zap.String("content_type", resp.Header.Get("Content-Type")),
			zap.String("x_accel_buffering", resp.Header.Get("X-Accel-Buffering")),
			zap.Bool("looks_streaming", respStreaming),
			zap.Strings("streaming_reasons", respStreamingReasons),
			zap.Duration("upstream_header_latency", respHeaderReadAt.Sub(reqTimestamp)),
		)

		// Response modifications for sync/sampling modes.
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				exchangeLogger.Debug("Detected response body that may stream in force-close mode. Releasing lock.",
					zap.Bool("synchronous_mode", pm.synchronous),
					zap.Bool("sampling_mode", pm.sampling),
					zap.Bool("acquired_lock", acquiredLock),
					zap.Int64("content_length", resp.ContentLength),
					zap.Strings("transfer_encoding", resp.TransferEncoding),
					zap.String("content_type", resp.Header.Get("Content-Type")),
				)
				releaseLock()
				chunked = true
			}

			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		respTimestamp := time.Now()
		respCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if resp.Body != nil && resp.Body != http.NoBody {
			resp.Body = newTeeReadCloser(resp.Body, respCapture)
		}

		responseForwardStart := time.Now()
		if err := resp.Write(clientConn); err != nil {
			exchangeLogger.Error("Failed to forward response to client",
				zap.Error(err),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Bool("response_capture_truncated", respCapture.Truncated()),
				zap.Duration("response_forward_duration", time.Since(responseForwardStart)),
				zap.Duration("exchange_duration", time.Since(reqTimestamp)),
			)
			resp.Body.Close()
			return
		}
		resp.Body.Close() // Close explicitly.
		exchangeLogger.Debug("Forwarded response to client",
			zap.Int("status_code", resp.StatusCode),
			zap.Duration("response_forward_duration", time.Since(responseForwardStart)),
			zap.Duration("exchange_duration", time.Since(reqTimestamp)),
			zap.Int64("response_bytes_seen", respCapture.Total()),
			zap.Bool("response_capture_truncated", respCapture.Truncated()),
		)

		shouldCapture := true
		if forceCloseMode {
			if chunked {
				shouldCapture = false
				exchangeLogger.Debug("Skipping testcase capture for force-close streaming exchange",
					zap.Bool("synchronous_mode", pm.synchronous),
					zap.Bool("sampling_mode", pm.sampling),
					zap.Int64("request_bytes_seen", reqCapture.Total()),
					zap.Int64("response_bytes_seen", respCapture.Total()),
				)
			} else if pm.sampling && !acquiredLock {
				shouldCapture = false
				exchangeLogger.Debug("Skipping testcase capture because no sampling slot was acquired",
					zap.Int64("request_bytes_seen", reqCapture.Total()),
					zap.Int64("response_bytes_seen", respCapture.Total()),
				)
			}
		}

		if reqCapture.Truncated() || respCapture.Truncated() {
			exchangeLogger.Debug("Skipping HTTP capture because body exceeded capture budget while streaming",
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

		reqData, err := dumpCapturedRequest(req, reqCapture.Bytes())
		if err != nil {
			exchangeLogger.Error("Failed to dump captured request",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int("captured_request_bytes", len(reqCapture.Bytes())),
			)
			return
		}

		parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
		if err != nil {
			exchangeLogger.Error("Failed to parse captured request for testcase",
				zap.Error(err),
				zap.Int("captured_request_dump_bytes", len(reqData)),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
			)
			return
		}

		respData, err := dumpCapturedResponse(resp, parsedHTTPReq, respCapture.Bytes())
		if err != nil {
			exchangeLogger.Error("Failed to dump captured response",
				zap.Error(err),
				zap.Int("status_code", resp.StatusCode),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Int("captured_response_bytes", len(respCapture.Bytes())),
			)
			return
		}
		parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
		if err != nil {
			exchangeLogger.Error("Failed to parse captured response for testcase",
				zap.Error(err),
				zap.Int("captured_response_dump_bytes", len(respData)),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Int("status_code", resp.StatusCode),
			)
			return
		}
		exchangeLogger.Debug("Dispatching captured HTTP testcase",
			zap.String("method", parsedHTTPReq.Method),
			zap.String("url", parsedHTTPReq.URL.String()),
			zap.Int("status_code", parsedHTTPRes.StatusCode),
			zap.Int64("request_bytes_seen", reqCapture.Total()),
			zap.Int64("response_bytes_seen", respCapture.Total()),
			zap.Duration("request_to_response_header_latency", respTimestamp.Sub(reqTimestamp)),
		)

		go func() {
			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.CaptureHook(ctx, exchangeLogger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
		}()

		if forceCloseMode {
			return
		}
	}
}

func requestLooksStreaming(req *http.Request) (bool, []string) {
	var reasons []string
	if req.ContentLength == -1 {
		reasons = append(reasons, "content-length=-1")
	}
	if isChunked(req.TransferEncoding) {
		reasons = append(reasons, "transfer-encoding=chunked")
	}
	contentType := strings.ToLower(req.Header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		reasons = append(reasons, "content-type=text/event-stream")
	case strings.Contains(contentType, "application/x-ndjson"), strings.Contains(contentType, "application/ndjson"):
		reasons = append(reasons, "content-type=ndjson")
	}
	return len(reasons) > 0, reasons
}

func responseLooksStreaming(resp *http.Response) (bool, []string) {
	var reasons []string
	if resp.ContentLength == -1 {
		reasons = append(reasons, "content-length=-1")
	}
	if isChunked(resp.TransferEncoding) {
		reasons = append(reasons, "transfer-encoding=chunked")
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		reasons = append(reasons, "content-type=text/event-stream")
	case strings.Contains(contentType, "application/x-ndjson"), strings.Contains(contentType, "application/ndjson"):
		reasons = append(reasons, "content-type=ndjson")
	case strings.Contains(contentType, "application/octet-stream") && resp.ContentLength == -1:
		reasons = append(reasons, "content-type=octet-stream-without-content-length")
	}
	return len(reasons) > 0, reasons
}

func isChunked(te []string) bool {
	for _, s := range te {
		if strings.EqualFold(s, "chunked") {
			return true
		}
	}
	return false
}
