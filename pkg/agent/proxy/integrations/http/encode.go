package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// probeHTTPOnce, probeHTTPEnabled mirror the proxy.go probe gate. See
// KEPLOY_PROBE_FANOUT in pkg/agent/proxy/proxy.go. encode.go cannot import
// proxy (would cycle), so the toggle is replicated here.
var (
	probeHTTPOnce    sync.Once
	probeHTTPEnabled atomic.Bool
)

func probeHTTPOn() bool {
	probeHTTPOnce.Do(func() {
		if os.Getenv("KEPLOY_PROBE_FANOUT") == "1" {
			probeHTTPEnabled.Store(true)
		}
	})
	return probeHTTPEnabled.Load()
}

// probeHTTP emits a [PROBE/http] log tagged with the connID pulled from
// the parser context and a phase name. Cheap to call when the probe is
// off (single atomic load + early return).
func probeHTTP(ctx context.Context, logger *zap.Logger, phase string, fields ...zap.Field) {
	if !probeHTTPOn() {
		return
	}
	connID, _ := ctx.Value(models.ClientConnectionIDKey).(string)
	base := []zap.Field{
		zap.String("probe", "http"),
		zap.String("phase", phase),
		zap.String("connID", connID),
		zap.Int64("ts_ns", time.Now().UnixNano()),
	}
	logger.Info("[PROBE/http]", append(base, fields...)...)
}

// encodeHTTP records outgoing HTTP traffic. The read/forward/chunked-handling
// logic is identical to the original synchronous implementation. The only
// difference is that parseFinalHTTP (HTTP parsing, body decompression, mock
// creation) is offloaded to a background goroutine so it never blocks the
// forwarding path.
func (h *HTTP) encodeHTTP(ctx context.Context, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions, onMockRecorded integrations.PostRecordHook) error {
	remoteAddr := destConn.RemoteAddr().(*net.TCPAddr)
	destPort := uint(remoteAddr.Port)

	probeHTTP(ctx, h.Logger, "encode-start",
		zap.String("dstAddr", destConn.RemoteAddr().String()),
		zap.Int("reqBufLen", len(reqBuf)))

	// Forward initial request to server.
	reqWriteStart := time.Now()
	_, err := destConn.Write(reqBuf)
	probeHTTP(ctx, h.Logger, "encode-req-written",
		zap.Int64("write_dur_ns", time.Since(reqWriteStart).Nanoseconds()),
		zap.Error(err))
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

			chunkedReqStart := time.Now()
			err := h.HandleChunkedRequests(ctx, &finalReq, clientConn, destConn)
			probeHTTP(ctx, h.Logger, "encode-chunked-req-done",
				zap.Int64("dur_ns", time.Since(chunkedReqStart).Nanoseconds()),
				zap.Int("finalReqLen", len(finalReq)),
				zap.Error(err))
			if err != nil {
				utils.LogError(h.Logger, err, "failed to handle chunked requests")
				errCh <- err
				return nil
			}

			h.Logger.Debug(fmt.Sprintf("This is the complete request:\n%v", string(finalReq)))

			// Read the response from the actual server.
			probeHTTP(ctx, h.Logger, "encode-read-resp-start")
			readRespStart := time.Now()
			resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
			probeHTTP(ctx, h.Logger, "encode-read-resp-done",
				zap.Int64("dur_ns", time.Since(readRespStart).Nanoseconds()),
				zap.Int("respLen", len(resp)),
				zap.Error(err))
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
				// Upstream error from pUtil.ReadBytes. The happy-path
				// assumption is "no response bytes received yet" (timeout,
				// connection refused, reset before any byte arrived, etc.).
				// Verify that invariant explicitly: pUtil.ReadBytes can, in
				// principle, return a non-EOF error after partial bytes were
				// already appended to its buffer (see the read loop in
				// pkg/agent/proxy/util/util.go — non-EOF errors surface
				// immediately and any bytes collected in the same iteration
				// are returned alongside the error). Copilot PR #4101 r6
				// flagged the missing precondition check on this branch.
				//
				// Two cases:
				//  1. len(resp) == 0 — the expected path. Historically the
				//     recorder dropped the call on the floor here, which
				//     meant replay had no mock to match the re-issued request
				//     and served a synthetic "no matching mock" 502 —
				//     producing body- and elapsed-ms diffs against the
				//     recorded run. Synthesize a well-formed HTTP response
				//     that captures the observed error (504 for timeouts,
				//     502 for everything else) and persist it as a mock,
				//     plus write it back to the client so record-mode
				//     matches replay-mode. See upstream_error.go.
				//  2. len(resp) != 0 — partial response bytes already
				//     received, then upstream errored. We DON'T write the
				//     partial bytes back and we DON'T persist them: a
				//     truncated HTTP message is unsafe to replay (downstream
				//     parseFinalHTTP would fail and we'd still drop the
				//     mock). Log a WARNING so operators can see the partial
				//     capture, then fall through to synthesize a fresh
				//     error response — same behaviour as the mid-body
				//     error branch below (line ~318).
				resTimestampMock := time.Now()
				reqMethod, reqURI := parseRequestMethodAndURL(finalReq)
				synthResp := synthesizeUpstreamErrorResponse(reqMethod, reqURI, err)
				if len(resp) == 0 {
					h.Logger.Info("upstream call errored before any response bytes; synthesized mock persisted so replay stays deterministic",
						zap.String("upstream_url", upstreamRequestURL(finalReq, destConn.RemoteAddr())),
						zap.String("error_class", upstreamErrorClass(err)),
						zap.Error(err))
					// Write the synthesized response back to the client so
					// record-mode behaviour matches replay-mode: in replay
					// the captured 502/504 is served from the mock store,
					// so the real live client in record mode should see
					// the same response instead of an unexplained
					// EOF/timeout. Safe to do here because no response
					// bytes were forwarded yet. A write failure is
					// non-fatal — the mock has already been persisted for
					// replay determinism.
					if _, werr := clientConn.Write(synthResp); werr != nil {
						h.Logger.Debug("failed to write synthesized upstream-error response to client; mock already persisted",
							zap.Error(werr))
					}
				} else {
					// Partial-response-then-error: we can't safely write
					// the synth response back to the client because some
					// real upstream bytes were already forwarded-pending
					// (not yet flushed to clientConn — the forward happens
					// on the non-error path at line ~292). The client has
					// not seen anything from us on this response yet, but
					// the capture is still truncated — drop the partial
					// bytes on the floor for replay safety and persist
					// the synthesized error mock instead.
					h.Logger.Error("upstream errored AFTER partial response bytes were received; discarding truncated bytes and persisting synthesized error mock",
						zap.Int("partial_resp_len", len(resp)),
						zap.String("upstream_url", upstreamRequestURL(finalReq, destConn.RemoteAddr())),
						zap.String("error_class", upstreamErrorClass(err)),
						zap.Error(err),
						zap.String("next_step", "investigate upstream stability (check connection resets, timeouts, keep-alive settings); recorded synthesized 5xx mock in place of the truncated response"))
				}
				enqueueMock(finalReq, synthResp, reqTimestampMock, resTimestampMock)
				errCh <- nil
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

			chunkedRespStart := time.Now()
			err = h.handleChunkedResponses(ctx, &finalResp, clientConn, destConn, resp)
			probeHTTP(ctx, h.Logger, "encode-chunked-resp-done",
				zap.Int64("dur_ns", time.Since(chunkedRespStart).Nanoseconds()),
				zap.Int("finalRespLen", len(finalResp)),
				zap.Error(err))
			if err != nil {
				if err == io.EOF {
					h.Logger.Debug("conn closed by the server", zap.Error(err))
					enqueueMock(finalReq, finalResp, reqTimestampMock, resTimestampMock)
					errCh <- nil
					return nil
				}
				// Upstream error while we were already reading the response
				// body (timeout mid-body, peer reset, broken pipe, etc.).
				// We always synthesize a fresh well-formed response here
				// rather than persisting the partial finalResp bytes: a
				// truncated HTTP message is unsafe to replay (downstream
				// parseFinalHTTP would fail and we'd still drop the mock),
				// and the synthesized response already carries the error
				// class marker + original error text so operators can see
				// exactly what went wrong. The important invariant — that
				// we persist SOMETHING instead of dropping — is preserved.
				utils.LogError(h.Logger, err, "failed to handle chunk response; persisting synthesized mock")
				reqMethod, reqURI := parseRequestMethodAndURL(finalReq)
				synthResp := synthesizeUpstreamErrorResponse(reqMethod, reqURI, err)
				enqueueMock(finalReq, synthResp, reqTimestampMock, time.Now())
				errCh <- nil
				return nil
			}

			h.Logger.Debug("This is the final response: " + string(finalResp))

			// Offload mock creation to async recorder.
			enqueueMock(finalReq, finalResp, reqTimestampMock, resTimestampMock)

			// Reset for the next request and response.
			finalReq = []byte("")

			// Read the next request from the client.
			h.Logger.Debug("Reading the request from the user client again from the same connection")
			probeHTTP(ctx, h.Logger, "encode-next-req-read-start")
			nextReqStart := time.Now()

			finalReq, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
			probeHTTP(ctx, h.Logger, "encode-next-req-read-done",
				zap.Int64("dur_ns", time.Since(nextReqStart).Nanoseconds()),
				zap.Int("reqLen", len(finalReq)),
				zap.Error(err))
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
