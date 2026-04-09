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

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	// forceCloseMode is true if we are running in sync or sampling mode.
	// In these modes, disable keep-alive and drop the loop after one request.
	forceCloseMode := pm.synchronous || pm.sampling

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
		captureEligible := !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock)

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
		captureEligible = !(forceCloseMode && chunked) && (!pm.sampling || acquiredLock)
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

func isChunked(te []string) bool {
	for _, s := range te {
		if strings.EqualFold(s, "chunked") {
			return true
		}
	}
	return false
}
