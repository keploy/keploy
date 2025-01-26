// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
)

func (a *AgentRequest) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)

	mockRes := models.AgentResp{
		ClientID:  OutgoingReq.ClientID,
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

	err = a.agent.MockOutgoing(r.Context(), OutgoingReq.ClientID, OutgoingReq.OutgoingOptions)
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
		ClientID: SetMocksReq.ClientID,
		Error:    nil,
	}
	if err != nil {
		setmockRes.Error = err
		setmockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.SetMocks(r.Context(), SetMocksReq.ClientID, SetMocksReq.Filtered, SetMocksReq.UnFiltered)
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

	clientID := r.URL.Query().Get("id")

	// convert string to uint64
	clientIDInt, err := strconv.ParseUint(clientID, 10, 64)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	consumedMocks, err := a.agent.GetConsumedMocks(r.Context(), clientIDInt)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, consumedMocks)
	render.Status(r, http.StatusOK)

}
