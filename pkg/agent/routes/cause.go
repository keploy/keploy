package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
)

// causeTaker takes a call the open test's harness saw a repeating timer make
// (agent.Agent.Cause).
type causeTaker interface {
	Cause(ctx context.Context, req models.CauseReq) *models.ScopeError
}

// HandleCause takes a call of the open test that its harness saw a repeating
// timer make (models.CauseReq). A record keeps it with the test's recording; a
// replay takes it and does nothing with it. The proven wire's: with the switch
// off it answers 404, to every method.
func (a *Agent) HandleCause(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.proven(); !ok {
		http.NotFound(w, r)
		return
	}
	c, ok := a.svc.(causeTaker)
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req models.CauseReq
	if err := json.NewDecoder(io.LimitReader(r.Body, maxScopeBody)).Decode(&req); err != nil {
		badScopeBody(w, r, "cause", err)
		return
	}
	if serr := c.Cause(r.Context(), req); serr != nil {
		writeScopeError(w, r, serr)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]any{"v": models.ScopeProtocolVersion, "status": "ok"})
}
