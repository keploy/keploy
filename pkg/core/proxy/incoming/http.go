//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg"
	hooksUtils "go.keploy.io/server/v2/pkg/core/hooks/conn"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, opts models.IncomingOptions) {
	upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
	clientReader := bufio.NewReader(clientConn)
	if err != nil {
		logger.Warn("Failed to dial upstream new app port", zap.String("New_App_Port", newAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	upstreamReader := bufio.NewReader(upConn)

	for {
		reqTimestamp := time.Now()

		// Peek to check for Expect: 100-continue header without consuming data
		peekBuf, err := clientReader.Peek(4096)
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			if errors.Is(err, io.EOF) {
				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				logger.Warn("Failed to peek client request", zap.Error(err))
			}
			return
		}

		// Check if Expect: 100-continue header is present
		hasExpectContinue := bytes.Contains(peekBuf, []byte("Expect: 100-continue"))

		var headerBuf []byte

		if hasExpectContinue {
			// Read headers manually for Expect: 100-continue flow
			headerBuf = make([]byte, 0, 4096)
			for {
				chunk := make([]byte, 4096)
				readN, err := clientReader.Read(chunk)
				if readN > 0 {
					headerBuf = append(headerBuf, chunk[:readN]...)
				}
				if err != nil {
					if err == io.EOF {
						break
					}
					logger.Warn("Failed to read client request headers", zap.Error(err))
					return
				}

				// Check if we have complete headers
				if bytes.Contains(headerBuf, []byte("\r\n\r\n")) {
					break
				}
			}
		}

		var reqData []byte
		var req *http.Request

		if hasExpectContinue {
			logger.Debug("Expect: 100-continue header detected, handling 100-continue flow")

			// Find header end
			headerEnd := bytes.Index(headerBuf, []byte("\r\n\r\n"))
			if headerEnd == -1 {
				logger.Error("Failed to find header end in request")
				return
			}

			// Extract Content-Length from headers
			contentLength := 0
			headerLines := bytes.Split(headerBuf[:headerEnd], []byte("\r\n"))
			for _, line := range headerLines {
				if bytes.HasPrefix(bytes.ToLower(line), []byte("content-length:")) {
					parts := bytes.SplitN(line, []byte(":"), 2)
					if len(parts) == 2 {
						clStr := strings.TrimSpace(string(parts[1]))
						if cl, err := strconv.Atoi(clStr); err == nil {
							contentLength = cl
							break
						}
					}
				}
			}

			// Forward headers to upstream server
			_, err = upConn.Write(headerBuf[:headerEnd+4])
			if err != nil {
				logger.Error("Failed to forward request headers to upstream", zap.Error(err))
				return
			}

			// Read 100 Continue response from upstream
			contResp, err := http.ReadResponse(upstreamReader, nil)
			if err != nil {
				logger.Error("Failed to read 100 Continue response from upstream", zap.Error(err))
				return
			}

			if contResp.StatusCode == 100 {
				// Forward 100 Continue to client
				contRespData, err := httputil.DumpResponse(contResp, false)
				if err != nil {
					logger.Error("Failed to dump 100 Continue response", zap.Error(err))
					contResp.Body.Close()
					return
				}
				_, err = clientConn.Write(contRespData)
				if err != nil {
					logger.Error("Failed to forward 100 Continue to client", zap.Error(err))
					contResp.Body.Close()
					return
				}
				contResp.Body.Close()

				// Now read the body from client
				if contentLength > 0 {
					bodyBuf := make([]byte, contentLength)
					n, err := io.ReadFull(clientReader, bodyBuf)
					if err != nil && err != io.EOF {
						logger.Error("Failed to read request body", zap.Error(err))
						return
					}

					// Forward body to upstream
					_, err = upConn.Write(bodyBuf[:n])
					if err != nil {
						logger.Error("Failed to forward request body to upstream", zap.Error(err))
						return
					}

					// Combine headers and body for parsing
					headerBuf = append(headerBuf, bodyBuf[:n]...)
				}
			} else {
				// Not a 100 Continue - upstream rejected the request early (e.g., 417 Expectation Failed)
				// Treat this as the final response
				logger.Debug("Upstream rejected Expect: 100-continue request", zap.Int("status", contResp.StatusCode))
				respTimestamp := time.Now()

				respData, err := httputil.DumpResponse(contResp, true)
				if err != nil {
					logger.Error("Failed to dump response for capturing", zap.Error(err))
					contResp.Body.Close()
					return
				}
				if err := contResp.Write(clientConn); err != nil {
					logger.Error("Failed to forward response to client", zap.Error(err))
					contResp.Body.Close()
					return
				}

				// Parse request (headers only, no body since request was rejected)
				req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(headerBuf)))
				if err != nil {
					utils.LogError(logger, err, "Failed to parse HTTP request")
					contResp.Body.Close()
					return
				}
				defer req.Body.Close()

				reqData, err := httputil.DumpRequest(req, true)
				if err != nil {
					logger.Error("Failed to dump request for capturing", zap.Error(err))
					contResp.Body.Close()
					return
				}

				parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
				if err != nil {
					utils.LogError(logger, err, "Failed to parse HTTP request")
					contResp.Body.Close()
					return
				}
				parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
				if err != nil {
					utils.LogError(logger, err, "Failed to parse HTTP response")
					contResp.Body.Close()
					return
				}

				go func() {
					defer parsedHTTPReq.Body.Close()
					defer parsedHTTPRes.Body.Close()
					defer contResp.Body.Close()
					hooksUtils.Capture(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts)
				}()
				continue // Move to next request
			}
		}

		var req *http.Request
		var reqData []byte

		if hasExpectContinue {
			// Parse the complete request (headers + body) from headerBuf
			req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(headerBuf)))
			if err != nil {
				logger.Warn("Failed to parse client request", zap.Error(err))
				return
			}
			defer req.Body.Close()

			// Dump request for capturing
			reqData, err = httputil.DumpRequest(req, true)
			if err != nil {
				logger.Error("Failed to dump request for capturing", zap.Error(err))
				return
			}
			// Headers and body are already forwarded above
		} else {
			// Normal flow - read request directly
			req, err = http.ReadRequest(clientReader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
				} else {
					logger.Warn("Failed to read client request", zap.Error(err))
				}
				return
			}
			defer req.Body.Close()

			// Dump request for capturing
			reqData, err = httputil.DumpRequest(req, true)
			if err != nil {
				logger.Error("Failed to dump request for capturing", zap.Error(err))
				return
			}

			// Forward the request normally
			if err := req.Write(upConn); err != nil {
				logger.Error("Failed to forward request to upstream", zap.Error(err))
				return
			}
		}

		// Read the FINAL response (not the 100 Continue)
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}
		defer resp.Body.Close()
		respTimestamp := time.Now()

		respData, err := httputil.DumpResponse(resp, true)
		if err != nil {
			logger.Error("Failed to dump response for capturing", zap.Error(err))
			return
		}
		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			return
		}

		// Now we create New HTTPRequest and New HTTPResponse from the dumped data
		// Since we have already read the body in the write calls for forwarding traffic
		parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
		if err != nil {
			utils.LogError(logger, err, "Failed to parse HTTP request")
			return
		}
		parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
		if err != nil {
			utils.LogError(logger, err, "Failed to parse HTTP response")
			return
		}

		go func() {
			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.Capture(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, opts)
		}()
	}
}
