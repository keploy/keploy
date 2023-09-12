package pkg

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
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

func SimulateHttp(tc models.TestCase, logger *zap.Logger) (*models.HttpResp, error) {
	resp := &models.HttpResp{}

	logger.Info("making a http request", zap.Any("test case id", tc.Name))
	req, err := http.NewRequest(string(tc.HttpReq.Method), tc.HttpReq.URL, bytes.NewBufferString(tc.HttpReq.Body))
	if err != nil {
		logger.Error("failed to create a http request from the yaml document", zap.Error(err))
		return nil, err
	}
	req.Header = ToHttpHeader(tc.HttpReq.Header)
	req.Header.Set("KEPLOY_TEST_ID", tc.Name)
	req.ProtoMajor = tc.HttpReq.ProtoMajor
	req.ProtoMinor = tc.HttpReq.ProtoMinor
	req.Close = true

	logger.Debug(fmt.Sprintf("Sending request to user app:%v", req))

	// Creating the client and disabling redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	httpResp, err := client.Do(req)
	if err != nil {
		logger.Error("failed sending testcase request to app", zap.Error(err))
		return nil, err
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		logger.Error("failed reading response body", zap.Error(err))
		return nil, err
	}

	resp = &models.HttpResp{
		StatusCode: httpResp.StatusCode,
		Body:       string(respBody),
		Header:     ToYamlHttpHeader(httpResp.Header),
	}

	return resp, nil
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
