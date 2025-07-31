package provider

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	basePath := "keploy"
	existing, _ := yaml.ReadSessionIndices(ctx, basePath, logger) // ignore error if directory doesn't exist yet
	testSetID := pkg.NextID(existing, models.TestSetPattern)
	testSetPath := filepath.Join(basePath, testSetID)

	if err := os.MkdirAll(filepath.Join(testSetPath, "tests"), 0o777); err != nil {
		logger.Warn("failed to create tests directory", zap.Error(err))
	}

	logger.Info("[Keploy] Reverse proxy mode started", zap.Int("port", proxyPort), zap.String("forwardTo", forwardTo), zap.String("testSet", testSetID))

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
		go handleReverseProxyConn(ctx, logger, conn, forwardTo, testSetPath)
	}
}

func handleReverseProxyConn(ctx context.Context, logger *zap.Logger, clientConn net.Conn, backendAddr string, testSetPath string) {
	defer clientConn.Close()

	req, err := http.ReadRequest(bufio.NewReader(clientConn))
	if err != nil {
		logger.Error("Failed to read HTTP request", zap.Error(err))
		return
	}

	var reqBodyBytes []byte
	if req.Body != nil {
		reqBodyBytes, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
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

	recordHTTPMock(ctx, logger, req, reqBodyBytes, resp, respBodyBytes, testSetPath)
}

func recordHTTPMock(ctx context.Context, logger *zap.Logger, req *http.Request, reqBodyBytes []byte, resp *http.Response, respBodyBytes []byte, testSetPath string) {
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
				Timestamp:  time.Now(),
			},
			HTTPResp: &models.HTTPResp{
				StatusCode:    resp.StatusCode,
				Header:        pkg.ToYamlHTTPHeader(resp.Header),
				Body:          string(respBodyBytes),
				StatusMessage: resp.Status,
				Timestamp:     time.Now(),
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

	if err := yaml.WriteFile(ctx, logger, testSetPath, "mocks", mockYaml, true); err != nil {
		logger.Error("Failed to write mock file", zap.Error(err))
	} else {
		logger.Info("Mock recorded for frontend→backend call")
	}

	testsDir := filepath.Join(testSetPath, "tests")

	testFiles, _ := os.ReadDir(testsDir)
	testIndex := len(testFiles)
	testName := fmt.Sprintf("test-%d", testIndex)

	httpReq := models.HTTPReq{
		Method:     models.Method(req.Method),
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		URL:        req.URL.String(),
		Header:     pkg.ToYamlHTTPHeader(req.Header),
		Body:       string(reqBodyBytes),
		URLParams:  pkg.URLParams(req),
		Timestamp:  time.Now(),
	}

	httpResp := models.HTTPResp{
		StatusCode:    resp.StatusCode,
		Header:        pkg.ToYamlHTTPHeader(resp.Header),
		Body:          string(respBodyBytes),
		StatusMessage: resp.Status,
		Timestamp:     time.Now(),
	}

	type SpecFormat struct {
		Metadata   map[string]interface{} `yaml:"metadata"`
		Req        models.HTTPReq         `yaml:"req"`
		Resp       models.HTTPResp        `yaml:"resp"`
		Objects    []interface{}          `yaml:"objects"`
		Assertions map[string]interface{} `yaml:"assertions,omitempty"`
		Created    int64                  `yaml:"created"`
	}

	type TestCaseFormat struct {
		Version string     `yaml:"version"`
		Kind    string     `yaml:"kind"`
		Name    string     `yaml:"name"`
		Spec    SpecFormat `yaml:"spec"`
		Curl    string     `yaml:"curl,omitempty"`
	}

	tcFormat := TestCaseFormat{
		Version: string(models.GetVersion()),
		Kind:    string(models.HTTP),
		Name:    testName,
		Spec: SpecFormat{
			Metadata: make(map[string]interface{}),
			Req:      httpReq,
			Resp:     httpResp,
			Objects:  []interface{}{},
			Created:  time.Now().Unix(),
		},
		Curl: pkg.MakeCurlCommand(httpReq),
	}

	tcYaml, err := yamlLib.Marshal(tcFormat)
	if err != nil {
		logger.Error("Failed to marshal test case", zap.Error(err))
		return
	}

	if err := yaml.WriteFile(ctx, logger, testsDir, testName, tcYaml, false); err != nil {
		logger.Error("Failed to write testcase", zap.Error(err))
	} else {
		logger.Info("Testcase recorded", zap.String("name", testName))
	}
}
