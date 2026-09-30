// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func (a *Agent) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var OutgoingReq models.OutgoingReq
	if err := json.NewDecoder(r.Body).Decode(&OutgoingReq); err != nil {
		respondAgent(w, r, http.StatusBadRequest, err)
		return
	}

	if err := a.svc.MockOutgoing(r.Context(), OutgoingReq.OutgoingOptions); err != nil {
		utils.LogError(a.logger, err, "failed to mock outgoing")
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}

	respondAgent(w, r, http.StatusOK, nil)
}

// respondAgent writes an AgentResp with the given status. It exists so no
// handler can repeat either of the two mistakes this file used to make:
// rendering a bare `error` (which does not survive the wire — see
// models.AgentResp), and calling render.Status AFTER render.JSON.
//
// render.Status only stashes the code in the request context; render.JSON is
// what calls WriteHeader. Calling Status second is therefore a NO-OP and the
// reply goes out as 200 — which is how /updatemockparams came to answer every
// failure with "HTTP 200 {\"isSuccess\":false}", defeating any status check a
// client might make.
func respondAgent(w http.ResponseWriter, r *http.Request, status int, err error) {
	resp := models.AgentResp{IsSuccess: err == nil}
	if err != nil {
		resp.ErrorMsg = err.Error()
	}
	render.Status(r, status)
	render.JSON(w, r, resp)
}

func (a *Agent) GetConsumedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	consumedMocks, err := a.svc.GetConsumedMocks(r.Context())
	if err != nil {
		// The CLI decodes a 200 here straight into []models.MockState, so
		// a failure MUST carry a non-2xx status — otherwise the error
		// object lands in the slice decoder and the caller sees
		// "cannot unmarshal object into Go value of type
		// []models.MockState" instead of the reason below.
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, consumedMocks)
}

func (a *Agent) GetMockErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockErrors, err := a.svc.GetMockErrors(r.Context())
	if err != nil {
		respondAgent(w, r, http.StatusInternalServerError, err)
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
			respondAgent(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// StoreMocks receives the mock corpus as a stream: a gob MockStreamHeader
// followed by one gob Mock per frame, decoded mock-by-mock by StoreMocksStream.
func (a *Agent) StoreMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")

	writeErr := func(status int, err error) {
		w.WriteHeader(status)
		// Encode errors used to be discarded here, which hid the real
		// problem: gob ABORTS on a non-nil `error` field ("type not
		// registered for interface"). The abort happens PARTWAY -- a partial
		// type descriptor is already on the wire -- so the CLI got a truncated
		// value, failed with "unexpected EOF", and could only report
		// "storemocks http <status>". ErrorMsg is a string, so this now
		// encodes; log it if it ever does not.
		if encErr := gob.NewEncoder(w).Encode(models.AgentResp{ErrorMsg: err.Error()}); encErr != nil {
			utils.LogError(a.logger, encErr, "failed to encode storemocks error response",
				zap.String("underlying", err.Error()))
		}
	}

	dec := gob.NewDecoder(r.Body)
	var header models.MockStreamHeader
	if err := dec.Decode(&header); err != nil {
		writeErr(http.StatusBadRequest, fmt.Errorf("storemocks: decode stream header: %w", err))
		return
	}

	streamer, ok := a.svc.(interface {
		StoreMocksStream(context.Context, models.MockStreamHeader, *gob.Decoder) error
	})
	if !ok {
		writeErr(http.StatusInternalServerError, fmt.Errorf("storemocks: service does not support streaming"))
		return
	}

	if err := streamer.StoreMocksStream(r.Context(), header, dec); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&updateParamsReq); err != nil {
		respondAgent(w, r, http.StatusBadRequest, err)
		return
	}

	if err := a.svc.UpdateMockParams(r.Context(), updateParamsReq.FilterParams); err != nil {
		utils.LogError(a.logger, err, "failed to update mock params")
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}

	a.logger.Debug("Time taken to update mock params duration :", zap.Duration("duration", time.Since(start)))

	respondAgent(w, r, http.StatusOK, nil)
}
