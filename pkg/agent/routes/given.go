package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
)

// givenTaker takes a value the open test's harness gave (agent.Agent.Give).
type givenTaker interface {
	Give(ctx context.Context, req models.GivenReq) *models.ScopeError
}

// testGivenSetter installs, by test, what each test's harness gave in its
// recording (agent.Agent.SetTestGiven).
type testGivenSetter interface {
	SetTestGiven(ctx context.Context, given, keys map[string][]models.GivenValue) error
}

// HandleGiven takes a value the test made itself, given by its harness as the
// test installs it (models.GivenReq): the open test's, or, while no test is
// open, the claim key's. A record keeps it with the recording, a replay binds
// it as the test's (or the key's) given variable. The
// proven wire's: with the switch off it answers 404, to every method.
func (a *Agent) HandleGiven(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.proven(); !ok {
		http.NotFound(w, r)
		return
	}
	g, ok := a.svc.(givenTaker)
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req models.GivenReq
	if err := json.NewDecoder(io.LimitReader(r.Body, maxScopeBody)).Decode(&req); err != nil {
		badScopeBody(w, r, "given", err)
		return
	}
	if serr := g.Give(r.Context(), req); serr != nil {
		writeScopeError(w, r, serr)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]any{"v": models.ScopeProtocolVersion, "status": "ok"})
}

// HandleTestGiven installs, by test, what each test's harness gave when it was
// recorded, and by claim key, what each key's harness gave outside every test
// (models.TestGivenReq), for the replay's HandleGiven to bind. The
// proven wire's: with the switch off it answers 404, to every method.
func (a *Agent) HandleTestGiven(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.proven(); !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	var req models.TestGivenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid given-values request: %v", err), http.StatusBadRequest)
		return
	}
	s, ok := a.svc.(testGivenSetter)
	if !ok {
		http.Error(w, "this keploy agent cannot bind the values a test's harness gives", http.StatusNotImplemented)
		return
	}
	if err := s.SetTestGiven(r.Context(), req.Given, req.Keys); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}
