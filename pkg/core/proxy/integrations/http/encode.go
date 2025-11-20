//go:build linux

package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// encodeHTTP function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func (h *HTTP) encodeHTTP(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {

	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	// Check if this request should be passed through BEFORE processing
	// This prevents issues with streaming/long-lived connections
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse the http request message for passthrough check")
		// Continue with normal flow even if parsing fails
	} else {
		req.Header.Set("Host", req.Host)
		h.Logger.Debug("Checking bypass rules", zap.String("host", req.Host), zap.Uint("destPort", destPort), zap.String("url", req.URL.String()))
		if utils.IsPassThrough(h.Logger, req, destPort, opts) {
			h.Logger.Info("The request is a passThrough request - bypassing completely", zap.Any("metadata", utils.GetReqMeta(req)))
			return h.relayPassThroughTraffic(ctx, reqBuf, clientConn, destConn)
		}
		h.Logger.Debug("Request does not match bypass rules, proceeding with normal recording")
	}

	//Writing the request to the server.
	_, err = destConn.Write(reqBuf)
	if err != nil {
		h.Logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	h.Logger.Debug("This is the initial request: " + string(reqBuf))
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
		defer pUtil.Recover(h.Logger, clientConn, destConn)
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
				resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to read the response message from the server after 100-continue request")
					errCh <- err
					return nil
				}

				// write the response message to the client
				_, err = clientConn.Write(resp)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(h.Logger, err, "failed to write response message to the user client")
					errCh <- err
					return nil
				}

				h.Logger.Debug("This is the response from the server after the expect header" + string(resp))

				if string(resp) != "HTTP/1.1 100 Continue\r\n\r\n" {
					utils.LogError(h.Logger, nil, "failed to get the 100 continue response from the user client")
					errCh <- err
					return nil
				}
				//Reading the request buffer again
				reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to read the request buffer from the user client")
					errCh <- err
					return nil
				}
				// write the request message to the actual destination server
				_, err = destConn.Write(reqBuf)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(h.Logger, err, "failed to write request message to the destination server")
					errCh <- err
					return nil
				}
				finalReq = append(finalReq, reqBuf...)
			}

			// Capture the request timestamp
			reqTimestampMock := time.Now()

			err := h.HandleChunkedRequests(ctx, &finalReq, clientConn, destConn)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to handle chunked requests")
				errCh <- err
				return nil
			}

			h.Logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))
			// read the response from the actual server
			resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
			if err != nil {
				if err == io.EOF {
					h.Logger.Debug("Response complete, exiting the loop.")
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
							utils.LogError(h.Logger, err, "failed to write response message to the user client")
							errCh <- err
							return nil
						}

						// saving last request/response on this conn.
						m := &FinalHTTP{
							Req:              finalReq,
							Resp:             resp,
							ReqTimestampMock: reqTimestampMock,
							ResTimestampMock: resTimestampMock,
						}
						err := h.parseFinalHTTP(ctx, m, destPort, mocks, opts)
						if err != nil {
							utils.LogError(h.Logger, err, "failed to parse the final http request and response")
							errCh <- err
							return nil
						}
					}
					break
				}
				utils.LogError(h.Logger, err, "failed to read the response message from the destination server")
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
				utils.LogError(h.Logger, err, "failed to write response message to the user client")
				errCh <- err
				return nil
			}
			var finalResp []byte
			finalResp = append(finalResp, resp...)
			h.Logger.Debug("This is the initial response: " + string(resp))

			err = h.handleChunkedResponses(ctx, &finalResp, clientConn, destConn, resp)
			if err != nil {
				if err == io.EOF {
					h.Logger.Debug("conn closed by the server", zap.Error(err))
					//check if before EOF complete response came, and try to parse it.
					m := &FinalHTTP{
						Req:              finalReq,
						Resp:             finalResp,
						ReqTimestampMock: reqTimestampMock,
						ResTimestampMock: resTimestampMock,
					}
					parseErr := h.parseFinalHTTP(ctx, m, destPort, mocks, opts)
					if parseErr != nil {
						utils.LogError(h.Logger, parseErr, "failed to parse the final http request and response")
						errCh <- parseErr
					}
					errCh <- nil
					return nil
				}
				utils.LogError(h.Logger, err, "failed to handle chunk response")
				errCh <- err
				return nil
			}

			h.Logger.Debug("This is the final response: " + string(finalResp))

			m := &FinalHTTP{
				Req:              finalReq,
				Resp:             finalResp,
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: resTimestampMock,
			}

			err = h.parseFinalHTTP(ctx, m, destPort, mocks, opts)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to parse the final http request and response")
				errCh <- err
				return nil
			}

			//resetting for the new request and response.
			finalReq = []byte("")
			finalResp = []byte("")

			// read the request from the same connection
			h.Logger.Debug("Reading the request from the user client again from the same connection")

			finalReq, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
			if err != nil {
				if err != io.EOF {
					h.Logger.Debug("failed to read the request message from the user client", zap.Error(err))
					h.Logger.Debug("This was the last response from the server: " + string(resp))
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
				utils.LogError(h.Logger, err, "failed to write request message to the destination server")
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

// relayPassThroughTraffic relays traffic bidirectionally between client and destination
// without any processing, recording, or buffering. This is used for bypass rules.
func (h *HTTP) relayPassThroughTraffic(ctx context.Context, initialReq []byte, clientConn, destConn net.Conn) error {
	// Write the initial request to destination
	_, err := destConn.Write(initialReq)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to write initial request to destination during passthrough")
		return err
	}

	clientBuffChan := make(chan []byte, 1)
	destBuffChan := make(chan []byte, 1)
	errChan := make(chan error, 2)

	// Read from client in background
	err = pUtil.ReadFromPeer(ctx, h.Logger, clientConn, clientBuffChan, errChan, pUtil.Client)
	if err != nil {
		return fmt.Errorf("error setting up client reader for passthrough: %v", err)
	}

	// Read from destination in background
	err = pUtil.ReadFromPeer(ctx, h.Logger, destConn, destBuffChan, errChan, pUtil.Destination)
	if err != nil {
		return fmt.Errorf("error setting up destination reader for passthrough: %v", err)
	}

	// Relay traffic bidirectionally
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case buffer := <-clientBuffChan:
			// Write request from client to destination
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to write request to destination during passthrough")
				return fmt.Errorf("error writing to destination during passthrough")
			}
		case buffer := <-destBuffChan:
			// Write response from destination to client
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to write response to client during passthrough")
				return fmt.Errorf("error writing to client during passthrough")
			}
		case err := <-errChan:
			if err == io.EOF {
				h.Logger.Debug("passthrough connection closed normally")
				// When destination closes, close the client connection read side
				// to ensure client gets clean EOF instead of "unexpected EOF"
				// Use CloseRead if available (TCP), otherwise just close
				if tcpConn, ok := clientConn.(*net.TCPConn); ok {
					if err := tcpConn.CloseRead(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
						h.Logger.Debug("error closing client connection read side after passthrough EOF", zap.Error(err))
					}
				}
				return nil
			}
			if err != nil {
				utils.LogError(h.Logger, err, "error during passthrough relay")
			}
			return err
		}
	}
}
