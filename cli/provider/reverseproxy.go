package provider

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func StartReverseProxy(ctx context.Context, proxyPort int, forwardTo string) error {
	logger, _ := zap.NewProduction()
	logger.Info("[Keploy] Reverse proxy mode started", zap.Int("port", proxyPort), zap.String("forwardTo", forwardTo))

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(proxyPort))
	if err != nil {
		logger.Error("Failed to start reverse proxy listener", zap.Error(err))
		return err
	}
	defer ln.Close()

	logger.Info("Reverse proxy listening", zap.Int("port", proxyPort), zap.String("forwardTo", forwardTo))
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleReverseProxyConn(ctx, logger, conn, forwardTo)
	}
}

func handleReverseProxyConn(ctx context.Context, logger *zap.Logger, clientConn net.Conn, backendAddr string) {
	defer clientConn.Close()

	req, err := http.ReadRequest(bufio.NewReader(clientConn))
	if err != nil {
		logger.Error("Failed to read HTTP request", zap.Error(err))
		return
	}

	req.Header.Del("If-None-Match")
	req.Header.Del("If-Modified-Since")

	backendConn, err := net.Dial("tcp", backendAddr)
	if err != nil {
		logger.Error("Failed to connect to backend", zap.Error(err))
		return
	}
	defer backendConn.Close()

	if err := req.Write(backendConn); err != nil {
		logger.Error("Failed to write request to backend", zap.Error(err))
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(backendConn), req)
	if err != nil {
		logger.Error("Failed to read response from backend", zap.Error(err))
		return
	}

	respBodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))

	if err := resp.Write(clientConn); err != nil {
		logger.Error("Failed to write response to client", zap.Error(err))
		return
	}

	recordHTTPMock(ctx, logger, req, resp, respBodyBytes)
}

func recordHTTPMock(ctx context.Context, logger *zap.Logger, req *http.Request, resp *http.Response, respBodyBytes []byte) {
	var reqBuf bytes.Buffer
	req.Write(&reqBuf)

	reqParsed, _ := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf.Bytes())))
	var reqBodyBytes []byte
	if reqParsed.Body != nil {
		reqBodyBytes, _ = io.ReadAll(reqParsed.Body)
	}

	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":      "Http",
				"type":      models.HTTPClient,
				"operation": req.Method,
			},
			HTTPReq: &models.HTTPReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHTTPHeader(req.Header),
				Body:       string(reqBodyBytes),
				URLParams:  pkg.URLParams(req),
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: resp.StatusCode,
				Header:     pkg.ToYamlHTTPHeader(resp.Header),
				Body:       string(respBodyBytes),
			},
			Created:          time.Now().Unix(),
			ReqTimestampMock: time.Now(),
			ResTimestampMock: time.Now(),
		},
	}

	mockYaml, err := yamlLib.Marshal(mock)
	if err != nil {
		logger.Error("Failed to marshal mock", zap.Error(err))
		return
	}

	if err := yaml.WriteFile(ctx, logger, "keploy/test-set-0", "mocks", mockYaml, true); err != nil {
		logger.Error("Failed to write mock file", zap.Error(err))
	} else {
		logger.Info("Mock recorded for frontend→backend call")
	}
}
