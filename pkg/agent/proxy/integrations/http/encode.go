package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// httpDecodeItem carries a forwarded chunk to the async decode goroutine.
type httpDecodeItem struct {
	fromClient bool
	data       []byte
	ts         time.Time
}

// encodeHTTP records outgoing HTTP traffic. Forwarding runs in the main
// select loop at wire speed. All HTTP message reassembly, parsing, and
// mock creation is offloaded to a background goroutine via a buffered
// decode channel, so it never slows the data path.
func (h *HTTP) encodeHTTP(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	// Forward initial request to destination immediately — critical path.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		h.Logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// If recording is already paused, pure passthrough.
	if memoryguard.IsRecordingPaused() {
		h.Logger.Debug("memory pressure detected, stopping HTTP recording and falling back to passthrough")
		return forwardHTTPBidirectional(clientConn, destConn)
	}

	// Buffered channels let ReadBuffConn read ahead without waiting
	// for the main loop, preventing TCP flow-control throttling.
	clientBuffChan := make(chan []byte, 256)
	destBuffChan := make(chan []byte, 256)
	errChan := make(chan error, 1)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// Read requests from client.
	g.Go(func() error {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		defer close(clientBuffChan)
		pUtil.ReadBuffConn(ctx, h.Logger, clientConn, clientBuffChan, errChan, false)
		return nil
	})

	// Read responses from destination.
	g.Go(func() error {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		defer close(destBuffChan)
		pUtil.ReadBuffConn(ctx, h.Logger, destConn, destBuffChan, errChan, false)
		return nil
	})

	go func() {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		err := g.Wait()
		if err != nil {
			h.Logger.Debug("error group is returning an error", zap.Error(err))
		}
		close(errChan)
	}()

	// Async decode channel and background goroutine.
	decodeChan := make(chan httpDecodeItem, 512)
	decodeDone := make(chan struct{})
	go func() {
		defer pUtil.Recover(h.Logger, clientConn, destConn)
		defer close(decodeDone)
		h.asyncHTTPDecode(ctx, decodeChan, destPort, mocks, opts)
	}()

	// Seed initial request buffer into decode channel.
	initialBuf := make([]byte, len(reqBuf))
	copy(initialBuf, reqBuf)
	decodeChan <- httpDecodeItem{fromClient: true, data: initialBuf, ts: time.Now()}

	// cleanup ensures the decode goroutine is stopped before we return.
	cleanup := func() {
		close(decodeChan)
		<-decodeDone
	}

	// Main loop: forward only, send copies for async decode.
	for {
		select {
		case <-ctx.Done():
			cleanup()
			return ctx.Err()

		case buffer, ok := <-clientBuffChan:
			if !ok {
				clientBuffChan = nil
				continue
			}
			if buffer == nil {
				continue
			}

			// Forward to destination immediately — critical path.
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to write request to destination server")
				cleanup()
				return err
			}

			// Non-blocking send to async decode.
			if !memoryguard.IsRecordingPaused() && len(decodeChan) < cap(decodeChan) {
				buf := make([]byte, len(buffer))
				copy(buf, buffer)
				select {
				case decodeChan <- httpDecodeItem{fromClient: true, data: buf, ts: time.Now()}:
				default:
				}
			}

		case buffer, ok := <-destBuffChan:
			if !ok {
				destBuffChan = nil
				continue
			}
			if buffer == nil {
				continue
			}

			// Forward to client immediately — critical path.
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to write response to client")
				cleanup()
				return err
			}

			// Non-blocking send to async decode.
			if !memoryguard.IsRecordingPaused() && len(decodeChan) < cap(decodeChan) {
				buf := make([]byte, len(buffer))
				copy(buf, buffer)
				select {
				case decodeChan <- httpDecodeItem{fromClient: false, data: buf, ts: time.Now()}:
				default:
				}
			}

		case err, ok := <-errChan:
			if !ok || err == nil {
				cleanup()
				return nil
			}

			// Drain buffered data before exiting.
		drain:
			for {
				select {
				case buf, ok := <-clientBuffChan:
					if !ok {
						clientBuffChan = nil
						continue
					}
					if buf == nil {
						continue
					}
					_, _ = destConn.Write(buf)
				case buf, ok := <-destBuffChan:
					if !ok {
						destBuffChan = nil
						continue
					}
					if buf == nil {
						continue
					}
					_, _ = clientConn.Write(buf)
				default:
					break drain
				}
			}

			cleanup()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// forwardHTTPBidirectional does raw TCP passthrough without any capture.
func forwardHTTPBidirectional(clientConn, destConn net.Conn) error {
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

// is100Continue returns true if the data starts with an HTTP 100 Continue
// response, which is an intermediate response before the actual response.
func is100Continue(data []byte) bool {
	return bytes.HasPrefix(data, []byte("HTTP/1.1 100")) ||
		bytes.HasPrefix(data, []byte("HTTP/1.0 100"))
}

// asyncHTTPDecode runs in a background goroutine and processes forwarded
// chunks. It uses direction-alternation to detect HTTP message boundaries:
// all consecutive client chunks form a request, all consecutive dest chunks
// form a response. When direction changes, the previous exchange is complete
// and gets parsed/recorded.
func (h *HTTP) asyncHTTPDecode(ctx context.Context, decodeChan <-chan httpDecodeItem, destPort uint, mocks chan<- *models.Mock, opts models.OutgoingOptions) {
	var reqBuf []byte
	var respBuf []byte
	var reqTimestamp time.Time
	var resTimestamp time.Time
	prevWasClient := true // first chunk is always a request (seeded initial reqBuf)

	flushMock := func() {
		if len(reqBuf) == 0 || len(respBuf) == 0 {
			return
		}
		m := &FinalHTTP{
			Req:              reqBuf,
			Resp:             respBuf,
			ReqTimestampMock: reqTimestamp,
			ResTimestampMock: resTimestamp,
		}
		err := h.parseFinalHTTP(ctx, m, destPort, mocks, opts)
		if err != nil {
			h.Logger.Debug("failed to parse HTTP request/response in async decoder", zap.Error(err))
		}
		reqBuf = nil
		respBuf = nil
	}

	for item := range decodeChan {
		if item.fromClient {
			if !prevWasClient && len(reqBuf) > 0 && len(respBuf) > 0 {
				// Direction changed from dest→client.
				// Check if the accumulated response is just a 100 Continue.
				if is100Continue(respBuf) {
					// 100 Continue is an intermediate response — the actual
					// response hasn't arrived yet. Append the 100 Continue
					// to the request buffer as part of the full exchange and
					// keep accumulating.
					reqBuf = append(reqBuf, respBuf...)
					respBuf = nil
					reqBuf = append(reqBuf, item.data...)
					prevWasClient = true
					continue
				}
				// Normal case: previous request-response exchange is complete.
				flushMock()
				reqTimestamp = item.ts
			}
			if len(reqBuf) == 0 {
				reqTimestamp = item.ts
			}
			reqBuf = append(reqBuf, item.data...)
			prevWasClient = true
		} else {
			if prevWasClient && len(respBuf) == 0 {
				resTimestamp = item.ts
			}
			respBuf = append(respBuf, item.data...)
			resTimestamp = item.ts
			prevWasClient = false
		}
	}

	// Channel closed — flush remaining exchange.
	flushMock()
}
