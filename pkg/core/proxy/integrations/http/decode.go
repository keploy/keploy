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

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type matchParams struct {
	req           *http.Request
	reqBodyIsJson bool
	reqBuf        []byte
}

// Decodes the mocks in test mode so that they can be sent to the user application.
func decodeHttp(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			//Check if the expected header is present
			if bytes.Contains(reqBuf, []byte("Expect: 100-continue")) {
				//Send the 100 continue response
				_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				if err != nil {
					utils.LogError(logger, err, "failed to write the 100 continue response to the user application")
					return err
				}
				//Read the request buffer again
				newRequest, err := util.ReadBytes(ctx, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the request buffer from the user application")
					return err
				}
				//Append the new request buffer to the old request buffer
				reqBuf = append(reqBuf, newRequest...)
			}

			err := handleChunkedRequests(ctx, logger, &reqBuf, clientConn, nil)
			if err != nil {
				utils.LogError(logger, err, "failed to handle chunked requests")
				return err
			}

			logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(reqBuf)))

			//Parse the request buffer
			request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
			if err != nil {
				utils.LogError(logger, err, "failed to parse the http request message")
				return err
			}

			reqBody, err := io.ReadAll(request.Body)
			if err != nil {
				utils.LogError(logger, err, "failed to read from request body", zap.Any("metadata", getReqMeta(request)))
				return err
			}

			//check if reqBuf body is a json

			param := &matchParams{
				req:           request,
				reqBodyIsJson: isJSON(reqBody),
				reqBuf:        reqBuf,
			}
			match, stub, err := match(ctx, logger, param, mockDb)
			if err != nil {
				utils.LogError(logger, err, "error while matching http mocks", zap.Any("metadata", getReqMeta(request)))
			}

			if !match {
				if !isPassThrough(logger, request, dstCfg.Port, opts) {
					utils.LogError(logger, nil, "Didn't match any preExisting http mock", zap.Any("metadata", getReqMeta(request)))
				}
				// making destConn
				destConn, err := net.Dial("tcp", dstCfg.Addr)
				if err != nil {
					utils.LogError(logger, err, "failed to dial the destination server")
					return err
				}
				_, err = util.PassThrough(ctx, logger, clientConn, destConn, [][]byte{reqBuf})
				if err != nil {
					utils.LogError(logger, err, "failed to passThrough http request", zap.Any("metadata", getReqMeta(request)))
					return err
				}
			}

			statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor, stub.Spec.HTTPResp.StatusCode, http.StatusText(stub.Spec.HTTPResp.StatusCode))

			body := stub.Spec.HTTPResp.Body
			var respBody string
			var responseString string

			// Fetching the response headers
			header := pkg.ToHttpHeader(stub.Spec.HTTPResp.Header)

			//Check if the gzip encoding is present in the header
			if header["Content-Encoding"] != nil && header["Content-Encoding"][0] == "gzip" {
				var compressedBuffer bytes.Buffer
				gw := gzip.NewWriter(&compressedBuffer)
				_, err := gw.Write([]byte(body))
				if err != nil {
					utils.LogError(logger, err, "failed to compress the response body", zap.Any("metadata", getReqMeta(request)))
					return err
				}
				err = gw.Close()
				if err != nil {
					utils.LogError(logger, err, "failed to close the gzip writer", zap.Any("metadata", getReqMeta(request)))
					return err
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
				utils.LogError(logger, err, "failed to write the mock output to the user application", zap.Any("metadata", getReqMeta(request)))
				return err
			}

			reqBuf, err = util.ReadBytes(ctx, clientConn)
			if err != nil {
				logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				logger.Debug("This was the last response from the server:\n" + string(responseString))
				break
			}
		}
	}
}
