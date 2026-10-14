package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// realDepsSetter is a service that relays the run's mock.real destinations
// (design v14 §4).
type realDepsSetter interface {
	SetRealDeps(ctx context.Context, real []string) error
}

// realRunReader is a service that reports what the run saw of mock.real
// beyond its tests.
type realRunReader interface {
	RealRun(ctx context.Context) (models.RealRun, error)
}

// HandleRealDeps installs the run's mock.real (models.RealDepsReq), which the
// CLI sends as the run starts, and starts the run's REAL bookkeeping. A list
// keploy cannot read answers 400; a service that cannot relay real
// destinations answers 501 to a list that names any, so the run fails
// instead of recording or mocking them, and 200 to an empty one.
//
// The route is the proven model's: with the switch off it answers 404, as it
// did before it existed, to every method.
func (a *Agent) HandleRealDeps(w http.ResponseWriter, r *http.Request) {
	if !provenswitch.On() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	var req models.RealDepsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid mock.real request: %v", err), http.StatusBadRequest)
		return
	}
	s, ok := a.svc.(realDepsSetter)
	if !ok {
		if len(req.Real) == 0 {
			// Nothing to relay, and nothing an earlier run left to clear.
			render.Status(r, http.StatusOK)
			render.JSON(w, r, map[string]string{"status": "ok"})
			return
		}
		http.Error(w, "this keploy agent cannot relay mock.real destinations", http.StatusNotImplemented)
		return
	}
	if err := s.SetRealDeps(r.Context(), req.Real); err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, realdeps.ErrBadEntry):
			code = http.StatusBadRequest
		case errors.Is(err, models.ErrNoRealRelay):
			code = http.StatusNotImplemented
		}
		http.Error(w, err.Error(), code)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// HandleRealRun answers what the run saw of mock.real beyond its tests
// (models.RealRun): the destinations it reached, those first reached outside
// a test, and the capture_incomplete evidence charged to no test, which this
// read takes. The CLI reads it once, after the run's last windows.
//
// With the switch off it answers 404, as it did before it existed.
func (a *Agent) HandleRealRun(w http.ResponseWriter, r *http.Request) {
	if !provenswitch.On() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	s, ok := a.svc.(realRunReader)
	if !ok {
		http.Error(w, "this keploy agent cannot report its mock.real destinations", http.StatusNotImplemented)
		return
	}
	run, err := s.RealRun(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, run)
}
