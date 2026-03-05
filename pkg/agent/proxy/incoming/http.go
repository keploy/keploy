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
	if pm.synchronous {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}

	var releaseOnce sync.Once
	releaseLock := func() {
		if pm.synchronous {
			releaseOnce.Do(func() {
				<-sem
				logger.Debug("Lock released early for concurrent streaming")
			})
		}
	}
	// Ensure lock is released eventually if we exit early or finish normally
	defer releaseLock()

	// Get the actual destination address (handles Windows vs others platform logic)
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

	// 1. Dial Upstream
	upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream app port", zap.String("Final_App_Port", finalAppAddr), zap.Error(err))
		return
	}
	// Disable Nagle's algorithm for low-latency forwarding
	if tc, ok := upConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	// This closes the upstream connection when this function returns
	defer upConn.Close()

	// Create bidirectional TeeForwardConns for zero-latency forwarding.
	// Forwarding goroutines start immediately — data flows at wire speed
	// between client↔upstream. The parser reads from internal buffers
	// containing already-forwarded data, adding zero latency to the request path.
	clientTee := orchestrator.NewTeeForwardConn(ctx, logger, clientConn, upConn)
	upstreamTee := orchestrator.NewTeeForwardConn(ctx, logger, upConn, clientConn)

	clientReader := bufio.NewReader(clientTee)
	upstreamReader := bufio.NewReader(upstreamTee)

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

		// SYNCHRONOUS : Disable Keep-Alive for the Upstream
		if pm.synchronous && (req.ContentLength == -1 || isChunked(req.TransferEncoding)) {
			logger.Debug("Detected chunked request. Releasing lock.")
			releaseLock()
			chunked = true

		} else if pm.synchronous {

			mgr := syncMock.Get()
			if !mgr.GetFirstReqSeen() {
				mgr.SetFirstRequestSignaled()
			}
			// Note: Connection headers are not modified because raw bytes
			// are already forwarded by TeeForwardConn at wire speed.
			// The connection will be closed when this function returns.
		}

		// Read request body from TeeForwardConn buffer.
		// The data has ALREADY been forwarded to upstream by the forwarding goroutine.
		// We're just reading the buffered copy for test case capture.
		var reqBodyBytes []byte
		if req.Body != nil {
			reqBodyBytes, err = io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				logger.Error("Failed to read request body. Check if the client connection is still active or verify the request body format", zap.Error(err))
				return
			}
		}

		// Read response from upstream's TeeForwardConn buffer.
		// The response has ALREADY been forwarded to the client at wire speed.
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response. Check if the upstream server is running or verify network connectivity to the upstream", zap.Error(err))
			return
		}

		// SYNCHRONOUS : Disable Keep-Alive for the Client
		if pm.synchronous && (resp.ContentLength == -1 || isChunked(resp.TransferEncoding)) {
			logger.Debug("Detected chunked response. Releasing lock.")
			releaseLock()
			chunked = true
		}

		respTimestamp := time.Now()

		// Read response body from TeeForwardConn buffer.
		// Already forwarded to client — just reading for capture.
		var respBodyBytes []byte
		if resp.Body != nil {
			respBodyBytes, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				logger.Error("Failed to read response body. Check if the response was truncated or verify the upstream server response format", zap.Error(err))
				return
			}
		}

		if chunked && pm.synchronous {
			// Capture test case before returning for chunked+synchronous
			req.Header.Set("Host", req.Host)
			req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
			resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
			go func() {
				defer req.Body.Close()
				defer resp.Body.Close()
				hooksUtils.CaptureHook(ctx, logger, t, req, resp, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
			}()
			return
		}

		// Async capture — data already forwarded, just constructing test case
		req.Header.Set("Host", req.Host)
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
		resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))

		go func() {
			defer req.Body.Close()
			defer resp.Body.Close()
			hooksUtils.CaptureHook(ctx, logger, t, req, resp, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
		}()

		if pm.synchronous {
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
