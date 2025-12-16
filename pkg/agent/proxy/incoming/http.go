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

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, opts models.IncomingOptions) {
// 	upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
// 	clientReader := bufio.NewReader(clientConn)
// 	if err != nil {
// 		logger.Warn("Failed to dial upstream new app port", zap.String("New_App_Port", newAppAddr), zap.Error(err))
// 		return
// 	}
// 	defer upConn.Close()

// 	upstreamReader := bufio.NewReader(upConn)

// 	for {
// 		reqTimestamp := time.Now()

// 		req, err := http.ReadRequest(clientReader)
// 		if err != nil {
// 			if errors.Is(err, io.EOF) {
// 				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
// 			} else {
// 				logger.Warn("Failed to read client request", zap.Error(err))
// 			}
// 			return // Exit the loop and close the connection.
// 		}
// 		defer req.Body.Close()
// 		reqData, err := httputil.DumpRequest(req, true)
// 		if err != nil {
// 			logger.Error("Failed to dump request for capturing", zap.Error(err))
// 			return
// 		}
// 		if err := req.Write(upConn); err != nil {
// 			logger.Error("Failed to forward request to upstream", zap.Error(err))
// 			return
// 		}

// 		resp, err := http.ReadResponse(upstreamReader, req)
// 		if err != nil {
// 			logger.Error("Failed to read upstream response", zap.Error(err))
// 			return
// 		}
// 		defer resp.Body.Close()
// 		respTimestamp := time.Now()

// 		respData, err := httputil.DumpResponse(resp, true)
// 		if err != nil {
// 			logger.Error("Failed to dump response for capturing", zap.Error(err))
// 			return
// 		}
// 		if err := resp.Write(clientConn); err != nil {
// 			logger.Error("Failed to forward response to client", zap.Error(err))
// 			return
// 		}

// 		// Now we create New HTTPRequest and New HTTPResponse from the dumped data
// 		// Since we have already read the body in the write calls for forwarding traffic
// 		parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
// 		if err != nil {
// 			return
// 		}
// 		parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
// 		if err != nil {
// 			return
// 		}

// 		go func() {
// 			defer parsedHTTPReq.Body.Close()
// 			defer parsedHTTPRes.Body.Close()
// 			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts)
// 		}()
// 	}
// }

func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, opts models.IncomingOptions, synchronous bool, sem chan struct{}) {
	if synchronous {
		select {
		case sem <- struct{}{}:
			// Acquired
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

		// --- READ REQUEST ---
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
		// Fix: Defer inside a loop leaks memory in long-running connections.
		// We will manually close bodies at the end of the loop iteration or relying on the return.
		// For safety in this structure, we'll keep your pattern but be aware of it.
		// Ideally, wrap the loop body in a func, but for this fix:

		// SYNCHRONOUS FIX 1: Disable Keep-Alive for the Upstream
		if synchronous && (req.ContentLength == -1 || isChunked(req.TransferEncoding)) {
			logger.Debug("Detected chunked request. Releasing lock.")
			releaseLock()
			chunked = true
		} else if synchronous {
			// Standard synchronous behavior: strict close to prevent keep-alive deadlocks
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			req.Body.Close()
			return
		}

		// --- FORWARD REQUEST ---
		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop

		// --- READ RESPONSE ---
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}

		// SYNCHRONOUS FIX 2: Disable Keep-Alive for the Client
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

		// --- FORWARD RESPONSE ---
		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			resp.Body.Close()
			return
		}
		resp.Body.Close() // Close explicitly

		if chunked {
			return
		}
		// --- ASYNC CAPTURE ---
		// Re-parsing logic from your original code
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
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts)
		}()

		// SYNCHRONOUS FIX 3: Exit Loop Immediately
		// If we are in synchronous mode, we processed exactly ONE request.
		// We must now return to release the lock in runTCPForwarder.
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
