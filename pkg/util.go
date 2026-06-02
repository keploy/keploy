// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

var SortCounter int64 = -1
var templateValuesMu sync.RWMutex

const maxStreamTokenSize = 10 * 1024 * 1024

// HTTPStreamMode represents the type of HTTP streaming response.
type HTTPStreamMode string

const (
	HTTPStreamModeNone      HTTPStreamMode = "none"
	HTTPStreamModeSSE       HTTPStreamMode = "sse"
	HTTPStreamModeNDJSON    HTTPStreamMode = "ndjson"
	HTTPStreamModeMultipart HTTPStreamMode = "multipart"
	HTTPStreamModePlainText HTTPStreamMode = "plain-text"
	HTTPStreamModeBinary    HTTPStreamMode = "binary"
)

// HTTPStreamConfig holds configuration for HTTP streaming responses.
type HTTPStreamConfig struct {
	Mode     HTTPStreamMode
	Boundary string
}

// StreamMismatchInfo holds details about a streaming frame mismatch for reporting.
type StreamMismatchInfo struct {
	FrameIndex    int    // Index of the mismatched frame (-1 if no specific frame)
	ExpectedFrame string // The expected frame content
	ActualFrame   string // The actual frame content received
	Reason        string // Description of why frames don't match
}

// StreamingHTTPResponse holds the response metadata and a reader for streaming responses.
// The caller is responsible for reading from the Reader and closing it when done.
type StreamingHTTPResponse struct {
	StatusCode    int
	StatusMessage string
	Header        map[string]string
	Reader        io.ReadCloser
	StreamConfig  HTTPStreamConfig
}

func InitSortCounter(counter int64) {
	atomic.StoreInt64(&SortCounter, counter)
}

func GetNextSortNum() int64 {
	return atomic.AddInt64(&SortCounter, 1)
}

func cloneStringAnyMap(src map[string]interface{}) map[string]interface{} {
	if len(src) == 0 {
		return map[string]interface{}{}
	}

	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func snapshotTemplateState() (map[string]interface{}, map[string]interface{}) {
	templateValuesMu.RLock()
	defer templateValuesMu.RUnlock()

	return cloneStringAnyMap(utils.TemplatizedValues), cloneStringAnyMap(utils.SecretValues)
}

func buildTemplateDataSnapshot() map[string]interface{} {
	templatedValues, secretValues := snapshotTemplateState()
	if len(templatedValues) == 0 && len(secretValues) == 0 {
		return map[string]interface{}{}
	}

	templateData := make(map[string]interface{}, len(templatedValues)+1)
	for k, v := range templatedValues {
		templateData[k] = v
	}
	if len(secretValues) > 0 {
		templateData["secret"] = secretValues
	}

	return templateData
}

func UpdateSortCounterIfHigher(val int64) {
	for {
		curr := atomic.LoadInt64(&SortCounter)
		if val <= curr {
			return
		}
		if atomic.CompareAndSwapInt64(&SortCounter, curr, val) {
			return
		}
	}
}

// URLParams returns the Url and Query parameters from the request url.
func URLParams(r *http.Request) map[string]string {
	qp := r.URL.Query()
	result := make(map[string]string)

	for key, values := range qp {
		result[key] = strings.Join(values, ", ")
	}

	return result
}

// ToYamlHTTPHeader converts the http header into yaml format
func ToYamlHTTPHeader(httpHeader http.Header) map[string]string {
	header := map[string]string{}
	for i, j := range httpHeader {
		header[i] = strings.Join(j, ",")
	}
	return header
}

// CompareMultiValueHeaders compares a mock header value (as a comma-separated string)
// with an input header value (as a slice of strings). It normalizes whitespace,
// splits the mock header value by commas, trims spaces, sorts both sets of values,
// and returns true if they contain the same elements in any order.
func CompareMultiValueHeaders(mockHeaderValue string, inputHeaderValue []string) bool {
	// early returns
	if mockHeaderValue == "" && len(inputHeaderValue) == 0 {
		return true
	}

	if mockHeaderValue == "" || len(inputHeaderValue) == 0 {
		return false
	}

	mockValues := strings.Split(mockHeaderValue, ",")
	normalizedMockValues := make([]string, len(mockValues))
	for i, v := range mockValues {
		normalizedMockValues[i] = strings.TrimSpace(v)
	}

	// Normalize input header values
	normalizedInputValues := make([]string, len(inputHeaderValue))
	for i, v := range inputHeaderValue {
		normalizedInputValues[i] = strings.TrimSpace(v)
	}

	// Sort both slices for comparison
	sort.Strings(normalizedMockValues)
	sort.Strings(normalizedInputValues)

	// Compare lengths first
	if len(normalizedMockValues) != len(normalizedInputValues) {
		return false
	}

	// Compare each value
	for i, mockVal := range normalizedMockValues {
		if mockVal != normalizedInputValues[i] {
			return false
		}
	}

	return true
}

func ToHTTPHeader(mockHeader map[string]string) http.Header {
	header := http.Header{}
	for i, j := range mockHeader {
		// Emit one wire header per YAML key. The recorded value is kept
		// intact even when it contains commas — this is the canonical
		// comma-folded form for RFC 7230 list-valued headers (Accept,
		// Accept-Encoding, Baggage, Forwarded, Via, ...) and is what
		// most HTTP clients (axios, requests, fetch) actually emit on
		// the wire. Splitting on "," and sending one wire header per
		// element fans out into multiple same-name headers; that breaks
		// strict-mapping server middlewares such as
		// `Response(headers=dict(**request.headers), ...)` which raise
		// `TypeError: dict() got multiple values for keyword argument
		// 'accept'` on duplicate field names — a known FastAPI /
		// Starlette failure mode reproducible during replay even when
		// the original client sent exactly one Accept header.
		//
		// Trade-off: a recording whose on-wire request actually used
		// repeated same-name headers (rare on the request side; common
		// only for Set-Cookie which is a response header and never
		// built by this helper) replays as a single comma-folded
		// header. Per RFC 7230 §3.2.2 the receiver-side parse is
		// semantically equivalent for list-valued headers, so this
		// folding is wire-safe for the headers that actually matter.
		header[i] = []string{j}
	}
	return header
}

// looksLikeTime is a fast-path byte-prefix check that rejects strings which
// cannot possibly match any of the date/time formats we try below. Avoiding
// the time.Parse loop on the 99%+ of strings that obviously aren't times
// saves an enormous amount of allocations (each failed time.Parse allocates
// a *time.parseError + a copied error message).
//
// We accept a string as a candidate iff its first non-whitespace byte is:
//   - a digit (most ISO/RFC formats start with year or day)
//   - one of Mon/Tue/Wed/Thu/Fri/Sat/Sun (RFC1123, RFC850 weekday prefixes)
//   - one of Jan/Feb/Mar/Apr/May/Jun/Jul/Aug/Sep/Oct/Nov/Dec (ANSIC,
//     UnixDate, RubyDate, time.Stamp* prefixes)
//   - one of '0'..'9' for the time.Kitchen "3:04PM" format (digit handled above)
//
// Anything else (random hex blobs, JSON keys, "true"/"false"/"null",
// payload values) returns false immediately, skipping 20 failed time.Parse
// calls — each of which would allocate ~120 bytes.
func looksLikeTime(s string) bool {
	// strip leading whitespace cheaply
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return false
	}
	c := s[i]
	if c >= '0' && c <= '9' {
		return true
	}
	// Check 3-letter prefix against weekday/month abbreviations.
	if len(s)-i < 3 {
		return false
	}
	switch s[i : i+3] {
	case "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun",
		"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
		return true
	}
	return false
}

// IsTime verifies whether a given string represents a valid date or not.
//
// Hot-path optimisation: a fast-path looksLikeTime check rules out 99%+ of
// non-time strings before the 20-format time.Parse loop. Without this, every
// non-time string allocates ~20 *time.parseError objects (~2.4 KB) — when
// called millions of times during a load test, that's GBs of GC pressure.
func IsTime(stringDate string) bool {
	date := strings.TrimSpace(stringDate)
	if date == "" {
		return false
	}
	if secondsFloat, err := strconv.ParseFloat(date, 64); err == nil {
		seconds := int64(secondsFloat / 1e9)
		nanoseconds := int64(secondsFloat) % 1e9
		expectedTime := time.Unix(seconds, nanoseconds)
		currentTime := time.Now()
		if currentTime.Sub(expectedTime) < 24*time.Hour && currentTime.Sub(expectedTime) > -24*time.Hour {
			return true
		}
	}
	// Cheap byte-prefix gate to avoid the 20-format time.Parse loop on
	// obvious non-time inputs.
	if !looksLikeTime(date) {
		return false
	}
	for _, dateFormat := range dateFormats {
		_, err := time.Parse(dateFormat, date)
		if err == nil {
			return true
		}
	}
	return false
}

type SimulationConfig struct {
	APITimeout      uint64
	RequestTimeout  time.Duration
	ConfigPort      uint32
	KeployPath      string
	ConfigHost      string
	URLReplacements map[string]string
	PortMappings    map[uint32]uint32
	// TLSConfig, when non-nil, is applied to the http.Transport used by
	// the replay client. Lets callers pin a specific cert (e.g. for
	// short-lived replay pods serving a self-signed cert) without
	// relaxing TLS verification globally. Default nil preserves the
	// stdlib system-pool behaviour.
	TLSConfig *tls.Config
}

// preparedHTTPRequest holds the prepared HTTP request and client for execution.
type preparedHTTPRequest struct {
	Request *http.Request
	Client  *http.Client
}

// prepareHTTPRequest handles all common request preparation logic shared between
// SimulateHTTP and SimulateHTTPStreaming: URL decoding, template rendering,
// body loading, multipart construction, compression, and client creation.
func prepareHTTPRequest(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*preparedHTTPRequest, error) {
	// case in which URL string has encoded template placeholders
	if strings.Contains(tc.HTTPReq.URL, "%7B") {
		decoded, err := url.QueryUnescape(tc.HTTPReq.URL)
		if err == nil {
			tc.HTTPReq.URL = decoded
		}
	}

	// TODO: adjust this logic in the render function in order to remove the redundant code.
	// Convert testcase to string and render template values before simulation.
	templateData := buildTemplateDataSnapshot()
	if len(templateData) > 0 {
		testCaseBytes, err := json.Marshal(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the testcase for templating")
			return nil, err
		}

		// Render only real Keploy placeholders ({{ .x }}, {{ string .y }}, etc.),
		// ignoring LaTeX/HTML like {{\pi}}.
		renderedStr, rerr := utils.RenderTemplatesInString(logger, string(testCaseBytes), templateData)
		if rerr != nil {
			logger.Debug("template rendering had recoverable errors", zap.Error(rerr))
		}

		err = json.Unmarshal([]byte(renderedStr), &tc)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal the rendered testcase")
			return nil, err
		}
	}

	reqBody := []byte(tc.HTTPReq.Body)
	var err error

	// If the request body was offloaded to an asset file (>1MB), load it back
	if tc.HTTPReq.BodyRef.Path != "" {
		bodyRefPath := tc.HTTPReq.BodyRef.Path
		// Resolve relative paths against keployPath so assets work even if
		// the keploy directory has been moved since recording.
		if cfg.KeployPath != "" && !filepath.IsAbs(bodyRefPath) {
			bodyRefPath = filepath.Join(cfg.KeployPath, bodyRefPath)
		}
		bodyData, readErr := os.ReadFile(bodyRefPath)
		if readErr != nil {
			utils.LogError(logger, readErr, "failed to read request body from asset file", zap.String("path", bodyRefPath))
			return nil, readErr
		}
		reqBody = bodyData
		logger.Debug("loaded request body from asset file",
			zap.String("path", bodyRefPath),
			zap.Int("size", len(bodyData)))
	}

	// If form field values were offloaded to asset files (>1MB) and they were not actual files (json,html,xml,txt etc...), load them back
	for i, form := range tc.HTTPReq.Form {
		if len(form.FileNames) == 0 && len(form.Paths) > 0 && len(form.Values) > 0 {
			for j, value := range form.Values {
				if value == "" && j < len(form.Paths) && form.Paths[j] != "" {
					formPath := form.Paths[j]
					if cfg.KeployPath != "" && !filepath.IsAbs(formPath) {
						formPath = filepath.Join(cfg.KeployPath, formPath)
					}
					valData, readErr := os.ReadFile(formPath)
					if readErr != nil {
						utils.LogError(logger, readErr, "failed to read form value from asset file",
							zap.String("path", formPath),
							zap.String("key", form.Key))
						return nil, readErr
					}
					tc.HTTPReq.Form[i].Values[j] = string(valData)
					logger.Debug("loaded form value from asset file",
						zap.String("key", form.Key),
						zap.String("path", formPath),
						zap.Int("size", len(valData)))
				}
			}
			// Clear Paths after restoring values so the multipart builder
			// doesn't treat these asset paths as file uploads.
			tc.HTTPReq.Form[i].Paths = nil
		}
	}

	// Build multipart body if needed
	contentType := tc.HTTPReq.Header["Content-Type"]
	if strings.HasPrefix(contentType, "multipart/form-data") && len(tc.HTTPReq.Form) > 0 {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		logger.Debug("building multipart body",
			zap.Int("form_fields", len(tc.HTTPReq.Form)),
			zap.String("content_type", contentType),
		)
		for _, field := range tc.HTTPReq.Form {
			logger.Debug("multipart field",
				zap.String("key", field.Key),
				zap.Int("values", len(field.Values)),
				zap.Int("paths", len(field.Paths)),
			)
			for _, path := range field.Paths {
				// Resolve relative paths against keployPath
				resolvedPath := path
				if cfg.KeployPath != "" && !filepath.IsAbs(path) {
					resolvedPath = filepath.Join(cfg.KeployPath, path)
				}
				logger.Debug("multipart file path", zap.String("path", resolvedPath), zap.String("field", field.Key))
				file, ferr := os.Open(resolvedPath)
				if ferr != nil {
					utils.LogError(logger, ferr, "failed to open multipart file", zap.String("path", resolvedPath))
					return nil, ferr
				}
				part, perr := writer.CreateFormFile(field.Key, filepath.Base(resolvedPath))
				if perr != nil {
					file.Close()
					utils.LogError(logger, perr, "failed to create multipart file part", zap.String("field", field.Key))
					return nil, perr
				}
				if _, cerr := io.Copy(part, file); cerr != nil {
					file.Close()
					utils.LogError(logger, cerr, "failed to write multipart file part", zap.String("field", field.Key))
					return nil, cerr
				}
				if cerr := file.Close(); cerr != nil {
					utils.LogError(logger, cerr, "failed to close multipart file", zap.String("path", path))
					return nil, cerr
				}
			}
			for valueIndex, value := range field.Values {
				logger.Debug("multipart field value",
					zap.String("field", field.Key),
					zap.Int("value_len", len(value)),
					zap.Bool("looks_binary", looksBinary(value)),
				)
				isFileValue := false
				fileName := ""
				if len(field.Paths) == 0 && len(field.FileNames) > 0 {
					isFileValue = true
				} else if len(field.Paths) == 0 && len(field.FileNames) == 0 && looksBinary(value) {
					isFileValue = true
				}

				if isFileValue {
					if len(field.FileNames) > 0 && valueIndex < len(field.FileNames) {
						fileName = field.FileNames[valueIndex]
					}
					if fileName == "" {
						fileName = "upload.bin"
					}
					fileName = filepath.Base(fileName)
					if fileName == "." || fileName == string(filepath.Separator) || fileName == "" {
						fileName = "upload.bin"
					}
					part, perr := writer.CreateFormFile(field.Key, fileName)
					if perr != nil {
						utils.LogError(logger, perr, "failed to create multipart file part", zap.String("field", field.Key))
						return nil, perr
					}
					if _, werr := part.Write([]byte(value)); werr != nil {
						utils.LogError(logger, werr, "failed to write multipart file content", zap.String("field", field.Key))
						return nil, werr
					}
					continue
				}
				if werr := writer.WriteField(field.Key, value); werr != nil {
					utils.LogError(logger, werr, "failed to write multipart field", zap.String("field", field.Key))
					return nil, werr
				}
			}
		}
		if cerr := writer.Close(); cerr != nil {
			utils.LogError(logger, cerr, "failed to close multipart writer")
			return nil, cerr
		}
		logger.Debug("multipart body built", zap.Int("body_len", body.Len()), zap.String("content_type", writer.FormDataContentType()))
		reqBody = body.Bytes()
		tc.HTTPReq.Header["Content-Type"] = writer.FormDataContentType()
		delete(tc.HTTPReq.Header, "Content-Length")
	}

	if tc.HTTPReq.Header["Content-Encoding"] != "" {
		reqBody, err = Compress(logger, tc.HTTPReq.Header["Content-Encoding"], reqBody)
		if err != nil {
			utils.LogError(logger, err, "failed to compress the request body")
			return nil, err
		}
	}

	// Resolve the execution target using ResolveTestTarget's precedence:
	// app_port < config port < replaceWith URL replacements
	// (explicit replacement port short-circuits lower-priority port overrides)
	// < replaceWith port mappings.
	testURL, err := ResolveTestTarget(tc.HTTPReq.URL, cfg.URLReplacements, cfg.PortMappings, cfg.ConfigHost, tc.AppPort, cfg.ConfigPort, true, logger)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), testURL, bytes.NewBuffer(reqBody))
	if err != nil {
		utils.LogError(logger, err, "failed to create a http request from the yaml document")
		return nil, err
	}
	req.Header = ToHTTPHeader(tc.HTTPReq.Header)
	req.ProtoMajor = tc.HTTPReq.ProtoMajor
	req.ProtoMinor = tc.HTTPReq.ProtoMinor
	req.Header.Set("KEPLOY-TEST-ID", tc.Name)
	req.Header.Set("KEPLOY-TEST-SET-ID", testSet)
	// send if its the last testcase
	if tc.IsLast {
		req.Header.Set("KEPLOY-LAST-TESTCASE", "true")
	}

	// override host header if present in the request
	hostHeader := tc.HTTPReq.Header["Host"]
	if hostHeader != "" {
		logger.Debug("overriding host header", zap.String("host", hostHeader))
		req.Host = hostHeader
	}

	// Creating the client and disabling redirects
	_, hasAcceptEncoding := req.Header["Accept-Encoding"]
	disableCompression := !hasAcceptEncoding

	var client *http.Client
	keepAlive, ok := req.Header["Connection"]
	if ok && strings.EqualFold(keepAlive[0], "keep-alive") {
		logger.Debug("simulating request with conn:keep-alive")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	} else if ok && strings.EqualFold(keepAlive[0], "close") {
		logger.Debug("simulating request with conn:close")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  true,
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	} else {
		logger.Debug("simulating request with conn:keep-alive (maxIdleConn=1)")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  false,
				MaxIdleConns:       1,
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	}

	return &preparedHTTPRequest{
		Request: req,
		Client:  client,
	}, nil
}

func SimulateHTTP(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*models.HTTPResp, error) {
	templatedResponse := tc.HTTPResp // keep a copy of the original templatized response

	logger.Info("starting test for", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))

	// Prepare the HTTP request using the shared helper
	cfg.RequestTimeout = time.Second * time.Duration(cfg.APITimeout)
	prepared, err := prepareHTTPRequest(ctx, tc, testSet, logger, cfg)
	if err != nil {
		return nil, err
	}

	logger.Debug(fmt.Sprintf("Sending request to user app:%v", prepared.Request))

	// Execute the request
	httpResp, errHTTPReq := prepared.Client.Do(prepared.Request)
	if errHTTPReq != nil {
		utils.LogError(logger, errHTTPReq, "failed to send testcase request to app")
		return nil, errHTTPReq
	}

	defer func() {
		if httpResp != nil && httpResp.Body != nil {
			if err := httpResp.Body.Close(); err != nil {
				utils.LogError(logger, err, "failed to close response body")
			}
		}
	}()

	// Read full response body
	respBody, errReadRespBody := io.ReadAll(httpResp.Body)
	if errReadRespBody != nil {
		utils.LogError(logger, errReadRespBody, "failed reading response body")
		return nil, errReadRespBody
	}

	// Decompress if needed
	if httpResp.Header.Get("Content-Encoding") != "" {
		respBody, err = Decompress(logger, httpResp.Header.Get("Content-Encoding"), respBody)
		if err != nil {
			utils.LogError(logger, err, "failed to decode response body")
			return nil, err
		}
	}

	statusMessage := http.StatusText(httpResp.StatusCode)

	resp := &models.HTTPResp{
		StatusCode:    httpResp.StatusCode,
		StatusMessage: statusMessage,
		Body:          string(respBody),
		Header:        ToYamlHTTPHeader(httpResp.Header),
	}

	// Centralized template update: walk the response body and headers to refresh template values.
	// NOTE: header-only updates are important for e.g. listmonk where the login 302 has an empty
	// body but carries a fresh Set-Cookie: session=... that every subsequent test consumes.
	if len(respBody) > 0 || len(resp.Header) > 0 {
		templateValuesMu.Lock()
		defer templateValuesMu.Unlock()
		prevTemplatedValues := cloneStringAnyMap(utils.TemplatizedValues)
		if len(prevTemplatedValues) > 0 {
			logger.Debug("Received response from user app", zap.Any("response", resp))

			updated := updateTemplateValuesFromHTTPResp(logger, templatedResponse, *resp, utils.TemplatizedValues, prevTemplatedValues)
			if updated {
				logger.Debug("Updated template values", zap.Any("templatized_values", utils.TemplatizedValues))
			}
		}
	}
	return resp, errHTTPReq
}

// SimulateHTTPStreaming sends an HTTP request and returns the streaming response with a reader.
// The caller is responsible for reading from the response Reader and closing it when done.
// Use CompareHTTPStream to compare the streaming response with expected values.
func SimulateHTTPStreaming(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*StreamingHTTPResponse, error) {
	logger.Info("starting streaming test for", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))

	// Calculate streaming timeout
	APITimeout := ComputeStreamingTimeoutSeconds(tc, cfg.APITimeout)
	cfg.RequestTimeout = time.Second * time.Duration(APITimeout)

	// Prepare the HTTP request using the shared helper
	prepared, err := prepareHTTPRequest(ctx, tc, testSet, logger, cfg)
	if err != nil {
		return nil, err
	}

	logger.Debug(fmt.Sprintf("Sending streaming request to user app:%v", prepared.Request))

	// Execute the request
	httpResp, errHTTPReq := prepared.Client.Do(prepared.Request)
	if errHTTPReq != nil {
		utils.LogError(logger, errHTTPReq, "failed to send testcase request to app")
		return nil, errHTTPReq
	}

	statusMessage := http.StatusText(httpResp.StatusCode)
	streamCfg := DetectHTTPStreamConfig(tc, httpResp)

	logger.Debug("detected HTTP streaming response",
		zap.String("testcase", tc.Name),
		zap.String("content_type", httpResp.Header.Get("Content-Type")),
		zap.String("stream_mode", string(streamCfg.Mode)))

	// Wrap the response body with decompression if needed
	streamReader := io.ReadCloser(httpResp.Body)
	contentEncoding := strings.ToLower(strings.TrimSpace(httpResp.Header.Get("Content-Encoding")))
	switch contentEncoding {
	case "gzip":
		gzipReader, gzErr := gzip.NewReader(httpResp.Body)
		if gzErr != nil {
			httpResp.Body.Close()
			utils.LogError(logger, gzErr, "failed to create gzip reader for streaming response")
			return nil, gzErr
		}
		streamReader = &gzipReadCloser{gzipReader: gzipReader, underlying: httpResp.Body}
	case "br":
		streamReader = &brotliReadCloser{reader: brotli.NewReader(httpResp.Body), underlying: httpResp.Body}
	case "":
		// no-op, use httpResp.Body directly
	default:
		logger.Debug("unsupported content-encoding for stream; returning raw response body",
			zap.String("content_encoding", contentEncoding))
	}

	return &StreamingHTTPResponse{
		StatusCode:    httpResp.StatusCode,
		StatusMessage: statusMessage,
		Header:        ToYamlHTTPHeader(httpResp.Header),
		Reader:        streamReader,
		StreamConfig:  streamCfg,
	}, nil
}

// gzipReadCloser wraps a gzip reader and its underlying body to close both.
type gzipReadCloser struct {
	gzipReader *gzip.Reader
	underlying io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	return g.gzipReader.Read(p)
}

func (g *gzipReadCloser) Close() error {
	gzErr := g.gzipReader.Close()
	underErr := g.underlying.Close()
	if gzErr != nil {
		return gzErr
	}
	return underErr
}

// brotliReadCloser wraps a brotli reader and its underlying body.
type brotliReadCloser struct {
	reader     io.Reader
	underlying io.ReadCloser
}

func (b *brotliReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *brotliReadCloser) Close() error {
	return b.underlying.Close()
}

// DetectHTTPStreamConfig detects the streaming mode of an HTTP response based on content type and other factors.
func DetectHTTPStreamConfig(tc *models.TestCase, resp *http.Response) HTTPStreamConfig {
	contentType := ""
	if resp != nil {
		contentType = resp.Header.Get("Content-Type")
	}
	if contentType == "" && tc != nil {
		contentType = getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type")
	}

	mediaType := ""
	params := map[string]string{}
	if contentType != "" {
		parsedType, parsedParams, err := mime.ParseMediaType(contentType)
		if err == nil {
			mediaType = strings.ToLower(strings.TrimSpace(parsedType))
			params = parsedParams
		} else {
			mediaType = strings.ToLower(strings.TrimSpace(contentType))
		}
	}

	switch mediaType {
	case "text/event-stream":
		if isSSETestCase(tc, resp) {
			return HTTPStreamConfig{Mode: HTTPStreamModeSSE}
		}
		return HTTPStreamConfig{Mode: HTTPStreamModePlainText}
	case "application/x-ndjson", "application/ndjson":
		return HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}
	case "multipart/x-mixed-replace", "multipart/mixed":
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" && tc != nil {
			boundary = boundaryFromContentTypeHeader(getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type"))
		}
		if boundary != "" {
			return HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: boundary}
		}
	case "text/plain":
		if tc != nil && len(tc.HTTPResp.StreamBody) > 0 {
			return HTTPStreamConfig{Mode: HTTPStreamModePlainText}
		}
		if isLikelyStreamingHTTPResponse(tc, resp) {
			return HTTPStreamConfig{Mode: HTTPStreamModePlainText}
		}
	case "application/octet-stream":
		if tc != nil && len(tc.HTTPResp.StreamBody) > 0 {
			return HTTPStreamConfig{Mode: HTTPStreamModeBinary}
		}
		if isLikelyStreamingHTTPResponse(tc, resp) {
			return HTTPStreamConfig{Mode: HTTPStreamModeBinary}
		}
	}

	return HTTPStreamConfig{Mode: HTTPStreamModeNone}
}

// IsHTTPStreamingTestCase returns true if the testcase response is identified as a stream format
// supported by replay-time incremental validators.
func IsHTTPStreamingTestCase(tc *models.TestCase) bool {
	if tc == nil {
		return false
	}
	return DetectHTTPStreamConfig(tc, nil).Mode != HTTPStreamModeNone
}

// CompareHTTPStream compares an expected HTTP response with a streaming response.
// It returns whether they match, the captured body content, mismatch details (if any), and any error.
func CompareHTTPStream(expectedResp models.HTTPResp, stream io.Reader, cfg HTTPStreamConfig, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	switch cfg.Mode {
	case HTTPStreamModeSSE:
		return compareSSEStream(expectedResp, stream, jsonNoiseKeys, logger)
	case HTTPStreamModeNDJSON:
		return compareNDJSONStream(expectedResp, stream, jsonNoiseKeys, logger)
	case HTTPStreamModeMultipart:
		return compareMultipartStream(expectedResp, stream, cfg.Boundary, jsonNoiseKeys, logger)
	case HTTPStreamModePlainText:
		return comparePlainTextStream(expectedResp, stream, logger)
	case HTTPStreamModeBinary:
		return compareBinaryStream(expectedResp, stream, logger)
	default:
		return false, "", nil, fmt.Errorf("unsupported HTTP stream mode: %s", cfg.Mode)
	}
}

func ComputeStreamingTimeoutSeconds(tc *models.TestCase, defaultSeconds uint64) uint64 {
	baseTimeout := defaultSeconds
	if baseTimeout == 0 {
		baseTimeout = 10
	}

	if tc == nil {
		return baseTimeout
	}

	reqTs := tc.HTTPReq.Timestamp
	respTs := tc.HTTPResp.Timestamp
	if reqTs.IsZero() || respTs.IsZero() {
		return baseTimeout
	}

	diff := respTs.Sub(reqTs)
	if diff < 0 {
		diff = -diff
	}

	timeout := diff + 10*time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	streamTimeoutSeconds := uint64(math.Ceil(timeout.Seconds()))
	if streamTimeoutSeconds < 10 {
		streamTimeoutSeconds = 10
	}
	if baseTimeout > streamTimeoutSeconds {
		return baseTimeout
	}
	return streamTimeoutSeconds
}

// CollectStreamingGlobalNoiseKeys extracts noise keys from global body noise and test case noise configurations
// that should be ignored during streaming response comparison.
func CollectStreamingGlobalNoiseKeys(globalBodyNoise map[string][]string, tcNoise map[string][]string) map[string]struct{} {
	keys := make(map[string]struct{})
	add := func(candidate string) {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			return
		}
		if strings.HasPrefix(candidate, "body.") {
			candidate = strings.TrimPrefix(candidate, "body.")
		}
		if strings.Contains(candidate, ".") {
			return
		}
		keys[candidate] = struct{}{}
	}

	for k := range globalBodyNoise {
		add(k)
	}
	for k := range tcNoise {
		add(k)
	}
	return keys
}

func isSSETestCase(tc *models.TestCase, resp *http.Response) bool {
	if tc != nil {
		respContentType := getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type")
		if hasSSEContentType(respContentType) {
			return true
		}
		acceptHeader := getHeaderValueCaseInsensitive(tc.HTTPReq.Header, "Accept")
		if hasSSEContentType(acceptHeader) {
			return true
		}
	}
	if resp != nil && hasSSEContentType(resp.Header.Get("Content-Type")) {
		return true
	}
	return false
}

func getHeaderValueCaseInsensitive(headers map[string]string, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == key {
			return v
		}
	}
	return ""
}

func hasSSEContentType(value string) bool {
	return strings.Contains(strings.ToLower(value), "text/event-stream")
}

// IsSSERequest checks whether a test case targets an SSE endpoint based on
// the recorded response Content-Type or request Accept header.
func IsSSERequest(tc *models.TestCase) bool {
	if tc == nil || tc.Kind != models.HTTP {
		return false
	}
	return isSSETestCase(tc, nil)
}

func boundaryFromContentTypeHeader(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["boundary"])
}

func isLikelyStreamingHTTPResponse(tc *models.TestCase, resp *http.Response) bool {
	if resp != nil {
		for _, te := range resp.TransferEncoding {
			if strings.EqualFold(strings.TrimSpace(te), "chunked") {
				return true
			}
		}
		if strings.Contains(strings.ToLower(resp.Header.Get("Transfer-Encoding")), "chunked") {
			return true
		}
		if resp.ContentLength == -1 {
			return true
		}
	}

	if tc != nil {
		respTE := strings.ToLower(getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Transfer-Encoding"))
		if strings.Contains(respTE, "chunked") {
			return true
		}
	}

	return false
}

type expectedSSEFrame struct {
	fields []sseField
	raw    string
}

func compareSSEStream(expectedResp models.HTTPResp, stream io.Reader, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	expectedQueue := extractExpectedSSEQueue(expectedResp)
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamTokenSize)
	scanner.Split(splitSSEFrames)

	for scanner.Scan() {
		rawFrame := normalizeLineEndings(scanner.Text())
		frame := strings.Trim(rawFrame, "\n")
		if strings.TrimSpace(frame) == "" {
			continue
		}

		if nextExpected >= len(expectedQueue) {
			logger.Debug("received additional SSE data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, frame)
		expectedFrame := expectedQueue[nextExpected]
		match, reason := compareSSEFields(expectedFrame.fields, parseSSEFrame(frame), jsonNoiseKeys, logger)
		if !match {
			logger.Debug("SSE frame mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_frame", expectedFrame.raw),
				zap.String("actual_frame", frame))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expectedFrame.raw,
				ActualFrame:   frame,
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) {
			logger.Debug("all expected SSE frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n\n"), nil, err
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("SSE stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		// Build expected frames that were not received
		var missingFrames []string
		for i := nextExpected; i < len(expectedQueue); i++ {
			missingFrames = append(missingFrames, expectedQueue[i].raw)
		}
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: strings.Join(missingFrames, "\n\n"),
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n\n"), nil, nil
}

func compareNDJSONStream(expectedResp models.HTTPResp, stream io.Reader, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	expectedQueue := extractExpectedRawQueue(expectedResp, canonicalizeNDJSONLine, true)
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamTokenSize)

	for scanner.Scan() {
		line := canonicalizeNDJSONLine(scanner.Text())
		if line == "" {
			continue
		}

		if nextExpected >= len(expectedQueue) {
			logger.Debug("received additional NDJSON data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, line)
		expected := expectedQueue[nextExpected]
		ok, cmpErr := compareJSONTextWithNoise(expected, line, jsonNoiseKeys)
		if cmpErr != nil || !ok {
			reason := "json mismatch"
			if cmpErr != nil {
				reason = cmpErr.Error()
			}
			logger.Debug("NDJSON stream mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_frame", expected),
				zap.String("actual_frame", line))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected,
				ActualFrame:   line,
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) {
			logger.Debug("all expected NDJSON frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n"), nil, err
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("NDJSON stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected],
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n"), nil, nil
}

func comparePlainTextStream(expectedResp models.HTTPResp, stream io.Reader, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	expectedQueue := extractExpectedRawQueue(expectedResp, canonicalizePlainTextLine, false)
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamTokenSize)

	for scanner.Scan() {
		line := canonicalizePlainTextLine(scanner.Text())

		if nextExpected >= len(expectedQueue) {
			logger.Debug("received additional plain-text stream data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, line)
		expected := expectedQueue[nextExpected]
		if line != expected {
			logger.Debug("plain-text stream mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("expected", expected),
				zap.String("actual", line))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected,
				ActualFrame:   line,
				Reason:        fmt.Sprintf("content mismatch at frame %d", nextExpected),
			}
			return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) {
			logger.Debug("all expected plain-text frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n"), nil, err
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("plain-text stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected],
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n"), nil, nil
}

func compareBinaryStream(expectedResp models.HTTPResp, stream io.Reader, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	expectedBytes := expectedBinaryBytes(expectedResp)
	expectedSize := len(expectedBytes)
	actualSize := 0
	contentMatch := true
	mismatchOffset := -1
	buffer := make([]byte, 32*1024)

	for {
		n, err := stream.Read(buffer)
		if n > 0 {
			if contentMatch && actualSize < expectedSize {
				end := actualSize + n
				if end > expectedSize {
					end = expectedSize
				}
				if !bytes.Equal(buffer[:end-actualSize], expectedBytes[actualSize:end]) {
					contentMatch = false
					mismatchOffset = actualSize
				}
			}
			actualSize += n
			if actualSize >= expectedSize {
				if actualSize > expectedSize {
					logger.Debug("received additional binary stream data after expected size was matched; closing stream capture",
						zap.Int("expected_size", expectedSize),
						zap.Int("actual_size", actualSize))
				}
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, strconv.Itoa(actualSize), nil, err
		}
	}

	if actualSize != expectedSize {
		logger.Debug("binary stream size mismatch",
			zap.Int("expected_size", expectedSize),
			zap.Int("actual_size", actualSize))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    0,
			ExpectedFrame: fmt.Sprintf("%d bytes", expectedSize),
			ActualFrame:   fmt.Sprintf("%d bytes", actualSize),
			Reason:        fmt.Sprintf("size mismatch: expected %d bytes, got %d bytes", expectedSize, actualSize),
		}
		return false, strconv.Itoa(actualSize), mismatchInfo, nil
	}

	if !contentMatch {
		logger.Debug("binary stream content mismatch",
			zap.Int("size", actualSize),
			zap.Int("first_mismatch_offset", mismatchOffset))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    0,
			ExpectedFrame: fmt.Sprintf("%d bytes", expectedSize),
			ActualFrame:   fmt.Sprintf("%d bytes (content differs starting near byte %d)", actualSize, mismatchOffset),
			Reason:        fmt.Sprintf("content mismatch starting near byte %d of %d bytes", mismatchOffset, actualSize),
		}
		return false, strconv.Itoa(actualSize), mismatchInfo, nil
	}

	return true, strconv.Itoa(actualSize), nil, nil
}

func expectedBinaryBytes(expectedResp models.HTTPResp) []byte {
	if len(expectedResp.StreamBody) > 0 {
		var buf bytes.Buffer
		for _, chunk := range expectedResp.StreamBody {
			raw, ok := streamChunkFieldValue(chunk, "raw")
			if ok {
				buf.WriteString(raw)
			}
		}
		if buf.Len() > 0 {
			return buf.Bytes()
		}
	}
	return []byte(expectedResp.Body)
}

func extractExpectedSSEQueue(expectedResp models.HTTPResp) []expectedSSEFrame {
	if len(expectedResp.StreamBody) == 0 {
		return nil
	}
	queue := make([]expectedSSEFrame, 0, len(expectedResp.StreamBody))
	for _, chunk := range expectedResp.StreamBody {
		if len(chunk.Data) == 0 {
			continue
		}
		fields := make([]sseField, 0, len(chunk.Data))
		lines := make([]string, 0, len(chunk.Data))
		for _, dataField := range chunk.Data {
			key := strings.TrimSpace(dataField.Key)
			if key == "" {
				continue
			}
			if strings.EqualFold(key, "comment") {
				fields = append(fields, sseField{
					key:      ":",
					value:    dataField.Value,
					hasValue: true,
					comment:  true,
				})
				lines = append(lines, ":"+dataField.Value)
				continue
			}

			lowerKey := strings.ToLower(key)
			fields = append(fields, sseField{
				key:      lowerKey,
				value:    dataField.Value,
				hasValue: true,
			})
			lines = append(lines, lowerKey+":"+dataField.Value)
		}
		if len(fields) == 0 {
			continue
		}
		queue = append(queue, expectedSSEFrame{
			fields: fields,
			raw:    strings.Join(lines, "\n"),
		})
	}
	return queue
}

func extractExpectedRawQueue(expectedResp models.HTTPResp, canonicalizer func(string) string, ignoreEmpty bool) []string {
	if len(expectedResp.StreamBody) == 0 {
		return nil
	}
	queue := make([]string, 0, len(expectedResp.StreamBody))
	for _, chunk := range expectedResp.StreamBody {
		raw, ok := streamChunkFieldValue(chunk, "raw")
		if !ok {
			continue
		}
		raw = canonicalizer(raw)
		if ignoreEmpty && raw == "" {
			continue
		}
		queue = append(queue, raw)
	}
	return queue
}

func streamChunkFieldValue(chunk models.HTTPStreamChunk, key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, field := range chunk.Data {
		if strings.ToLower(strings.TrimSpace(field.Key)) == key {
			return field.Value, true
		}
	}
	return "", false
}

type sseField struct {
	key      string
	value    string
	hasValue bool
	comment  bool
}

func parseSSEFrame(frame string) []sseField {
	frame = normalizeLineEndings(frame)
	frame = strings.Trim(frame, "\n")
	if strings.TrimSpace(frame) == "" {
		return nil
	}

	lines := strings.Split(frame, "\n")
	fields := make([]sseField, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, ":") {
			fields = append(fields, sseField{
				key:      ":",
				value:    strings.TrimPrefix(line, ":"),
				hasValue: true,
				comment:  true,
			})
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			fields = append(fields, sseField{
				key:      strings.ToLower(strings.TrimSpace(line)),
				value:    "",
				hasValue: false,
			})
			continue
		}

		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		fields = append(fields, sseField{
			key:      strings.ToLower(strings.TrimSpace(key)),
			value:    value,
			hasValue: true,
		})
	}
	return fields
}

func compareSSEFrame(expectedFrame, actualFrame string, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string) {
	return compareSSEFields(parseSSEFrame(expectedFrame), parseSSEFrame(actualFrame), jsonNoiseKeys, logger)
}

func compareSSEFields(expectedFields, actualFields []sseField, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string) {
	expectedFields = mergeConsecutiveSSEDataFields(expectedFields)
	actualFields = mergeConsecutiveSSEDataFields(actualFields)

	if len(expectedFields) != len(actualFields) {
		return false, "field-count mismatch"
	}

	for i := range expectedFields {
		e := expectedFields[i]
		a := actualFields[i]

		if e.comment {
			if !a.comment {
				return false, "comment-position mismatch"
			}
			if len(strings.TrimSpace(e.value)) != len(strings.TrimSpace(a.value)) {
				logger.Debug("SSE comment size differs (ignored)",
					zap.Int("frame_field_index", i),
					zap.Int("expected_size", len(strings.TrimSpace(e.value))),
					zap.Int("actual_size", len(strings.TrimSpace(a.value))))
			}
			continue
		}

		if e.key != a.key {
			return false, "field-key-order mismatch"
		}

		if e.hasValue != a.hasValue {
			return false, "field-value-presence mismatch"
		}

		if !e.hasValue {
			continue
		}

		if e.key == "data" {
			eVal := strings.TrimSpace(e.value)
			aVal := strings.TrimSpace(a.value)
			expJSON := json.Valid([]byte(eVal))
			actJSON := json.Valid([]byte(aVal))
			if expJSON || actJSON {
				if !(expJSON && actJSON) {
					return false, "data-json-type mismatch"
				}
				ok, err := compareJSONTextWithNoise(eVal, aVal, jsonNoiseKeys)
				if err != nil {
					return false, "data-json-parse-error"
				}
				if !ok {
					return false, "data-json-mismatch"
				}
				continue
			}

			if eVal != aVal {
				return false, "data-value mismatch"
			}
			continue
		}

		if strings.TrimSpace(e.value) != strings.TrimSpace(a.value) {
			return false, "field-value mismatch"
		}
	}

	return true, ""
}

func mergeConsecutiveSSEDataFields(fields []sseField) []sseField {
	if len(fields) == 0 {
		return fields
	}

	merged := make([]sseField, 0, len(fields))
	for _, field := range fields {
		if !field.comment && strings.EqualFold(field.key, "data") {
			current := field
			if !current.hasValue {
				current.hasValue = true
				current.value = ""
			}

			if len(merged) > 0 {
				last := &merged[len(merged)-1]
				if !last.comment && strings.EqualFold(last.key, "data") && last.hasValue {
					last.value = last.value + "\n" + current.value
					continue
				}
			}
			merged = append(merged, current)
			continue
		}

		merged = append(merged, field)
	}

	return merged
}

func compareJSONTextWithNoise(expected, actual string, jsonNoiseKeys map[string]struct{}) (bool, error) {
	var exp any
	var act any

	if err := json.Unmarshal([]byte(expected), &exp); err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(actual), &act); err != nil {
		return false, err
	}

	exp = removeGlobalNoiseKeys(exp, jsonNoiseKeys)
	act = removeGlobalNoiseKeys(act, jsonNoiseKeys)

	return reflect.DeepEqual(exp, act), nil
}

func removeGlobalNoiseKeys(node any, jsonNoiseKeys map[string]struct{}) any {
	switch v := node.(type) {
	case map[string]any:
		filtered := make(map[string]any, len(v))
		for k, child := range v {
			lk := strings.ToLower(strings.TrimSpace(k))
			if _, ignored := jsonNoiseKeys[lk]; ignored {
				continue
			}
			filtered[k] = removeGlobalNoiseKeys(child, jsonNoiseKeys)
		}
		return filtered
	case []any:
		arr := make([]any, 0, len(v))
		for _, child := range v {
			arr = append(arr, removeGlobalNoiseKeys(child, jsonNoiseKeys))
		}
		return arr
	default:
		return node
	}
}

func compareMultipartStream(expectedResp models.HTTPResp, stream io.Reader, boundary string, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	if strings.TrimSpace(boundary) == "" {
		return false, "", nil, fmt.Errorf("missing multipart boundary for stream comparison")
	}

	expectedBody := expectedResp.Body
	expectedQueue, err := parseMultipartQueue(strings.NewReader(expectedBody), boundary)
	if err != nil {
		return false, "", nil, err
	}

	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0
	reader := multipart.NewReader(stream, boundary)

	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, partErr
		}

		actualPart, partReadErr := readMultipartPart(part)
		_ = part.Close()
		if partReadErr != nil {
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, partReadErr
		}

		if nextExpected >= len(expectedQueue) {
			logger.Debug("received additional multipart stream data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_parts", len(expectedQueue)))
			break
		}

		expected := expectedQueue[nextExpected]
		actualQueue = append(actualQueue, actualPart.describe())
		ok, reason := compareMultipartPart(expected, actualPart, jsonNoiseKeys)
		if !ok {
			logger.Debug("multipart stream mismatch",
				zap.Int("part_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_part", expected.describe()),
				zap.String("actual_part", actualPart.describe()))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected.describe(),
				ActualFrame:   actualPart.describe(),
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) {
			logger.Debug("all expected multipart parts matched; closing stream capture early to avoid waiting for extra stream parts",
				zap.Int("matched_parts", nextExpected))
			break
		}
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("multipart stream ended before all expected parts were received",
			zap.Int("expected_parts", len(expectedQueue)),
			zap.Int("matched_parts", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected].describe(),
			ActualFrame:   "(stream ended - no more parts)",
			Reason:        fmt.Sprintf("expected %d parts but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, nil
}

type multipartPartPayload struct {
	contentType string
	body        []byte
}

func (m multipartPartPayload) describe() string {
	return fmt.Sprintf("content-type:%s size:%d", m.contentType, len(m.body))
}

func parseMultipartQueue(reader io.Reader, boundary string) ([]multipartPartPayload, error) {
	if strings.TrimSpace(boundary) == "" {
		return nil, fmt.Errorf("multipart boundary is empty")
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	queue, parseErr := parseMultipartQueueBytes(data, boundary)
	if parseErr == nil {
		return queue, nil
	}

	closingBoundary := []byte("--" + boundary + "--")
	if !bytes.Contains(data, closingBoundary) {
		patchedData := append([]byte{}, data...)
		patchedData = append(patchedData, []byte("\r\n--"+boundary+"--\r\n")...)
		if patchedQueue, patchedErr := parseMultipartQueueBytes(patchedData, boundary); patchedErr == nil {
			return patchedQueue, nil
		}
	}

	return nil, parseErr
}

func parseMultipartQueueBytes(data []byte, boundary string) ([]multipartPartPayload, error) {
	mr := multipart.NewReader(bytes.NewReader(data), boundary)
	queue := make([]multipartPartPayload, 0)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		payload, readErr := readMultipartPart(part)
		_ = part.Close()
		if readErr != nil {
			return nil, readErr
		}
		if payload.contentType == "" && len(payload.body) == 0 {
			continue
		}
		queue = append(queue, payload)
	}

	return queue, nil
}

func readMultipartPart(part *multipart.Part) (multipartPartPayload, error) {
	bodyBytes, err := io.ReadAll(part)
	if err != nil {
		return multipartPartPayload{}, err
	}

	contentType := strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Type")))
	if parsedType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = strings.ToLower(strings.TrimSpace(parsedType))
	}

	if isJSONContentType(contentType) {
		bodyBytes = []byte(strings.TrimSpace(string(bodyBytes)))
	} else if strings.HasPrefix(contentType, "text/") || contentType == "application/xml" || contentType == "application/x-ndjson" || contentType == "application/ndjson" {
		normalized := normalizeLineEndings(string(bodyBytes))
		normalized = strings.TrimSuffix(normalized, "\n")
		bodyBytes = []byte(normalized)
	}

	return multipartPartPayload{
		contentType: contentType,
		body:        append([]byte(nil), bodyBytes...),
	}, nil
}

func compareMultipartPart(expected multipartPartPayload, actual multipartPartPayload, jsonNoiseKeys map[string]struct{}) (bool, string) {
	if expected.contentType != actual.contentType {
		return false, "content-type mismatch"
	}

	if isJSONContentType(expected.contentType) {
		ok, err := compareJSONTextWithNoise(strings.TrimSpace(string(expected.body)), strings.TrimSpace(string(actual.body)), jsonNoiseKeys)
		if err != nil {
			return false, err.Error()
		}
		if !ok {
			return false, "json body mismatch"
		}
		return true, ""
	}

	if !bytes.Equal(expected.body, actual.body) {
		return false, "body mismatch"
	}
	return true, ""
}

func isJSONContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json") || contentType == "application/x-ndjson" || contentType == "application/ndjson"
}

func canonicalizeNDJSONLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}

	var parsed interface{}
	if err := json.NewDecoder(strings.NewReader(line)).Decode(&parsed); err == nil {
		if marshaled, err := json.Marshal(parsed); err == nil {
			return string(marshaled)
		}
	}

	return line
}

func canonicalizePlainTextLine(line string) string {
	return strings.TrimRight(normalizeLineEndings(line), "\n")
}

func splitSSEFrames(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	doubleLFIdx := bytes.Index(data, []byte("\n\n"))
	doubleCRLFIdx := bytes.Index(data, []byte("\r\n\r\n"))

	switch {
	case doubleLFIdx == -1 && doubleCRLFIdx == -1:
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	case doubleLFIdx == -1:
		return doubleCRLFIdx + len("\r\n\r\n"), data[:doubleCRLFIdx], nil
	case doubleCRLFIdx == -1:
		return doubleLFIdx + len("\n\n"), data[:doubleLFIdx], nil
	case doubleLFIdx < doubleCRLFIdx:
		return doubleLFIdx + len("\n\n"), data[:doubleLFIdx], nil
	default:
		return doubleCRLFIdx + len("\r\n\r\n"), data[:doubleCRLFIdx], nil
	}
}

func canonicalizeSSEFrame(frame string) string {
	frame = normalizeLineEndings(frame)
	frame = strings.Trim(frame, "\n")
	if strings.TrimSpace(frame) == "" {
		return ""
	}

	lines := strings.Split(frame, "\n")
	canonicalLines := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, ":") {
			comment := strings.TrimSpace(strings.TrimPrefix(line, ":"))
			canonicalLines = append(canonicalLines, ":"+comment)
			continue
		}

		key, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			canonicalLines = append(canonicalLines, strings.ToLower(strings.TrimSpace(line)))
			continue
		}

		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}

		switch key {
		case "data":
			value = canonicalizeSSEDataValue(value)
		case "event", "id", "retry":
			value = strings.TrimSpace(value)
		default:
			value = strings.TrimSpace(value)
		}

		canonicalLines = append(canonicalLines, key+":"+value)
	}

	return strings.Join(canonicalLines, "\n")
}

func canonicalizeSSEDataValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	var parsed interface{}
	if json.Unmarshal([]byte(trimmed), &parsed) == nil {
		marshaled, err := json.Marshal(parsed)
		if err == nil {
			return string(marshaled)
		}
	}

	if strings.Contains(trimmed, ";base64,") {
		prefix, encoded, ok := strings.Cut(trimmed, ",")
		if ok {
			if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				return prefix + "," + base64.StdEncoding.EncodeToString(decoded)
			}
			if decoded, err := base64.RawStdEncoding.DecodeString(encoded); err == nil {
				return prefix + "," + base64.StdEncoding.EncodeToString(decoded)
			}
		}
	}

	return trimmed
}

func normalizeLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return value
}

func looksBinary(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

// updateTemplateValuesFromHTTPResp checks the HTTP response body and the previous
// templatized response body and updates the template values that are currently held
// under templateValuesMu.
func updateTemplateValuesFromHTTPResp(logger *zap.Logger, templatedResponse, resp models.HTTPResp, currentTemplatedValues, prevTemplatedValues map[string]interface{}) bool {
	// We derive template keys directly from the templated response body & headers by
	// scanning for placeholder patterns like {{key}} (go text/template simple identifiers)
	// and then recursively locating the same JSON path in the new response to fetch
	// the concrete value. This avoids relying on updateTemplatesFromJSON and gives
	// deterministic path-based updates.
	if len(currentTemplatedValues) == 0 { // nothing to update
		logger.Debug("no templatized values present, nothing to update")
		return false
	}

	// Capture entire inner expression (supports: {{string .token}}, {{ .id }}, {{token}}, {{ float .price | printf "%f" }})
	placeholderRe := regexp.MustCompile(`{{\s*([^{}]+?)\s*}}`)
	changed := false

	// --- 1. Handle JSON body path-based updates ---
	// Problem: templated response can contain raw placeholders (e.g. array: [{{int .x}},{{int .y}}]) which is not valid JSON.
	// Solution: produce a sanitized JSON by wrapping any unquoted placeholder token in quotes so that the body becomes parseable.
	var templatedParsed interface{}
	var actualParsed interface{}
	sanitizedTemplatedBody := sanitizeTemplatedJSON(templatedResponse.Body, placeholderRe)
	templatedIsJSON := json.Valid([]byte(sanitizedTemplatedBody))
	actualIsJSON := json.Valid([]byte(resp.Body))

	if templatedIsJSON && actualIsJSON {
		if err := json.Unmarshal([]byte(sanitizedTemplatedBody), &templatedParsed); err == nil {
			if err2 := json.Unmarshal([]byte(resp.Body), &actualParsed); err2 == nil {
				if traverseAndUpdateTemplates(logger, templatedParsed, actualParsed, "", placeholderRe, currentTemplatedValues, prevTemplatedValues) {
					changed = true
				}
			}
		}
	} else {
		logger.Debug("response body or templated body is not JSON, skipping body path-based template updates", zap.Bool("templatedIsJSON", templatedIsJSON), zap.Bool("actualIsJSON", actualIsJSON))
	}

	// --- 2. Handle response header updates ---
	// The record-side (pkg/service/tools/templatize.go buildValueIndexV2) can rewrite
	// header values (including Set-Cookie sub-components and "Authorization: Bearer <tok>")
	// into {{.VarName}} placeholders. At replay time the live server sends fresh values
	// for those same headers — we must pull them out so downstream test cases that
	// consume the placeholder (e.g. Cookie: session={{.session}}) render the live value.
	if updateTemplateValuesFromHeaders(logger, templatedResponse.Header, resp.Header, placeholderRe, currentTemplatedValues, prevTemplatedValues) {
		changed = true
	}
	return changed
}

// updateTemplateValuesFromHeaders walks the recorded (templated) response headers side-by-side
// with the live response headers. For each recorded header value that carries a {{.Var}}
// placeholder it extracts the corresponding live substring and writes it into
// currentTemplatedValues[Var]. Returns true if any template was updated.
//
// It mirrors the record-side decomposition performed in
// pkg/service/tools/templatize.go buildValueIndexV2:
//   - Set-Cookie lines are split into per-cookie name=value pairs; the placeholder lives in
//     the value slot (`name={{.var}}; Path=/; HttpOnly`).
//   - `Authorization: Bearer {{.token}}` has the Bearer prefix stripped before extracting.
//   - Every other header is handled generically by substring-between-literals extraction.
func updateTemplateValuesFromHeaders(
	logger *zap.Logger,
	templatedHeaders map[string]string,
	liveHeaders map[string]string,
	placeholderRe *regexp.Regexp,
	currentTemplatedValues, prevTemplatedValues map[string]interface{},
) bool {
	changed := false
	if len(templatedHeaders) == 0 || len(liveHeaders) == 0 {
		return false
	}

	// Case-insensitive lookup into the live header map (HTTP header names are
	// case-insensitive per RFC 7230 and our yaml map can preserve either casing).
	liveByLower := make(map[string]string, len(liveHeaders))
	for k, v := range liveHeaders {
		liveByLower[strings.ToLower(k)] = v
	}

	for name, templatedVal := range templatedHeaders {
		if !placeholderRe.MatchString(templatedVal) {
			continue
		}
		liveVal, ok := liveByLower[strings.ToLower(name)]
		if !ok || liveVal == "" {
			continue
		}

		switch {
		case strings.EqualFold(name, "Set-Cookie"):
			if updateTemplatesFromSetCookie(logger, templatedVal, liveVal, placeholderRe, currentTemplatedValues, prevTemplatedValues) {
				changed = true
			}
		case strings.EqualFold(name, "Authorization") && strings.HasPrefix(strings.TrimSpace(templatedVal), "Bearer "):
			tmplToken := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(templatedVal), "Bearer "))
			liveToken := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(liveVal), "Bearer "))
			if liveToken == "" {
				continue
			}
			if updateTemplateFromTokenized(logger, tmplToken, liveToken, placeholderRe, currentTemplatedValues, prevTemplatedValues, "header.Authorization") {
				changed = true
			}
		default:
			if updateTemplateFromTokenized(logger, templatedVal, liveVal, placeholderRe, currentTemplatedValues, prevTemplatedValues, "header."+name) {
				changed = true
			}
		}
	}
	return changed
}

// updateTemplatesFromSetCookie splits both recorded and live Set-Cookie blobs into per-cookie
// lines, matches them by cookie name, and extracts the live value for each cookie whose
// recorded value was a placeholder.
func updateTemplatesFromSetCookie(
	logger *zap.Logger,
	templatedVal, liveVal string,
	placeholderRe *regexp.Regexp,
	currentTemplatedValues, prevTemplatedValues map[string]interface{},
) bool {
	changed := false
	tmplLines := splitSetCookieLines(templatedVal)
	liveLines := splitSetCookieLines(liveVal)
	if len(tmplLines) == 0 || len(liveLines) == 0 {
		return false
	}

	liveByName := make(map[string]string, len(liveLines))
	for _, ln := range liveLines {
		name, val := splitSetCookieNV(ln)
		if name == "" {
			continue
		}
		liveByName[name] = val
	}

	for _, ln := range tmplLines {
		name, tmplCookieVal := splitSetCookieNV(ln)
		if name == "" || !placeholderRe.MatchString(tmplCookieVal) {
			continue
		}
		liveCookieVal, ok := liveByName[name]
		if !ok || liveCookieVal == "" {
			continue
		}
		if updateTemplateFromTokenized(logger, tmplCookieVal, liveCookieVal, placeholderRe, currentTemplatedValues, prevTemplatedValues, "Set-Cookie."+name) {
			changed = true
		}
	}
	return changed
}

// updateTemplateFromTokenized handles a single "templated value vs live value" pair where
// the templated value contains one or more {{.VarName}} placeholders. It supports:
//   - Whole-value placeholder: "{{.var}}" -> var = liveVal
//   - Prefix/suffix literal wrapping: "trace-{{.id}}-v1" -> id = substring between literals
//   - Multi-placeholder values are best-effort: they only update when the literal separators
//     between placeholders can unambiguously split the live value.
func updateTemplateFromTokenized(
	logger *zap.Logger,
	templatedVal, liveVal string,
	placeholderRe *regexp.Regexp,
	currentTemplatedValues, prevTemplatedValues map[string]interface{},
	debugPath string,
) bool {
	trim := strings.TrimSpace(templatedVal)
	matches := placeholderRe.FindAllStringSubmatchIndex(templatedVal, -1)
	if len(matches) == 0 {
		return false
	}

	// Fast path: the entire (trimmed) templated value is a single placeholder.
	if len(matches) == 1 && strings.HasPrefix(trim, "{{") && strings.HasSuffix(trim, "}}") {
		inner := placeholderRe.FindStringSubmatch(trim)
		if len(inner) < 2 {
			return false
		}
		keys := extractTemplateKeys(inner[1])
		if len(keys) != 1 {
			return false
		}
		return applyTemplateUpdate(logger, keys[0], strings.TrimSpace(liveVal), currentTemplatedValues, prevTemplatedValues, debugPath)
	}

	// General path: split the templated value into alternating literals & placeholders,
	// then use each literal as an anchor in liveVal to carve out each live substitution.
	type segment struct {
		isPlaceholder bool
		literal       string // only set when !isPlaceholder
		key           string // only set when isPlaceholder
	}
	var segs []segment
	cursor := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > cursor {
			segs = append(segs, segment{literal: templatedVal[cursor:start]})
		}
		innerMatch := placeholderRe.FindStringSubmatch(templatedVal[start:end])
		if len(innerMatch) < 2 {
			cursor = end
			continue
		}
		keys := extractTemplateKeys(innerMatch[1])
		if len(keys) != 1 {
			cursor = end
			continue
		}
		segs = append(segs, segment{isPlaceholder: true, key: keys[0]})
		cursor = end
	}
	if cursor < len(templatedVal) {
		segs = append(segs, segment{literal: templatedVal[cursor:]})
	}

	// Walk live value consuming segments.
	changed := false
	pos := 0
	for i, s := range segs {
		if !s.isPlaceholder {
			if !strings.HasPrefix(liveVal[pos:], s.literal) {
				// Literal mismatch — bail out; we can't trust the alignment.
				return changed
			}
			pos += len(s.literal)
			continue
		}
		// placeholder: consume up to the next literal (or end-of-string).
		var nextLit string
		if i+1 < len(segs) && !segs[i+1].isPlaceholder {
			nextLit = segs[i+1].literal
		}
		var extracted string
		if nextLit == "" {
			extracted = liveVal[pos:]
			pos = len(liveVal)
		} else {
			idx := strings.Index(liveVal[pos:], nextLit)
			if idx < 0 {
				return changed
			}
			extracted = liveVal[pos : pos+idx]
			pos += idx
		}
		if extracted == "" {
			continue
		}
		if applyTemplateUpdate(logger, s.key, extracted, currentTemplatedValues, prevTemplatedValues, debugPath) {
			changed = true
		}
	}
	return changed
}

// applyTemplateUpdate writes key=value into currentTemplatedValues, but only when the key
// already exists (we never invent new template vars at replay time) and the value is
// actually different from the previous one.
func applyTemplateUpdate(
	logger *zap.Logger,
	key, value string,
	currentTemplatedValues, prevTemplatedValues map[string]interface{},
	debugPath string,
) bool {
	if _, ok := currentTemplatedValues[key]; !ok {
		return false
	}
	prevStr := fmt.Sprintf("%v", prevTemplatedValues[key])
	if prevStr == value {
		return false
	}
	logger.Debug("updating template value from header",
		zap.String("key", key),
		zap.String("path", debugPath),
		zap.String("old_value", prevStr),
		zap.String("new_value", value),
	)
	currentTemplatedValues[key] = value
	return true
}

// splitSetCookieLines decomposes a yaml-stored Set-Cookie header into individual cookie lines.
// Mirrors the record-side splitSetCookie in pkg/service/tools/templatize.go — kept as a small,
// duplicated helper here to avoid importing a service package into pkg/util.go.
func splitSetCookieLines(s string) []string {
	if s == "" {
		return nil
	}
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		out := make([]string, 0, len(lines))
		for _, ln := range lines {
			if ln = strings.TrimSpace(ln); ln != "" {
				out = append(out, ln)
			}
		}
		return out
	}
	raw := strings.Split(s, ",")
	if len(raw) == 1 {
		return []string{strings.TrimSpace(raw[0])}
	}
	out := []string{strings.TrimSpace(raw[0])}
	for _, token := range raw[1:] {
		t := strings.TrimSpace(token)
		name, _, hasEq := strings.Cut(t, "=")
		startsName := hasEq && name != "" && isCookieNameStart(name)
		if startsName {
			out = append(out, t)
		} else {
			out[len(out)-1] = out[len(out)-1] + "," + token
		}
	}
	return out
}

func isCookieNameStart(name string) bool {
	if name == "" {
		return false
	}
	r := rune(name[0])
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '$'
}

// splitSetCookieNV extracts the cookie name and value (pre-attribute) from a single Set-Cookie
// line. E.g. "session=abc123; Path=/; HttpOnly" -> ("session", "abc123"). Mirrors the
// record-side splitSetCookieNameValue.
func splitSetCookieNV(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	head := line
	if i := strings.IndexByte(line, ';'); i >= 0 {
		head = line[:i]
	}
	name, val, ok := strings.Cut(head, "=")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(name), strings.TrimSpace(val)
}

// traverseAndUpdateTemplates walks the templated JSON tree in lock-step with the actual JSON.
// Whenever it finds a string containing a placeholder, it extracts the template key(s) and updates
// the current template values with the concrete value from the actual JSON at the same path.
func traverseAndUpdateTemplates(logger *zap.Logger, templatedNode, actualNode interface{}, path string, placeholderRe *regexp.Regexp, currentTemplatedValues, prevTemplatedValues map[string]interface{}) bool {
	changed := false
	switch t := templatedNode.(type) {
	case map[string]interface{}:
		actMap, _ := actualNode.(map[string]interface{})
		for k, v := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if traverseAndUpdateTemplates(logger, v, actMap[k], p, placeholderRe, currentTemplatedValues, prevTemplatedValues) {
				changed = true
			}
		}
	case []interface{}:
		actArr, _ := actualNode.([]interface{})
		for i, v := range t {
			var actElem interface{}
			if i < len(actArr) {
				actElem = actArr[i]
			}
			p := fmt.Sprintf("%s[%d]", path, i)
			if path == "" {
				p = fmt.Sprintf("[%d]", i)
			}
			if traverseAndUpdateTemplates(logger, v, actElem, p, placeholderRe, currentTemplatedValues, prevTemplatedValues) {
				changed = true
			}
		}
	case string:
		matches := placeholderRe.FindAllStringSubmatch(t, -1)
		if len(matches) == 0 {
			return changed
		}
		concrete := fmt.Sprintf("%v", actualNode)
		if concrete == "<nil>" || concrete == "" {
			return changed
		}
		trimT := strings.TrimSpace(t)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			keys := extractTemplateKeys(m[1])
			if len(keys) != 1 {
				continue
			}
			key := keys[0]
			if _, ok := currentTemplatedValues[key]; !ok {
				continue
			}
			prevStr := fmt.Sprintf("%v", prevTemplatedValues[key])
			if prevStr == concrete {
				continue
			}
			// Update only if original value is a single placeholder expression (no static mix)
			if !(strings.HasPrefix(trimT, "{{") && strings.HasSuffix(trimT, "}}") && len(matches) == 1) {
				continue
			}
			logger.Debug("updating template value from JSON path", zap.String("key", key), zap.String("path", path), zap.String("old_value", prevStr), zap.String("new_value", concrete))
			currentTemplatedValues[key] = concrete
			changed = true
		}
	default:
		// non-string primitives can't contain placeholders; nothing to do
	}
	return changed
}

// RenderTestCaseWithTemplates returns a copy of the provided TestCase with
// current templated and secret values applied. This is useful for producing
// a concrete "expected" testcase (for example expected responses) before
// the test is executed and templates may get updated by the runtime.
func RenderTestCaseWithTemplates(tc *models.TestCase) (*models.TestCase, error) {
	templatedValues, secretValues := snapshotTemplateState()

	// If there are no templated or secret values, just return a deep copy
	if len(templatedValues) == 0 && len(secretValues) == 0 {
		copy := *tc
		return &copy, nil
	}

	// Marshal the testcase and execute the template with current values
	testCaseStr, err := json.Marshal(tc)
	if err != nil {
		return nil, err
	}

	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}
	tmpl, err := template.New("template").Funcs(funcMap).Parse(string(testCaseStr))
	if err != nil || tmpl == nil {
		return nil, err
	}

	data := make(map[string]interface{})
	for k, v := range templatedValues {
		data[k] = v
	}
	if len(secretValues) > 0 {
		data["secret"] = secretValues
	}

	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}

	var rendered models.TestCase
	if err := json.Unmarshal(output.Bytes(), &rendered); err != nil {
		return nil, err
	}
	return &rendered, nil
}

// DetectNoiseFieldsInResp inspects a rendered HTTP response and returns a map
// of noise fields that should be marked on the testcase so matchers ignore
// them during comparison. It uses current templated values from utils.
func DetectNoiseFieldsInResp(resp *models.HTTPResp) map[string][]string {
	noise := make(map[string][]string)
	if resp == nil {
		return noise
	}

	templatedValues, _ := snapshotTemplateState()

	// headers: if a header value contains a templated value, mark header.<name>
	for hk, hv := range resp.Header {
		for _, v := range templatedValues {
			if v == nil {
				continue
			}
			lit := fmt.Sprintf("%v", v)
			if lit == "" {
				continue
			}
			if strings.Contains(hv, lit) {
				key := fmt.Sprintf("header.%s", strings.ToLower(hk))
				noise[key] = []string{}
				break
			}
		}
	}

	// body: if JSON, traverse and mark specific json paths where templated values appear
	var parsed interface{}
	if json.Valid([]byte(resp.Body)) {
		if err := json.Unmarshal([]byte(resp.Body), &parsed); err == nil {
			for _, v := range templatedValues {
				if v == nil {
					continue
				}
				lit := fmt.Sprintf("%v", v)
				if lit == "" {
					continue
				}
				paths := findJSONPathsWithValue(parsed, lit, "")
				for _, p := range paths {
					key := fmt.Sprintf("body.%s", p)
					noise[key] = []string{}
				}
				// also mark literal occurrences in raw body (fallback)
				if strings.Contains(resp.Body, lit) && len(paths) == 0 {
					noise["body"] = []string{}
				}
			}
		}
	} else {
		// non-json body: if any templated literal present, mark the full body as noisy
		for _, v := range templatedValues {
			if v == nil {
				continue
			}
			lit := fmt.Sprintf("%v", v)
			if lit == "" {
				continue
			}
			if strings.Contains(resp.Body, lit) {
				noise["body"] = []string{}
				break
			}
		}
	}

	return noise
}

// findJSONPathsWithValue recursively searches parsed JSON for values equal to target
// and returns dot-separated paths (no leading dot). For arrays, indices are used.
func findJSONPathsWithValue(node interface{}, target, prefix string) []string {
	var paths []string
	switch t := node.(type) {
	case map[string]interface{}:
		for k, v := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			paths = append(paths, findJSONPathsWithValue(v, target, p)...)
		}
	case []interface{}:
		for i, v := range t {
			idx := fmt.Sprintf("%d", i)
			p := idx
			if prefix != "" {
				p = prefix + "." + idx
			}
			paths = append(paths, findJSONPathsWithValue(v, target, p)...)
		}
	case string:
		if t == target {
			paths = append(paths, prefix)
		}
	case float64, bool, nil:
		if fmt.Sprintf("%v", t) == target {
			paths = append(paths, prefix)
		}
	}
	return paths
}

func ParseHTTPRequest(requestBytes []byte) (*http.Request, error) {
	// Parse the request using the http.ReadRequest function
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBytes)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Host", request.Host)

	return request, nil
}

func ParseHTTPResponse(data []byte, request *http.Request) (*http.Response, error) {
	buffer := bytes.NewBuffer(data)
	reader := bufio.NewReader(buffer)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func MakeCurlCommand(tc models.HTTPReq) string {
	curl := fmt.Sprintf("curl --request %s \\\n", string(tc.Method))
	curl = curl + fmt.Sprintf("  --url %s \\\n", tc.URL)
	header := ToHTTPHeader(tc.Header)

	for k, v := range ToYamlHTTPHeader(header) {
		if k != "Content-Length" {
			curl = curl + fmt.Sprintf("  --header '%s: %s' \\\n", k, v)
		}
	}
	if len(tc.Form) > 0 {
		for _, form := range tc.Form {
			key := form.Key
			if len(form.Values) == 0 {
				continue
			}
			value := form.Values[0]
			curl = curl + fmt.Sprintf("  --form '%s=%s' \\\n", key, value)
		}
	} else if tc.Body != "" {
		curl = curl + fmt.Sprintf("  --data %s", strconv.Quote(tc.Body))
	}
	return curl
}

func ReadSessionIndices(path string, Logger *zap.Logger) ([]string, error) {
	indices := []string{}

	dir, err := os.OpenFile(path, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy  generated testcases", zap.Error(err))
		return indices, err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			Logger.Debug("failed to close directory", zap.Error(closeErr))
		}
	}()

	files, err := dir.ReadDir(0)
	if err != nil {
		Logger.Debug("failed to read directory contents", zap.Error(err))
		return indices, err
	}

	for _, v := range files {
		if v.Name() != "reports" && v.IsDir() {
			indices = append(indices, v.Name())
		}
	}

	return indices, nil
}

func NextID(IDs []string, identifier string) string {
	latestIndx := 0
	for _, ID := range IDs {
		namePackets := strings.Split(ID, "-")
		if len(namePackets) == 3 {
			Indx, err := strconv.Atoi(namePackets[2])
			if err != nil {
				continue
			}
			if latestIndx < Indx+1 {
				latestIndx = Indx + 1
			}
		}
	}
	return fmt.Sprintf("%s%v", identifier, latestIndx)
}

func LastID(IDs []string, identifier string) string {
	latestIndx := 0
	for _, ID := range IDs {
		namePackets := strings.Split(ID, "-")
		if len(namePackets) == 3 {
			Indx, err := strconv.Atoi(namePackets[2])
			if err != nil {
				continue
			}
			if latestIndx < Indx {
				latestIndx = Indx
			}
		}
	}
	return fmt.Sprintf("%s%v", identifier, latestIndx)
}

var (
	dateFormats = []string{
		time.Layout,
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		time.RFC822,
		time.RFC822Z,
		time.RFC850,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
		time.RFC3339Nano,
		time.Kitchen,
		time.Stamp,
		time.StampMilli,
		time.StampMicro,
		time.StampNano,
		time.DateTime,
		time.DateOnly,
		time.TimeOnly,
	}
)

// ExtractPort extracts the port from a given URL string, defaulting to 80 if no port is specified.
func ExtractPort(rawURL string) (uint32, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}

	host := parsedURL.Host
	if strings.Contains(host, ":") {
		// Split the host by ":" and return the port part
		parts := strings.Split(host, ":")
		port, err := strconv.ParseUint(parts[len(parts)-1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid port in URL: %s", rawURL)
		}
		return uint32(port), nil
	}

	// Default ports based on scheme
	switch parsedURL.Scheme {
	case "https":
		return 443, nil
	default:
		return 80, nil
	}
}

func ExtractHostAndPort(curlCmd string) (string, string, error) {
	// Split the command string to find the URL
	parts := strings.Split(curlCmd, " ")
	for _, part := range parts {
		if strings.HasPrefix(part, "http") {
			u, err := url.Parse(part)
			if err != nil {
				return "", "", err
			}
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				if u.Scheme == "https" {
					port = "443"
				} else {
					port = "80"
				}
			}
			return host, port, nil
		}
	}
	return "", "", fmt.Errorf("no URL found in CURL command")
}

func WaitForPort(ctx context.Context, host string, port string, timeout time.Duration) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 800*time.Millisecond)
			if err == nil {
				err := conn.Close()
				if err != nil {
					return err
				}
				return nil
			}
		case <-timer.C:
			msg := "Please add delay if your application takes more time to start"
			return fmt.Errorf("timeout after %v waiting for port %s:%s, %s", timeout, host, port, msg)
		}
	}
}

// AgentHealthTicker continuously monitors the agent health endpoint at specified intervals
// and signals on the provided channel when the agent becomes available or unavailable.
// It respects the context timeout and returns when the context is cancelled.
func AgentHealthTicker(ctx context.Context, logger *zap.Logger, agentURI string, agentReadyCh chan<- bool, checkInterval time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	defer close(agentReadyCh)

	client := &http.Client{
		Timeout: 500 * time.Millisecond, // short timeout for health checks
	}
	agentStarted := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			isHealthy := isAgentHealthy(ctx, logger, client, agentURI)

			if isHealthy && !agentStarted {
				// Agent became healthy
				agentStarted = true
				select {
				case agentReadyCh <- true:
					return
				case <-ctx.Done():
					return
				}
			} else if !isHealthy && agentStarted {
				// Agent became unhealthy
				agentStarted = false
				select {
				case agentReadyCh <- false:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// isAgentHealthy checks if the agent is running and healthy by calling the /agent/health endpoint
func isAgentHealthy(ctx context.Context, logger *zap.Logger, client *http.Client, agentURI string) bool {
	healthURL := fmt.Sprintf("%s/health", agentURI)
	logger.Debug("Checking agent health", zap.String("url", healthURL))

	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		logger.Debug("Failed to create health check request", zap.Error(err))
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Debug("Failed to read agent health response body", zap.Error(err))
		return false
	}
	logger.Debug("Agent health check response", zap.Int("status_code", resp.StatusCode), zap.String("body", string(body)))

	return resp.StatusCode == http.StatusOK
}

// FilterTcsMocks applies the per-test time-window filter to candidate
// mocks. Pass strict=true for Option-1 containment (out-of-window
// non-config mocks are dropped instead of promoted to the cross-test
// config pool); pass strict=false for legacy Option-2 behaviour. Note
// that Go has no default parameter values — callers resolve the
// effective strict value from config.Test.StrictMockWindow (ships
// true by default) combined with the KEPLOY_STRICT_MOCK_WINDOW env
// override, via strictWindowEnabled. This function only sees the
// resolved strict flag.
func FilterTcsMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) []*models.Mock {
	filteredMocks, _ := filterByTimeStamp(ctx, logger, m, afterTime, beforeTime, strict)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	return filteredMocks
}

// FilterPerTestAndLaxPromoted splits per-test candidate mocks into two
// slices using filterByTimeStamp:
//
//   - perTestInWindow  — mocks whose request-timestamp is inside
//     [afterTime, beforeTime]. These go into the per-test pool for
//     the current test.
//   - promotedToSession — mocks that filterByTimeStamp's lax branch
//     moved to its "unfiltered" return slice because they are out-of-
//     window (but neither strict-dropped nor invalid-order dropped).
//     They represent the pre-Phase-2 kind-fallback's "reuse across
//     tests" semantic — fixture queries recorded in test 1 that
//     every subsequent test legitimately re-issues.
//
// Under strict mode the second slice is always empty (strict drops
// rather than promotes). Under lax mode callers should append the
// promoted slice to the session pool passed to the proxy so cross-
// test fixture reuse keeps working for suites that relied on the
// old kind-fallback's implicit promotion.
//
// Both slices are sorted by request-timestamp for deterministic
// matcher ordering.
func FilterPerTestAndLaxPromoted(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) ([]*models.Mock, []*models.Mock) {
	return FilterPerTestAndLaxPromotedTierAware(ctx, logger, m, afterTime, beforeTime, strict, time.Time{})
}

// FilterPerTestAndLaxPromotedTierAware is the tier-aware variant of
// FilterPerTestAndLaxPromoted. It accepts firstWindowStart (the earliest
// test window start observed by the agent's MockManager, or zero before
// any real test has fired) so the strict gate can preserve per-test
// startup-init mocks (req < firstWindowStart) in the returned perTestIn
// slice instead of dropping them. MockManager.SetMocksWithWindow's
// startup-tier partition then routes those preserved mocks into the
// dedicated startup tree, and the v3 tier-aware dispatcher reaches them
// via GetStartupMocks.
//
// Passing firstWindowStart == time.Time{} reproduces the legacy blanket-
// drop contract (strict mode drops every per-test mock outside the
// current window, regardless of whether the mock predates the first
// test). Legacy callers that don't know firstWindowStart can keep
// calling FilterPerTestAndLaxPromoted unchanged.
func FilterPerTestAndLaxPromotedTierAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool, firstWindowStart time.Time) ([]*models.Mock, []*models.Mock) {
	perTestInWindow, promotedToSession := filterByTimeStampTierAware(ctx, logger, m, afterTime, beforeTime, strict, firstWindowStart)

	sort.SliceStable(perTestInWindow, func(i, j int) bool {
		return perTestInWindow[i].Spec.ReqTimestampMock.Before(perTestInWindow[j].Spec.ReqTimestampMock)
	})
	sort.SliceStable(promotedToSession, func(i, j int) bool {
		return promotedToSession[i].Spec.ReqTimestampMock.Before(promotedToSession[j].Spec.ReqTimestampMock)
	})

	return perTestInWindow, promotedToSession
}

// FilterConfigMocks applies the per-test time-window filter to the config
// mock pool. Pass strict=true for Option-1 containment (out-of-window
// non-config mocks are dropped); pass strict=false for legacy Option-2.
// See FilterTcsMocks for the env / config precedence that determines
// the runtime-effective value.
func FilterConfigMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) []*models.Mock {
	return FilterConfigMocksTierAware(ctx, logger, m, afterTime, beforeTime, strict, time.Time{})
}

// FilterConfigMocksTierAware is the tier-aware variant of
// FilterConfigMocks. Identical to FilterConfigMocks except it threads
// firstWindowStart through to filterByTimeStampTierAware. Used by the
// agent's UpdateMockParams so startup-init entries in the unfiltered
// (config) pool are kept in the filtered return slice (via the
// tier-aware strict branch) and reach MockManager.SetMocksWithWindow
// where the startup-tier partition sorts them correctly.
func FilterConfigMocksTierAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool, firstWindowStart time.Time) []*models.Mock {
	filteredMocks, unfilteredMocks := filterByTimeStampTierAware(ctx, logger, m, afterTime, beforeTime, strict, firstWindowStart)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	sort.SliceStable(unfilteredMocks, func(i, j int) bool {
		return unfilteredMocks[i].Spec.ReqTimestampMock.Before(unfilteredMocks[j].Spec.ReqTimestampMock)
	})

	return append(filteredMocks, unfilteredMocks...)
}

func FilterTcsMocksMapping(ctx context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) []*models.Mock {
	filteredMocks, _ := filterByMapping(ctx, logger, m, mocksPresentInMapping)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	return filteredMocks
}

func FilterConfigMocksMapping(ctx context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) []*models.Mock {
	filteredMocks, unfilteredMocks := filterByMapping(ctx, logger, m, mocksPresentInMapping)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	sort.SliceStable(unfilteredMocks, func(i, j int) bool {
		return unfilteredMocks[i].Spec.ReqTimestampMock.Before(unfilteredMocks[j].Spec.ReqTimestampMock)
	})

	return append(filteredMocks, unfilteredMocks...)
}

// strictWindowEnvOverride holds the result of one-time env-var parsing
// at process start. Env var KEPLOY_STRICT_MOCK_WINDOW=1|true|yes|on
// enables strict containment process-wide, overriding the per-call
// flag plumbed via MockFilterParams (both routes OR together).
// KEPLOY_STRICT_MOCK_WINDOW=0|false|no|off explicitly disables it,
// overriding the per-call flag too — this is the escape hatch for
// users who need to turn off strict mode after the Phase 2.5 default
// flip (see config.Test.StrictMockWindow in config.go) without
// editing keploy.yaml.
//
// Evaluated ONCE at package init — changing the env var later in the
// same process has no effect. Tests that need to exercise the env
// path must set the variable before the binary starts; in-process
// tests should drive the per-call bool instead, which is both
// simpler and deterministic.
var strictWindowEnvOverride = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

// strictWindowEnvExplicitOff reports whether the env var was set to a
// disabling value. Used so KEPLOY_STRICT_MOCK_WINDOW=0 can force
// strict off even when the per-call flag says true or the config
// default has flipped.
var strictWindowEnvExplicitOff = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW")))
	switch v {
	case "0", "false", "no", "off":
		return true
	}
	return false
}()

// IsStrictMockWindow reports whether strict mock-window containment is
// effectively enabled — either via the per-call flag (from
// config.Test.StrictMockWindow / MockFilterParams.StrictMockWindow) or the
// process-wide KEPLOY_STRICT_MOCK_WINDOW env override. Exposed so the
// agent's UpdateMockParams can log the effective state (covering both
// opt-in routes) without duplicating the env-parsing logic.
func IsStrictMockWindow(perCall bool) bool {
	return strictWindowEnabled(perCall)
}

// strictWindowEnabled returns true when strict containment should apply.
//
// Strict mode ships ON by default (config/default.go
// StrictMockWindow: true) now that every stateful-protocol recorder
// classifies mocks finely enough for legitimate cross-test sharing
// to be encoded as session/connection lifetime. Opting out is
// available via config.Test.StrictMockWindow: false or the
// KEPLOY_STRICT_MOCK_WINDOW=0 env override for older recordings
// that still rely on the legacy lax behaviour.
//
// Precedence implemented here (perCall is the already-resolved
// effective call-site decision — config.Test.StrictMockWindow has
// already been OR-ed in by the caller):
//   - If the env var is explicitly set to a disabling value
//     (KEPLOY_STRICT_MOCK_WINDOW=0), strict is DISABLED unconditionally.
//     This wins over the per-call/config decision so users who hit
//     problems with new recordings can opt out instantly without
//     editing config or redeploying callers.
//   - Otherwise, strict follows perCall; an enabling env var can also
//     force it on even if perCall was false.
//
// In practice, opting out of strict mode is done either by:
//   - setting KEPLOY_STRICT_MOCK_WINDOW=0 in env, or
//   - setting test.strictMockWindow: false in keploy.yaml (which
//     flows through perCall).
//
// When strict:
//   - per-test (LifetimePerTest) mocks must have their request
//     timestamp inside the outer test's [req,res] window
//   - out-of-window per-test mocks are DROPPED instead of promoted to
//     the cross-test pool (stops the cross-test bleed bug)
//   - session (LifetimeSession) and connection (LifetimeConnection)
//     mocks are NEVER dropped — their visibility is bounded by
//     test-set / connID lifetime rather than the per-test window.
//
// Lax (opt-out) preserves pre-unification behaviour: out-of-window
// per-test mocks remain visible via the unfiltered pool so
// scoring-based matchers can still pick them up.
func strictWindowEnabled(perCall bool) bool {
	// Explicit env-var disable wins over everything else — escape
	// hatch for users whose recordings predate full Phase 2
	// classification coverage.
	if strictWindowEnvExplicitOff {
		return false
	}
	return perCall || strictWindowEnvOverride
}

func filterByTimeStamp(_ context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strictPerCall bool) ([]*models.Mock, []*models.Mock) {
	return filterByTimeStampTierAware(nil, logger, m, afterTime, beforeTime, strictPerCall, time.Time{})
}

// filterByTimeStampTierAware is the tier-aware form of filterByTimeStamp.
// It accepts firstWindowStart — the earliest test window start the agent's
// MockManager has observed — so the strict gate can distinguish the two
// out-of-window classes a per-test mock can fall into:
//
//   - Startup-init (req < firstWindowStart): genuine app-bootstrap traffic
//     recorded before any test fired (Flyway migrations, Hibernate init,
//     HikariCP pool warm-up, listmonk's cacheUsers SELECT). Under the
//     legacy blanket-drop semantics these were incorrectly treated as
//     bleed; under tier-aware semantics they are PRESERVED in the
//     filtered slice so MockManager.SetMocksWithWindow can route them
//     into the startup tree (its own scan for req < firstStart picks
//     them up and rebuilds the startup tier — see pkg/agent/proxy/
//     mockmanager.go's SetMocksWithWindow).
//
//   - Stale previous-test (firstWindowStart <= req < afterTime, OR
//     req > beforeTime): real cross-test bleed. These are DROPPED under
//     strict mode, same as before — this is the containment guarantee
//     strictMockWindow exists to provide.
//
// Session- and connection-scoped mocks are kept unconditionally (they
// span tests by definition; their visibility is bounded by test-set /
// connID lifetime, not the per-test window). Mocks with zero or
// inverted-order timestamps get the same defensive handling as the
// legacy path.
//
// Authoritative-routing contract (mockdb/filter-layer owns attribution):
// the filter routes each mock using ONLY two signals — the derived
// Lifetime enum and the request-timestamp. Lifetime-first: Session and
// Connection mocks unconditionally land in the unfiltered (session)
// pool; PerTest mocks consult the window. The recorder-stamped
// Metadata["scope"] field is informational on the wire and is NOT
// consulted by this function (nor by any pool-routing site in the
// filter/mockmanager layer). A perTest-lifetime mock whose recorder
// stamped scope=session but whose request-timestamp falls inside the
// current [afterTime, beforeTime] window lands in the per-test pool —
// timestamp attribution is the source of truth. This resolves the
// "27 perTest mocks with scope=session dropped into session pool"
// symptom the sap-demo-java debug bundle captured.
//
// Passing firstWindowStart == time.Time{} restores the legacy blanket-
// drop behaviour (no startup-tier preservation). Callers that can
// supply a real cutoff — principally the agent, via the
// FirstWindowStartReader optional extension on *Proxy — should thread
// it through. Third-party Proxy implementations that don't implement
// the reader fall through to the zero-time behaviour and remain
// functionally identical to the pre-fix blanket-drop contract.
func filterByTimeStampTierAware(_ context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strictPerCall bool, firstWindowStart time.Time) ([]*models.Mock, []*models.Mock) {

	filteredMocks := make([]*models.Mock, 0)
	unfilteredMocks := make([]*models.Mock, 0)

	if afterTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	if beforeTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	strict := strictWindowEnabled(strictPerCall)
	isNonKeploy := false
	var droppedOutOfWindow, droppedInvalidOrder, preservedStartup int

	for _, mock := range m {
		if mock == nil {
			continue
		}
		// doing deep copy to prevent data race, which was happening due to the write to isFiltered
		// field in this for loop, and write in mockmanager functions.
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		if p.Spec.ReqTimestampMock.Equal(time.Time{}) || p.Spec.ResTimestampMock.Equal(time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing",
				zap.String("mock", p.Name), zap.Bool("strict", strict))
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		// Defensive sanity check: if the response-timestamp is BEFORE the
		// request-timestamp the recording is inconsistent (clock skew,
		// serialisation bug, or file corruption). Skip it — keeping such
		// a mock in either pool risks confusing downstream scoring.
		if p.Spec.ResTimestampMock.Before(p.Spec.ReqTimestampMock) {
			logger.Debug("mock has response timestamp before request timestamp; dropping",
				zap.String("mock", p.Name),
				zap.Time("req", p.Spec.ReqTimestampMock),
				zap.Time("res", p.Spec.ResTimestampMock))
			droppedInvalidOrder++
			continue
		}

		// Lifetime-first routing (mockdb/filter-layer authoritative
		// attribution design): Session- and Connection-lifetime mocks
		// belong in the session/unfiltered pool REGARDLESS of whether
		// their request-timestamp happens to fall inside the current
		// test window. Semantics: these are cross-test reusable by
		// contract — a BEGIN / SET / SHOW / handshake probe that was
		// captured during test-N's window is still a session-lifetime
		// probe; routing it into test-N's per-test pool would make it
		// invisible to every subsequent test (per-test pools are
		// single-test-scoped). This is load-bearing for the user's
		// "route by lifetime + reqTimestampMock, ignore metadata.scope"
		// design — a perTest-lifetime mock stamped scope=session by the
		// recorder (scope is informational-only on the wire) but whose
		// req-timestamp sits inside test-N's window still falls through
		// to the timestamp-based in-window check below and lands in
		// test-N's per-test pool. The metadata.scope field is NOT read
		// for pool routing anywhere in the filter/mockmanager layer;
		// scope and lifetime may legitimately disagree on the wire when
		// the recorder's context-probe captured a session-scope fire
		// inside a live test window.
		if p.TestModeInfo.Lifetime == models.LifetimeSession ||
			p.TestModeInfo.Lifetime == models.LifetimeConnection {
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
			continue
		}
		// Defensive fallback: DeriveLifetime may not have run on mocks
		// constructed inline by tests or produced by legacy ingest
		// paths. Fall back to the raw Metadata["type"] tag so a
		// config/connection-tagged mock still routes to the session
		// pool even when the cached Lifetime field is the zero value
		// (LifetimePerTest). metadata.scope is deliberately NOT
		// consulted here.
		if p.Spec.Metadata != nil &&
			(p.Spec.Metadata["type"] == "config" || p.Spec.Metadata["type"] == "connection") {
			if p.Spec.Metadata["type"] == "config" {
				p.TestModeInfo.Lifetime = models.LifetimeSession
			} else {
				p.TestModeInfo.Lifetime = models.LifetimeConnection
			}
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
			continue
		}

		// In-window = request-timestamp inside [afterTime, beforeTime].
		// The response may straggle past beforeTime — DB/HTTP downstream
		// responses routinely complete a few micro-to-milliseconds after
		// the outer test response is captured. The cross-test bleed bug
		// strict mode protects against is caused by out-of-window REQUESTS
		// (a different test's data mock getting reused here), not by
		// in-flight responses landing late. Checking req is sufficient
		// in both modes — the strict-vs-lax difference is solely in the
		// out-of-window handling below (strict drops, lax promotes to
		// the cross-test config pool).
		//
		// Only per-test-lifetime mocks reach this predicate — session/
		// connection-lifetime mocks short-circuited above to the session
		// pool. A perTest-lifetime mock with reqTimestamp inside
		// [afterTime, beforeTime] lands in the per-test pool; outside
		// the window it falls through to the strict/lax out-of-window
		// handling below.
		inWindow := !p.Spec.ReqTimestampMock.Before(afterTime) && !p.Spec.ReqTimestampMock.After(beforeTime)
		if inWindow {
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		// Out-of-window handling for per-test-lifetime mocks only.
		// Session/connection-lifetime mocks were already routed into
		// the unfiltered pool by the lifetime-first short-circuit
		// above, so the strict/lax divergence here applies exclusively
		// to the perTest class that actually needs window containment.
		if strict {
			// Per-test, out-of-window. Tier-aware split: startup-init
			// (req < firstWindowStart) is preserved in the filtered slice
			// so MockManager.SetMocksWithWindow's startup-tier partition
			// routes it into the startup tree. Genuine stale cross-test
			// bleed (firstWindowStart <= req < afterTime, or req >
			// beforeTime) is dropped — the strictMockWindow guarantee.
			//
			// When firstWindowStart is zero we have no cutoff yet (either
			// the agent hasn't observed a real test window on this
			// MockManager, or the caller didn't thread the value through).
			// In that case fall back to the legacy blanket-drop contract
			// so the behaviour is strictly no worse than before.
			if !firstWindowStart.IsZero() && p.Spec.ReqTimestampMock.Before(firstWindowStart) {
				p.TestModeInfo.IsFiltered = true
				filteredMocks = append(filteredMocks, p)
				preservedStartup++
				continue
			}
			// Per-mock diagnostic: emit the hash + window + actual ts
			// at Debug for the strict-drop path so a CI log can
			// pinpoint which postgres / http / mongo mock the per-test
			// cohort lost. Without this, "candidates: 0" downstream is
			// the only visible signal and operators have no way to
			// confirm whether the mock was on disk at all vs filtered
			// out. Aggregate count is preserved below for parity.
			if logger != nil {
				// Info-level (not Debug) so this surfaces in default CI
				// runs without --debug. Tagged with "diag/" so it is
				// obviously diagnostic. Will be demoted/removed once the
				// strict-window regression on listmonk + pgtype-tour is
				// root-caused.
				logger.Info("diag/strict-mode drop: per-test mock outside window",
					zap.String("mock", p.Name),
					zap.String("kind", string(p.Kind)),
					zap.String("connID", p.ConnectionID),
					zap.Time("mock_req_ts", p.Spec.ReqTimestampMock),
					zap.Time("mock_res_ts", p.Spec.ResTimestampMock),
					zap.Time("window_after", afterTime),
					zap.Time("window_before", beforeTime),
					zap.Duration("delta_before_after", p.Spec.ReqTimestampMock.Sub(afterTime)),
					zap.Duration("delta_before_before", p.Spec.ReqTimestampMock.Sub(beforeTime)),
				)
			}
			droppedOutOfWindow++
		} else {
			// Legacy: anything out-of-window goes to the unfiltered pool
			// (where parsers may still pick it up as a fallback).
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
		}
	}
	if (strict && droppedOutOfWindow > 0) || droppedInvalidOrder > 0 || preservedStartup > 0 {
		logger.Debug("filterByTimeStamp tier-aware outcome (see separate counts for reasons)",
			zap.Int("dropped_out_of_window", droppedOutOfWindow),
			zap.Int("dropped_invalid_timestamp_order", droppedInvalidOrder),
			zap.Int("preserved_startup_pre_first_window", preservedStartup),
			zap.Bool("strict", strict),
			zap.Time("after", afterTime),
			zap.Time("before", beforeTime),
			zap.Time("firstWindowStart", firstWindowStart))
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filteredMocks, unfilteredMocks
}

// FilterByTimeStampThreeTier is the three-pool, Lifetime-authoritative
// variant of filterByTimeStampTierAware. It elevates the startup tier
// (LifetimeConnection) to a first-class split at the core filter level
// so integrations that consume the pools (Postgres v3's BuildIndex,
// MySQL's per-connID trees) can read the three tiers as disjoint slices
// without having to re-partition from metadata["type"] themselves.
//
// Return slice contract:
//   - filteredMocks:   LifetimePerTest with request-timestamp inside
//     [afterTime, beforeTime]. These go into the per-test pool for
//     the current test. Under strict mode, out-of-window per-test
//     mocks are DROPPED, except startup-init (req < firstWindowStart)
//     which lands in startupMocks instead (preserving genuine app-
//     bootstrap traffic that fired before any test started).
//   - unfilteredMocks: LifetimeSession. Reusable across every test in
//     the session; never window-filtered. This is the "session" pool
//     in three-tier parlance.
//   - startupMocks:    LifetimeConnection (per-connection reusable,
//     bounded by connID lifetime) + any LifetimePerTest mock whose
//     request-timestamp predates firstWindowStart (legitimate
//     bootstrap traffic under strict mode).
//
// Backward compatibility with unstamped / legacy-tagged mocks:
//  1. metadata["type"]=="config" with no Lifetime → LifetimeSession,
//     routed to unfilteredMocks.
//  2. metadata["type"]=="connection" with no Lifetime → LifetimeConnection,
//     routed to startupMocks.
//  3. No Lifetime, no "type" tag → falls through to per-test window logic.
//
// Integrations that don't emit LifetimeConnection mocks (HTTP, HTTP/2,
// gRPC, generic, Kafka, Redis data-plane, etc.) get an empty startupMocks
// slice — safe to ignore. Integrations that DO care (Postgres v3,
// MySQL's prepared-statement setup) consume it as an authoritative
// "connection-scoped reusable" pool without having to re-derive the
// classification from metadata.
func FilterByTimeStampThreeTier(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime, beforeTime time.Time, strictPerCall bool, firstWindowStart time.Time) (filtered, unfiltered, startup []*models.Mock) {
	filtered = make([]*models.Mock, 0)
	unfiltered = make([]*models.Mock, 0)
	startup = make([]*models.Mock, 0)

	if afterTime.Equal(time.Time{}) || beforeTime.Equal(time.Time{}) {
		// No window supplied — fall back to lifetime-only partitioning
		// so callers still get a three-way split. PerTest (and legacy-
		// untagged) mocks return in `filtered` to match the two-slice
		// legacy contract where the first return slice was "every mock
		// with no effective window to apply".
		for _, mk := range m {
			if mk == nil {
				continue
			}
			p := mk.DeepCopy()
			lt := effectiveLifetimeForRouting(p)
			switch lt {
			case models.LifetimeSession:
				p.TestModeInfo.IsFiltered = false
				unfiltered = append(unfiltered, p)
			case models.LifetimeConnection:
				p.TestModeInfo.IsFiltered = false
				startup = append(startup, p)
			default:
				p.TestModeInfo.IsFiltered = true
				filtered = append(filtered, p)
			}
		}
		return filtered, unfiltered, startup
	}

	strict := strictWindowEnabled(strictPerCall)
	var droppedOutOfWindow, droppedInvalidOrder, preservedStartup int
	isNonKeploy := false

	for _, mk := range m {
		if mk == nil {
			continue
		}
		p := mk.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		if p.Spec.ReqTimestampMock.Equal(time.Time{}) || p.Spec.ResTimestampMock.Equal(time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing",
				zap.String("mock", p.Name), zap.Bool("strict", strict))
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
			continue
		}
		if p.Spec.ResTimestampMock.Before(p.Spec.ReqTimestampMock) {
			logger.Debug("mock has response timestamp before request timestamp; dropping",
				zap.String("mock", p.Name),
				zap.Time("req", p.Spec.ReqTimestampMock),
				zap.Time("res", p.Spec.ResTimestampMock))
			droppedInvalidOrder++
			continue
		}

		lt := effectiveLifetimeForRouting(p)
		switch lt {
		case models.LifetimeConnection:
			p.TestModeInfo.IsFiltered = false
			p.TestModeInfo.Lifetime = models.LifetimeConnection
			startup = append(startup, p)
			continue
		case models.LifetimeSession:
			p.TestModeInfo.IsFiltered = false
			p.TestModeInfo.Lifetime = models.LifetimeSession
			unfiltered = append(unfiltered, p)
			continue
		}

		// PerTest: window containment on request timestamp.
		inWindow := !p.Spec.ReqTimestampMock.Before(afterTime) && !p.Spec.ReqTimestampMock.After(beforeTime)
		if inWindow {
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
			continue
		}

		// Out-of-window per-test: strict drops (with startup-init
		// preservation), lax promotes to unfiltered.
		if strict {
			if !firstWindowStart.IsZero() && p.Spec.ReqTimestampMock.Before(firstWindowStart) {
				p.TestModeInfo.IsFiltered = true
				startup = append(startup, p)
				preservedStartup++
				continue
			}
			// Per-mock diagnostic: emit hash + window deltas at Debug
			// for the three-tier strict-drop path so a CI log can
			// identify which postgres / mongo / etc. mock the per-test
			// cohort lost. Mirrors the same diagnostic added to the
			// two-tier filterByTimeStampTierAware so callers using
			// either path get the visibility uniformly.
			if logger != nil {
				logger.Info("diag/strict-mode drop (3-tier): per-test mock outside window",
					zap.String("mock", p.Name),
					zap.String("kind", string(p.Kind)),
					zap.String("connID", p.ConnectionID),
					zap.Time("mock_req_ts", p.Spec.ReqTimestampMock),
					zap.Time("mock_res_ts", p.Spec.ResTimestampMock),
					zap.Time("window_after", afterTime),
					zap.Time("window_before", beforeTime),
					zap.Duration("delta_before_after", p.Spec.ReqTimestampMock.Sub(afterTime)),
					zap.Duration("delta_before_before", p.Spec.ReqTimestampMock.Sub(beforeTime)),
				)
			}
			droppedOutOfWindow++
		} else {
			p.TestModeInfo.IsFiltered = false
			unfiltered = append(unfiltered, p)
		}
	}

	if (strict && droppedOutOfWindow > 0) || droppedInvalidOrder > 0 || preservedStartup > 0 {
		logger.Debug("FilterByTimeStampThreeTier outcome",
			zap.Int("dropped_out_of_window", droppedOutOfWindow),
			zap.Int("dropped_invalid_timestamp_order", droppedInvalidOrder),
			zap.Int("preserved_startup_pre_first_window", preservedStartup),
			zap.Int("perTest", len(filtered)),
			zap.Int("session", len(unfiltered)),
			zap.Int("startup", len(startup)),
			zap.Bool("strict", strict),
			zap.Time("after", afterTime),
			zap.Time("before", beforeTime),
			zap.Time("firstWindowStart", firstWindowStart))
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filtered, unfiltered, startup
}

// effectiveLifetimeForRouting resolves a mock's Lifetime for
// routing purposes, applying the legacy-tag fallback when the cached
// enum is not explicitly derived. Mirrors the "defensive fallback"
// branch of filterByTimeStampTierAware but returns a value instead of
// mutating the mock — the caller decides whether to stamp the result
// onto the DeepCopy.
//
// Precedence:
//  1. Explicit LifetimeDerived + non-zero Lifetime wins (the recorder
//     stamped it at emit time; honour the authoritative classification).
//  2. LifetimeSession / LifetimeConnection cached wins even without
//     LifetimeDerived (legacy DeriveLifetime paths may have derived
//     the non-zero enum without setting the bool).
//  3. metadata["type"]=="config" → LifetimeSession.
//  4. metadata["type"]=="connection" → LifetimeConnection.
//  5. Anything else → LifetimePerTest (zero value).
//
// Item 3/4 is the backward-compat path for mocks produced before every
// integration stamped Lifetime at emit time, AND for mocks constructed
// inline by tests that set metadata but never called DeriveLifetime.
func effectiveLifetimeForRouting(m *models.Mock) models.Lifetime {
	if m == nil {
		return models.LifetimePerTest
	}
	// Explicit non-zero Lifetime wins regardless of LifetimeDerived.
	// This honours recorder-stamped values AND any DeriveLifetime path
	// that cached a non-zero enum without setting the derived bool.
	if m.TestModeInfo.Lifetime == models.LifetimeSession ||
		m.TestModeInfo.Lifetime == models.LifetimeConnection {
		return m.TestModeInfo.Lifetime
	}
	if m.TestModeInfo.LifetimeDerived {
		return m.TestModeInfo.Lifetime
	}
	if m.Spec.Metadata != nil {
		switch m.Spec.Metadata["type"] {
		case "config":
			return models.LifetimeSession
		case "connection":
			return models.LifetimeConnection
		}
	}
	return models.LifetimePerTest
}

func filterByMapping(_ context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) ([]*models.Mock, []*models.Mock) {
	mapping := make(map[string]bool, len(mocksPresentInMapping))
	for _, name := range mocksPresentInMapping {
		mapping[name] = true
	}
	var filtered, unfiltered []*models.Mock
	isNonKeploy := false
	for _, mock := range m {
		if mock == nil {
			continue
		}
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		if mapping[p.Name] {
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
		} else {
			p.TestModeInfo.IsFiltered = false
			unfiltered = append(unfiltered, p)
		}
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filtered, unfiltered
}

func GuessContentType(data []byte) models.BodyType {
	// Use net/http library's DetectContentType for basic MIME type detection
	mimeType := http.DetectContentType(data)

	// Additional checks to further specify the content type
	switch {
	case IsJSON(data):
		return models.JSON
	case IsXML(data):
		return models.XML
	case strings.Contains(mimeType, "text/html"):
		return models.HTML
	case strings.Contains(mimeType, "text/plain"):
		if IsCSV(data) {
			return models.CSV
		}
		return models.Plain
	}

	return models.UnknownType
}

func IsJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}

func IsXML(data []byte) bool {
	var xm xml.Name
	return xml.Unmarshal(data, &xm) == nil
}

// IsCSV checks if data can be parsed as CSV by looking for common characteristics
func IsCSV(data []byte) bool {
	// Very simple CSV check: look for commas in the first line
	content := string(data)
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		return strings.Contains(lines[0], ",")
	}
	return false
}

func Decompress(logger *zap.Logger, encoding string, data []byte) ([]byte, error) {
	switch encoding {
	case "br":
		logger.Debug("decompressing brotli compressed data")
		reader := brotli.NewReader(bytes.NewReader(data))
		decodedData, err := io.ReadAll(reader)
		if err != nil {
			utils.LogError(logger, err, "failed to read the brotli compressed data")
			return nil, err
		}
		return decodedData, nil
	case "gzip":
		logger.Debug("decoding gzip compressed data")
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			utils.LogError(logger, err, "failed to create gzip reader")
			return nil, err
		}
		defer reader.Close()
		decodedData, err := io.ReadAll(reader)
		if err != nil {
			utils.LogError(logger, err, "failed to read the gzip compressed data")
			return nil, err
		}
		return decodedData, nil
	}
	return data, nil
}

func Compress(logger *zap.Logger, encoding string, data []byte) ([]byte, error) {
	switch encoding {
	case "gzip":
		logger.Debug("compressing data using gzip")
		var compressedBuffer bytes.Buffer
		gw := gzip.NewWriter(&compressedBuffer)
		_, err := gw.Write(data)
		if err != nil {
			utils.LogError(logger, err, "failed to write compressed data to gzip writer")
			return nil, err
		}
		err = gw.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close gzip writer")
			return nil, err
		}
		return compressedBuffer.Bytes(), nil
	case "br":
		logger.Debug("compressing data using brotli")
		var compressedBuffer bytes.Buffer
		bw := brotli.NewWriter(&compressedBuffer)
		_, err := bw.Write(data)
		if err != nil {
			utils.LogError(logger, err, "failed to write compressed data to brotli writer")
			return nil, err
		}
		err = bw.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close brotli writer")
			return nil, err
		}
		return compressedBuffer.Bytes(), nil
	}
	return data, nil
}

// extractTemplateKeys parses the inner segment of a template placeholder and returns variable keys.
// Supports patterns like:
//
//	token
//	.token
//	string .token
//	int .id
//	float .price
//	.user.id  (returns last path segment id)
//
// Only the first pipeline segment is considered (text before |).
func extractTemplateKeys(inner string) []string {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil
	}
	if idx := strings.Index(inner, "|"); idx >= 0 {
		inner = inner[:idx]
	}
	parts := strings.Fields(inner)
	if len(parts) == 0 {
		return nil
	}
	funcNames := map[string]struct{}{"int": {}, "string": {}, "float": {}}
	varToken := parts[0]
	if _, isFunc := funcNames[varToken]; isFunc {
		if len(parts) < 2 {
			return nil
		}
		varToken = parts[1]
	}
	varToken = strings.TrimSpace(varToken)
	if varToken == "" {
		return nil
	}
	varToken = strings.TrimLeft(varToken, ".")
	if varToken == "" {
		return nil
	}
	segs := strings.Split(varToken, ".")
	key := segs[len(segs)-1]
	return []string{key}
}

// sanitizeTemplatedJSON makes a JSON string containing go-template placeholders parseable by wrapping
// any placeholder tokens that appear in value positions without surrounding quotes with quotes.
// Example: {"arr":[{{int .a}},{{int .b}}]} => {"arr":["{{int .a}}","{{int .b}}"]}
// This lets us unmarshal while still preserving the placeholder text for later extraction.
func sanitizeTemplatedJSON(raw string, placeholderRe *regexp.Regexp) string {
	if raw == "" {
		return raw
	}
	// Precompute which byte positions are inside JSON string literals.
	inString := make([]bool, len(raw))
	escaped := false
	inside := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\\' && !escaped { // potential escape for next char
			escaped = true
			if inside {
				inString[i] = true
			}
			continue
		}
		if c == '"' && !escaped { // toggle string state
			inside = !inside
			inString[i] = inside
			continue
		}
		if inside {
			inString[i] = true
		}
		escaped = false
	}

	matches := placeholderRe.FindAllStringIndex(raw, -1)
	if len(matches) == 0 {
		return raw
	}

	// Build sanitized output.
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		// Determine if this placeholder starts inside a string literal.
		insideString := start < len(inString) && inString[start]
		b.WriteString(raw[last:start])
		if !insideString {
			// Insert quotes to force valid JSON token.
			b.WriteByte('"')
			b.WriteString(raw[start:end])
			b.WriteByte('"')
		} else {
			b.WriteString(raw[start:end])
		}
		last = end
	}
	b.WriteString(raw[last:])
	return b.String()
}

// LooksLikeJSON checks if a string appears to be JSON by checking for opening and closing brackets/braces.
// It trims whitespace and returns false for empty strings.
func LooksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return (strings.HasPrefix(s, "{") && strings.Contains(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.Contains(s, "]"))
}

// hasExplicitPort checks if the given host string (or host:port)
// has an explicit port defined. It handles IPv6 addresses (e.g. [::1]:8080) correctly.
func hasExplicitPort(hostStr string) bool {
	// Basic check: if no colon, definitely no port (unless it's a bare IPv6, but that doesn't have a port either)
	if !strings.Contains(hostStr, ":") {
		return false
	}

	// Attempt to split host/port
	// net.SplitHostPort handles IPv6 addresses enclosed in brackets [::1]:8080
	_, port, err := net.SplitHostPort(hostStr)
	if err != nil {
		return false
	}

	// If a port is found, verify it is numeric
	if port != "" {
		if _, err := strconv.Atoi(port); err == nil {
			return true
		}
	}

	return false
}

// ResolveTestTarget determines the final target (URL for HTTP, Authority for gRPC)
// by applying replacement logic and precedence rules.
//
// Priority logic (lowest → highest):
//  1. AppPort (recorded port).
//  2. ConfigPort (top-level port/grpcPort/ssePort combined with protocol overrides).
//  3. replaceWith URL replacements – if replacement has explicit port, skip steps 1-2.
//  4. replaceWith port mappings – always applied last, overrides everything.
func ResolveTestTarget(originalTarget string, urlReplacements map[string]string, portMappings map[uint32]uint32, configHost string, appPort uint16, configPort uint32, isHTTP bool, logger *zap.Logger) (string, error) {
	finalTarget := originalTarget
	replacementHasPort := false
	replacementMatched := false

	// Step 1: Check replaceWith URL
	if len(urlReplacements) > 0 {
		for substr, replacement := range urlReplacements {
			if strings.Contains(finalTarget, substr) {
				replacementMatched = true
				finalTarget = strings.Replace(finalTarget, substr, replacement, 1)

				// Check if the replacement value explicitly defines a port.
				checkStr := replacement
				if isHTTP {
					if strings.HasPrefix(replacement, "http://") {
						checkStr = strings.TrimPrefix(replacement, "http://")
					} else if strings.HasPrefix(replacement, "https://") {
						checkStr = strings.TrimPrefix(replacement, "https://")
					}
				}

				if hasExplicitPort(checkStr) {
					replacementHasPort = true
				}

				logger.Debug("Applied replaceWith substitution",
					zap.String("find", substr),
					zap.String("replace", replacement),
					zap.String("result_target", finalTarget),
					zap.Bool("replacement_has_port", replacementHasPort))
				break
			}
		}
	}

	// If URL replacement set a port and there are no port mappings, we are done
	if replacementHasPort && len(portMappings) == 0 {
		return finalTarget, nil
	}

	// Step 2: ConfigHost override (only if no URL replacement match)
	if !replacementMatched && !replacementHasPort && configHost != "" {
		var err error
		if isHTTP {
			finalTarget, err = utils.ReplaceHost(finalTarget, configHost)
		} else {
			finalTarget, err = utils.ReplaceGrpcHost(finalTarget, configHost)
		}
		if err != nil {
			utils.LogError(logger, err, "failed to replace host with config host")
			return "", err
		}
		logger.Debug("Replaced host with config host", zap.String("host", configHost), zap.String("target", finalTarget))
	}

	// Parse host and port from finalTarget
	var host string
	var port string
	var scheme string // only for HTTP

	if isHTTP {
		parsedURL, parseErr := url.Parse(finalTarget)
		if parseErr != nil {
			utils.LogError(logger, parseErr, "failed to parse test case URL")
			return "", parseErr
		}
		host = parsedURL.Hostname()
		port = parsedURL.Port()
		scheme = parsedURL.Scheme
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
			logger.Debug("URL has no explicit port, using scheme default",
				zap.String("target", finalTarget), zap.String("default_port", port))
		}
	} else {
		host = finalTarget
		if colonIdx := strings.LastIndex(finalTarget, ":"); colonIdx != -1 {
			h, p, err := net.SplitHostPort(finalTarget)
			if err == nil {
				host = h
				port = p
			}
		}
		if port == "" {
			logger.Debug("Authority has no explicit port, using gRPC default",
				zap.String("target", finalTarget), zap.String("default_port", "443"))
			port = "443"
		}
	}

	// Steps 3-4: AppPort/ConfigPort (only if URL replacement didn't set port)
	if !replacementHasPort {
		if appPort > 0 {
			port = fmt.Sprintf("%d", appPort)
			logger.Debug("Overriding port with app_port",
				zap.Uint16("app_port", appPort))
		}

		if configPort > 0 {
			if appPort > 0 && uint32(appPort) != configPort {
				logger.Debug("Config port overrides recorded app_port",
					zap.Uint32("config_port", configPort),
					zap.Uint16("recorded_app_port", appPort))
			}
			port = fmt.Sprintf("%d", configPort)
		}
	}

	// Step 5: Port mappings (highest priority, always checked)
	if len(portMappings) > 0 {
		currentPort, convErr := strconv.ParseUint(port, 10, 32)
		if convErr == nil {
			if mappedPort, ok := portMappings[uint32(currentPort)]; ok {
				logger.Debug("Applied replaceWith port mapping",
					zap.Uint32("from", uint32(currentPort)),
					zap.Uint32("to", mappedPort))
				port = fmt.Sprintf("%d", mappedPort)
			}
		}
	}

	// Reassemble
	if isHTTP {
		u, _ := url.Parse(finalTarget)
		u.Host = net.JoinHostPort(host, port)
		finalTarget = u.String()
	} else {
		finalTarget = net.JoinHostPort(host, port)
	}

	logger.Debug("Final resolved target", zap.String("target", finalTarget))
	return finalTarget, nil
}
