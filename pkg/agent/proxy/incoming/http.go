package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, opts models.IncomingOptions) {
	upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
	clientReader := bufio.NewReader(clientConn)
	if err != nil {
		logger.Warn("Failed to dial upstream new app port", zap.String("New_App_Port", newAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

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
			return // Exit the loop and close the connection.
		}
		defer req.Body.Close()
		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			return
		}
		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			return
		}

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}
		defer resp.Body.Close()
		respTimestamp := time.Now()

		respData, err := httputil.DumpResponse(resp, true)
		if err != nil {
			logger.Error("Failed to dump response for capturing", zap.Error(err))
			return
		}
		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			return
		}

		// Now we create New HTTPRequest and New HTTPResponse from the dumped data
		// Since we have already read the body in the write calls for forwarding traffic
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
			hooksUtils.Capture(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts)
		}()
	}
}
