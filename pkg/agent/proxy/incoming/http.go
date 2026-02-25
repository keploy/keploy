package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
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
	// This closes the upstream connection when this function returns
	defer upConn.Close()

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

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

			// we will close connection in case of keep alive (to allow multiple clients to make connections)
			// if we don't close a connection in pm.synchronous mode, the next request from other client will be blocked
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			req.Body.Close()
			return
		}

		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}

		// SYNCHRONOUS : Disable Keep-Alive for the Client
		if pm.synchronous && (resp.ContentLength == -1 || isChunked(resp.TransferEncoding)) {
			logger.Debug("Detected chunked response. Releasing lock.")
			releaseLock()
			chunked = true
		} else if pm.synchronous {
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		respTimestamp := time.Now()
		respData, err := httputil.DumpResponse(resp, true)
		if err != nil {
			logger.Error("Failed to dump response for capturing", zap.Error(err))
			resp.Body.Close()
			return
		}

		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			resp.Body.Close()
			return
		}
		resp.Body.Close() // Close explicitly

		if chunked && pm.synchronous { // for chunked requests/responses, we will not capture test cases in case of pm.synchronous mode
			return
		}

		parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
		if err != nil {
			return
		}
		parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
		if err != nil {
			return
		}

		go func() {
			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
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
