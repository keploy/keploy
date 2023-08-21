package httpparser

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"time"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

// IsOutgoingHTTP function determines if the outgoing network call is HTTP by comparing the
// message format with that of an HTTP text message.
func IsOutgoingHTTP(buffer []byte) bool {
	return bytes.HasPrefix(buffer[:], []byte("HTTP/")) ||
		bytes.HasPrefix(buffer[:], []byte("GET ")) ||
		bytes.HasPrefix(buffer[:], []byte("POST ")) ||
		bytes.HasPrefix(buffer[:], []byte("PUT ")) ||
		bytes.HasPrefix(buffer[:], []byte("PATCH ")) ||
		bytes.HasPrefix(buffer[:], []byte("DELETE ")) ||
		bytes.HasPrefix(buffer[:], []byte("OPTIONS ")) ||
		bytes.HasPrefix(buffer[:], []byte("HEAD "))
}

func isJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}

func mapsHaveSameKeys(map1 map[string]string, map2 map[string][]string) bool {
	if len(map1) != len(map2) {
		return false
	}

	for key := range map1 {
		if _, exists := map2[key]; !exists {
			return false
		}
	}

	for key := range map2 {
		if _, exists := map1[key]; !exists {
			return false
		}
	}

	return true
}

func ProcessOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		// *deps = append(*deps, encodeOutgoingHttp(request,  clientConn,  destConn, logger))
		h.AppendMocks(encodeOutgoingHttp(request, clientConn, destConn, logger))
		// h.TestCaseDB.WriteMock(encodeOutgoingHttp(request, clientConn, destConn, logger))
	case models.MODE_TEST:
		decodeOutgoingHttp(request, clientConn, destConn, h, logger)
	default:
		logger.Info(Emoji+"Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

// decodeOutgoingHttp
func decodeOutgoingHttp(requestBuffer []byte, clienConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	//Matching algorithmm
	//Get the mocks
	tcsMocks := h.GetTcsMocks()
	var bestMatch string
	//Parse the request buffer
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
	if err != nil {
		logger.Error(Emoji+"failed to parse the http request message", zap.Error(err))
		return
	}

	reqbody, err := ioutil.ReadAll(req.Body)
	if err != nil {
		logger.Error(Emoji+"failed to read from request body", zap.Error(err))

	}

	//parse request url
	reqURL, err := url.Parse(req.URL.String())
	if err != nil {
		logger.Error(Emoji+"failed to parse request url", zap.Error(err))
	}

	//check if req body is a json
	isReqBodyJSON := isJSON(reqbody)

	var eligibleMock []*models.Mock

	for _, mock := range tcsMocks {
		isMockBodyJSON := isJSON([]byte(mock.Spec.HttpReq.Body))

		//the body of mock and request aren't of same type
		if isMockBodyJSON != isReqBodyJSON {
			continue
		}

		//parse request body url
		parsedURL, err := url.Parse(mock.Spec.HttpReq.URL)
		if err != nil {
			logger.Error(Emoji+"failed to parse mock url", zap.Error(err))
			continue
		}

		//Check if the path matches
		if parsedURL.Path != reqURL.Path {
			//If it is not the same, continue
			continue
		}

		//Check if the method matches
		if mock.Spec.HttpReq.Method != models.Method(req.Method) {
			//If it is not the same, continue
			continue
		}

		// Check if the header keys match
		if !mapsHaveSameKeys(mock.Spec.HttpReq.Header, req.Header) {
			// Different headers, so not a match
			continue
		}

		if !mapsHaveSameKeys(mock.Spec.HttpReq.URLParams, req.URL.Query()) {
			// Different query params, so not a match
			continue
		}
		eligibleMock = append(eligibleMock, mock)
	}

	if len(eligibleMock) == 0 {
		logger.Error(Emoji + "Didn't match any prexisting http mock")
		return
	}

	_, bestMatch = util.Fuzzymatch(eligibleMock, requestBuffer, h)

	var bestMatchIndex int
	for idx, mock := range tcsMocks {
		if mock.Spec.HttpReq.Body == bestMatch {
			bestMatchIndex = idx
		}
	}
	if h.GetDepsSize() == 0 {
		// logger.Error("failed to mock the output for unrecorded outgoing http call")
		return
	}

	// var httpSpec spec.HttpSpec
	// err := deps[0].Spec.Decode(&httpSpec)
	// if err != nil {
	// 	logger.Error("failed to decode the yaml spec for the outgoing http call")
	// 	return
	// }
	// httpSpec := deps[0]
	stub := h.FetchDep(0)
	// fmt.Println("http mock in test: ", stub)

	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HttpReq.ProtoMajor, stub.Spec.HttpReq.ProtoMinor, stub.Spec.HttpResp.StatusCode, http.StatusText(int(stub.Spec.HttpResp.StatusCode)))

	// Fetching the response headers
	header := pkg.ToHttpHeader(stub.Spec.HttpResp.Header)
	var headers string
	for key, values := range header {
		for _, value := range values {
			headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
			headers += headerLine
		}
	}
	body := stub.Spec.HttpResp.Body
	var responseString string

	//Check if the gzip encoding is present in the header
	if header["Content-Encoding"] != nil && header["Content-Encoding"][0] == "gzip" {
		var compressedBuffer bytes.Buffer
		gw := gzip.NewWriter(&compressedBuffer)
		_, err := gw.Write([]byte(body))
		if err != nil {
			logger.Error(Emoji+"failed to compress the response body", zap.Error(err))
			return
		}
		err = gw.Close()
		if err != nil {
			logger.Error(Emoji+"failed to close the gzip writer", zap.Error(err))
			return
		}
		responseString = statusLine + headers + "\r\n" + compressedBuffer.String()
	} else {
		responseString = statusLine + headers + "\r\n" + body
	}
	_, err = clienConn.Write([]byte(responseString))
	if err != nil {
		logger.Error(Emoji+"failed to write the mock output to the user application", zap.Error(err))
		return
	}
	// pop the mocked output from the dependency queue
	// deps = deps[1:]
	h.PopIndex(bestMatchIndex)
}

// encodeOutgoingHttp function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeOutgoingHttp(request []byte, clientConn, destConn net.Conn, logger *zap.Logger) *models.Mock {
	defer destConn.Close()
	var resp []byte
	var finalResp []byte
	var finalReq []byte
	var err error
	// write the request message to the actual destination server
	_, err = destConn.Write(request)
	if err != nil {
		logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err))
		return nil
	}
	finalReq = append(finalReq, request...)

	//Handle chunked requests
	handleChunkedRequests(&finalReq, clientConn, destConn, logger, request)

	// read the response from the actual server
	resp, err = util.ReadBytes(destConn)
	if err != nil {
		logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
		return nil
	}
	// write the response message to the user client
	_, err = clientConn.Write(resp)
	if err != nil {
		logger.Error(Emoji+"failed to write response message to the user client", zap.Error(err))
		return nil
	}
	finalResp = append(finalResp, resp...)
	handleChunkedResponses(&finalResp, clientConn, destConn, logger, resp)
	var req *http.Request
	// converts the request message buffer to http request
	req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(finalReq)))
	if err != nil {
		logger.Error(Emoji+"failed to parse the http request message", zap.Error(err))
		return nil
	}
	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			// TODO right way to log errors
			logger.Error(Emoji+"failed to read the http request body", zap.Error(err))
			return nil
		}
	}
	// converts the response message buffer to http response
	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(finalResp)), req)
	if err != nil {
		logger.Error(Emoji+"failed to parse the http response message", zap.Error(err))
		return nil
	}
	var respBody []byte
	if respParsed.Body != nil { // Read
		if respParsed.Header.Get("Content-Encoding") == "gzip" {
			check := respParsed.Body
			ok, reader := checkIfGzipped(check)
			fmt.Println("ok: ", ok)
			if ok {
				gzipReader, err := gzip.NewReader(reader)
				if err != nil {
					logger.Error(Emoji+"failed to create a gzip reader", zap.Error(err))
					return nil
				}
				respParsed.Body = gzipReader
			}
		}
		respBody, err = io.ReadAll(respParsed.Body)
		if err != nil {
			logger.Error(Emoji+"failed to read the the http repsonse body", zap.Error(err))
			return nil
		}
	}
	// store the request and responses as mocks
	meta := map[string]string{
		"name":      "Http",
		"type":      models.HttpClient,
		"operation": req.Method,
	}
	// httpMock := &models.Mock{
	// 	Version: models.V1Beta2,
	// 	Name:    "",
	// 	Kind:    models.HTTP,
	// }

	// // encode the message into yaml
	// err = httpMock.Spec.Encode(&spec.HttpSpec{
	// 		Metadata: meta,
	// 		Request: spec.HttpReqYaml{
	// 			Method:     spec.Method(req.Method),
	// 			ProtoMajor: req.ProtoMajor,
	// 			ProtoMinor: req.ProtoMinor,
	// 			URL:        req.URL.String(),
	// 			Header:     pkg.ToYamlHttpHeader(req.Header),
	// 			Body:       string(reqBody),
	// 			URLParams: pkg.UrlParams(req),
	// 		},
	// 		Response: spec.HttpRespYaml{
	// 			StatusCode: resp.StatusCode,
	// 			Header:     pkg.ToYamlHttpHeader(resp.Header),
	// 			Body: string(respBody),
	// 		},
	// })
	// if err != nil {
	// 	logger.Error("failed to encode the http messsage into the yaml")
	// 	return nil
	// }

	// return httpMock

	return &models.Mock{
		Version: models.V1Beta2,
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HttpReq: &models.HttpReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHttpHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.UrlParams(req),
			},
			HttpResp: &models.HttpResp{
				StatusCode: respParsed.StatusCode,
				Header:     pkg.ToYamlHttpHeader(respParsed.Header),
				Body:       string(respBody),
			},
			Created: time.Now().Unix(),
		},
	}

	// if val, ok := Deps[string(port)]; ok {
	// keploy.Deps = append(keploy.Deps, httpMock)
}
