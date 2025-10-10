// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"encoding/gob"
	"encoding/json"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
)

func (a *AgentRequest) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)

	mockRes := models.AgentResp{
		Error:     nil,
		IsSuccess: true,
	}

	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, mockRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.MockOutgoing(r.Context(), uint64(0), OutgoingReq.OutgoingOptions)
	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, mockRes)
	render.Status(r, http.StatusOK)
}

func (a *AgentRequest) SetMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var SetMocksReq models.SetMocksReq
	err := json.NewDecoder(r.Body).Decode(&SetMocksReq)

	setmockRes := models.AgentResp{

		Error: nil,
	}
	if err != nil {
		setmockRes.Error = err
		setmockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.SetMocks(r.Context(), uint64(0), SetMocksReq.Filtered, SetMocksReq.UnFiltered)
	if err != nil {
		setmockRes.Error = err
		setmockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, setmockRes)
	render.Status(r, http.StatusOK)

}

func (a *AgentRequest) GetConsumedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	consumedMocks, err := a.agent.GetConsumedMocks(r.Context(), uint64(0))
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, consumedMocks)
	render.Status(r, http.StatusOK)
}

func (a *AgentRequest) StoreMocks(w http.ResponseWriter, r *http.Request) {
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

	err := a.agent.StoreMocks(r.Context(), uint64(0), storeMocksReq.Filtered, storeMocksReq.UnFiltered)

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

func (a *AgentRequest) UpdateMockParams(w http.ResponseWriter, r *http.Request) {
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

	err = a.agent.UpdateMockParams(r.Context(), uint64(0), updateParamsReq.FilterParams)
	if err != nil {
		updateParamsRes.Error = err
		updateParamsRes.IsSuccess = false
		render.JSON(w, r, updateParamsRes)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, updateParamsRes)
	render.Status(r, http.StatusOK)
}
