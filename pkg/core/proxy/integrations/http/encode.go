package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// encodeHTTP function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeHTTP(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {

	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	//Writing the request to the server.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	logger.Debug("This is the initial request: " + string(reqBuf))
	var finalReq []byte
	errCh := make(chan error, 1)

	finalReq = append(finalReq, reqBuf...)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	//for keeping conn alive
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)
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
				resp, err := util.ReadBytes(ctx, logger, destConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the response message from the server after 100-continue request")
					errCh <- err
					return nil
				}

				// write the response message to the client
				_, err = clientConn.Write(resp)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write response message to the user client")
					errCh <- err
					return nil
				}

				logger.Debug("This is the response from the server after the expect header" + string(resp))

				if string(resp) != "HTTP/1.1 100 Continue\r\n\r\n" {
					utils.LogError(logger, nil, "failed to get the 100 continue response from the user client")
					errCh <- err
					return nil
				}
				//Reading the request buffer again
				reqBuf, err = util.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read the request buffer from the user client")
					errCh <- err
					return nil
				}
				// write the request message to the actual destination server
				_, err = destConn.Write(reqBuf)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write request message to the destination server")
					errCh <- err
					return nil
				}
				finalReq = append(finalReq, reqBuf...)
			}

			// Capture the request timestamp
			reqTimestampMock := time.Now()

			err := handleChunkedRequests(ctx, logger, &finalReq, clientConn, destConn)
			if err != nil {
				utils.LogError(logger, err, "failed to handle chunked requests")
				errCh <- err
				return nil
			}

			logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))
			// read the response from the actual server
			resp, err := util.ReadBytes(ctx, logger, destConn)
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
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write response message to the user client")
							errCh <- err
							return nil
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
							return nil
						}
					}
					break
				}
				utils.LogError(logger, err, "failed to read the response message from the destination server")
				errCh <- err
				return nil
			}

			// Capturing the response timestamp
			resTimestampMock := time.Now()

			// write the response message to the user client
			_, err = clientConn.Write(resp)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write response message to the user client")
				errCh <- err
				return nil
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
					return nil
				}
				utils.LogError(logger, err, "failed to handle chunk response")
				errCh <- err
				return nil
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
				return nil
			}

			//resetting for the new request and response.
			finalReq = []byte("")
			finalResp = []byte("")

			finalReq, err = util.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					logger.Debug("failed to read the request message from the user client", zap.Error(err))
					logger.Debug("This was the last response from the server: " + string(resp))
					errCh <- nil
					return nil
				}
				errCh <- err
				return nil
			}
			// write the request message to the actual destination server
			_, err = destConn.Write(finalReq)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write request message to the destination server")
				errCh <- err
				return nil
			}
		}
		return nil
	})

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
