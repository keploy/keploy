package routes

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
)

func (a *AgentRequest) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// w.Header().Set("Transfer-Encoding", "chunked")
	// w.Header().Set("Cache-Control", "no-cache")
	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)

	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.MockOutgoing(r.Context(), 0, OutgoingReq.OutgoingOptions)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, "Mock Outgoing call successfully")
	render.Status(r, http.StatusOK)
}

func (a *AgentRequest) SetMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// w.Header().Set("Transfer-Encoding", "chunked")
	// w.Header().Set("Cache-Control", "no-cache")
	fmt.Println("Setting mocks!!")

	var SetMocksReq models.SetMocksReq
	err := json.NewDecoder(r.Body).Decode(&SetMocksReq)

	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.SetMocks(r.Context(), 0, SetMocksReq.Filtered, SetMocksReq.UnFiltered)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, "Mocks set successfully")
	render.Status(r, http.StatusOK)

}
