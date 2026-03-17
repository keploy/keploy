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

	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
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

	// Dial Upstream
	upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream app port", zap.String("Final_App_Port", finalAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	// forceCloseMode is true if we are running in Sync or Sampling mode.
	// In these modes, we strictly disable Keep-Alive and drop the loop after one request.
	forceCloseMode := pm.synchronous || pm.sampling

	for {
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				logger.Warn("Failed to read client request", zap.Error(err))
			}
			return
		}
		reqTimestamp := time.Now()
		var chunked bool = false

		// Request modifications for Sync/Sampling modes
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				logger.Debug("Detected chunked request. Releasing lock early.")
				releaseLock()
				chunked = true
			} else if pm.synchronous && acquiredLock {
				mgr := syncMock.Get()
				if !mgr.GetFirstReqSeen() {
					mgr.SetFirstRequestSignaled()
				}
			}

			// Requirement: "mark the connection as closed for keep alive"
			// This applies to ALL requests in these modes, even chunked or ignored (6th+) requests.
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			req.Body.Close()
			if err != nil {
				logger.Error("Failed to read request body. Check if the client connection is still active or verify the request body format", zap.Error(err))
				return
			}
		}
		req.Body.Close()

		// Read response from upstream's TeeForwardConn buffer.
		// The response has ALREADY been forwarded to the client at wire speed.
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response. Check if the upstream server is running or verify network connectivity to the upstream", zap.Error(err))
			return
		}

		// Response modifications for Sync/Sampling modes
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				logger.Debug("Detected chunked response. Releasing lock early.")
				releaseLock()
				chunked = true
			}

			// Close the connection on the response side as well
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		respTimestamp := time.Now()

		// Read response body from TeeForwardConn buffer.
		// Already forwarded to client — just reading for capture.
		var respBodyBytes []byte
		if resp.Body != nil {
			respBodyBytes, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			return
		}
		resp.Body.Close()

		// Capture Evaluation
		shouldCapture := true
		if forceCloseMode {
			if chunked {
				shouldCapture = false
			} else if pm.sampling && !acquiredLock {
				shouldCapture = false
			}
		}

		// Only parse and invoke the hook if it's eligible for capture
		if shouldCapture {
			parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
			if err == nil {
				parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
				if err == nil {
					go func() {
						defer parsedHTTPReq.Body.Close()
						defer parsedHTTPRes.Body.Close()
						hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
					}()
				}
			}
		}

		// Break the keep-alive loop and exit if we are in sync/sampling mode
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
