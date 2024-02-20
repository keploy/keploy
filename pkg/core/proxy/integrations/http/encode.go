package http

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"io"
	"net"
	"strings"
	"time"
)

// encodeOutgoingHttp function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeOutgoingHttp(ctx context.Context, logger *zap.Logger, req []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	//closing the destination conn
	defer destConn.Close()

	var resp []byte
	var finalResp []byte
	var finalReq []byte
	var err error

	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	//Writing the request to the server.
	_, err = destConn.Write(req)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}

	logger.Debug("This is the initial request: " + string(req))
	finalReq = append(finalReq, req...)

	//for keeping the conn alive
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
			req, err = util.ReadBytes(clientConn)
			if err != nil {
				logger.Error("failed to read the request message from the user client", zap.Error(err))
				return err
			}
			// write the request message to the actual destination server
			_, err = destConn.Write(req)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return err
			}
			finalReq = append(finalReq, req...)
		}

		// Capture the request timestamp
		reqTimestampMock := time.Now()

		err := handleChunkedRequests(&finalReq, clientConn, destConn, logger)
		if err != nil {
			logger.Error("failed to handle chunk request", zap.Error(err))
			return err
		}

		logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))
		// read the response from the actual server
		resp, err = util.ReadBytes(destConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("Response complete, exiting the loop.")
				// if there is any buffer left before EOF, we must send it to the client and save this as mock
				if len(resp) != 0 {

					// Capturing the response timestamp
					resTimestampcMock := time.Now()
					// write the response message to the user client
					_, err = clientConn.Write(resp)
					if err != nil {
						logger.Error("failed to write response message to the user client", zap.Error(err))
						return err
					}

					// saving last request/response on this conn.
					m := &finalHttp{
						req:              finalReq,
						resp:             resp,
						reqTimestampMock: reqTimestampMock,
						resTimestampMock: resTimestampcMock,
					}
					err := ParseFinalHttp(ctx, logger, m, destPort, mocks, opts)
					if err != nil {
						logger.Error("failed to parse the final http request and response", zap.Error(err))
						return err
					}
				}
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

		err = handleChunkedResponses(&finalResp, clientConn, destConn, logger, resp)
		if err != nil {
			if err == io.EOF {
				logger.Debug("conn closed by the server", zap.Error(err))
				//check if before EOF complete response came, and try to parse it.
				m := &finalHttp{
					req:              finalReq,
					resp:             finalResp,
					reqTimestampMock: reqTimestampMock,
					resTimestampMock: resTimestampcMock,
				}
				parseErr := ParseFinalHttp(ctx, logger, m, destPort, mocks, opts)
				if parseErr != nil {
					logger.Error("failed to parse the final http request and response", zap.Error(parseErr))
					return parseErr
				}
				return nil
			} else {
				logger.Error("failed to handle chunk response", zap.Error(err))
				return err
			}
		}

		logger.Debug("This is the final response: " + string(finalResp))

		m := &finalHttp{
			req:              finalReq,
			resp:             finalResp,
			reqTimestampMock: reqTimestampMock,
			resTimestampMock: resTimestampcMock,
		}

		err = ParseFinalHttp(ctx, logger, m, destPort, mocks, opts)
		if err != nil {
			logger.Error("failed to parse the final http request and response", zap.Error(err))
			return err
		}

		//resetting for the new request and response.
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
