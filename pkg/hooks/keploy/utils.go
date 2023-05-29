package keploy

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
)

// import (
// 	"bufio"
// 	"bytes"
// 	"fmt"
// 	"io/ioutil"
// 	"net"

// 	// "fmt"
// 	"go.keploy.io/server/http/regression"
// 	"go.keploy.io/server/pkg/models"
// 	"net/http"
// 	"strings"
// 	"time"
// )

// func CaptureHttpTC(k *Keploy, req *http.Request, resp *http.Response) {

// 	reqBody, err1 := getRequestBody(req)
// 	if err1 != nil {
// 		fmt.Println("unable to extract request body:", err1)
// 		return
// 	}

// 	resBody, err2 := getResponseBody(resp)

// 	if err2 != nil {
// 		fmt.Println("unable to extract response body:", err2)
// 		return
// 	}

// 	fmt.Println("inside CaptureHttpTC")

// 	k.Capture(regression.TestCaseReq{
// 		Captured: time.Now().Unix(),
// 		AppID:    k.cfg.App.Name,
// 		URI:      urlPath(req.URL.Path, urlParams(req)),
// 		HttpReq: models.HttpReq{
// 			Method:     models.Method(req.Method),
// 			ProtoMajor: req.ProtoMajor,
// 			ProtoMinor: req.ProtoMinor,
// 			URL:        req.URL.String(),
// 			URLParams:  urlParams(req),
// 			Header:     req.Header,
// 			Body:       reqBody,
// 		},
// 		HttpResp: models.HttpResp{
// 			StatusCode:    resp.StatusCode,
// 			StatusMessage: resp.Status,
// 			ProtoMajor:    resp.ProtoMajor,
// 			ProtoMinor:    resp.ProtoMinor,
// 			Header:        resp.Header,
// 			Body:          resBody,
// 		},
// 		TestCasePath: k.cfg.App.TestPath,
// 		Type:         models.HTTP,
// 	})
// }

func ParseHTTPRequest(requestBytes []byte) (*http.Request, error) {

	// Parse the request using the http.ReadRequest function
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBytes)))
	if err != nil {
		fmt.Println("[ParseHTTPRequest]:error parsing http request:%v", err)
		return nil, err
	}

	return request, nil
}

// func urlParams(req *http.Request) map[string]string {
// 	// Retrieve the URL query parameters as a map
// 	queryValues := req.URL.Query()
// 	queryParams := make(map[string]string)
// 	for key, values := range queryValues {
// 		queryParams[key] = values[0]
// 	}
// 	return queryParams
// }

// func urlPath(url string, params map[string]string) string {
// 	res := url
// 	for i, j := range params {
// 		res = strings.Replace(res, "/"+j+"/", "/:"+i+"/", -1)
// 		if strings.HasSuffix(res, "/"+j) {
// 			res = strings.TrimSuffix(res, "/"+j) + "/:" + i
// 		}
// 	}
// 	return res
// }

// func getRequestBody(req *http.Request) (string, error) {
// 	method := req.Method

// 	if method == "GET" || method == "DELETE" {
// 		return "", nil
// 	}
// 	// Read the request body
// 	bodyBytes, err := ioutil.ReadAll(req.Body)
// 	if err != nil {
// 		return "", err
// 	}

// 	// Restore the original request body
// 	req.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

// 	// Convert the body bytes to a string and return it
// 	bodyString := string(bodyBytes)
// 	return bodyString, nil
// }

// func getResponseBody(resp *http.Response) (string, error) {
// 	defer resp.Body.Close()

// 	body, err := ioutil.ReadAll(resp.Body)
// 	if err != nil {
// 		return "", err
// 	}

// 	return string(body), nil
// }

// func ExtractPortFromHost(host string) (string, error) {
// 	_, port, err := net.SplitHostPort(host)
// 	if err != nil {
// 		return "", err
// 	}

// 	return port, nil
// }

func ParseHTTPResponse(data []byte, request *http.Request) (*http.Response, error) {
	buffer := bytes.NewBuffer(data)
	reader := bufio.NewReader(buffer)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}
