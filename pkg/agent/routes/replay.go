// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func (a *Agent) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockRes := models.AgentResp{
		Error:     nil,
		IsSuccess: true,
	}

	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)
	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, mockRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.svc.MockOutgoing(r.Context(), OutgoingReq.OutgoingOptions)
	if err != nil {
		// Bug fix: previously `render.JSON(w, r, err)` serialized the
		// raw Go error interface. Numeric-typed errors (e.g.,
		// syscall.Errno which is type Errno uintptr) marshaled as bare
		// JSON numbers, breaking the CLI's AgentResp decoder with
		// "cannot unmarshal number into Go value of type
		// models.AgentResp" — masking the real error message.
		// Always render the structured AgentResp so the CLI gets a
		// consistent shape and can extract the underlying error
		// string via mockRes.Error.
		// MARK_MOCKOUTGOING_FIX_2026_05_14: this string must appear in /usr/local/bin/keploy if the OSS replace is taking effect.
		a.logger.Info("MARK_MOCKOUTGOING_FIX_2026_05_14: MockOutgoing handler error path producing proper AgentResp",
			zap.Error(err))
		mockRes.IsSuccess = false
		mockRes.Error = nil // intentionally nil — error interface serializes inconsistently; surface message via a string field below
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]any{
			"isSuccess": false,
			"error":     err.Error(),
		})
		return
	}

	render.JSON(w, r, mockRes)
	render.Status(r, http.StatusOK)
}

func (a *Agent) GetConsumedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	consumedMocks, err := a.svc.GetConsumedMocks(r.Context())
	if err != nil {
		// Same bug class as MockOutgoing's old `render.JSON(w, r, err)`
		// — raw error interface produces inconsistent JSON shapes for
		// the caller. Return a structured error wrapper instead so the
		// CLI gets a predictable shape.
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}

	render.JSON(w, r, consumedMocks)
	render.Status(r, http.StatusOK)
}

func (a *Agent) GetMockErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockErrors, err := a.svc.GetMockErrors(r.Context())
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, mockErrors)
}

// BeginTestErrorCapture opens a per-test mock-error capture window in the proxy
// so the next GetMockErrors returns only this test's misses. Implemented via a
// capability type-assertion so the agent.Service interface stays unchanged.
func (a *Agent) BeginTestErrorCapture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if b, ok := a.svc.(interface {
		BeginTestErrorCapture(context.Context) error
	}); ok {
		if err := b.BeginTestErrorCapture(r.Context()); err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]string{"error": err.Error()})
			return
		}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

func (a *Agent) StoreMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")

	// Streaming path: a new k8s-proxy sends the mock corpus mock-by-mock
	// (Content-Type application/x-gob-stream) so the agent never materializes
	// the whole gob payload + a full decoded slice at once — the transient
	// that OOM-kills the auto-replay agent. Selected by Content-Type; an old
	// client (or any non-stream body) falls through to the legacy whole-decode
	// below, so a new agent still serves old clients.
	if r.Header.Get("Content-Type") == models.StoreMocksStreamContentType {
		a.storeMocksStream(w, r)
		return
	}

	var storeMocksReq models.StoreMocksReq
	if err := gob.NewDecoder(r.Body).Decode(&storeMocksReq); err != nil {
		storeMocksRes := models.AgentResp{
			Error:     err,
			IsSuccess: false,
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = gob.NewEncoder(w).Encode(storeMocksRes)
		return
	}

	err := a.svc.StoreMocks(r.Context(), storeMocksReq.Filtered, storeMocksReq.UnFiltered)

	storeMocksRes := models.AgentResp{
		Error:     err,
		IsSuccess: err == nil,
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = gob.NewEncoder(w).Encode(storeMocksRes)
}

// storeMocksStream handles the streaming /storemocks body: raw magic line, one
// gob MockStreamHeader, then header.FilteredCount+UnfilteredCount bare Mock
// values. It verifies the magic, decodes the header, then hands the live
// decoder to the service's StoreMocksStream (accessed via a capability
// type-assertion so the agent.Service interface stays unchanged — same pattern
// as BeginTestErrorCapture). Response framing (a single AgentResp gob) matches
// the legacy path so the client decode is identical.
func (a *Agent) storeMocksStream(w http.ResponseWriter, r *http.Request) {
	writeErr := func(status int, err error) {
		w.WriteHeader(status)
		_ = gob.NewEncoder(w).Encode(models.AgentResp{Error: err, IsSuccess: false})
	}

	magic := make([]byte, len(models.StoreMocksStreamMagic))
	if _, err := io.ReadFull(r.Body, magic); err != nil {
		writeErr(http.StatusBadRequest, fmt.Errorf("storemocks stream: read magic: %w", err))
		return
	}
	if string(magic) != models.StoreMocksStreamMagic {
		writeErr(http.StatusBadRequest, fmt.Errorf("storemocks stream: unrecognized magic %q", magic))
		return
	}

	dec := gob.NewDecoder(r.Body)
	var header models.MockStreamHeader
	if err := dec.Decode(&header); err != nil {
		writeErr(http.StatusBadRequest, fmt.Errorf("storemocks stream: decode header: %w", err))
		return
	}

	streamer, ok := a.svc.(interface {
		StoreMocksStream(context.Context, models.MockStreamHeader, *gob.Decoder) error
	})
	if !ok {
		writeErr(http.StatusInternalServerError, fmt.Errorf("storemocks stream: service does not support streaming"))
		return
	}

	err := streamer.StoreMocksStream(r.Context(), header, dec)
	if err != nil {
		writeErr(http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
}

func (a *Agent) UpdateMockParams(w http.ResponseWriter, r *http.Request) {

	start := time.Now()

	w.Header().Set("Content-Type", "application/json")
	var updateParamsReq models.UpdateMockParamsReq
	err := json.NewDecoder(r.Body).Decode(&updateParamsReq)

	updateParamsRes := models.AgentResp{
		Error:     nil,
		IsSuccess: true,
	}

	if err != nil {
		updateParamsRes.Error = err
		updateParamsRes.IsSuccess = false
		render.JSON(w, r, updateParamsRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.svc.UpdateMockParams(r.Context(), updateParamsReq.FilterParams)
	if err != nil {
		updateParamsRes.Error = err
		updateParamsRes.IsSuccess = false
		render.JSON(w, r, updateParamsRes)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	a.logger.Debug("Time taken to update mock params duration :", zap.Duration("duration", time.Since(start)))

	render.JSON(w, r, updateParamsRes)
	render.Status(r, http.StatusOK)
}
