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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/cfssl/log"
	"github.com/fatih/color"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

type HttpParser struct {
	logger        *zap.Logger
	hooks         *hooks.Hook
	baseUrl       string
	MockAssert    bool
	ReplaySession uint64
}

// ProcessOutgoing implements proxy.DepInterface.
func (http *HttpParser) ProcessOutgoing(request []byte, clientConn, destConn net.Conn, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		err := encodeOutgoingHttp(request, clientConn, destConn, http.logger, http.hooks, ctx)
		if err != nil {
			http.logger.Error("failed to encode the http message into the yaml", zap.Error(err))
			return
		}

	case models.MODE_TEST:
		decodeOutgoingHttp(request, clientConn, destConn, http.hooks, http.logger, http.baseUrl, http.MockAssert, http.ReplaySession)
	default:
		http.logger.Info("Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

func NewHttpParser(logger *zap.Logger, h *hooks.Hook, baseUrl string, mockAssert bool, replaySession uint64) *HttpParser {
	return &HttpParser{
		logger:        logger,
		hooks:         h,
		baseUrl:       baseUrl,
		MockAssert:    mockAssert,
		ReplaySession: replaySession,
	}
}

// IsOutgoingHTTP function determines if the outgoing network call is HTTP by comparing the
// message format with that of an HTTP text message.
func (h *HttpParser) OutgoingType(buffer []byte) bool {
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
	var rv, rv2 string
	for key, v := range map1 {
		// if _, exists := map2[key]; !exists {
		// 	return false
		// }
		if key == "Keploy-Header" {
			rv = v
		}
	}

	for key, v := range map2 {
		// if _, exists := map1[key]; !exists {
		// 	return false
		// }
		if key == "Keploy-Header" {
			rv2 = v[0]
		}
	}
	if rv != rv2 {
		return mapsHaveSameKeys2(map1, map2)
	}
	return true
}
func mapsHaveSameKeys2(map1 map[string]string, map2 map[string][]string) bool {
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

// Handled chunked requests when content-length is given.
func contentLengthRequest(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, contentLength int, writeDest bool) {
	for contentLength > 0 {
		clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
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
		if writeDest {
			_, err = destConn.Write(requestChunked)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return
			}
		}
	}
	clientConn.SetReadDeadline(time.Time{})
}

// Handled chunked requests when transfer-encoding is given.
func chunkedRequest(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, transferEncodingHeader string) {
	if transferEncodingHeader == "chunked" {
		for {
			clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
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

			//check if the intial request is completed
			if strings.HasSuffix(string(requestChunked), "0\r\n\r\n") {
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
	destConn.SetReadDeadline(time.Time{})
}

// Handled chunked responses when transfer-encoding is given.
func chunkedResponse(chunkedTime *[]int64, chunkedLength *[]int, finalResp *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, transferEncodingHeader string) {
	if transferEncodingHeader == "chunked" {
		for {
			resp, err := util.ReadBytes(destConn)
			if err != nil {
				if err != io.EOF {
					logger.Debug("failed to read the response message from the destination server", zap.Error(err))
					return
				} else {
					logger.Debug("recieved EOF, exiting loop as response is complete", zap.Error(err))
					break
				}
			}

			// get all the chunks mapped with that time at least get the number of chunks at that time
			if len(resp) != 0 {
				t := time.Now().UnixMilli()
				//Get the length of the chunk <- check this

				count, err := countHTTPChunks(resp)
				if err != nil {
					logger.Error("Error extracting length: %s", zap.Any("countHTTPChunks", err.Error()))
				}
				logger.Debug("Count ", zap.Any("count", count))
				*chunkedTime = append(*chunkedTime, t)
				*chunkedLength = append(*chunkedLength, count)
			}

			//get the hexa decimal and then convert it to length
			*finalResp = append(*finalResp, resp...)
			logger.Debug("This is a chunk of response[chunked]: " + string(resp))
			// write the response message to the user client

			_, err = clientConn.Write(resp)
			if err != nil {
				logger.Error("failed to write response message to the user client", zap.Error(err))
				return
			}
			if string(resp) == "0\r\n\r\n" {
				break
			}
		}
	}
}
func handleChunkedRequests(finalReq *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, writeDest bool) {
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
			contentLengthRequest(finalReq, clientConn, destConn, logger, contentLength, writeDest)
		}
	} else if transferEncodingHeader != "" {
		// check if the intial request is the complete request.
		if strings.HasSuffix(string(*finalReq), "0\r\n\r\n") {
			return
		}
		chunkedRequest(finalReq, clientConn, destConn, logger, transferEncodingHeader)
	}
}

func handleChunkedResponses(chunkedTime *[]int64, chunkedLength *[]int, finalResp *[]byte, clientConn, destConn net.Conn, logger *zap.Logger, resp []byte) {
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
		//check if the intial response is the complete response.
		if strings.HasSuffix(string(*finalResp), "0\r\n\r\n") {
			return
		}

		chunkedResponse(chunkedTime, chunkedLength, finalResp, clientConn, destConn, logger, transferEncodingHeader)
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
func decodeOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, baseUrl string, mockAssert bool, replaySession uint64) {
	//Matching algorithmm
	//Get the mocks
	tcsMocks, err := h.GetTcsMocks()
	if err != nil {
		logger.Error("failed to get the mocks from the yamls", zap.Error(err))
		return
	}

	var basePoints int
	if baseUrl != "" {
		// sort the mocks based on the timestamp in the metadata
		// and then send the response to the user in the same order application
		// and as soon as the mocks of the base url gets empty just exit the loop

		var baseMocks []*models.Mock
		for _, mock := range tcsMocks {
			if mock.Spec.HttpReq.URL == baseUrl {
				baseMocks = append(baseMocks, mock)
			}
		}
		basePoints = len(baseMocks)
	}

	for {

		//Check if the expected header is present
		if bytes.Contains(requestBuffer, []byte("Expect: 100-continue")) {
			//Send the 100 continue response
			_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
			if err != nil {
				logger.Error("failed to write the 100 continue response to the user application", zap.Error(err))
				return
			}
			//Read the request buffer again
			newRequest, err := util.ReadBytes(clientConn)
			if err != nil {
				logger.Error("failed to read the request buffer from the user application", zap.Error(err))
				return
			}
			//Append the new request buffer to the old request buffer
			requestBuffer = append(requestBuffer, newRequest...)
		}
		handleChunkedRequests(&requestBuffer, clientConn, destConn, logger, false)

		//Parse the request buffer
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
		if err != nil {
			logger.Error("failed to parse the http request message", zap.Error(err))
			return
		}

		reqBody, err := ioutil.ReadAll(req.Body)
		if err != nil {
			logger.Error("failed to read from request body", zap.Error(err))

		}

		//parse request url
		reqURL, err := url.Parse(req.URL.String())
		if err != nil {
			logger.Error("failed to parse request url", zap.Error(err))
		}

		//check if req body is a json
		isReqBodyJSON := isJSON(reqBody)

		isMatched, stub, err := match(req, reqBody, reqURL, isReqBodyJSON, h, logger, clientConn, destConn, requestBuffer, h.Recover, mockAssert)

		if err != nil {
			logger.Error("error while matching http mocks", zap.Error(err))
		}

		if !isMatched {
			passthroughHost := false
			for _, host := range models.PassThroughHosts {
				if req.Host == host {
					passthroughHost = true
				}
			}
			if !passthroughHost && !mockAssert {
				logger.Error("Didn't match any prexisting http mock")
				logger.Error("Cannot Find eligible mocks for the outgoing http call", zap.Any("request", string(requestBuffer)))
				util.Passthrough(clientConn, destConn, [][]byte{requestBuffer}, h.Recover, logger)
			}

			return
		}
		statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HttpReq.ProtoMajor, stub.Spec.HttpReq.ProtoMinor, stub.Spec.HttpResp.StatusCode, http.StatusText(int(stub.Spec.HttpResp.StatusCode)))

		body := stub.Spec.HttpResp.Body
		var respBody string
		var responseString string
		// TODO: Handle the case when reqBody is empty but stub.Spec.HttpReq.Body is not empty
		if string(reqBody) != "" {
			diffs, err := assertJSONReqBody(stub.Spec.HttpReq.Body, string(reqBody))
			if err != nil {
				logger.Error("failed to assert the json request body for the url", zap.String("url", stub.Spec.HttpReq.URL), zap.Error(err))
				// return
			}

			// Print the differences
			if len(diffs) > 0 {
				logger.Debug("Differences found URL: ", zap.Any("", req.Method+":"+req.URL.String()+"Audit-id:: "+stub.Spec.HttpResp.Header["Audit-Id"]+":"+color.RedString("MISMATCH")))
				for _, diff := range diffs {
					logger.Info("difference between", zap.Any("difference", diff))
				}
			} else {
				logger.Debug("No differences found")
			}
		}
		var minTime int64
		var prevTime int64
		minTime = 1000000
		prevTime = pkg.GetUnixMilliTime(stub.Spec.ReqTimestampMock)
		//calculate chunk time

		var chunkedResponses []string
		var chunkedTime []int64
		var watchCall bool
		if stub.Spec.Metadata["chunkedLength"] != "" {
			chunkedTime = pkg.GetChunkTime(logger, stub.Spec.Metadata["chunkedTime"])

			// Split the JSON input by newline
			jsonObjects := strings.Split(stub.Spec.HttpResp.Body, "\n")
			// Process each JSON object
			for _, jsonObject := range jsonObjects {
				// Skip empty lines
				if jsonObject == "" {
					continue
				}
				// Unmarshal the JSON object
				var data map[string]interface{}
				err := json.Unmarshal([]byte(jsonObject), &data)
				if err != nil {
					logger.Error("Error decoding JSON object ", zap.Any("chunkedTime unmarshal", err.Error()))
					continue
				}
				// Print or process the JSON object as needed
				jsonSize := strconv.FormatInt(int64(len(jsonObject)), 16)
				chunkedResponse := fmt.Sprintf("%s\r\n%s\r\n", jsonSize, jsonObject)
				chunkedResponses = append(chunkedResponses, chunkedResponse)
			}
			watchCall = true
		}

		for _, chunktime := range chunkedTime {
			if (chunktime-prevTime) < minTime && chunktime != 0 && (chunktime-prevTime) != 0 {
				minTime = (chunktime - prevTime)
				prevTime = chunktime
			}
		}

		// Calculate average
		var averageDuration time.Duration
		if len(chunkedTime) > 0 {
			averageDuration = time.Duration(minTime)
		} else {
			averageDuration = 0
		}

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
			// responseString = statusLine + headers + "\r\n" + compressedBuffer.String()
		} else {
			respBody = body
			// responseString = statusLine + headers + "\r\n" + body
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

		if len(chunkedResponses) == 0 {
			responseString = statusLine + headers + "\r\n" + "" + respBody
			logger.Debug("the content-length header" + headers)
			_, err = clientConn.Write([]byte(responseString))
			if err != nil {
				logger.Error("failed to write the mock output to the user application", zap.Error(err))
				return
			}
		} else {
			// calculate responsebody length for each chunk , and at last append 0 also.
			headers = "Transfer-Encoding: chunked\r\n"
			for key, values := range header {
				if key == "Content-Length" {
					continue
				}
				for _, value := range values {
					headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
					headers += headerLine
				}
			}

			for idx, v := range chunkedResponses {
				if idx == 0 {
					responseString = statusLine + headers + "\r\n" + v
				} else {
					responseString = v
				}
				if idx == len(chunkedResponses)-1 {
					responseString = responseString + "0\r\n\r\n"
				}

				if watchCall {
					time.Sleep(averageDuration * time.Second)
				}
				_, err = clientConn.Write([]byte(responseString))
				if err != nil {
					logger.Error("failed to write the mock output to the user application", zap.Error(err))
					return
				}
			}
		}

		if reqURL.String() == baseUrl && baseUrl != "" {
			basePoints--
			if basePoints == 0 {
				sigChan := make(chan os.Signal, 1)
				signal.Notify(sigChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)
				go func() {
					select {
					case <-sigChan:
						logger.Info("Received SIGTERM, exiting")
						h.StopUserApplication()
					}
				}()
			}
		}
		requestBuffer, err = util.ReadBytes(clientConn)
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
	var chunkedLength []int
	var chunkedTime []int64
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
	var reqTimestampMock, resTimestampcMock time.Time
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)
	go func() {
		select {
		case <-sigChan:
			logger.Debug("Received SIGTERM, exiting")
			if finalReq == nil || finalResp == nil {
				logger.Debug("Sorry request and response are nil")
				return
			} else {
				logger.Debug("Signal-finalReq:\n", zap.Any("finalRequest", finalReq))
				logger.Debug("Signal-finalResp:\n", zap.Any("finalResponse", finalResp))
				logger.Debug("Length of finalReq and finalResp"+"", zap.Any("finalReq", len(finalReq)), zap.Any("finalResp", len(finalResp)))
				err := ParseFinalHttp(chunkedTime, chunkedLength, finalReq, finalResp, reqTimestampMock, resTimestampcMock, ctx, logger, h)
				if err != nil {
					logger.Error("failed to parse the final http request and response", zap.Error(err))
					return
				}
			}
		}
	}()
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
		reqTimestampMock = time.Now()

		handleChunkedRequests(&finalReq, clientConn, destConn, logger, true)
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
		resTimestampcMock = time.Now()
		// write the response message to the user client
		_, err = clientConn.Write(resp)
		if err != nil {
			logger.Error("failed to write response message to the user client", zap.Error(err))
			return err
		}
		finalResp = append(finalResp, resp...)
		logger.Debug("This is the initial response: " + string(resp))
		handleChunkedResponses(&chunkedTime, &chunkedLength, &finalResp, clientConn, destConn, logger, resp)
		logger.Debug("This is the final response: " + string(finalResp))
		//Parse the finalReq and finalResp
		err := ParseFinalHttp(chunkedTime, chunkedLength, finalReq, finalResp, reqTimestampMock, resTimestampcMock, ctx, logger, h)
		if err != nil {
			logger.Error("failed to parse the final http request and response", zap.Error(err))
			return err
		}

		//Resetting the finalReq and finalResp
		finalReq = nil
		finalResp = nil
		chunkedLength = nil
		chunkedTime = nil

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

func ParseFinalHttp(chunkedTime []int64, chunkedLength []int, finalReq []byte, finalResp []byte, reqTimestampMock, resTimestampcMock time.Time, ctx context.Context, logger *zap.Logger, h *hooks.Hook) error {
	var req *http.Request
	// converts the request message buffer to http request
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(finalReq)))
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
	logger.Debug("PARSED RESPONSE" + fmt.Sprint(respParsed))
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
			logger.Debug("fmt.Sprint(PerConncounter) " + "The body is gzip or not" + strconv.FormatBool(ok))
			logger.Debug("fmt.Sprint(PerConncounter) "+"", zap.Any("isGzipped", ok))
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
		// logger.Debug("fmt.Sprint(PerConncounter) " + fmt.Sprint(PerConncounter) + " OverallCounter " + fmt.Sprint(atomic.LoadInt64(&Overallcounter)) + "This is the response body after reading: " + string(respBody))
		if err != nil && err.Error() != "unexpected EOF" {
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

	if chunkedLength != nil || chunkedTime != nil || len(chunkedLength) != 0 || len(chunkedTime) != 0 {
		meta["chunkedLength"] = fmt.Sprint(chunkedLength)
		meta["chunkedTime"] = fmt.Sprint(chunkedTime)
	}

	err = h.AppendMocks(&models.Mock{
		Version: models.GetVersion(),
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
			Created:          time.Now().UnixMilli(),
			ReqTimestampMock: reqTimestampMock,
			ResTimestampMock: resTimestampcMock,
		},
	}, ctx)

	if err != nil {
		logger.Error("failed to store the http mock", zap.Error(err))
		return err
	}

	return nil
}
