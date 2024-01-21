package pkg

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/araddon/dateparse"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

// UrlParams returns the Url and Query parameters from the request url.
func UrlParams(r *http.Request) map[string]string {
	qp := r.URL.Query()
	result := make(map[string]string)

	for key, values := range qp {
		result[key] = strings.Join(values, ", ")
	}

	return result
}

// ToYamlHttpHeader converts the http header into yaml format
func ToYamlHttpHeader(httpHeader http.Header) map[string]string {
	header := map[string]string{}
	for i, j := range httpHeader {
		header[i] = strings.Join(j, ",")
	}
	return header
}

func ToHttpHeader(mockHeader map[string]string) http.Header {
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
	s := strings.TrimSpace(stringDate)
	_, err := dateparse.ParseAny(s)
	return err == nil
}

func SimulateHttp(tc models.TestCase, testSet string, logger *zap.Logger, apiTimeout uint64) (*models.HttpResp, error) {
	resp := &models.HttpResp{}

	logger.Info("starting test for of", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))
	req, err := http.NewRequest(string(tc.HttpReq.Method), tc.HttpReq.URL, bytes.NewBufferString(tc.HttpReq.Body))
	if err != nil {
		logger.Error("failed to create a http request from the yaml document", zap.Error(err))
		return nil, err
	}
	req.Header = ToHttpHeader(tc.HttpReq.Header)
	req.Header.Set("KEPLOY-TEST-ID", tc.Name)
	req.ProtoMajor = tc.HttpReq.ProtoMajor
	req.ProtoMinor = tc.HttpReq.ProtoMinor

	logger.Debug(fmt.Sprintf("Sending request to user app:%v", req))

	// Creating the client and disabling redirects
	var client *http.Client

	keepAlive, ok := req.Header["Connection"]
	if ok && strings.EqualFold(keepAlive[0], "keep-alive") {
		logger.Debug("simulating request with connection:keep-alive")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	} else if ok && strings.EqualFold(keepAlive[0], "close") {
		logger.Debug("simulating request with connection:close")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
	} else {
		logger.Debug("simulating request with connection:keep-alive (maxIdleConn=1)")
		client = &http.Client{
			Timeout: time.Second * time.Duration(apiTimeout),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives: false,
				MaxIdleConns:      1,
			},
		}
	}

	httpResp, errHttpReq := client.Do(req)
	if httpResp != nil {
		// Cases covered, non-nil httpResp with non-nil errHttpReq and non-nil httpResp
		// with nil errHttpReq
		respBody, errReadRespBody := io.ReadAll(httpResp.Body)
		if errReadRespBody != nil {
			logger.Error("failed reading response body", zap.Error(errReadRespBody))
			return nil, err
		}

		resp = &models.HttpResp{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
			Header:     ToYamlHttpHeader(httpResp.Header),
		}
	} else if errHttpReq != nil {
		// Case covered, nil HTTP response with non-nil error
		logger.Error("failed sending testcase request to app", zap.Error(err))

		resp = &models.HttpResp{
			Body: errHttpReq.Error(),
		}
	}

	return resp, errHttpReq
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

// Generate unique random id
func GenerateRandomID() int {
	rand.Seed(time.Now().UnixNano())
	id := rand.Intn(1000000000) // Adjust the range as needed
	return id
}

func MakeCurlCommand(method string, url string, header map[string]string, body string) string {
	curl := fmt.Sprintf("curl --request %s \\\n", method)
	curl = curl + fmt.Sprintf("  --url %s \\\n", url)
	for k, v := range header {
		if k != "Content-Length" {
			curl = curl + fmt.Sprintf("  --header '%s: %s' \\\n", k, v)
		}
	}
	if body != "" {
		curl = curl + fmt.Sprintf("  --data '%s'", body)
	}
	return curl
}

func GetNextTestReportDir(testReportPath, subDirPrefix string) (string, error) {
	latestReportNumber := 0

	if _, err := os.Stat(testReportPath); !os.IsNotExist(err) {
		file, err := os.Open(testReportPath)
		if err != nil {
			return "", fmt.Errorf("failed to open directory: %w", err)
		}
		defer file.Close()

		files, err := file.Readdir(-1) // -1 to read all files and directories
		if err != nil {
			return "", fmt.Errorf("failed to read directory: %w", err)
		}

		for _, f := range files {
			if f.IsDir() && strings.HasPrefix(f.Name(), subDirPrefix) {
				reportNumber, err := strconv.Atoi(strings.TrimPrefix(f.Name(), subDirPrefix))
				if err != nil {
					return "", fmt.Errorf("failed to parse report number: %w", err)
				}
				if reportNumber > latestReportNumber {
					latestReportNumber = reportNumber
				}
			}
		}
		latestReportNumber++ // increment to create a new report directory
	}

	newTestReportPath := filepath.Join(testReportPath, fmt.Sprintf("%s%d", subDirPrefix, latestReportNumber))
	return newTestReportPath, nil
}
