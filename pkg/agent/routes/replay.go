// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"encoding/gob"
	"encoding/json"
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

func (a *Agent) StoreMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")

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
