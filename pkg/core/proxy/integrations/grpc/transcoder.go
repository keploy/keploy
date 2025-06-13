//go:build linux

// Package grpc contains the mock‑server implementation that Keploy runs in test mode
// in order to replay previously‑recorded gRPC traffic.  It speaks raw HTTP/2 on the
// wire and therefore implements **flow‑control**, header decoding and frame I/O by
// hand using x/net/http2’s low‑level Framer.
//
// The code below fixes two long‑standing problems that caused random EOFs and data
// corruption:
//
//   1. **HTTP/2 flow‑control** is now honoured.  We keep track of the peer’s
//      connection‑ and per‑stream windows and block writes until WINDOW_UPDATE
//      frames replenish the credit.
//   2. **Single reader principle**: exactly one goroutine calls Framer.ReadFrame
//      to avoid races inside Framer.  The reader pushes frames to a channel that
//      the main goroutine consumes.
//
// In addition we guard every Framer.Write* invocation with a mutex (wmu) so that
// concurrent writers can never interleave bytes and corrupt a frame.
//
// The external behaviour is unchanged: Keploy still selects a matching mock and
// answers the client with the recorded headers / payload / trailers.

package grpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// -----------------------------------------------------------------------------
// Window accounting helpers ---------------------------------------------------
// -----------------------------------------------------------------------------

type sendWindow struct {
	conn  int32            // connection‑level credit
	perSt map[uint32]int32 // per‑stream window credit

	mu   sync.Mutex // protects conn, perSt AND the cond
	cond *sync.Cond // broadcast when credit grows
	wmu  sync.Mutex // serialises every Framer.Write* call
}

func newSendWindow() sendWindow {
	w := sendWindow{
		conn:  65535, // default until peer SETTINGS arrives
		perSt: make(map[uint32]int32),
	}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// -----------------------------------------------------------------------------
// Transcoder ------------------------------------------------------------------
// -----------------------------------------------------------------------------

type Transcoder struct {
	sic     *StreamInfoCollection
	mockDb  integrations.MockMemDb
	logger  *zap.Logger
	framer  *http2.Framer
	decoder *hpack.Decoder
	window  sendWindow
}

func NewTranscoder(logger *zap.Logger, framer *http2.Framer, mockDb integrations.MockMemDb) *Transcoder {
	return &Transcoder{
		logger:  logger,
		framer:  framer,
		mockDb:  mockDb,
		sic:     NewStreamInfoCollection(),
		decoder: NewDecoder(),
		window:  newSendWindow(),
	}
}

// max frame size – 8 KiB keeps memory down and is plenty for gRPC payloads.
const maxFrameSize = 8 * 1024

// -----------------------------------------------------------------------------
// tiny helper to run ANY "framer.Write…" under the write mutex ---------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) withWrite(fn func() error) error {
	srv.window.wmu.Lock()
	defer srv.window.wmu.Unlock()
	return fn()
}

// -----------------------------------------------------------------------------
// Outgoing SETTINGS / PING helpers -------------------------------------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) writeInitialSettingsFrame() error {
	settings := []http2.Setting{{ID: http2.SettingMaxFrameSize, Val: maxFrameSize}}
	return srv.withWrite(func() error { return srv.framer.WriteSettings(settings...) })
}

func (srv *Transcoder) writePingAck(data [8]byte) error {
	return srv.withWrite(func() error { return srv.framer.WritePing(true, data) })
}

func (srv *Transcoder) writeHeaders(p http2.HeadersFrameParam) error {
	return srv.withWrite(func() error { return srv.framer.WriteHeaders(p) })
}

func (srv *Transcoder) writeData(streamID uint32, end bool, b []byte) error {
	return srv.withWrite(func() error { return srv.framer.WriteData(streamID, end, b) })
}

func (srv *Transcoder) writeSettingsAck() error {
	return srv.withWrite(srv.framer.WriteSettingsAck)
}

// -----------------------------------------------------------------------------
// Reader‑side frame handling ---------------------------------------------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) processPingFrame(pf *http2.PingFrame) error {
	if pf.IsAck() {
		return nil // nothing to do
	}
	if pf.StreamID != 0 {
		srv.logger.Error("PING frame had non‑zero stream ID", zap.Uint32("id", pf.StreamID))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	return srv.writePingAck(pf.Data)
}

func (srv *Transcoder) processSettingsFrame(sf *http2.SettingsFrame) error {
	if sf.IsAck() {
		return nil // we never sent SETTINGS that expect an ACK
	}
	if v, ok := sf.Value(http2.SettingInitialWindowSize); ok {
		delta := int32(v) - 65535

		srv.window.mu.Lock()
		srv.window.conn += delta
		for id := range srv.window.perSt {
			srv.window.perSt[id] += delta
		}
		srv.window.mu.Unlock()
	}
	return srv.writeSettingsAck()
}

func (srv *Transcoder) processWindowUpdate(wu *http2.WindowUpdateFrame) error {
	inc := int32(wu.Increment)

	srv.window.mu.Lock()
	if wu.StreamID == 0 {
		srv.window.conn += inc
	} else {
		srv.window.perSt[wu.StreamID] += inc
	}
	srv.window.cond.Broadcast()
	srv.window.mu.Unlock()

	srv.logger.Debug("WINDOW_UPDATE", zap.Uint32("stream", wu.StreamID), zap.Int32("inc", inc))
	return nil
}

// -----------------------------------------------------------------------------
// Header / data generation -----------------------------------------------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) serveMock(ctx context.Context, streamID uint32, mock *models.Mock) error {
	resp := mock.Spec.GRPCResp

	// ----- 1️⃣ HEADERS ---------------------------------------------------------
	buf := new(bytes.Buffer)
	enc := hpack.NewEncoder(buf)
	for k, v := range resp.Headers.PseudoHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	for k, v := range resp.Headers.OrdinaryHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	if err := srv.writeHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: buf.Bytes(),
		EndStream:     false,
		EndHeaders:    true,
	}); err != nil {
		return err
	}

	// ----- 2️⃣ DATA ------------------------------------------------------------
	payload, err := pkg.CreatePayloadFromLengthPrefixedMessage(resp.Body)
	if err != nil {
		return err
	}
	if err := srv.writePayloadWithFlowControl(ctx, streamID, payload); err != nil {
		return err
	}

	// ----- 3️⃣ TRAILERS --------------------------------------------------------
	buf.Reset()
	enc = hpack.NewEncoder(buf)
	for k, v := range resp.Trailers.PseudoHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	for k, v := range resp.Trailers.OrdinaryHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	if err := srv.writeHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: buf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		return err
	}

	// garbage‑collect per‑stream window bookkeeping
	srv.window.mu.Lock()
	delete(srv.window.perSt, streamID)
	srv.window.mu.Unlock()
	return nil
}

// split out so we can unit‑test easily
func (srv *Transcoder) writePayloadWithFlowControl(ctx context.Context, streamID uint32, payload []byte) error {
	off := 0
	total := len(payload)

	for off < total {
		// honour ctx cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		remain := total - off
		chunk := min(remain, maxFrameSize)

		// ---- FLOW CONTROL ---------------------------------------------------
		srv.window.mu.Lock()
		for srv.window.conn < int32(chunk) || srv.window.perSt[streamID] < int32(chunk) {
			srv.window.cond.Wait()
		}
		srv.window.conn -= int32(chunk)
		srv.window.perSt[streamID] -= int32(chunk)
		srv.window.mu.Unlock()
		//--------------------------------------------------------------------

		if err := srv.writeData(streamID, false, payload[off:off+chunk]); err != nil {
			return err
		}
		off += chunk
	}
	return nil
}

// -----------------------------------------------------------------------------
// Frame dispatcher -------------------------------------------------------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) processHeadersFrame(hf *http2.HeadersFrame) error {
	id := hf.StreamID
	if id%2 != 1 {
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	// If END_HEADERS not set we ignore: header block continues in CONTINUATION
	if !hf.HeadersEnded() {
		srv.logger.Debug("partial header block – skipping until END_HEADERS")
		return nil
	}

	pseudo, ordinary, err := extractHeaders(hf, srv.decoder)
	if err != nil {
		return err
	}

	srv.sic.AddHeadersForRequest(id, pseudo, true)
	srv.sic.AddHeadersForRequest(id, ordinary, false)

	srv.window.mu.Lock()
	if _, ok := srv.window.perSt[id]; !ok {
		srv.window.perSt[id] = 65535 // will be adjusted by SETTINGS later
	}
	srv.window.mu.Unlock()
	return nil
}

func (srv *Transcoder) processDataFrame(ctx context.Context, df *http2.DataFrame) error {
	id := df.StreamID
	if id == 0 {
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	srv.sic.AddPayloadForRequest(id, df.Data())

	// when END_STREAM the client has sent complete request – respond now
	if !df.StreamEnded() {
		return nil
	}

	grpcReq := srv.sic.FetchRequestForStream(id)
	mock, err := FilterMocksBasedOnGrpcRequest(ctx, srv.logger, grpcReq, srv.mockDb)
	if err != nil {
		return err
	}
	if mock == nil {
		return fmt.Errorf("no mock found for stream %d", id)
	}

	return srv.serveMock(ctx, id, mock)
}

// -----------------------------------------------------------------------------
// Public entry ‑ ListenAndServe ------------------------------------------------
// -----------------------------------------------------------------------------

func (srv *Transcoder) ListenAndServe(ctx context.Context) error {
	if err := srv.writeInitialSettingsFrame(); err != nil {
		return err
	}

	frames := make(chan http2.Frame, 32)
	errs := make(chan error, 1)

	// 👂 dedicated reader
	go func() {
		defer close(frames)
		for {
			if ctx.Err() != nil {
				errs <- ctx.Err()
				return
			}
			fr, err := srv.framer.ReadFrame()
			if err != nil {
				errs <- err
				return
			}
			frames <- fr
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errs:
			return err

		case fr := <-frames:
			if fr == nil { // channel closed
				return io.EOF
			}

			var perr error
			switch f := fr.(type) {
			case *http2.DataFrame:
				perr = srv.processDataFrame(ctx, f)
			case *http2.HeadersFrame:
				perr = srv.processHeadersFrame(f)
			case *http2.WindowUpdateFrame:
				perr = srv.processWindowUpdate(f)
			case *http2.SettingsFrame:
				perr = srv.processSettingsFrame(f)
			case *http2.PingFrame:
				perr = srv.processPingFrame(f)
			case *http2.GoAwayFrame, *http2.PriorityFrame,
				*http2.ContinuationFrame, *http2.PushPromiseFrame, *http2.RSTStreamFrame:
				// unsupported or intentionally ignored
			default:
				srv.logger.Warn("unknown frame type", zap.String("type", fmt.Sprintf("%T", fr)))
			}

			if perr != nil {
				srv.logger.Error("frame processing failed", zap.Error(perr))
				return perr
			}
		}
	}
}

// -----------------------------------------------------------------------------
// utils -----------------------------------------------------------------------
// -----------------------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
