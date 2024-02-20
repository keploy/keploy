package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

type matchParams struct {
	reqBody       []byte
	reqURL        *url.URL
	isReqBodyJSON bool
	clientConn    net.Conn
	destConn      net.Conn
	requestBuffer []byte
	recover       func(id int)
}

// Decodes the mocks in test mode so that they can be sent to the user application.
func decodeOutgoingHttp(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mocks []*models.Mock, opts models.OutgoingOptions) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			//Check if the expected header is present
			if bytes.Contains(reqBuf, []byte("Expect: 100-continue")) {
				//Send the 100 continue response
				_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				if err != nil {
					logger.Error("failed to write the 100 continue response to the user application", zap.Error(err))
					return err
				}
				//Read the request buffer again
				newRequest, err := util.ReadBytes(clientConn)
				if err != nil {
					logger.Error("failed to read the request buffer from the user application", zap.Error(err))
					return err
				}
				//Append the new request buffer to the old request buffer
				reqBuf = append(reqBuf, newRequest...)
			}

			err := handleChunkedRequests(&reqBuf, clientConn, nil, logger)
			if err != nil {
				logger.Error("failed to handle chunk request", zap.Error(err))
				return err
			}

			logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(reqBuf)))

			//Parse the request buffer
			request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
			if err != nil {
				logger.Error("failed to parse the http request message", zap.Error(err))
				return err
			}

			reqBody, err := io.ReadAll(request.Body)
			if err != nil {
				logger.Error("failed to read from request body", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
				return err
			}

			//parse request url
			reqURL, err := url.Parse(request.URL.String())
			if err != nil {
				logger.Error("failed to parse request url", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
				return err
			}

			//check if reqBuf body is a json
			reqBodyIsJson := isJSON(reqBody)

			match, stub, err := match(ctx, logger, request, reqURL, reqBuf, reqBodyIsJson, mocks)
			if err != nil {
				logger.Error("error while matching http mocks", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
			}

			if !match {
				if !isPassThrough(logger, request, dstCfg.Port, opts) {
					logger.Error("Didn't match any prexisting http mock", zap.Any("metadata", getReqMeta(request)))
				}
				// making destConn
				destConn, err := net.Dial("tcp", dstCfg.Addr)
				if err != nil {
					logger.Error("failed to dial the destination server", zap.Error(err))
					return err
				}
				_, err = util.Passthrough(ctx, logger, clientConn, destConn, [][]byte{reqBuf}, nil)
				if err != nil {
					logger.Error("failed to passthrough http request", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
					return err
				}
			}

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
					logger.Error("failed to compress the response body", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
					return err
				}
				err = gw.Close()
				if err != nil {
					logger.Error("failed to close the gzip writer", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
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
				logger.Error("failed to write the mock output to the user application", zap.Any("metadata", getReqMeta(request)), zap.Error(err))
				return err
			}

			reqBuf, err = util.ReadBytes(clientConn)
			if err != nil {
				logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				logger.Debug("This was the last response from the server:\n" + string(responseString))
				break
			}
		}
	}
}
