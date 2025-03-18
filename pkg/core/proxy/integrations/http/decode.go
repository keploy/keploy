//go:build linux

// Package http provides functionality for handling HTTP outgoing calls.
package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// Decodes the mocks in test mode so that they can be sent to the user application.
func (h *HTTP) decodeHTTP(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)
	go func(errCh chan error, reqBuf []byte, opts models.OutgoingOptions) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)
		for {
			//Check if the expected header is present
			if bytes.Contains(reqBuf, []byte("Expect: 100-continue")) {
				logger.Debug("The expect header is present in the request buffer and writing the 100 continue response to the client")
				//Send the 100 continue response
				_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(logger, err, "failed to write the 100 continue response to the user application")
					errCh <- err
					return
				}
				logger.Debug("The 100 continue response has been sent to the user application")
				//Read the request buffer again
				newRequest, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the request buffer from the user application")
					errCh <- err
					return
				}
				//Append the new request buffer to the old request buffer
				reqBuf = append(reqBuf, newRequest...)
			}

			logger.Debug("handling the chunked requests to read the complete request")
			err := h.HandleChunkedRequests(ctx, logger, &reqBuf, clientConn, nil)
			if err != nil {
				utils.LogError(logger, err, "failed to handle chunked requests")
				errCh <- err
				return
			}

			logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(reqBuf)))

			//Parse the request buffer
			request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
			if err != nil {
				utils.LogError(logger, err, "failed to parse the http request message")
				errCh <- err
				return
			}
			request.Header.Set("Host", request.Host)

			reqBody, err := io.ReadAll(request.Body)
			if err != nil {
				utils.LogError(logger, err, "failed to read from request body", zap.Any("metadata", GetReqMeta(request)))
				errCh <- err
				return
			}

			input := &req{
				method: request.Method,
				url:    request.URL,
				header: request.Header,
				body:   reqBody,
				raw:    reqBuf,
			}

			ok, stub, err := h.match(ctx, logger, input, mockDb) // calling match function to match mocks
			if err != nil {
				utils.LogError(logger, err, "error while matching http mocks", zap.Any("metadata", GetReqMeta(request)))
				errCh <- err
				return
			}
			logger.Debug("after matching the http request", zap.Any("isMatched", ok), zap.Any("stub", stub), zap.Error(err))

			if !ok {
				if !IsPassThrough(logger, request, dstCfg.Port, opts) {
					utils.LogError(logger, nil, "Didn't match any preExisting http mock", zap.Any("metadata", GetReqMeta(request)))
				}
				if opts.FallBackOnMiss {
					_, err = pUtil.PassThrough(ctx, logger, clientConn, dstCfg, [][]byte{reqBuf})
					if err != nil {
						utils.LogError(logger, err, "failed to passThrough http request", zap.Any("metadata", GetReqMeta(request)))
						errCh <- err
						return
					}
				}
				errCh <- nil
				return
			}

			statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor, stub.Spec.HTTPResp.StatusCode, http.StatusText(stub.Spec.HTTPResp.StatusCode))

			body := stub.Spec.HTTPResp.Body
			var respBody string
			var responseString string

			// Fetching the response headers
			header := pkg.ToHTTPHeader(stub.Spec.HTTPResp.Header)

			//Check if the gzip encoding is present in the header
			if header["Content-Encoding"] != nil && header["Content-Encoding"][0] == "gzip" {
				var compressedBuffer bytes.Buffer
				gw := gzip.NewWriter(&compressedBuffer)
				_, err := gw.Write([]byte(body))
				if err != nil {
					utils.LogError(logger, err, "failed to compress the response body", zap.Any("metadata", GetReqMeta(request)))
					errCh <- err
					return
				}
				err = gw.Close()
				if err != nil {
					utils.LogError(logger, err, "failed to close the gzip writer", zap.Any("metadata", GetReqMeta(request)))
					errCh <- err
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
			responseString = statusLine + headers + "\r\n" + "" + respBody

			logger.Debug(fmt.Sprintf("Mock Response sending back to client:\n%v", responseString))

			_, err = clientConn.Write([]byte(responseString))
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				utils.LogError(logger, err, "failed to write the mock output to the user application", zap.Any("metadata", GetReqMeta(request)))
				errCh <- err
				return
			}

			reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				logger.Debug("This was the last response from the server:\n" + string(responseString))
				errCh <- nil
				return
			}
		}
	}(errCh, reqBuf, opts)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func (h *HTTP) HandleChunkedRequests(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn) error {

	if hasCompleteHeaders(*finalReq) {
		logger.Debug("this request has complete headers in the first chunk itself.")
	}

	for !hasCompleteHeaders(*finalReq) {
		logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
		reqHeader, err := pUtil.ReadBytes(ctx, logger, clientConn)
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
			err := h.contentLengthRequest(ctx, logger, finalReq, clientConn, destConn, contentLength)
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
			err := h.chunkedRequest(ctx, logger, finalReq, clientConn, destConn, transferEncodingHeader)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Handled chunked requests when content-length is given.
func (h *HTTP) contentLengthRequest(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn, contentLength int) error {
	for contentLength > 0 {
		err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			utils.LogError(logger, err, "failed to set the read deadline for the client conn")
			return err
		}
		requestChunked, err := pUtil.ReadBytes(ctx, logger, clientConn)
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
func (h *HTTP) chunkedRequest(ctx context.Context, logger *zap.Logger, finalReq *[]byte, clientConn, destConn net.Conn, _ string) error {

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
			requestChunked, err := pUtil.ReadBytes(ctx, logger, clientConn)
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
