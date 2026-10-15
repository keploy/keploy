package routes

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/render"

	"go.keploy.io/server/v3/pkg/models"
)

// pidMapReader is the agent service's count of the inserts into its hooks' PID
// maps the kernel refused (models.PIDMapRefusals).
type pidMapReader interface {
	PIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// HandlePIDMapRefusals answers GET /agent/pidmaps: the inserts into the
// hooks' PID maps the kernel has refused since they loaded. Safe to poll; it
// takes nothing away.
//
// An agent that cannot count them answers 501, not 200 with zeros: a caller
// fails a session on a refusal, and must be able to tell "none refused" from
// "this agent cannot say", or every agent without the count would pass every
// session as complete.
func (a *Agent) HandlePIDMapRefusals(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s, ok := a.svc.(pidMapReader)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": models.ErrPIDMapRefusalsUnsupported.Error()})
		return
	}
	refusals, err := s.PIDMapRefusals(r.Context())
	switch {
	case errors.Is(err, models.ErrPIDMapRefusalsUnsupported):
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	case err != nil:
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, refusals)
}
