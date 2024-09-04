package routes

import (
	"net/http"

	"go.keploy.io/server/v2/pkg/models"
)

func (a *AgentRequest) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	a.agent.MockOutgoing(r.Context(), 0, models.OutgoingOptions{})
}
