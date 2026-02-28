package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"os"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// In-flight request queue scoped to a single TCP connection.
// Responses are matched using FIFO ordering, as mandated by HTTP/1.1.

func isHTTPPipeliningEnabled() bool {
	return os.Getenv("KEPLOY_HTTP_PIPELINING_ENABLED") == "true"
}

// encodeHTTP function parses the HTTP request and response text messages to capture outgoing network calls as mocks in Sync mode.
func (h *HTTP) encodeHTTPSync(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	//Writing the request to the server.
	_, err := destConn.Write(reqBuf)
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

// encodeHTTP function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func (h *HTTP) encodeHTTP(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	if !isHTTPPipeliningEnabled() {
		return h.encodeHTTPSync(ctx, reqBuf, clientConn, destConn, mocks, opts)
	}

	return h.encodeHTTPPipelined(ctx, reqBuf, clientConn, destConn, mocks, opts)
}

func (h *HTTP) encodeHTTPPipelined(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	var (
		inflight []*FinalHTTP
		mu       sync.Mutex
	)
	h.Logger.Warn("HTTP pipelining path enabled")
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")

	}

	firstReq := &FinalHTTP{
		Req:              append([]byte{}, reqBuf...),
		ReqTimestampMock: time.Now(),
	}
	mu.Lock()
	inflight = append(inflight, firstReq)
	h.Logger.Warn("enqueue request")
	mu.Unlock()

	if _, err := destConn.Write(reqBuf); err != nil {
		return err
	}

	// Separate goroutines for request ingestion and response handling.
	// This allows pipelined requests without blocking on responses.

	g.Go(func() error {
		defer pUtil.Recover(h.Logger, clientConn, destConn)

		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			req, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
			if err != nil {
				if err == io.EOF {
					destConn.Close()
				}
				return err
			}

			mock := &FinalHTTP{
				Req:              append([]byte{}, req...),
				ReqTimestampMock: time.Now(),
			}
			mu.Lock()
			inflight = append(inflight, mock)
			mu.Unlock()

			if _, err := destConn.Write(req); err != nil {
				return err
			}
		}
	})

	g.Go(func() error {
		defer pUtil.Recover(h.Logger, clientConn, destConn)

		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
			if err != nil {
				return err
			}

			mu.Lock()
			if len(inflight) == 0 {
				mu.Unlock()
				return errors.New("received response without matching request")
			}

			mock := inflight[0]
			inflight = inflight[1:]
			mu.Unlock()

			mock.Resp = append([]byte{}, resp...)
			mock.ResTimestampMock = time.Now()
			h.Logger.Warn("dequeue request")

			if err := h.parseFinalHTTP(ctx, mock, destPort, mocks, opts); err != nil {
				utils.LogError(h.Logger, err, "failed to parse final http mock")
			}
			if _, err := clientConn.Write(resp); err != nil {
				return err
			}

		}

	})

	err := g.Wait()

	mu.Lock()
	if len(inflight) > 0 {
		h.Logger.Warn("connection closed with unfilled http requests", zap.Int("count", len(inflight)))
	}
	mu.Unlock()

	if err == io.EOF {
		return nil
	}
	return err
}
