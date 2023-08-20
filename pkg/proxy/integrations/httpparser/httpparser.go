package httpparser

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type Response struct {
}

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

func ProcessOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		// *deps = append(*deps, encodeOutgoingHttp(requestBuffer,  clientConn,  destConn, logger))
		h.AppendMocks(encodeOutgoingHttp(requestBuffer, clientConn, destConn, logger))
		// h.TestCaseDB.WriteMock(encodeOutgoingHttp(requestBuffer, clientConn, destConn, logger))
	case models.MODE_TEST:
		decodeOutgoingHttp(requestBuffer, clientConn, destConn, h, logger)
	default:
		logger.Info(Emoji+"Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

// decodeOutgoingHttp
func decodeOutgoingHttp(requestBuffer []byte, clienConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	// if len(deps) == 0 {

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
	httpSpec := h.FetchDep(0)
	// fmt.Println("http mock in test: ", httpSpec)

	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", httpSpec.Spec.HttpReq.ProtoMajor, httpSpec.Spec.HttpReq.ProtoMinor, httpSpec.Spec.HttpResp.StatusCode, http.StatusText(int(httpSpec.Spec.HttpResp.StatusCode)))

	// Generate the response headers
	header := pkg.ToHttpHeader(httpSpec.Spec.HttpResp.Header)
	var headers string
	for key, values := range header {
		for _, value := range values {
			headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
			headers += headerLine
		}
	}

	body := httpSpec.Spec.HttpResp.Body
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
	responseString := statusLine + headers + "\r\n" + compressedBuffer.String()
	_, err = clienConn.Write([]byte(responseString))
	if err != nil {
		logger.Error(Emoji+"failed to write the mock output to the user application", zap.Error(err))
		return
	}
	// pop the mocked output from the dependency queue
	// deps = deps[1:]
	h.PopFront()
}

// encodeOutgoingHttp function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) *models.Mock {
	defer destConn.Close()

	var respBuffer []byte
	var finalRespBuffer []byte
	var finalRequestBuffer []byte
	var err error
	// write the request message to the actual destination server
	_, err = destConn.Write(requestBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err))
		return nil
	}
	finalRequestBuffer = append(finalRequestBuffer, requestBuffer...)
	lines := strings.Split(string(requestBuffer), "\n")
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
			logger.Error(Emoji+"failed to get the content-length header", zap.Error(err))
			return nil
		}
		for contentLength > 0 {
			clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			requestBufferChunked, err := util.ReadBytes(clientConn)
			if err != nil {
				if err == io.EOF {
					logger.Error(Emoji+"connection closed by the user client", zap.Error(err))
					break
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Info(Emoji+"Stopped getting data from the connection", zap.Error(err))
					break
				} else {
					logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
					return nil
				}
			}
			finalRequestBuffer = append(finalRequestBuffer, requestBufferChunked...)
			contentLength -= len(requestBufferChunked)
			_, err = destConn.Write(requestBufferChunked)
			if err != nil {
				logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err))
				return nil
			}
		}
	} else if transferEncodingHeader != "" {
		for {
			clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			requestBufferChunked, err := util.ReadBytes(clientConn)
			if err != nil {
				if err == io.EOF {
					logger.Error(Emoji+"connection closed by the user client", zap.Error(err))
					break
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					break
				} else {
					logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
					return nil
				}
			}
			finalRequestBuffer = append(finalRequestBuffer, requestBufferChunked...)
			_, err = destConn.Write(requestBufferChunked)
			if err != nil {
				logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err))
				return nil
			}
			if string(requestBufferChunked) == "0\r\n\r\n" {
				break
			}
		}
	}

	// read the response from the actual server
	respBuffer, err = util.ReadBytes(destConn)
	if err != nil {
		logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
		return nil
	}
	// write the response message to the user client
	_, err = clientConn.Write(respBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write response message to the user client", zap.Error(err))
		return nil
	}
	finalRespBuffer = append(finalRespBuffer, respBuffer...)
	//Getting the content-length or the transfer-encoding header
	lines = strings.Split(string(respBuffer), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Content-Length:") {
			contentLengthHeader = strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			break
		} else if strings.HasPrefix(line, "Transfer-Encoding:") {
			transferEncodingHeader = strings.TrimSpace(strings.TrimPrefix(line, "Transfer-Encoding:"))
			break
		}
	}
	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			logger.Error(Emoji+"failed to get the content-length header", zap.Error(err))
			return nil
		}
		for contentLength > 0 {
			//Set deadline of 5 seconds
			destConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			respBuffer, err = util.ReadBytes(destConn)
			if err != nil {
				//Check if the connection closed.
				if err == io.EOF {
					logger.Error(Emoji+"connection closed by the destination server", zap.Error(err))
					break
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Info(Emoji+"Stopped getting data from the connection", zap.Error(err))
					break
				} else {
					logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
					return nil
				}
			}
			finalRespBuffer = append(finalRespBuffer, respBuffer...)
			contentLength -= len(respBuffer)
			// write the response message to the user client
			_, err = clientConn.Write(respBuffer)
			if err != nil {
				logger.Error(Emoji+"failed to write response message to the user client", zap.Error(err))
				return nil
			}
		}
	} else if transferEncodingHeader != "" {
		//If the transfer-encoding header is chunked
		if transferEncodingHeader == "chunked" {
			for {
				//Set deadline of 5 seconds
				destConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				respBuffer, err = util.ReadBytes(destConn)
				if err != nil {
					//Check if the connection closed.
					if err == io.EOF {
						logger.Error(Emoji+"connection closed by the destination server", zap.Error(err))
						break
					} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						//Check if the deadline is reached.
						logger.Info(Emoji + "Stopped getting buffer from the destination server")
						break
					} else {
						logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
						return nil
					}
				}
				finalRespBuffer = append(finalRespBuffer, respBuffer...)
				// write the response message to the user client
				_, err = clientConn.Write(respBuffer)
				if err != nil {
					logger.Error(Emoji+"failed to write response message to the user client", zap.Error(err))
					return nil
				}
				if string(respBuffer) == "0\r\n\r\n" {
					break
				}
			}
		}

	}
	var req *http.Request
	// converts the request message buffer to http request
	req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(finalRequestBuffer)))
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
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(finalRespBuffer)), req)
	if err != nil {
		logger.Error(Emoji+"failed to parse the http response message", zap.Error(err))
		return nil
	}
	var respBody []byte
	if resp.Body != nil { // Read
		if resp.Header.Get("Content-Encoding") == "gzip" {
			resp.Body, err = gzip.NewReader(resp.Body)
			if err != nil {
				logger.Error(Emoji+"failed to read the the http response body", zap.Error(err))
				return nil
			}
		}
		respBody, err = io.ReadAll(resp.Body)
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
				StatusCode: resp.StatusCode,
				Header:     pkg.ToYamlHttpHeader(resp.Header),
				Body:       string(respBody),
			},
			Created: time.Now().Unix(),
		},
	}

	// if val, ok := Deps[string(port)]; ok {
	// keploy.Deps = append(keploy.Deps, httpMock)
}
