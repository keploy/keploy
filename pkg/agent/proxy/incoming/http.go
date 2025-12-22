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

func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, opts models.IncomingOptions, synchronous bool, sem chan struct{}) {
	if synchronous {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}

	var releaseOnce sync.Once
	releaseLock := func() {
		if synchronous {
			releaseOnce.Do(func() {
				<-sem
				logger.Debug("Lock released early for concurrent streaming")
			})
		}
	}
	// Ensure lock is released eventually if we exit early or finish normally
	defer releaseLock()

	// 1. Dial Upstream
	upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream new app port", zap.String("New_App_Port", newAppAddr), zap.Error(err))
		return
	}
	// This closes the upstream connection when this function returns
	defer upConn.Close()

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	for {
		reqTimestamp := time.Now()

		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				logger.Warn("Failed to read client request", zap.Error(err))
			}
			return
		}
		var chunked bool = false

		// SYNCHRONOUS : Disable Keep-Alive for the Upstream
		if synchronous && (req.ContentLength == -1 || isChunked(req.TransferEncoding)) {
			logger.Debug("Detected chunked request. Releasing lock.")
			releaseLock()
			chunked = true
		} else if synchronous {

			mgr := syncMock.Get()
			if !mgr.FirstReqSeen {
				mgr.SetFirstRequestSignaled()
			}

			// we will close connection in case of keep alive (to allow multiple clients to make connections)
			// if we don't close a connection in synchronous mode, the next request from other client will be blocked
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
		if synchronous && (resp.ContentLength == -1 || isChunked(resp.TransferEncoding)) {
			logger.Debug("Detected chunked response. Releasing lock.")
			releaseLock()
			chunked = true
		} else if synchronous {
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

		if chunked && synchronous { // for chunked requests/responses, we will not capture test cases in case of synchronous mode
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
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts, synchronous)
		}()

		if synchronous {
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
