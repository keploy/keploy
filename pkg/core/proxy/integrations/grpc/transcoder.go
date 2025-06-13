//go:build linux

// Package grpc contains the mock-server side that Keploy spins up in *test* mode.
// It replays previously-recorded gRPC traffic by speaking raw HTTP/2.  The code
// below removes the last crash seen by avoiding the unsafe pattern of passing
// `*http2.Frame` values across goroutines (the http2 package re-uses those
// structs).  We now have **one goroutine** that owns Framer.ReadFrame *and* the
// request/response state.  While writing a response the goroutine actively
// polls the connection for WINDOW_UPDATE or SETTINGS frames to replenish window
// credit – this corresponds to the “Option B” design discussed earlier.
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
	return t.win.write(t.framer, func() error { return t.framer.WriteSettings(settings...) })
}
func (t *Transcoder) writeSettingsAck() error {
	return t.win.write(t.framer, t.framer.WriteSettingsAck)
}
func (t *Transcoder) writePingAck(data [8]byte) error {
	return t.win.write(t.framer, func() error { return t.framer.WritePing(true, data) })
}
func (t *Transcoder) writeHeaders(p http2.HeadersFrameParam) error {
	return t.win.write(t.framer, func() error { return t.framer.WriteHeaders(p) })
}
func (t *Transcoder) writeData(sid uint32, end bool, b []byte) error {
	return t.win.write(t.framer, func() error { return t.framer.WriteData(sid, end, b) })
}

// ---------------------------------------------------------------------------
// Frame-specific handlers ----------------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) handlePing(pf *http2.PingFrame) error {
	if pf.IsAck() {
		return nil
	}
	if pf.StreamID != 0 {
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	return t.writePingAck(pf.Data)
}

func (t *Transcoder) handleSettings(sf *http2.SettingsFrame) error {
	if sf.IsAck() {
		return nil
	}
	if v, ok := sf.Value(http2.SettingInitialWindowSize); ok {
		delta := int32(v) - 65535
		t.win.mu.Lock()
		t.win.conn += delta
		for id := range t.win.perSt {
			t.win.perSt[id] += delta
		}
		t.win.mu.Unlock()
	}
	return t.writeSettingsAck()
}

func (t *Transcoder) handleWindowUpdate(wu *http2.WindowUpdateFrame) {
	inc := int32(wu.Increment)
	t.win.mu.Lock()
	if wu.StreamID == 0 {
		t.win.conn += inc
	} else {
		t.win.perSt[wu.StreamID] += inc
	}
	t.win.mu.Unlock()
	t.logger.Debug("WINDOW_UPDATE", zap.Uint32("stream", wu.StreamID), zap.Int32("inc", inc))
}

// ---------------------------------------------------------------------------
// Helpers for generating the mock response ----------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) serveMock(ctx context.Context, sid uint32, mock *models.Mock) error {
	r := mock.Spec.GRPCResp

	// HEADERS ----------
	buf := new(bytes.Buffer)
	enc := hpack.NewEncoder(buf)
	for k, v := range r.Headers.PseudoHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	for k, v := range r.Headers.OrdinaryHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	if err := t.writeHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: buf.Bytes(), EndHeaders: true}); err != nil {
		return err
	}

	// DATA -------------
	payload, err := pkg.CreatePayloadFromLengthPrefixedMessage(r.Body)
	if err != nil {
		return err
	}
	if err := t.writeWithFlowControl(ctx, sid, payload); err != nil {
		return err
	}

	// TRAILERS ---------
	buf.Reset()
	enc = hpack.NewEncoder(buf)
	for k, v := range r.Trailers.PseudoHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	for k, v := range r.Trailers.OrdinaryHeaders {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}
	if err := t.writeHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: buf.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		return err
	}

	// cleanup window map
	t.win.mu.Lock()
	delete(t.win.perSt, sid)
	t.win.mu.Unlock()
	return nil
}

// core flow-control loop – single goroutine so we can safely poll ReadFrame
func (t *Transcoder) writeWithFlowControl(ctx context.Context, sid uint32, payload []byte) error {
	off, total := 0, len(payload)
	for off < total {
		rem := total - off
		chunk := min(rem, maxFrame)

		// wait for credit, reading frames meanwhile
		for {
			t.win.mu.Lock()
			have := t.win.conn >= int32(chunk) && t.win.perSt[sid] >= int32(chunk)
			t.win.mu.Unlock()
			if have {
				break
			}
			fr, err := t.framer.ReadFrame()
			if err != nil {
				return err
			}
			switch f := fr.(type) {
			case *http2.WindowUpdateFrame:
				t.handleWindowUpdate(f)
			case *http2.SettingsFrame:
				if err := t.handleSettings(f); err != nil {
					return err
				}
			case *http2.PingFrame:
				if err := t.handlePing(f); err != nil {
					return err
				}
			default:
				// ignore others while waiting for credit
			}
		}

		// deduct credit and write
		t.win.mu.Lock()
		t.win.conn -= int32(chunk)
		t.win.perSt[sid] -= int32(chunk)
		t.win.mu.Unlock()

		if err := t.writeData(sid, false, payload[off:off+chunk]); err != nil {
			return err
		}
		off += chunk
	}
	return nil
}

// ---------------------------------------------------------------------------
// Main serve-loop ------------------------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) ListenAndServe(ctx context.Context) error {
	if err := t.writeSettings(http2.Setting{ID: http2.SettingMaxFrameSize, Val: maxFrame}); err != nil {
		return err
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fr, err := t.framer.ReadFrame()
		if err != nil {
			return err
		}

		switch f := fr.(type) {
		case *http2.DataFrame:
			if err := t.processDataFrame(ctx, f); err != nil {
				return err
			}
		case *http2.HeadersFrame:
			if err := t.processHeadersFrame(f); err != nil {
				return err
			}
		case *http2.WindowUpdateFrame:
			t.handleWindowUpdate(f)
		case *http2.SettingsFrame:
			if err := t.handleSettings(f); err != nil {
				return err
			}
		case *http2.PingFrame:
			if err := t.handlePing(f); err != nil {
				return err
			}
		default:
			// silently skip the rest (PRIORITY/GOAWAY/etc.)
		}
	}
}

// ---------------------------------------------------------------------------
// Per-frame helpers used above ----------------------------------------------
// ---------------------------------------------------------------------------

func (t *Transcoder) processHeadersFrame(hf *http2.HeadersFrame) error {
	sid := hf.StreamID
	if sid%2 != 1 {
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	// ignore continuation blocks – framer will present complete blocks only
	pseudo, ordinary, err := extractHeaders(hf, t.decoder)
	if err != nil {
		return err
	}

	t.sic.AddHeadersForRequest(sid, pseudo, true)
	t.sic.AddHeadersForRequest(sid, ordinary, false)

	t.win.mu.Lock()
	if _, ok := t.win.perSt[sid]; !ok {
		t.win.perSt[sid] = 65535
	}
	t.win.mu.Unlock()
	return nil
}

func (t *Transcoder) processDataFrame(ctx context.Context, df *http2.DataFrame) error {
	sid := df.StreamID
	if sid == 0 {
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	t.sic.AddPayloadForRequest(sid, df.Data())

	if !df.StreamEnded() {
		return nil // wait for more request body
	}

	grpcReq := t.sic.FetchRequestForStream(sid)
	mock, err := FilterMocksBasedOnGrpcRequest(ctx, t.logger, grpcReq, t.mockDb)
	if err != nil {
		return err
	}
	if mock == nil {
		return fmt.Errorf("no mock recorded for stream %d", sid)
	}
	return t.serveMock(ctx, sid, mock)
}

// util ----------------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
