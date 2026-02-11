// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

var SortCounter int64 = -1

func InitSortCounter(counter int64) {
	atomic.StoreInt64(&SortCounter, counter)
}

func GetNextSortNum() int64 {
	return atomic.AddInt64(&SortCounter, 1)
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
		match := IsTime(j)
		if match {
			//Values like "Tue, 17 Jan 2023 16:34:58 IST" should be considered as single element
			header[i] = []string{j}
			continue
		}
		header[i] = strings.Split(j, ",")
	}
	return header
}

// IsTime verifies whether a given string represents a valid date or not.
func IsTime(stringDate string) bool {
	date := strings.TrimSpace(stringDate)
	if secondsFloat, err := strconv.ParseFloat(date, 64); err == nil {
		seconds := int64(secondsFloat / 1e9)
		nanoseconds := int64(secondsFloat) % 1e9
		expectedTime := time.Unix(seconds, nanoseconds)
		currentTime := time.Now()
		if currentTime.Sub(expectedTime) < 24*time.Hour && currentTime.Sub(expectedTime) > -24*time.Hour {
			return true
		}
	}
	for _, dateFormat := range dateFormats {
		_, err := time.Parse(dateFormat, date)
		if err == nil {
			return true
		}
	}
	return false
}

func SimulateHTTP(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, apiTimeout uint64, configPort uint32) (*models.HTTPResp, error) {
	var resp *models.HTTPResp
	templatedResponse := tc.HTTPResp // keep a copy of the original templatized response

	if strings.Contains(tc.HTTPReq.URL, "%7B") { // case in which URL string has encoded template placeholders
		decoded, err := url.QueryUnescape(tc.HTTPReq.URL)
		if err == nil {
			tc.HTTPReq.URL = decoded
		}
	}
	//TODO: adjust this logic in the render function in order to remove the redundant code
	// convert testcase to string and render the template values.
	// Render any template values in the test case before simulation.
	// Render any template values in the test case before simulation.
	if len(utils.TemplatizedValues) > 0 || len(utils.SecretValues) > 0 {
		testCaseBytes, err := json.Marshal(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the testcase for templating")
			return nil, err
		}

		// Build the template data
		templateData := make(map[string]interface{}, len(utils.TemplatizedValues)+len(utils.SecretValues))
		for k, v := range utils.TemplatizedValues {
			templateData[k] = v
		}
		if len(utils.SecretValues) > 0 {
			templateData["secret"] = utils.SecretValues
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

	if tc.HTTPReq.Header["Content-Encoding"] != "" {
		reqBody, err = Compress(logger, tc.HTTPReq.Header["Content-Encoding"], reqBody)
		if err != nil {
			utils.LogError(logger, err, "failed to compress the request body")
			return nil, err
		}
	}

	logger.Info("starting test for", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))

	// Determine which port to use for test execution
	// Priority: 1. Config port (from flag/config file) 2. Test case AppPort 3. Original URL port (defaults to 80 for HTTP, 443 for HTTPS)
	testURL := tc.HTTPReq.URL
	parsedURL, parseErr := url.Parse(tc.HTTPReq.URL)
	if parseErr != nil {
		utils.LogError(logger, parseErr, "failed to parse test case URL")
		return nil, parseErr
	}

	// Get the port from URL (returns empty string if not specified, meaning default 80/443)
	urlPort := parsedURL.Port()

	if configPort > 0 {
		// Config port takes highest priority - use it for all test cases
		host := parsedURL.Hostname()
		parsedURL.Host = fmt.Sprintf("%s:%d", host, configPort)
		testURL = parsedURL.String()

		// Warn if config port differs from recorded app_port (may cause test failures)
		if tc.AppPort > 0 && uint32(tc.AppPort) != configPort {
			logger.Info("Using port from config/flag which differs from recorded app_port. This may cause test failures if the app behavior differs on different ports.",
				zap.Uint32("config_port", configPort),
				zap.Uint16("recorded_app_port", tc.AppPort),
				zap.String("url", testURL))
		} else {
			logger.Debug("Using port from config/flag", zap.Uint32("port", configPort), zap.String("url", testURL))
		}
	} else if tc.AppPort > 0 {
		// Use test case AppPort if no config port is provided
		host := parsedURL.Hostname()
		parsedURL.Host = fmt.Sprintf("%s:%d", host, tc.AppPort)
		testURL = parsedURL.String()
		logger.Debug("Using app_port from test case", zap.Uint16("app_port", tc.AppPort), zap.String("url", testURL))
	} else {
		// Neither configPort nor AppPort is set - use original URL
		// If URL has no explicit port, Go's http.Client uses scheme defaults (80 for HTTP, 443 for HTTPS)
		if urlPort == "" {
			defaultPort := "80"
			if parsedURL.Scheme == "https" {
				defaultPort = "443"
			}
			logger.Debug("No port specified in config or test case. Using URL as-is with default port.",
				zap.String("url", testURL),
				zap.String("default_port", defaultPort))
		} else {
			logger.Debug("Using port from URL", zap.String("url", testURL), zap.String("port", urlPort))
		}
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
	logger.Debug(fmt.Sprintf("Sending request to user app:%v", req))

	// override host header if present in the request
	hostHeader := tc.HTTPReq.Header["Host"]
	if hostHeader != "" {
		logger.Debug("overriding host header", zap.String("host", hostHeader))
		req.Host = hostHeader
	}

	// Creating the client and disabling redirects
	var client *http.Client

	_, hasAcceptEncoding := req.Header["Accept-Encoding"]
	disableCompression := !hasAcceptEncoding

	keepAlive, ok := req.Header["Connection"]
	if ok && strings.EqualFold(keepAlive[0], "keep-alive") {
		logger.Debug("simulating request with conn:keep-alive")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableCompression: disableCompression,
			},
		}
	} else if ok && strings.EqualFold(keepAlive[0], "close") {
		logger.Debug("simulating request with conn:close")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  true,
				DisableCompression: disableCompression,
			},
		}
	} else {
		logger.Debug("simulating request with conn:keep-alive (maxIdleConn=1)")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  false,
				MaxIdleConns:       1,
				DisableCompression: disableCompression,
			},
		}
	}

	httpResp, errHTTPReq := client.Do(req)
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

	respBody, errReadRespBody := io.ReadAll(httpResp.Body)
	if errReadRespBody != nil {
		utils.LogError(logger, errReadRespBody, "failed reading response body")
		return nil, errReadRespBody
	}

	if httpResp.Header.Get("Content-Encoding") != "" {
		respBody, err = Decompress(logger, httpResp.Header.Get("Content-Encoding"), respBody)
		if err != nil {
			utils.LogError(logger, err, "failed to decode response body")
			return nil, err
		}
	}

	statusMessage := http.StatusText(httpResp.StatusCode)

	resp = &models.HTTPResp{
		StatusCode:    httpResp.StatusCode,
		StatusMessage: statusMessage,
		Body:          string(respBody),
		Header:        ToYamlHTTPHeader(httpResp.Header),
	}

	// Centralized template update: if response body present and templates exist, update them.
	if len(utils.TemplatizedValues) > 0 && len(respBody) > 0 {
		logger.Debug("Received response from user app", zap.Any("response", resp))

		prev := make(map[string]interface{}, len(utils.TemplatizedValues))
		for k, v := range utils.TemplatizedValues {
			prev[k] = v
		}

		// Compare the current response with previous template values and update if needed
		if len(utils.TemplatizedValues) > 0 && len(respBody) > 0 {
			updated := UpdateTemplateValuesFromHTTPResp(logger, templatedResponse, *resp, utils.TemplatizedValues)
			if updated {
				logger.Debug("Updated template values", zap.Any("templatized_values", utils.TemplatizedValues))
			}
		}
	}
	return resp, errHTTPReq
}

// UpdateTemplateValuesFromHTTPResp checks the HTTP response body and the previous templatized response body
// it updates the template values if it finds any changes in the response body's fields which were previously templatized
func UpdateTemplateValuesFromHTTPResp(logger *zap.Logger, templatedResponse, resp models.HTTPResp, prevTemplatedValues map[string]interface{}) bool {
	// We derive template keys directly from the templated response body & headers by
	// scanning for placeholder patterns like {{key}} (go text/template simple identifiers)
	// and then recursively locating the same JSON path in the new response to fetch
	// the concrete value. This avoids relying on updateTemplatesFromJSON and gives
	// deterministic path-based updates.
	if len(utils.TemplatizedValues) == 0 { // nothing to update
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
				if traverseAndUpdateTemplates(logger, templatedParsed, actualParsed, "", placeholderRe, prevTemplatedValues) {
					changed = true
				}
			}
		}
	} else {
		logger.Debug("response body or templated body is not JSON, skipping body path-based template updates", zap.Bool("templatedIsJSON", templatedIsJSON), zap.Bool("actualIsJSON", actualIsJSON))
	}
	return changed
}

// traverseAndUpdateTemplates walks the templated JSON tree in lock-step with the actual JSON.
// Whenever it finds a string containing a placeholder, it extracts the template key(s) and updates
// utils.TemplatizedValues with the concrete value from the actual JSON at the same path.
func traverseAndUpdateTemplates(logger *zap.Logger, templatedNode, actualNode interface{}, path string, placeholderRe *regexp.Regexp, prevTemplatedValues map[string]interface{}) bool {
	changed := false
	switch t := templatedNode.(type) {
	case map[string]interface{}:
		actMap, _ := actualNode.(map[string]interface{})
		for k, v := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if traverseAndUpdateTemplates(logger, v, actMap[k], p, placeholderRe, prevTemplatedValues) {
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
			if traverseAndUpdateTemplates(logger, v, actElem, p, placeholderRe, prevTemplatedValues) {
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
			if _, ok := utils.TemplatizedValues[key]; !ok {
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
			utils.TemplatizedValues[key] = concrete
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
	// If there are no templated or secret values, just return a deep copy
	if len(utils.TemplatizedValues) == 0 && len(utils.SecretValues) == 0 {
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
	for k, v := range utils.TemplatizedValues {
		data[k] = v
	}
	if len(utils.SecretValues) > 0 {
		data["secret"] = utils.SecretValues
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

	// headers: if a header value contains a templated value, mark header.<name>
	for hk, hv := range resp.Header {
		for _, v := range utils.TemplatizedValues {
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
			for _, v := range utils.TemplatizedValues {
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
		for _, v := range utils.TemplatizedValues {
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

func FilterTcsMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time) []*models.Mock {
	filteredMocks, _ := filterByTimeStamp(ctx, logger, m, afterTime, beforeTime)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	return filteredMocks
}

func FilterConfigMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time) []*models.Mock {
	filteredMocks, unfilteredMocks := filterByTimeStamp(ctx, logger, m, afterTime, beforeTime)

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

func filterByTimeStamp(_ context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, []*models.Mock) {

	filteredMocks := make([]*models.Mock, 0)
	unfilteredMocks := make([]*models.Mock, 0)

	if afterTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	if beforeTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	isNonKeploy := false

	for _, mock := range m {
		// doing deep copy to prevent data race, which was happening due to the write to isFiltered
		// field in this for loop, and write in mockmanager functions.
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		if p.Spec.ReqTimestampMock.Equal(time.Time{}) || p.Spec.ResTimestampMock.Equal(time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing")
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		if p.Spec.ReqTimestampMock.After(afterTime) && p.Spec.ResTimestampMock.Before(beforeTime) {
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}
		p.TestModeInfo.IsFiltered = false
		unfilteredMocks = append(unfilteredMocks, p)
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filteredMocks, unfilteredMocks
}

func filterByMapping(_ context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) ([]*models.Mock, []*models.Mock) {
	filteredMocks := make([]*models.Mock, 0)
	unfilteredMocks := make([]*models.Mock, 0)

	isNonKeploy := false

	for _, mock := range m {

		p := mock.DeepCopy()

		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}

		matched := false
		for _, name := range mocksPresentInMapping {
			if p.Name == name {
				p.TestModeInfo.IsFiltered = true
				filteredMocks = append(filteredMocks, p)
				matched = true
				break
			}
		}

		if matched {
			continue
		}

		p.TestModeInfo.IsFiltered = false
		unfilteredMocks = append(unfilteredMocks, p)
	}

	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
		return filteredMocks, unfilteredMocks
	}

	return filteredMocks, unfilteredMocks
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
