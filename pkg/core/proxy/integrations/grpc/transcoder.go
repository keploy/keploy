//go:build linux

// Package grpc contains the mock-server side that Keploy spins up in *test* mode.
// It replays previously-recorded gRPC traffic by speaking raw HTTP/2.  The code
// below removes the last crash seen by avoiding the unsafe pattern of passing
// `*http2.Frame` values across goroutines (the http2 package re-uses those
// structs).  We now have **one goroutine** that owns Framer.ReadFrame *and* the
// request/response state.  While writing a response the goroutine actively
// polls the connection for WINDOW_UPDATE or SETTINGS frames to replenish window
// credit – this corresponds to the "Option B" design discussed earlier.
//
// Flow-control, per-stream bookkeeping, and thread-safe writes are unchanged.
// No functionality has been removed; the panic in SettingsFrame.Value can no
// longer occur.

package grpc

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// ---------------------------------------------------------------------------
// send-window accounting -----------------------------------------------------
// ---------------------------------------------------------------------------

type sendWindow struct {
	conn  int32            // connection-level credit
	perSt map[uint32]int32 // per-stream credit

	mu  sync.Mutex // guards conn+perSt
	wmu sync.Mutex // serialises every framer.Write*
}

func newSendWindow() sendWindow {
	return sendWindow{
		conn:  65535,
		perSt: make(map[uint32]int32),
	}
}

// helper: run any framer.Write* atomically
func (w *sendWindow) write(f *http2.Framer, fn func() error) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return fn()
}

// ---------------------------------------------------------------------------
// Transcoder ----------------------------------------------------------------
// ---------------------------------------------------------------------------

type Transcoder struct {
	sic     *StreamInfoCollection
	mockDb  integrations.MockMemDb
	logger  *zap.Logger
	framer  *http2.Framer
	decoder *hpack.Decoder
	win     sendWindow
}

func NewTranscoder(l *zap.Logger, f *http2.Framer, db integrations.MockMemDb) *Transcoder {
	l.Info("creating new transcoder", zap.Uint32("initial_window_size", 65535))
	return &Transcoder{
		logger:  l,
		framer:  f,
		mockDb:  db,
		sic:     NewStreamInfoCollection(),
		decoder: NewDecoder(),
		win:     newSendWindow(),
	}
}

const maxFrame = 8 * 1024 // 8 KiB

// ---------------------------------------------------------------------------
// Low-level write helpers (all go through win.write) ------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) writeSettings(settings ...http2.Setting) error {
	t.logger.Debug("writing settings frame", zap.Int("settings_count", len(settings)))
	err := t.win.write(t.framer, func() error { return t.framer.WriteSettings(settings...) })
	if err != nil {
		t.logger.Error("failed to write settings frame", zap.Error(err))
	}
	return err
}

func (t *Transcoder) writeSettingsAck() error {
	t.logger.Debug("writing settings ack frame")
	err := t.win.write(t.framer, t.framer.WriteSettingsAck)
	if err != nil {
		t.logger.Error("failed to write settings ack frame", zap.Error(err))
	}
	return err
}

func (t *Transcoder) writePingAck(data [8]byte) error {
	t.logger.Debug("writing ping ack frame", zap.Binary("ping_data", data[:]))
	err := t.win.write(t.framer, func() error { return t.framer.WritePing(true, data) })
	if err != nil {
		t.logger.Error("failed to write ping ack frame", zap.Error(err))
	}
	return err
}

func (t *Transcoder) writeHeaders(p http2.HeadersFrameParam) error {
	t.logger.Debug("writing headers frame",
		zap.Uint32("stream_id", p.StreamID),
		zap.Bool("end_stream", p.EndStream),
		zap.Bool("end_headers", p.EndHeaders),
		zap.Int("block_fragment_size", len(p.BlockFragment)))
	err := t.win.write(t.framer, func() error { return t.framer.WriteHeaders(p) })
	if err != nil {
		t.logger.Error("failed to write headers frame",
			zap.Uint32("stream_id", p.StreamID),
			zap.Error(err))
	}
	return err
}

func (t *Transcoder) writeData(sid uint32, end bool, b []byte) error {
	t.logger.Debug("writing data frame",
		zap.Uint32("stream_id", sid),
		zap.Bool("end_stream", end),
		zap.Int("data_size", len(b)))
	err := t.win.write(t.framer, func() error { return t.framer.WriteData(sid, end, b) })
	if err != nil {
		t.logger.Error("failed to write data frame",
			zap.Uint32("stream_id", sid),
			zap.Error(err))
	}
	return err
}

// ---------------------------------------------------------------------------
// Frame-specific handlers ----------------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) handlePing(pf *http2.PingFrame) error {
	t.logger.Debug("handling ping frame",
		zap.Uint32("stream_id", pf.StreamID),
		zap.Bool("is_ack", pf.IsAck()),
		zap.Binary("ping_data", pf.Data[:]))

	if pf.IsAck() {
		t.logger.Debug("received ping ack, no response needed")
		return nil
	}
	if pf.StreamID != 0 {
		t.logger.Error("ping frame with non-zero stream id", zap.Uint32("stream_id", pf.StreamID))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	return t.writePingAck(pf.Data)
}

func (t *Transcoder) handleSettings(sf *http2.SettingsFrame) error {
	t.logger.Debug("handling settings frame",
		zap.Bool("is_ack", sf.IsAck()),
		zap.Uint32("stream_id", sf.StreamID))

	if sf.IsAck() {
		t.logger.Debug("received settings ack, no response needed")
		return nil
	}

	if v, ok := sf.Value(http2.SettingInitialWindowSize); ok {
		delta := int32(v) - 65535
		t.logger.Info("updating initial window size",
			zap.Uint32("new_window_size", v),
			zap.Int32("delta", delta))

		t.win.mu.Lock()
		oldConn := t.win.conn
		t.win.conn += delta
		streamCount := len(t.win.perSt)
		for id := range t.win.perSt {
			t.win.perSt[id] += delta
		}
		t.win.mu.Unlock()

		t.logger.Debug("window size updated",
			zap.Int32("old_conn_window", oldConn),
			zap.Int32("new_conn_window", t.win.conn),
			zap.Int("streams_updated", streamCount))
	}
	return t.writeSettingsAck()
}

func (t *Transcoder) handleWindowUpdate(wu *http2.WindowUpdateFrame) {
	inc := int32(wu.Increment)
	t.logger.Debug("handling window update",
		zap.Uint32("stream_id", wu.StreamID),
		zap.Int32("increment", inc))

	t.win.mu.Lock()
	if wu.StreamID == 0 {
		oldConn := t.win.conn
		t.win.conn += inc
		t.logger.Debug("updated connection window",
			zap.Int32("old_window", oldConn),
			zap.Int32("new_window", t.win.conn))
	} else {
		oldStream := t.win.perSt[wu.StreamID]
		t.win.perSt[wu.StreamID] += inc
		t.logger.Debug("updated stream window",
			zap.Uint32("stream_id", wu.StreamID),
			zap.Int32("old_window", oldStream),
			zap.Int32("new_window", t.win.perSt[wu.StreamID]))
	}
	t.win.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Helpers for generating the mock response ----------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) serveMock(ctx context.Context, sid uint32, mock *models.Mock) error {
	t.logger.Info("serving mock response",
		zap.Uint32("stream_id", sid),
		zap.String("mock_name", mock.Name))

	r := mock.Spec.GRPCResp

	// HEADERS ----------
	t.logger.Debug("encoding response headers",
		zap.Uint32("stream_id", sid),
		zap.Int("pseudo_headers", len(r.Headers.PseudoHeaders)),
		zap.Int("ordinary_headers", len(r.Headers.OrdinaryHeaders)))

	buf := new(bytes.Buffer)
	enc := hpack.NewEncoder(buf)
	for k, v := range r.Headers.PseudoHeaders {
		if err := enc.WriteField(hpack.HeaderField{Name: k, Value: v}); err != nil {
			t.logger.Error("failed to encode pseudo header",
				zap.String("key", k), zap.String("value", v), zap.Error(err))
			return err
		}
	}
	for k, v := range r.Headers.OrdinaryHeaders {
		if err := enc.WriteField(hpack.HeaderField{Name: k, Value: v}); err != nil {
			t.logger.Error("failed to encode ordinary header",
				zap.String("key", k), zap.String("value", v), zap.Error(err))
			return err
		}
	}
	if err := t.writeHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: buf.Bytes(), EndHeaders: true}); err != nil {
		t.logger.Error("failed to write response headers", zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}

	// DATA -------------
	t.logger.Debug("preparing response body", zap.Uint32("stream_id", sid))
	payload, err := pkg.CreatePayloadFromLengthPrefixedMessage(r.Body)
	if err != nil {
		t.logger.Error("failed to create payload from response body",
			zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}

	t.logger.Debug("writing response body with flow control",
		zap.Uint32("stream_id", sid),
		zap.Int("payload_size", len(payload)))
	if err := t.writeWithFlowControl(ctx, sid, payload); err != nil {
		t.logger.Error("failed to write response body", zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}

	// TRAILERS ---------
	t.logger.Debug("encoding response trailers",
		zap.Uint32("stream_id", sid),
		zap.Int("pseudo_trailers", len(r.Trailers.PseudoHeaders)),
		zap.Int("ordinary_trailers", len(r.Trailers.OrdinaryHeaders)))

	buf.Reset()
	enc = hpack.NewEncoder(buf)
	for k, v := range r.Trailers.PseudoHeaders {
		if err := enc.WriteField(hpack.HeaderField{Name: k, Value: v}); err != nil {
			t.logger.Error("failed to encode pseudo trailer",
				zap.String("key", k), zap.String("value", v), zap.Error(err))
			return err
		}
	}
	for k, v := range r.Trailers.OrdinaryHeaders {
		if err := enc.WriteField(hpack.HeaderField{Name: k, Value: v}); err != nil {
			t.logger.Error("failed to encode ordinary trailer",
				zap.String("key", k), zap.String("value", v), zap.Error(err))
			return err
		}
	}
	if err := t.writeHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: buf.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.logger.Error("failed to write response trailers", zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}

	// cleanup window map
	t.win.mu.Lock()
	delete(t.win.perSt, sid)
	t.win.mu.Unlock()

	t.logger.Info("successfully served mock response", zap.Uint32("stream_id", sid))
	return nil
}

// core flow-control loop – single goroutine so we can safely poll ReadFrame
func (t *Transcoder) writeWithFlowControl(ctx context.Context, sid uint32, payload []byte) error {
	off, total := 0, len(payload)
	t.logger.Debug("starting flow control write",
		zap.Uint32("stream_id", sid),
		zap.Int("total_bytes", total))

	for off < total {
		rem := total - off
		chunk := min(rem, maxFrame)

		t.logger.Debug("preparing to write chunk",
			zap.Uint32("stream_id", sid),
			zap.Int("chunk_size", chunk),
			zap.Int("offset", off),
			zap.Int("remaining", rem))

		// wait for credit, reading frames meanwhile
		for {
			t.win.mu.Lock()
			connCredit := t.win.conn
			streamCredit := t.win.perSt[sid]
			have := connCredit >= int32(chunk) && streamCredit >= int32(chunk)
			t.win.mu.Unlock()

			if have {
				t.logger.Debug("sufficient window credit available",
					zap.Uint32("stream_id", sid),
					zap.Int32("conn_credit", connCredit),
					zap.Int32("stream_credit", streamCredit),
					zap.Int("chunk_size", chunk))
				break
			}

			t.logger.Debug("waiting for window credit",
				zap.Uint32("stream_id", sid),
				zap.Int32("conn_credit", connCredit),
				zap.Int32("stream_credit", streamCredit),
				zap.Int("needed", chunk))

			fr, err := t.framer.ReadFrame()
			if err != nil {
				t.logger.Error("failed to read frame while waiting for credit",
					zap.Uint32("stream_id", sid), zap.Error(err))
				return err
			}

			switch f := fr.(type) {
			case *http2.WindowUpdateFrame:
				t.handleWindowUpdate(f)
			case *http2.SettingsFrame:
				if err := t.handleSettings(f); err != nil {
					t.logger.Error("failed to handle settings frame during flow control", zap.Error(err))
					return err
				}
			case *http2.PingFrame:
				if err := t.handlePing(f); err != nil {
					t.logger.Error("failed to handle ping frame during flow control", zap.Error(err))
					return err
				}
			default:
				t.logger.Debug("ignoring frame while waiting for credit",
					zap.String("frame_type", fmt.Sprintf("%T", f)))
			}
		}

		// deduct credit and write
		t.win.mu.Lock()
		t.win.conn -= int32(chunk)
		t.win.perSt[sid] -= int32(chunk)
		t.win.mu.Unlock()

		if err := t.writeData(sid, false, payload[off:off+chunk]); err != nil {
			t.logger.Error("failed to write data chunk",
				zap.Uint32("stream_id", sid),
				zap.Int("chunk_size", chunk),
				zap.Int("offset", off),
				zap.Error(err))
			return err
		}
		off += chunk

		t.logger.Debug("wrote data chunk",
			zap.Uint32("stream_id", sid),
			zap.Int("chunk_size", chunk),
			zap.Int("new_offset", off),
			zap.Int("remaining", total-off))
	}

	t.logger.Debug("completed flow control write",
		zap.Uint32("stream_id", sid),
		zap.Int("total_bytes", total))
	return nil
}

// ---------------------------------------------------------------------------
// Main serve-loop ------------------------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) ListenAndServe(ctx context.Context) error {
	t.logger.Info("starting transcoder server", zap.Uint32("max_frame_size", maxFrame))

	if err := t.writeSettings(http2.Setting{ID: http2.SettingMaxFrameSize, Val: maxFrame}); err != nil {
		t.logger.Error("failed to write initial settings", zap.Error(err))
		return err
	}

	for {
		if ctx.Err() != nil {
			t.logger.Info("context cancelled, stopping transcoder", zap.Error(ctx.Err()))
			return ctx.Err()
		}

		fr, err := t.framer.ReadFrame()
		if err != nil {
			t.logger.Error("failed to read frame", zap.Error(err))
			return err
		}

		t.logger.Debug("received frame", zap.String("frame_type", fmt.Sprintf("%T", fr)))

		switch f := fr.(type) {
		case *http2.DataFrame:
			if err := t.processDataFrame(ctx, f); err != nil {
				t.logger.Error("failed to process data frame",
					zap.Uint32("stream_id", f.StreamID), zap.Error(err))
				return err
			}
		case *http2.HeadersFrame:
			if err := t.processHeadersFrame(f); err != nil {
				t.logger.Error("failed to process headers frame",
					zap.Uint32("stream_id", f.StreamID), zap.Error(err))
				return err
			}
		case *http2.WindowUpdateFrame:
			t.handleWindowUpdate(f)
		case *http2.SettingsFrame:
			if err := t.handleSettings(f); err != nil {
				t.logger.Error("failed to handle settings frame", zap.Error(err))
				return err
			}
		case *http2.PingFrame:
			if err := t.handlePing(f); err != nil {
				t.logger.Error("failed to handle ping frame", zap.Error(err))
				return err
			}
		default:
			t.logger.Debug("ignoring frame type", zap.String("frame_type", fmt.Sprintf("%T", f)))
		}
	}
}

// ---------------------------------------------------------------------------
// Per-frame helpers used above ----------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) processHeadersFrame(hf *http2.HeadersFrame) error {
	sid := hf.StreamID
	t.logger.Debug("processing headers frame",
		zap.Uint32("stream_id", sid),
		zap.Bool("end_stream", hf.StreamEnded()),
		zap.Bool("end_headers", hf.HeadersEnded()))

	if sid%2 != 1 {
		t.logger.Error("invalid stream id for headers frame", zap.Uint32("stream_id", sid))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	// ignore continuation blocks – framer will present complete blocks only
	pseudo, ordinary, err := extractHeaders(hf, t.decoder)
	if err != nil {
		t.logger.Error("failed to extract headers", zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}

	t.logger.Debug("extracted headers",
		zap.Uint32("stream_id", sid),
		zap.Int("pseudo_headers", len(pseudo)),
		zap.Int("ordinary_headers", len(ordinary)))

	t.sic.AddHeadersForRequest(sid, pseudo, true)
	t.sic.AddHeadersForRequest(sid, ordinary, false)

	t.win.mu.Lock()
	if _, ok := t.win.perSt[sid]; !ok {
		t.win.perSt[sid] = 65535
		t.logger.Debug("initialized stream window",
			zap.Uint32("stream_id", sid),
			zap.Int32("initial_window", 65535))
	}
	t.win.mu.Unlock()

	t.logger.Debug("successfully processed headers frame", zap.Uint32("stream_id", sid))
	return nil
}

func (t *Transcoder) processDataFrame(ctx context.Context, df *http2.DataFrame) error {
	sid := df.StreamID
	t.logger.Debug("processing data frame",
		zap.Uint32("stream_id", sid),
		zap.Int("data_size", len(df.Data())),
		zap.Bool("stream_ended", df.StreamEnded()))

	if sid == 0 {
		t.logger.Error("data frame with stream id 0")
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	t.sic.AddPayloadForRequest(sid, df.Data())

	if !df.StreamEnded() {
		t.logger.Debug("waiting for more request body", zap.Uint32("stream_id", sid))
		return nil // wait for more request body
	}

	t.logger.Debug("request completed, fetching mock", zap.Uint32("stream_id", sid))
	grpcReq := t.sic.FetchRequestForStream(sid)
	mock, err := FilterMocksBasedOnGrpcRequest(ctx, t.logger, grpcReq, t.mockDb)
	if err != nil {
		t.logger.Error("failed to filter mocks", zap.Uint32("stream_id", sid), zap.Error(err))
		return err
	}
	if mock == nil {
		t.logger.Error("no mock found for request", zap.Uint32("stream_id", sid))
		return fmt.Errorf("no mock recorded for stream %d", sid)
	}

	t.logger.Info("found matching mock",
		zap.Uint32("stream_id", sid),
		zap.String("mock_name", mock.Name))
	return t.serveMock(ctx, sid, mock)
}

// util ----------------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
