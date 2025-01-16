package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func (a *AgentRequest) SendKtInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var testbenchReq models.TestBenchReq
	err := json.NewDecoder(r.Body).Decode(&testbenchReq)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		a.logger.Error("failed to decode kt info", zap.Error(err))
		return
	}

	err = a.agent.SendKtInfo(r.Context(), testbenchReq)
	if err != nil {
		a.logger.Error("failed to send kt info", zap.Error(err))
		render.Status(r, http.StatusInternalServerError)
		return
	}

	tbRes := models.TestBenchResp{
		IsSuccess: true,
		Error:     "",
	}

	render.JSON(w, r, tbRes)
	render.Status(r, http.StatusOK)
}
