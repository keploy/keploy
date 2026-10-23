package routes

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/render"

	"go.keploy.io/server/v3/pkg/models"
)

// relayedLiveReader is the agent service's count of the connections its proxy
// relayed live to a dependency (models.RelayedLives).
type relayedLiveReader interface {
	RelayedLive(ctx context.Context) (models.RelayedLives, error)
}

// HandleRelayedLive answers GET /agent/relayed: the connections the agent
// relayed live to a dependency instead of answering them from a recording,
// per reason and destination, each counted since it started (an empty list
// when it relayed none). Safe to poll; it takes nothing away. An agent that
// cannot count them answers 501, never 200 with nothing: `keploy mock
// replay` claims a run isolated only on an answer that says none were
// relayed, and must tell "none" from "cannot say". An agent older than the
// route answers 404, read alike.
func (a *Agent) HandleRelayedLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s, ok := a.svc.(relayedLiveReader)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": models.ErrRelayedLiveUnsupported.Error()})
		return
	}
	rows, err := s.RelayedLive(r.Context())
	switch {
	case errors.Is(err, models.ErrRelayedLiveUnsupported):
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	case err != nil:
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = models.RelayedLives{}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, rows)
}
