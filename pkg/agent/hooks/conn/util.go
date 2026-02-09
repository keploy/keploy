package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sync/atomic"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var GlobalTestCounter int64

type CaptureFunc func(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions, synchronous bool, appPort uint16)

var CaptureHook CaptureFunc = Capture

// MaxTestCaseSize is the maximum combined size of HTTP/gRPC request and response (5MB)
const MaxTestCaseSize = 5 * 1024 * 1024 // 5 MB

func Capture(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions, synchronous bool, appPort uint16) {
	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			logger.Warn("failed to read the http request body", zap.Any("metadata", utils.GetReqMeta(req)), zap.Int64("of size", int64(len(reqBody))), zap.String("body", base64.StdEncoding.EncodeToString(reqBody)), zap.Error(err))
		}

		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.Decompress(logger, req.Header.Get("Content-Encoding"), reqBody)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the http request body", zap.Any("metadata", utils.GetReqMeta(req)))
				return
			}
		}
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the http response body")
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http response body")
		return
	}

	if IsFiltered(logger, req, opts) {
		logger.Debug("The request is a filtered request")
		return
	}
	var formData []models.FormData
	if contentType := req.Header.Get("Content-Type"); strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, ";")
		if len(parts) > 1 {
			req.Header.Set("Content-Type", strings.TrimSpace(parts[0]))
		}
		formData = ExtractFormData(logger, reqBody, contentType)
		reqBody = []byte{}
	} else if contentType := req.Header.Get("Content-Type"); contentType == "application/x-www-form-urlencoded" {
		decodedBody, err := url.QueryUnescape(string(reqBody))
		if err != nil {
			utils.LogError(logger, err, "failed to decode the url-encoded request body")
			return
		}
		reqBody = []byte(decodedBody)
	}

	if resp.Header.Get("Content-Encoding") != "" {
		respBody, err = pkg.Decompress(logger, resp.Header.Get("Content-Encoding"), respBody)
		if err != nil {
			utils.LogError(logger, err, "failed to decompress the response body")
			return
		}
	}

	// Check if combined request and response size exceeds 5MB limit (after decompression)
	totalSize := len(reqBody) + len(respBody)
	if totalSize > MaxTestCaseSize {
		logger.Error("HTTP test case data exceeds 5MB limit, skipping capture",
			zap.Int("totalSize", totalSize),
			zap.Int("reqBodySize", len(reqBody)),
			zap.Int("respBodySize", len(respBody)),
			zap.String("url", req.URL.String()),
			zap.String("method", req.Method))
		return
	}

	testCase := &models.TestCase{
		Version: models.GetVersion(),
		Name:    pkg.ToYamlHTTPHeader(req.Header)["Keploy-Test-Name"],
		Kind:    models.HTTP,
		Created: time.Now().Unix(),
		HTTPReq: models.HTTPReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			URL:        fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			Form:       formData,
			Header:     pkg.ToYamlHTTPHeader(req.Header),
			Body:       string(reqBody),
			URLParams:  pkg.URLParams(req),
			Timestamp:  reqTimeTest,
		},
		HTTPResp: models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHTTPHeader(resp.Header),
			Body:          string(respBody),
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise:   map[string][]string{},
		AppPort: appPort,
		// Mocks: mocks,
	}

	if synchronous {
		currentID := atomic.AddInt64(&GlobalTestCounter, 1)
		testName := fmt.Sprintf("test-%d", currentID)
		testCase.Name = testName
		if mgr := syncMock.Get(); mgr != nil { // dumping the test case from mock manager in synchronous mode
			mgr.ResolveRange(reqTimeTest, resTimeTest, testCase.Name, true)
		}
	}
	select {
	case <-ctx.Done():
		return
	case t <- testCase:
		// Successfully sent test case
	}
}
func IsFiltered(logger *zap.Logger, req *http.Request, opts models.IncomingOptions) bool {
	dstPort := 0
	var err error
	if p := req.URL.Port(); p != "" {
		dstPort, err = strconv.Atoi(p)
		if err != nil {
			utils.LogError(logger, err, "failed to obtain destination port from request")
			return false
		}
	}

	var passThrough bool

	type cond struct {
		eligible bool
		match    bool
	}

	for _, filter := range opts.Filters {

		//  1. bypass rule
		bypassEligible := !(filter.BypassRule.Host == "" &&
			filter.BypassRule.Path == "" &&
			filter.BypassRule.Port == 0)

		opts := models.OutgoingOptions{Rules: []models.BypassRule{filter.BypassRule}}
		byPassMatch := utils.IsPassThrough(logger, req, uint(dstPort), opts)

		//  2. URL-method rule
		urlMethodEligible := len(filter.URLMethods) > 0
		urlMethodMatch := false
		if urlMethodEligible {
			for _, m := range filter.URLMethods {
				if m == req.Method {
					urlMethodMatch = true
					break
				}
			}
		}

		//  3. header rule
		headerEligible := len(filter.Headers) > 0
		headerMatch := false
		if headerEligible {
			for key, vals := range filter.Headers {
				rx, err := regexp.Compile(vals)
				if err != nil {
					utils.LogError(logger, err, "bad header regex")
					continue
				}
				for _, v := range req.Header.Values(key) {
					if rx.MatchString(v) {
						headerMatch = true
						break
					}
				}
				if headerMatch {
					break
				}
			}
		}

		conds := []cond{
			{bypassEligible, byPassMatch},
			{urlMethodEligible, urlMethodMatch},
			{headerEligible, headerMatch},
		}

		switch filter.MatchType {
		case models.AND:
			pass := true
			seen := false
			for _, c := range conds {
				if !c.eligible {
					continue
				} // ignore ineligible ones
				seen = true
				if !c.match {
					pass = false
					break
				}
			}
			if seen && pass {
				passThrough = true
				return passThrough
			}

		case models.OR:
			fallthrough
		default:
			for _, c := range conds {
				if c.eligible && c.match {
					passThrough = true
					return passThrough
				}
			}
		}
	}

	return passThrough
}

func ExtractFormData(logger *zap.Logger, body []byte, contentType string) []models.FormData {
	boundary := ""
	if strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, "boundary=")
		if len(parts) > 1 {
			boundary = strings.TrimSpace(parts[1])
		} else {
			utils.LogError(logger, nil, "Invalid multipart/form-data content type")
			return nil
		}
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var formData []models.FormData

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			utils.LogError(logger, err, "Error reading part")
			continue
		}
		key := part.FormName()
		if key == "" {
			continue
		}

		value, err := io.ReadAll(part)
		if err != nil {
			utils.LogError(logger, err, "Error reading part value")
			continue
		}

		formData = append(formData, models.FormData{
			Key:    key,
			Values: []string{string(value)},
		})
	}

	return formData
}

// CaptureGRPC captures a gRPC request/response pair and sends it to the test case channel
func CaptureGRPC(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, http2Stream *pkg.HTTP2Stream, appPort uint16) {
	if http2Stream == nil {
		logger.Error("Stream is nil")
		return
	}

	if http2Stream.GRPCReq == nil || http2Stream.GRPCResp == nil {
		logger.Error("gRPC request or response is nil")
		return
	}

	// Create test case from stream data
	testCase := &models.TestCase{
		Version:  models.GetVersion(),
		Name:     http2Stream.GRPCReq.Headers.OrdinaryHeaders["Keploy-Test-Name"],
		Kind:     models.GRPC_EXPORT,
		Created:  time.Now().Unix(),
		GrpcReq:  *http2Stream.GRPCReq,
		GrpcResp: *http2Stream.GRPCResp,
		Noise:    map[string][]string{},
		AppPort:  appPort,
	}

	select {
	case <-ctx.Done():
		return
	case t <- testCase:
		logger.Debug("Captured gRPC test case",
			zap.String("path", http2Stream.GRPCReq.Headers.PseudoHeaders[":path"]))
	}
}
