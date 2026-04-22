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

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// encodeHTTP records outgoing HTTP traffic. The read/forward/chunked-handling
// logic is identical to the original synchronous implementation. The only
// difference is that parseFinalHTTP (HTTP parsing, body decompression, mock
// creation) is offloaded to a background goroutine so it never blocks the
// forwarding path.
func (h *HTTP) encodeHTTP(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions, onMockRecorded integrations.PostRecordHook) error {
	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	// Forward initial request to server.
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

	// Get the error group from the context.
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// Async mock recorder: parseFinalHTTP runs here off the forwarding path.
	// The channel is buffered so the forwarding goroutine never blocks on send.
	mockDataCh := make(chan *FinalHTTP, 256)
	recorderDone := make(chan struct{})
	recordCtx := context.WithoutCancel(ctx)
	go func() {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		defer close(recorderDone)
		for m := range mockDataCh {
			err := h.parseFinalHTTP(recordCtx, m, destPort, mocks, opts, onMockRecorded)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to parse the final http request and response")
			}
		}
	}()

	// enqueueMock sends a paired request/response for async mock creation.
	// Non-blocking: if the recorder can't keep up, the mock is dropped
	// (same pattern as postgres/mongo).
	enqueueMock := func(req, resp []byte, reqTs, resTs time.Time) {
		m := &FinalHTTP{
			Req:              req,
			Resp:             resp,
			ReqTimestampMock: reqTs,
			ResTimestampMock: resTs,
		}
		select {
		case mockDataCh <- m:
		default:
			h.Logger.Debug("HTTP mock channel full, dropping mock")
		}
	}

	// Main forwarding goroutine — logic is identical to the original
	// synchronous implementation. Only parseFinalHTTP calls are replaced
	// with enqueueMock sends.
	g.Go(func() error {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		defer close(errCh)
		defer func() {
			close(mockDataCh)
			<-recorderDone
		}()

		for {
			if memoryguard.IsRecordingPaused() {
				h.Logger.Debug("memory pressure detected, stopping HTTP recording and falling back to passthrough")
				done := make(chan struct{}, 2)
				cp := func(dst, src net.Conn) {
					_, _ = io.Copy(dst, src)
					done <- struct{}{}
				}
				go cp(destConn, clientConn)
				go cp(clientConn, destConn)
				<-done
				<-done
				return nil
			}

			// Check if expect: 100-continue header is present.
			lines := strings.Split(string(finalReq), "\n")
			var expectHeader string
			for _, line := range lines {
				if strings.HasPrefix(line, "Expect:") {
					expectHeader = strings.TrimSpace(strings.TrimPrefix(line, "Expect:"))
					break
				}
			}
			if expectHeader == "100-continue" {
				resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to read the response message from the server after 100-continue request")
					errCh <- err
					return nil
				}

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
				reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to read the request buffer from the user client")
					errCh <- err
					return nil
				}
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

			// Capture the request timestamp.
			reqTimestampMock := time.Now()

			err := h.HandleChunkedRequests(ctx, &finalReq, clientConn, destConn)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to handle chunked requests")
				errCh <- err
				return nil
			}

			h.Logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))

			// Read the response from the actual server.
			resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
			if err != nil {
				if err == io.EOF {
					h.Logger.Debug("Response complete, exiting the loop.")
					if len(resp) != 0 {
						resTimestampMock := time.Now()
						_, err = clientConn.Write(resp)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(h.Logger, err, "failed to write response message to the user client")
							errCh <- err
							return nil
						}
						enqueueMock(finalReq, resp, reqTimestampMock, resTimestampMock)
					}
					break
				}
				utils.LogError(h.Logger, err, "failed to read the response message from the destination server")
				errCh <- err
				return nil
			}

			resTimestampMock := time.Now()

			// Forward response to client.
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
					enqueueMock(finalReq, finalResp, reqTimestampMock, resTimestampMock)
					errCh <- nil
					return nil
				}
				utils.LogError(h.Logger, err, "failed to handle chunk response")
				errCh <- err
				return nil
			}

			h.Logger.Debug("This is the final response: " + string(finalResp))

			// Offload mock creation to async recorder.
			enqueueMock(finalReq, finalResp, reqTimestampMock, resTimestampMock)

			// Reset for the next request and response.
			finalReq = []byte("")

			// Read the next request from the client.
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
			// Forward the next request to server.
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
