package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func handleChunkedRequests(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn) error {

	if hasCompleteHeaders(*finalReq) {
		logger.Debug("this request has complete headers in the first chunk itself.")
	}

	for !hasCompleteHeaders(*finalReq) {
		logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
		reqHeader, err := util.ReadBytes(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, nil, "failed to read the request message from the client")
			return err
		}
		// destConn is nil in case of test mode
		if destConn != nil {
			_, err = destConn.Write(reqHeader)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, nil, "failed to write request message to the destination server")
				return err
			}
		}

		*finalReq = append(*finalReq, reqHeader...)
	}

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
			utils.LogError(logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked request")
		}
		//Get the length of the body in the request.
		bodyLength := len(*finalReq) - strings.Index(string(*finalReq), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			err := contentLengthRequest(ctx, logger, finalReq, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if transferEncodingHeader != "" {
		// check if the initial request is the complete request.
		if strings.HasSuffix(string(*finalReq), "0\r\n\r\n") {
			return nil
		}
		if transferEncodingHeader == "chunked" {
			err := chunkedRequest(ctx, logger, finalReq, clientConn, destConn, transferEncodingHeader)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func handleChunkedResponses(ctx context.Context, logger *zap.Logger, finalResp *[]byte, clientConn, destConn net.Conn, resp []byte) error {

	if hasCompleteHeaders(*finalResp) {
		logger.Debug("this response has complete headers in the first chunk itself.")
	}

	for !hasCompleteHeaders(resp) {
		logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
		respHeader, err := util.ReadBytes(ctx, logger, destConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received EOF from the server")
				// if there is any buffer left before EOF, we must send it to the client and save this as mock
				if len(respHeader) != 0 {
					// write the response message to the user client
					_, err = clientConn.Write(resp)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, nil, "failed to write response message to the user client")
						return err
					}
					*finalResp = append(*finalResp, respHeader...)
				}
				return err
			}
			utils.LogError(logger, nil, "failed to read the response message from the destination server")
			return err
		}
		// write the response message to the user client
		_, err = clientConn.Write(respHeader)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(logger, nil, "failed to write response message to the user client")
			return err
		}

		*finalResp = append(*finalResp, respHeader...)
		resp = append(resp, respHeader...)
	}

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
			utils.LogError(logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked response")
		}
		bodyLength := len(resp) - strings.Index(string(resp), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			err := contentLengthResponse(ctx, logger, finalResp, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if transferEncodingHeader != "" {
		//check if the initial response is the complete response.
		if strings.HasSuffix(string(*finalResp), "0\r\n\r\n") {
			return nil
		}
		if transferEncodingHeader == "chunked" {
			err := chunkedResponse(ctx, logger, finalResp, clientConn, destConn)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Handled chunked requests when content-length is given.
func contentLengthRequest(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn, contentLength int) error {
	for contentLength > 0 {
		err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			utils.LogError(logger, err, "failed to set the read deadline for the client conn")
			return err
		}
		requestChunked, err := util.ReadBytes(ctx, logger, clientConn)
		if err != nil {
			if err == io.EOF {
				utils.LogError(logger, nil, "conn closed by the user client")
				return err
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Info("Stopped getting data from the conn", zap.Error(err))
				break
			}
			utils.LogError(logger, nil, "failed to read the response message from the destination server")
			return err
		}
		logger.Debug("This is a chunk of request[content-length]: " + string(requestChunked))
		*finalReq = append(*finalReq, requestChunked...)
		contentLength -= len(requestChunked)

		// destConn is nil in case of test mode.
		if destConn != nil {
			_, err = destConn.Write(requestChunked)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, nil, "failed to write request message to the destination server")
				return err
			}
		}
	}
	return nil
}

// Handled chunked requests when transfer-encoding is given.
func chunkedRequest(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn, _ string) error {

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			//TODO: we have to implement a way to read the buffer chunk wise according to the chunk size (chunk size comes in hexadecimal)
			// because it can happen that some chunks come after 5 seconds.
			err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return err
			}
			requestChunked, err := util.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					break
				}
				utils.LogError(logger, nil, "failed to read the response message from the destination server")
				return err
			}

			*finalReq = append(*finalReq, requestChunked...)
			// destConn is nil in case of test mode.
			if destConn != nil {
				_, err = destConn.Write(requestChunked)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, nil, "failed to write request message to the destination server")
					return err
				}
			}

			//check if the initial request is completed
			if strings.HasSuffix(string(requestChunked), "0\r\n\r\n") {
				return nil
			}
		}
	}
}

// Handled chunked responses when content-length is given.
func contentLengthResponse(ctx context.Context, logger *zap.Logger, finalResp *[]byte, clientConn, destConn net.Conn, contentLength int) error {
	isEOF := false
	for contentLength > 0 {
		//Set deadline of 5 seconds
		err := destConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			utils.LogError(logger, err, "failed to set the read deadline for the destination conn")
			return err
		}
		resp, err := util.ReadBytes(ctx, logger, destConn)
		if err != nil {
			if err == io.EOF {
				isEOF = true
				logger.Debug("received EOF, conn closed by the destination server")
				if len(resp) == 0 {
					break
				}
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Info("Stopped getting data from the conn", zap.Error(err))
				break
			} else {
				utils.LogError(logger, nil, "failed to read the response message from the destination server")
				return err
			}
		}

		logger.Debug("This is a chunk of response[content-length]: " + string(resp))
		*finalResp = append(*finalResp, resp...)
		contentLength -= len(resp)

		// write the response message to the user client
		_, err = clientConn.Write(resp)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(logger, nil, "failed to write response message to the user client")
			return err
		}

		if isEOF {
			break
		}
	}
	return nil
}

// Handled chunked responses when transfer-encoding is given.
func chunkedResponse(ctx context.Context, logger *zap.Logger, finalResp *[]byte, clientConn, destConn net.Conn) error {
	isEOF := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			resp, err := util.ReadBytes(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read the response message from the destination server")
					return err
				}
				isEOF = true
				logger.Debug("received EOF", zap.Error(err))
				if len(resp) == 0 {
					logger.Debug("exiting loop as response is complete")
					break
				}
			}

			*finalResp = append(*finalResp, resp...)
			// write the response message to the user client
			_, err = clientConn.Write(resp)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, nil, "failed to write response message to the user client")
				return err
			}

			//In some cases need to write the response to the client
			// where there is some response before getting the true EOF
			if isEOF {
				break
			}

			if string(resp) == "0\r\n\r\n" {
				return nil
			}
		}
	}
}

// Checks if the response is gzipped
func isGZipped(check io.ReadCloser, l *zap.Logger) (bool, *bufio.Reader) {
	bufReader := bufio.NewReader(check)
	peekedBytes, err := bufReader.Peek(2)
	if err != nil && err != io.EOF {
		l.Debug("failed to peek the response", zap.Error(err))
		return false, nil
	}
	if len(peekedBytes) < 2 {
		return false, nil
	}
	if peekedBytes[0] == 0x1f && peekedBytes[1] == 0x8b {
		return true, bufReader
	}
	return false, nil
}

// hasCompleteHeaders checks if the given byte slice contains the complete HTTP headers
func hasCompleteHeaders(httpChunk []byte) bool {
	// Define the sequence for header end: "\r\n\r\n"
	headerEndSequence := []byte{'\r', '\n', '\r', '\n'}

	// Check if the byte slice contains the header end sequence
	return bytes.Contains(httpChunk, headerEndSequence)
}

// extract the request metadata from the request
func getReqMeta(req *http.Request) map[string]string {
	reqMeta := map[string]string{}
	if req != nil {
		// get request metadata
		reqMeta = map[string]string{
			"method": req.Method,
			"url":    req.URL.String(),
			"host":   req.Host,
		}
	}
	return reqMeta
}

func isJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}

func IsPassThrough(logger *zap.Logger, req *http.Request, destPort uint, opts models.OutgoingOptions) bool {
	passThrough := false

	for _, bypass := range opts.Rules {
		if bypass.Host != "" {
			regex, err := regexp.Compile(bypass.Host)
			if err != nil {
				utils.LogError(logger, err, "failed to compile the host regex", zap.Any("metadata", getReqMeta(req)))
				continue
			}
			passThrough = regex.MatchString(req.Host)
			if !passThrough {
				continue
			}
		}
		if bypass.Path != "" {
			regex, err := regexp.Compile(bypass.Path)
			if err != nil {
				utils.LogError(logger, err, "failed to compile the path regex", zap.Any("metadata", getReqMeta(req)))
				continue
			}
			passThrough = regex.MatchString(req.URL.String())
			if !passThrough {
				continue
			}
		}

		if passThrough {
			if bypass.Port == 0 || bypass.Port == destPort {
				return true
			}
			passThrough = false
		}
	}

	return passThrough
}
