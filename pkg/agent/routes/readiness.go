package routes

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
)

type appReadinessReader interface {
	AppReadiness(ctx context.Context, since uint64) (models.AppReadiness, error)
}

type appReadinessMarker interface {
	MarkReadiness(ctx context.Context) (models.AppReadiness, error)
}

// HandleAppReadiness answers GET /app/readiness with what the agent knows of
// the application's listens and of its start's progress (models.AppReadiness),
// under the proven switch. 501 from an agent that cannot tell, 500 when it
// cannot tell right now: both mean "unknown", never "not ready".
func (a *Agent) HandleAppReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reader, ok := a.svc.(appReadinessReader)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": "this agent cannot tell whether the application listens"})
		return
	}
	var since uint64
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.ParseUint(q, 10, 64)
		if err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, map[string]string{"error": "since must be a count of listens: " + err.Error()})
			return
		}
		since = n
	}
	got, err := reader.AppReadiness(r.Context(), since)
	if errors.Is(err, models.ErrReadinessUnsupported) {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, got)
}

// HandleAppReadinessMark answers POST /app/readiness/mark, a set's mark
// before keploy starts its application anew: what the last set's processes
// did is forgotten, and the counts its listens begin after are answered
// (models.AppReadiness). 501 from an agent that cannot watch, 500 when it
// cannot now.
func (a *Agent) HandleAppReadinessMark(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	marker, ok := a.svc.(appReadinessMarker)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": "this agent cannot tell whether the application listens"})
		return
	}
	got, err := marker.MarkReadiness(r.Context())
	if errors.Is(err, models.ErrReadinessUnsupported) {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, got)
}
