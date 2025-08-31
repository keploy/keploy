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
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"text/template"

	"github.com/andybalholm/brotli"
	"github.com/davecgh/go-spew/spew"
	"go.keploy.io/server/v2/pkg/models"

	"go.keploy.io/server/v2/utils"
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

func SimulateHTTP(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, apiTimeout uint64) (*models.HTTPResp, error) {
	var resp *models.HTTPResp

	//TODO: adjust this logic in the render function in order to remove the redundant code
	// convert testcase to string and render the template values.
	if len(utils.TemplatizedValues) > 0 || len(utils.SecretValues) > 0 {
		testCaseStr, err := json.Marshal(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the testcase")
			return nil, err
		}
		funcMap := template.FuncMap{
			"int":    utils.ToInt,
			"string": utils.ToString,
			"float":  utils.ToFloat,
		}
		tmpl, err := template.New("template").Funcs(funcMap).Parse(string(testCaseStr))
		if err != nil || tmpl == nil {
			utils.LogError(logger, err, "failed to parse the template", zap.Any("TestCaseString", string(testCaseStr)), zap.Any("TestCase", tc.Name), zap.Any("TestSet", testSet))
			return nil, err
		}

		// fmt.Println("Templatized Values:", utils.TemplatizedValues)

		data := make(map[string]interface{})

		for k, v := range utils.TemplatizedValues {
			data[k] = v
		}

		if len(utils.SecretValues) > 0 {
			data["secret"] = utils.SecretValues
		}
		var output bytes.Buffer
		err = tmpl.Execute(&output, data)
		if err != nil {
			utils.LogError(logger, err, "failed to execute the template")
			return nil, err
		}
		testCaseStr = output.Bytes()
		err = json.Unmarshal([]byte(testCaseStr), &tc)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal the testcase")
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
	logger.Info("starting test for of", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))
	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), tc.HTTPReq.URL, bytes.NewBuffer(reqBody))
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
	fmt.Println("Request Body:")
	spew.Dump(req.Body)
	fmt.Println("With Headers:", req.Header)
	fmt.Println("To URL:", req.URL.String())
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

	resp = &models.HTTPResp{
		StatusCode: httpResp.StatusCode,
		Body:       string(respBody),
		Header:     ToYamlHTTPHeader(httpResp.Header),
	}

	// Centralized template update: if response body present and templates exist, update them.
	if len(utils.TemplatizedValues) > 0 && len(respBody) > 0 {
		fmt.Println("Received response from user app:", resp)
		// Snapshot for logging only (no propagation here; replay handles its own propagation logic)
		prev := make(map[string]interface{}, len(utils.TemplatizedValues))
		for k, v := range utils.TemplatizedValues {
			prev[k] = v
		}
		if updateTemplatesFromJSON(logger, respBody) {
			// Persist change to testset template if replay layer provided such a method via context (optional future extension)
			logger.Info("templates updated inside SimulateHTTP", zap.String("testSet", testSet), zap.Any("templates", utils.TemplatizedValues))
		}
	}

	return resp, errHTTPReq
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
		tmp := *mock
		p := &tmp
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
