package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

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

// realTestReader is a service that ends keploy test's open test case for
// mock.real.
type realTestReader interface {
	RealTest(ctx context.Context) (models.RealTest, error)
}

// realUseKeeper is a service that keeps the run's REAL uses while keploy
// record's incoming stream is open.
type realUseKeeper interface {
	KeepRealUses(keep bool)
}

// realUseReader is a service that names the mock.real destinations a
// recorded test case used, by its window.
type realUseReader interface {
	RealUsedDuring(from, to time.Time) []string
}

// testCaseWindow is the test case's window by its kind (TestCase.Window).
func testCaseWindow(tc *models.TestCase) (time.Time, time.Time) { return tc.Window() }

// realDepsKeeper is a service that installs a later push of the same run
// without starting its REAL bookkeeping afresh (models.RealDepsReq.Keep).
type realDepsKeeper interface {
	KeepRealDeps(ctx context.Context, real []string) error
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
	set := s.SetRealDeps
	if k, ok := a.svc.(realDepsKeeper); ok && req.Keep {
		set = k.KeepRealDeps
	}
	if err := set(r.Context(), req.Real); err != nil {
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
// read takes: each since the last read. keploy mock and keploy record read
// it once, after the run's last windows; keploy test once per test set.
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

// HandleRealTest ends keploy test's open test case for mock.real
// (models.RealTest): the REAL destinations it used, and the
// capture_incomplete evidence charged to it, which this read takes. The
// replayer reads it once per test case, after the case's misses.
//
// With the switch off it answers 404, as it did before it existed.
func (a *Agent) HandleRealTest(w http.ResponseWriter, r *http.Request) {
	if !provenswitch.On() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	s, ok := a.svc.(realTestReader)
	if !ok {
		render.Status(r, http.StatusOK)
		render.JSON(w, r, models.RealTest{})
		return
	}
	got, err := s.RealTest(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, got)
}
