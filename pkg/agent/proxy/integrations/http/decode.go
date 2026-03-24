// Package http provides functionality for handling HTTP outgoing calls.
package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// ErrMockNotMatched is returned by decodeHTTP when no recorded mock
// matches the outgoing HTTP request. Callers can check for this with
// errors.Is to distinguish a mock-miss from a real proxy error.
var ErrMockNotMatched = errors.New("no matching mock found")

// Decodes the mocks in test mode so that they can be sent to the user application.
func (h *HTTP) decodeHTTP(ctx context.Context, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)
	go func(errCh chan error, reqBuf []byte, opts models.OutgoingOptions) {
		defer pUtil.Recover(h.Logger, clientConn, nil)
		defer close(errCh)
		for {
			//Check if the expected header is present
			if bytes.Contains(reqBuf, []byte("Expect: 100-continue")) {
				h.Logger.Debug("The expect header is present in the request buffer and writing the 100 continue response to the client")
				//Send the 100 continue response
				_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(h.Logger, err, "failed to write the 100 continue response to the user application")
					errCh <- err
					return
				}
				h.Logger.Debug("The 100 continue response has been sent to the user application")
				//Read the request buffer again
				newRequest, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to read the request buffer from the user application")
					errCh <- err
					return
				}
				//Append the new request buffer to the old request buffer
				reqBuf = append(reqBuf, newRequest...)
			}

			h.Logger.Debug("handling the chunked requests to read the complete request")
			err := h.HandleChunkedRequests(ctx, &reqBuf, clientConn, nil)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to handle chunked requests")
				errCh <- err
				return
			}

			h.Logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(reqBuf)))

			//Parse the request buffer
			request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
			if err != nil {
				utils.LogError(h.Logger, err, "failed to parse the http request message")
				errCh <- err
				return
			}

			h.Logger.Debug("Decoded HTTP request headers", zap.Any("headers", request.Header))
			// Set the host header explicitly because the `http.ReadRequest`` trim the host header
			// func ReadRequest(b *bufio.Reader) (*Request, error) {
			// 	req, err := readRequest(b)
			// 	if err != nil {
			// 		return nil, err
			// 	}

			// 	delete(req.Header, "Host")
			// 	return req, err
			// }
			request.Header.Set("Host", request.Host)

			reqBody, err := io.ReadAll(request.Body)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to read from request body", zap.Any("metadata", utils.GetReqMeta(request)))
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

			if input.header.Get("Content-Encoding") != "" {
				input.body, err = pkg.Decompress(h.Logger, input.header.Get("Content-Encoding"), input.body)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to decode the http request body", zap.Any("metadata", utils.GetReqMeta(request)))
					errCh <- err
					return
				}
			}

			h.Logger.Debug("decodeHTTP debug logs for input",
				zap.Any("method", input.method),
				zap.Any("url", input.url),
				zap.Any("header", input.header),
				zap.Any("body", string(input.body)),
				zap.Any("raw", string(input.raw)))

			// Extract header noise from noise configuration
			var headerNoise map[string][]string
			if opts.NoiseConfig != nil {
				if hn, ok := opts.NoiseConfig["header"]; ok {
					headerNoise = hn
				}
			}

			h.Logger.Debug("header noise", zap.Any("header noise", headerNoise))

			ok, stub, err := h.match(ctx, input, mockDb, headerNoise) // calling match function to match mocks
			if err != nil {
				utils.LogError(h.Logger, err, "error while matching http mocks", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- err
				return
			}
			h.Logger.Debug("after matching the http request", zap.Any("isMatched", ok), zap.Any("stub", stub), zap.Error(err))

			if !ok {
				h.Logger.Debug("no matching http mock found", zap.Any("metadata", utils.GetReqMeta(request)))

				// Build mismatch report for the user (surfaced in the mismatch table)
				report := h.buildHTTPMismatchReport(request, mockDb, nil)
				if report != nil {
					h.Logger.Debug("mock miss",
						zap.String("protocol", report.Protocol),
						zap.String("actual", report.ActualSummary),
						zap.String("closest", report.ClosestMock),
						zap.String("diff", report.Diff))
				}

				// No mock matched — send a 502 so the application gets a
				// proper HTTP error instead of a silent connection close.
				noMockBody := "keploy: no matching mock found for this HTTP request\n"
				noMock := fmt.Sprintf("HTTP/%d.%d 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					request.ProtoMajor, request.ProtoMinor, len(noMockBody), noMockBody)
				if _, err := clientConn.Write([]byte(noMock)); err != nil {
					h.Logger.Debug("failed to write 502 response to client", zap.Error(err), zap.Any("metadata", utils.GetReqMeta(request)))
				}
				baseErr := fmt.Errorf("%w for %s %s", ErrMockNotMatched, request.Method, request.URL.Path)
				errCh <- models.NewMockMismatchError(baseErr, report)
				return
			}

			if stub == nil {
				utils.LogError(h.Logger, nil, "matched mock is nil", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- errors.New("matched mock is nil")
				return
			}

			var responseString string

			statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor, stub.Spec.HTTPResp.StatusCode, http.StatusText(stub.Spec.HTTPResp.StatusCode))

			if stub.Spec.HTTPResp.StreamRef != nil {
				// Streaming replay
				header := pkg.ToHTTPHeader(stub.Spec.HTTPResp.Header)
				// The raw stream data in the ndjson file contains HTTP chunked
				// transfer-encoding framing (size\r\ndata\r\n). During recording,
				// http.ReadResponse strips the Transfer-Encoding header, so we
				// must add it back for the client's HTTP parser to dechunk correctly.
				header.Set("Transfer-Encoding", "chunked")
				var headers string
				for key, values := range header {
					for _, value := range values {
						headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
						headers += headerLine
					}
				}

				// Write status line and headers
				if _, err := clientConn.Write([]byte(statusLine + headers + "\r\n")); err != nil {
					utils.LogError(h.Logger, err, "failed to write mock headers to client")
					errCh <- err
					return
				}

				configPath := opts.ConfigPath
				if configPath == "" {
					configPath = "."
				} else if !pUtil.IsDirectoryExist(configPath) {
					configPath = filepath.Dir(configPath)
				}

				filePath := filepath.Join(configPath, stub.Spec.HTTPResp.StreamRef.Path)
				file, err := os.Open(filePath)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to open stream file", zap.String("path", filePath))
					errCh <- err
					return
				}
				defer file.Close()

				decoder := json.NewDecoder(file)
				var prevTS time.Time
				firstChunk := true

				for {
					var chunk models.HTTPStreamChunk
					if err := decoder.Decode(&chunk); err != nil {
						if err == io.EOF {
							break
						}
						utils.LogError(h.Logger, err, "failed to decode stream chunk")
						break
					}

					if firstChunk {
						prevTS = chunk.TS
						firstChunk = false
					}

					diff := chunk.TS.Sub(prevTS)
					if diff > 0 {
						time.Sleep(diff)
					}
					prevTS = chunk.TS

					for _, field := range chunk.Data {
						if field.Key == "raw" {
							if _, err := clientConn.Write([]byte(field.Value)); err != nil {
								utils.LogError(h.Logger, err, "failed to write chunk to client")
								errCh <- err
								return
							}
						}
					}
				}
			} else {
				// Standard body replay
				body := stub.Spec.HTTPResp.Body
				var respBody string

				// Fetching the response headers
				header := pkg.ToHTTPHeader(stub.Spec.HTTPResp.Header)

				//Check if the content encoding is present in the header
				if encoding, ok := header["Content-Encoding"]; ok && len(encoding) > 0 {
					compressedBody, err := pkg.Compress(h.Logger, encoding[0], []byte(body))
					if err != nil {
						utils.LogError(h.Logger, err, "failed to compress the response body", zap.Any("metadata", utils.GetReqMeta(request)))
						errCh <- err
						return
					}
					h.Logger.Debug("the length of the response body: " + strconv.Itoa(len(compressedBody)))
					respBody = string(compressedBody)
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

				h.Logger.Debug(fmt.Sprintf("Mock Response sending back to client:\n%v", responseString))

				_, err = clientConn.Write([]byte(responseString))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(h.Logger, err, "failed to write the mock output to the user application", zap.Any("metadata", utils.GetReqMeta(request)))
					errCh <- err
					return
				}
			}

			reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
			if err != nil {
				h.Logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				if responseString == "" {
					responseString = "Streaming Response"
				}
				h.Logger.Debug("This was the last response from the server:\n" + string(responseString))
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
