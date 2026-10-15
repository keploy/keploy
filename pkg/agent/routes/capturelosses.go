package routes

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/render"

	"go.keploy.io/server/v3/pkg/models"
)

// captureLossReader is the agent service's count of every kind of capture
// loss it can see (models.CaptureLosses).
type captureLossReader interface {
	CaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// HandleCaptureLosses answers GET /agent/capturelosses: the calls the agent
// intercepted and could not record or mock, and the processes it could not
// track, by kind, each counted since it started. Safe to poll; it takes
// nothing away. An agent that cannot count answers 501, never 200 with
// nothing: a caller fails a test on a loss, and must tell "none" from
// "cannot say".
func (a *Agent) HandleCaptureLosses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s, ok := a.svc.(captureLossReader)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": models.ErrCaptureLossesUnsupported.Error()})
		return
	}
	losses, err := s.CaptureLosses(r.Context())
	switch {
	case errors.Is(err, models.ErrCaptureLossesUnsupported):
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	case err != nil:
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	if losses == nil {
		losses = models.CaptureLosses{}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, losses)
}
