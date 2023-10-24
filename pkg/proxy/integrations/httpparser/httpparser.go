package httpparser

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

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

func ProcessOutgoingHttp(request []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		err := encodeOutgoingHttp(request, clientConn, destConn, logger, h, ctx)
		if err != nil {
			logger.Error("failed to encode the http message into the yaml", zap.Error(err))
			return
		}

	case models.MODE_TEST:
		decodeOutgoingHttp(request, clientConn, destConn, h, logger)
	default:
		logger.Info("Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

// Handled chunked requests when content-length is given.
func contentLengthRequest(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, contentLength int) {
	for contentLength > 0 {
		clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		requestChunked, err := util.ReadBytes(clientConn)
		if err != nil {
			if err == io.EOF {
				logger.Error("connection closed by the user client", zap.Error(err))
				break
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Info("Stopped getting data from the connection", zap.Error(err))
				break
			} else {
				logger.Error("failed to read the response message from the destination server", zap.Error(err))
				return
			}
		}
		logger.Debug("This is a chunk of request[content-length]: " + string(requestChunked))
		*finalReq = append(*finalReq, requestChunked...)
		contentLength -= len(requestChunked)
		_, err = destConn.Write(requestChunked)
		if err != nil {
			logger.Error("failed to write request message to the destination server", zap.Error(err))
			return
		}
	}
}

// Handled chunked requests when transfer-encoding is given.
func chunkedRequest(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, transferEncodingHeader string) {
	if transferEncodingHeader == "chunked" {
		for {
			clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			requestChunked, err := util.ReadBytes(clientConn)
			if err != nil && err != io.EOF {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					break
				} else {
					logger.Error("failed to read the response message from the destination server", zap.Error(err))
					return
				}
			}
			*finalReq = append(*finalReq, requestChunked...)
			_, err = destConn.Write(requestChunked)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return
			}
			if string(requestChunked) == "0\r\n\r\n" {
				break
			}
		}
	}
}

// Handled chunked responses when content-length is given.
func contentLengthResponse(finalResp *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, contentLength int) {
	for contentLength > 0 {
		//Set deadline of 5 seconds
		destConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := util.ReadBytes(destConn)
		if err != nil {
			//Check if the connection closed.
			if err == io.EOF {
				logger.Error("connection closed by the destination server", zap.Error(err))
				break
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Info("Stopped getting data from the connection", zap.Error(err))
				break
			} else {
				logger.Error("failed to read the response message from the destination server", zap.Error(err))
				return
			}
		}
		logger.Debug("This is a chunk of response[content-length]: " + string(resp))
		*finalResp = append(*finalResp, resp...)
		contentLength -= len(resp)
		// write the response message to the user client
		_, err = clientConn.Write(resp)
		if err != nil {
			logger.Error("failed to write response message to the user client", zap.Error(err))
			return
		}
	}
}

// Handled chunked responses when transfer-encoding is given.
func chunkedResponse(finalResp *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, transferEncodingHeader string) {
	if transferEncodingHeader == "chunked" {
		for {
			//Set deadline of 5 seconds
			destConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := util.ReadBytes(destConn)

			if err != nil {
				//Check if the connection closed.
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					//Check if the deadline is reached.
					logger.Info("Stopped getting buffer from the destination server")
					break
				} else if err == io.EOF {
					if len(resp) == 0 {
						logger.Debug("complete response came in the first chunk")
						return
					}
					logger.Debug("successfully read complete chunk from the destination server")
				} else {
					logger.Warn("failed to read the response message from the destination server", zap.Error(err))
					if len(resp) == 0 {
						logger.Debug("didn't get any response chunk from destination server")
						return
					}
				}
			}

			*finalResp = append(*finalResp, resp...)
			// write the response message to the user client
			_, err = clientConn.Write(resp)
			if err != nil {
				logger.Error("failed to write response message to the user client", zap.Error(err))
				return
			}

			if strings.HasSuffix(string(resp), "0\r\n\r\n") {
				break
			}
		}
	}
}

func handleChunkedRequests(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger) {
	lines := strings.Split(string(*finalReq), "\n")
	var contentLengthHeader string
	var transferEncodingHeader string
	for _, line := range lines {
		if strings.HasPrefix(line, "Content-Length:") {
			contentLengthHeader = strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			break
		} else if strings.HasPrefix(line, "Transfer-Encoding:") {
			transferEncodingHeader = strings.TrimSpace(strings.TrimPrefix(line, "Transfer-Encoding:"))
			break
		}
	}
	//Handle chunked requests
	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			logger.Error("failed to get the content-length header", zap.Error(err))
			return
		}
		//Get the length of the body in the request.
		bodyLength := len(*finalReq) - strings.Index(string(*finalReq), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			contentLengthRequest(finalReq, clientConn, destConn, logger, contentLength)
		}
	} else if transferEncodingHeader != "" {
		chunkedRequest(finalReq, clientConn, destConn, logger, transferEncodingHeader)
	}
}

func handleChunkedResponses(finalResp *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, resp []byte) {
	//Getting the content-length or the transfer-encoding header
	var contentLengthHeader, transferEncodingHeader string
	lines := strings.Split(string(resp), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Content-Length:") {
			contentLengthHeader = strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			break
		} else if strings.HasPrefix(line, "Transfer-Encoding:") {
			transferEncodingHeader = strings.TrimSpace(strings.TrimPrefix(line, "Transfer-Encoding:"))
			break
		}
	}
	//Handle chunked responses
	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			logger.Error("failed to get the content-length header", zap.Error(err))
			return
		}
		bodyLength := len(resp) - strings.Index(string(resp), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			contentLengthResponse(finalResp, clientConn, destConn, logger, contentLength)
		}
	} else if transferEncodingHeader != "" {
		chunkedResponse(finalResp, clientConn, destConn, logger, transferEncodingHeader)
	}
}

// Checks if the response is gzipped
func checkIfGzipped(check io.ReadCloser) (bool, *bufio.Reader) {
	bufReader := bufio.NewReader(check)
	peekedBytes, err := bufReader.Peek(2)
	if err != nil && err != io.EOF {
		log.Debug("Error peeking:", err)
		return false, nil
	}
	if len(peekedBytes) < 2 {
		return false, nil
	}
	if peekedBytes[0] == 0x1f && peekedBytes[1] == 0x8b {
		return true, bufReader
	} else {
		return false, nil
	}
}

// Decodes the mocks in test mode so that they can be sent to the user application.
func decodeOutgoingHttp(requestBuffer []byte, clienConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	//Matching algorithmm
	//Get the mocks
	for {
		tcsMocks := h.GetTcsMocks()
		var bestMatch *models.Mock
		//Check if the expected header is present
		if bytes.Contains(requestBuffer, []byte("Expect: 100-continue")) {
			//Send the 100 continue response
			_, err := clienConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
			if err != nil {
				logger.Error("failed to write the 100 continue response to the user application", zap.Error(err))
				return
			}
			//Read the request buffer again
			newRequest, err := util.ReadBytes(clienConn)
			if err != nil {
				logger.Error("failed to read the request buffer from the user application", zap.Error(err))
				return
			}
			//Append the new request buffer to the old request buffer
			requestBuffer = append(requestBuffer, newRequest...)
		}
		handleChunkedRequests(&requestBuffer, clienConn, destConn, logger)
		//Parse the request buffer
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
		if err != nil {
			logger.Error("failed to parse the http request message", zap.Error(err))
			return
		}

		reqbody, err := ioutil.ReadAll(req.Body)
		if err != nil {
			logger.Error("failed to read from request body", zap.Error(err))

		}

		//parse request url
		reqURL, err := url.Parse(req.URL.String())
		if err != nil {
			logger.Error("failed to parse request url", zap.Error(err))
		}

		//check if req body is a json
		isReqBodyJSON := isJSON(reqbody)

		var eligibleMock []*models.Mock

		for _, mock := range tcsMocks {
			if mock.Kind == models.HTTP {
				isMockBodyJSON := isJSON([]byte(mock.Spec.HttpReq.Body))

				//the body of mock and request aren't of same type
				if isMockBodyJSON != isReqBodyJSON {
					continue
				}

				//parse request body url
				parsedURL, err := url.Parse(mock.Spec.HttpReq.URL)
				if err != nil {
					logger.Error("failed to parse mock url", zap.Error(err))
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
		}

		if len(eligibleMock) == 0 {
			logger.Error("Didn't match any prexisting http mock")
			util.Passthrough(clienConn, destConn, [][]byte{requestBuffer}, h.Recover, logger)
			return
		}

		_, bestMatch = util.Fuzzymatch(eligibleMock, requestBuffer, h)

		var bestMatchIndex int
		for idx, mock := range tcsMocks {
			if reflect.DeepEqual(mock, bestMatch) {
				bestMatchIndex = idx
				break
			}
		}
		if h.GetDepsSize() == 0 {
			return
		}
		stub := h.FetchDep(bestMatchIndex)

		statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HttpReq.ProtoMajor, stub.Spec.HttpReq.ProtoMinor, stub.Spec.HttpResp.StatusCode, http.StatusText(int(stub.Spec.HttpResp.StatusCode)))

		body := stub.Spec.HttpResp.Body
		var respBody string
		var responseString string

		// Fetching the response headers
		header := pkg.ToHttpHeader(stub.Spec.HttpResp.Header)

		//Check if the gzip encoding is present in the header
		if header["Content-Encoding"] != nil && header["Content-Encoding"][0] == "gzip" {
			var compressedBuffer bytes.Buffer
			gw := gzip.NewWriter(&compressedBuffer)
			_, err := gw.Write([]byte(body))
			if err != nil {
				logger.Error("failed to compress the response body", zap.Error(err))
				return
			}
			err = gw.Close()
			if err != nil {
				logger.Error("failed to close the gzip writer", zap.Error(err))
				return
			}
			logger.Debug("the length of the response body: " + strconv.Itoa(len(compressedBuffer.String())))
			respBody = compressedBuffer.String()
		} else {
			respBody = body
		}
		var headers string
		for key, values := range header {
			if key == "Content-Length" {
				values = []string{strconv.Itoa(len(respBody))}
			}
			for _, value := range values {
				headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
				headers += headerLine
			}
		}
		responseString = statusLine + headers + "\r\n" + "" + respBody

		logger.Debug(fmt.Sprintf("The response headers are:\n%v", headers))

		_, err = clienConn.Write([]byte(responseString))
		if err != nil {
			logger.Error("failed to write the mock output to the user application", zap.Error(err))
			return
		}
		h.PopIndex(bestMatchIndex)

		requestBuffer, err = util.ReadBytes(clienConn)
		if err != nil {
			logger.Debug("failed to read the request buffer from the client", zap.Error(err))
			logger.Debug("This was the last response from the server: " + string(responseString))
			break
		}

	}

}

// encodeOutgoingHttp function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeOutgoingHttp(request []byte, clientConn, destConn net.Conn, logger *zap.Logger, h *hooks.Hook, ctx context.Context) error {
	var resp []byte
	var finalResp []byte
	var finalReq []byte
	var err error
	defer destConn.Close()
	//Writing the request to the server.
	_, err = destConn.Write(request)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	logger.Debug("This is the initial request: " + string(request))
	finalReq = append(finalReq, request...)
	for {
		//check if the expect : 100-continue header is present
		lines := strings.Split(string(finalReq), "\n")
		var expectHeader string
		for _, line := range lines {
			if strings.HasPrefix(line, "Expect:") {
				expectHeader = strings.TrimSpace(strings.TrimPrefix(line, "Expect:"))
				break
			}
		}
		if expectHeader == "100-continue" {
			//Read if the response from the server is 100-continue
			resp, err = util.ReadBytes(destConn)
			if err != nil {
				logger.Error("failed to read the response message from the server after 100-continue request", zap.Error(err))
				return err
			}
			// write the response message to the client
			_, err = clientConn.Write(resp)
			if err != nil {
				logger.Error("failed to write response message to the user client", zap.Error(err))
				return err
			}
			logger.Debug("This is the response from the server after the expect header" + string(resp))
			if string(resp) != "HTTP/1.1 100 Continue\r\n\r\n" {
				logger.Error("failed to get the 100 continue response from the user client")
				return err
			}
			//Reading the request buffer again
			request, err = util.ReadBytes(clientConn)
			if err != nil {
				logger.Error("failed to read the request message from the user client", zap.Error(err))
				return err
			}
			// write the request message to the actual destination server
			_, err = destConn.Write(request)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return err
			}
			finalReq = append(finalReq, request...)
		}
		
		// Capture the request timestamp
		reqTimestampMock := time.Now()

		handleChunkedRequests(&finalReq, clientConn, destConn, logger)		
		// read the response from the actual server
		resp, err = util.ReadBytes(destConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("Response complete, exiting the loop.")
				break
			} else {
				logger.Error("failed to read the response message from the destination server", zap.Error(err))
				return err
			}
		}

		// Capturing the response timestamp
		resTimestampcMock := time.Now()
		// write the response message to the user client
		_, err = clientConn.Write(resp)
		if err != nil {
			logger.Error("failed to write response message to the user client", zap.Error(err))
			return err
		}
		finalResp = append(finalResp, resp...)
		logger.Debug("This is the initial response: " + string(resp))
		handleChunkedResponses(&finalResp, clientConn, destConn, logger, resp)
		logger.Debug("This is the final response: " + string(finalResp))

		var req *http.Request
		// converts the request message buffer to http request
		req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(finalReq)))
		if err != nil {
			logger.Error("failed to parse the http request message", zap.Error(err))
			return err
		}
		var reqBody []byte
		if req.Body != nil { // Read
			var err error
			reqBody, err = io.ReadAll(req.Body)
			if err != nil {
				// TODO right way to log errors
				logger.Error("failed to read the http request body", zap.Error(err))
				return err
			}
		}
		// converts the response message buffer to http response
		respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(finalResp)), req)
		if err != nil {
			logger.Error("failed to parse the http response message", zap.Error(err))
			return err
		}
		//Add the content length to the headers.
		var respBody []byte
		//Checking if the body of the response is empty or does not exist.


		if respParsed.Body != nil { // Read
			if respParsed.Header.Get("Content-Encoding") == "gzip" {
				check := respParsed.Body
				ok, reader := checkIfGzipped(check)
				logger.Debug("The body is gzip or not" + strconv.FormatBool(ok))
				logger.Debug("", zap.Any("isGzipped", ok))
				if ok {
					gzipReader, err := gzip.NewReader(reader)
					if err != nil {
						logger.Error("failed to create a gzip reader", zap.Error(err))
						return err
					}
					respParsed.Body = gzipReader
				}
			}
			respBody, err = io.ReadAll(respParsed.Body)
			if err != nil {
				logger.Error("failed to read the the http response body", zap.Error(err))
				return err
			}
			logger.Debug("This is the response body: " + string(respBody))
			//Add the content length to the headers.
			respParsed.Header.Add("Content-Length", strconv.Itoa(len(respBody)))
		}
		// store the request and responses as mocks
		meta := map[string]string{
			"name":      "Http",
			"type":      models.HttpClient,
			"operation": req.Method,
		}

		h.AppendMocks(&models.Mock{
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
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: resTimestampcMock,
			},
		}, ctx)
		finalReq = []byte("")
		finalResp = []byte("")

		finalReq, err = util.ReadBytes(clientConn)
		if err != nil {
			if err != io.EOF {
				logger.Debug("failed to read the request message from the user client", zap.Error(err))
				logger.Debug("This was the last response from the server: " + string(resp))
			}
			break
		}
		// write the request message to the actual destination server
		_, err = destConn.Write(finalReq)
		if err != nil {
			logger.Error("failed to write request message to the destination server", zap.Error(err))
			return err
		}
	}
	return nil
}
