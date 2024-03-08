package http

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// encodeHTTP function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeHTTP(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	//closing the destination conn
	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the destination connection")
		}
	}(destConn)

	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	//Writing the request to the server.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	logger.Debug("This is the initial request: " + string(reqBuf))
	var finalReq []byte
	errCh := make(chan error, 1)
	defer close(errCh)

	finalReq = append(finalReq, reqBuf...)

	//for keeping conn alive
	go func(errCh chan error, finalReq []byte) {
		for {
			//check if expect : 100-continue header is present
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
				resp, err := util.ReadBytes(ctx, destConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the response message from the server after 100-continue request")
					errCh <- err
				}

				// write the response message to the client
				_, err = clientConn.Write(resp)
				if err != nil {
					utils.LogError(logger, err, "failed to write response message to the user client")
					errCh <- err
				}

				logger.Debug("This is the response from the server after the expect header" + string(resp))

				if string(resp) != "HTTP/1.1 100 Continue\r\n\r\n" {
					utils.LogError(logger, nil, "failed to get the 100 continue response from the user client")
					errCh <- err
				}
				//Reading the request buffer again
				reqBuf, err = util.ReadBytes(ctx, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the request buffer from the user client")
					errCh <- err
				}
				// write the request message to the actual destination server
				_, err = destConn.Write(reqBuf)
				if err != nil {
					utils.LogError(logger, err, "failed to write request message to the destination server")
					errCh <- err
				}
				finalReq = append(finalReq, reqBuf...)
			}

			// Capture the request timestamp
			reqTimestampMock := time.Now()

			err := handleChunkedRequests(ctx, logger, &finalReq, clientConn, destConn)
			if err != nil {
				utils.LogError(logger, err, "failed to handle chunked requests")
				errCh <- err
			}

			logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))
			// read the response from the actual server
			resp, err := util.ReadBytes(ctx, destConn)
			if err != nil {
				if err == io.EOF {
					logger.Debug("Response complete, exiting the loop.")
					// if there is any buffer left before EOF, we must send it to the client and save this as mock
					if len(resp) != 0 {

						// Capturing the response timestamp
						resTimestampMock := time.Now()
						// write the response message to the user client
						_, err = clientConn.Write(resp)
						if err != nil {
							utils.LogError(logger, err, "failed to write response message to the user client")
							errCh <- err
						}

						// saving last request/response on this conn.
						m := &finalHTTP{
							req:              finalReq,
							resp:             resp,
							reqTimestampMock: reqTimestampMock,
							resTimestampMock: resTimestampMock,
						}
						err := ParseFinalHTTP(ctx, logger, m, destPort, mocks, opts)
						if err != nil {
							utils.LogError(logger, err, "failed to parse the final http request and response")
							errCh <- err
						}
					}
					break
				}
				utils.LogError(logger, err, "failed to read the response message from the destination server")
				errCh <- err
			}

			// Capturing the response timestamp
			resTimestampMock := time.Now()

			// write the response message to the user client
			_, err = clientConn.Write(resp)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the user client")
				errCh <- err
			}
			var finalResp []byte
			finalResp = append(finalResp, resp...)
			logger.Debug("This is the initial response: " + string(resp))

			err = handleChunkedResponses(ctx, logger, &finalResp, clientConn, destConn, resp)
			if err != nil {
				if err == io.EOF {
					logger.Debug("conn closed by the server", zap.Error(err))
					//check if before EOF complete response came, and try to parse it.
					m := &finalHTTP{
						req:              finalReq,
						resp:             finalResp,
						reqTimestampMock: reqTimestampMock,
						resTimestampMock: resTimestampMock,
					}
					parseErr := ParseFinalHTTP(ctx, logger, m, destPort, mocks, opts)
					if parseErr != nil {
						utils.LogError(logger, parseErr, "failed to parse the final http request and response")
						errCh <- parseErr
					}
					errCh <- nil
				}
				utils.LogError(logger, err, "failed to handle chunk response")
				errCh <- err
			}

			logger.Debug("This is the final response: " + string(finalResp))

			m := &finalHTTP{
				req:              finalReq,
				resp:             finalResp,
				reqTimestampMock: reqTimestampMock,
				resTimestampMock: resTimestampMock,
			}

			err = ParseFinalHTTP(ctx, logger, m, destPort, mocks, opts)
			if err != nil {
				utils.LogError(logger, err, "failed to parse the final http request and response")
				errCh <- err
			}

			//resetting for the new request and response.
			finalReq = []byte("")
			finalResp = []byte("")

			finalReq, err = util.ReadBytes(ctx, clientConn)
			if err != nil {
				if err != io.EOF {
					logger.Debug("failed to read the request message from the user client", zap.Error(err))
					logger.Debug("This was the last response from the server: " + string(resp))
					errCh <- nil
				}
				errCh <- err
			}
			// write the request message to the actual destination server
			_, err = destConn.Write(finalReq)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				errCh <- err
			}
		}
	}(errCh, finalReq)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
