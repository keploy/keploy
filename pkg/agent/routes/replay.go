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
		ClientId:  0,
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

	err = a.agent.MockOutgoing(r.Context(), 0, OutgoingReq.OutgoingOptions)
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

func checkForC(arr []string) bool {
	for _, v := range arr {
		if v == "C" {
			return true
		}
	}
	return false
}

func (a *AgentRequest) SetMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var SetMocksReq models.SetMocksReq
	err := json.NewDecoder(r.Body).Decode(&SetMocksReq)

	setmockRes := models.AgentResp{
		ClientId: 0,
		Error:    nil,
	}
	if err != nil {
		setmockRes.Error = err
		setmockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.SetMocks(r.Context(), 0, SetMocksReq.Filtered, SetMocksReq.UnFiltered)
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

	appId := r.URL.Query().Get("id")

	// convert string to uint64
	appIdInt, err := strconv.ParseUint(appId, 10, 64)

	consumedMocks, err := a.agent.GetConsumedMocks(r.Context(), appIdInt)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, consumedMocks)
	render.Status(r, http.StatusOK)

}
