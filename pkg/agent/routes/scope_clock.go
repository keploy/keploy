package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
)

type testBeginsSetter interface {
	SetTestBegins(ctx context.Context, begins map[string]time.Time) error
}

// HandleTestBegins installs when each test of the replayed recording began
// (models.TestBeginsReq). A service that cannot take them, or a build that
// freezes the run's clock but cannot move it per test (agent.ErrNoTestClock),
// answers 501, so a replay that needs its tests on their recorded clocks
// fails instead of running them on another one. An agent that does not
// freeze the clock takes them: its tests run on the real clock, as always.
//
// The route is the proven model's: with the switch off it answers 404, as it
// did before it existed, to every method.
func (a *Agent) HandleTestBegins(w http.ResponseWriter, r *http.Request) {
	if !provenswitch.On() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	var req models.TestBeginsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid replay-clock request: %v", err), http.StatusBadRequest)
		return
	}
	s, ok := a.svc.(testBeginsSetter)
	if !ok {
		http.Error(w, "this keploy agent cannot move the clock per test", http.StatusNotImplemented)
		return
	}
	if err := s.SetTestBegins(r.Context(), req.Begins); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, agent.ErrNoTestClock) {
			code = http.StatusNotImplemented
		}
		http.Error(w, err.Error(), code)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}
