package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The proven model's single-worker integration wire (models.ScopeProtocolVersion,
// design v14 §6.1): POST /agent/instance, and /agent/scope/begin|end answered
// with a version, a skip or a fail. Served only under the proven switch, by a
// service that implements provenScope; otherwise the scope API answers as
// before.

type provenScope interface {
	ClaimInstance(ctx context.Context, req models.InstanceReq, caller int) (models.InstanceResp, *models.ScopeError)
	ProvenBegin(ctx context.Context, req models.ScopeReq) (models.ScopeBeginResp, *models.ScopeError)
	ProvenEnd(ctx context.Context, req models.ScopeReq) (models.ScopeEndResp, *models.ScopeError)
	SetScopeSkips(ctx context.Context, skip map[string]string) error
}

// refusalNoter records what the wire refused (agent.Agent.NoteRefusal).
type refusalNoter interface {
	NoteRefusal(models.RunRefusal)
}

// refuse answers a wire call with e, and records it as a refusal of the run,
// so the CLI's verdict names it. A call whose caller went away (503) refused
// nothing.
func (a *Agent) refuse(w http.ResponseWriter, r *http.Request, e *models.ScopeError) {
	if n, ok := a.svc.(refusalNoter); ok && e.Status != http.StatusServiceUnavailable {
		n.NoteRefusal(e.Refusal())
	}
	writeScopeError(w, r, e)
}

// HandleRefusal records a refusal a test framework's integration made and
// tells keploy (models.RunRefusal): one that kept a test process from
// joining the run (go test running more than one test binary at once), so
// the run's verdict names it rather than "no test ran".
func (a *Agent) HandleRefusal(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.proven(); !ok {
		http.NotFound(w, r)
		return
	}
	n, ok := a.svc.(refusalNoter)
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req models.RunRefusal
	if err := json.NewDecoder(io.LimitReader(r.Body, maxScopeBody)).Decode(&req); err != nil || req.Code == "" || req.Message == "" {
		if err == nil {
			err = errors.New("a refusal names its code and its message")
		}
		badScopeBody(w, r, "refusal", err)
		return
	}
	n.NoteRefusal(req)
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]any{"v": models.ScopeProtocolVersion, "status": "ok"})
}

// proven returns the service's wire when the proven switch is on.
func (a *Agent) proven() (provenScope, bool) {
	if !provenswitch.On() {
		return nil, false
	}
	p, ok := a.svc.(provenScope)
	return p, ok
}

// maxScopeBody bounds a scope call's body.
const maxScopeBody = 1 << 20

// maxGateBody bounds a gate's body: a skip map of every quarantined test of
// a set, each with its reason.
const maxGateBody = 64 << 20

func writeScopeError(w http.ResponseWriter, r *http.Request, e *models.ScopeError) {
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	render.Status(r, status)
	render.JSON(w, r, e)
}

func badScopeBody(w http.ResponseWriter, r *http.Request, what string, err error) {
	writeScopeError(w, r, &models.ScopeError{
		Status:  http.StatusBadRequest,
		Code:    models.ScopeErrBadRequest,
		Message: fmt.Sprintf("invalid %s request: %v", what, err),
		Remedy:  "send a JSON body as keploy's scope protocol v1 describes",
	})
}

// HandleInstance claims the run for the calling test process
// (models.InstanceReq). The caller is read from the connection, never from
// the body.
func (a *Agent) HandleInstance(w http.ResponseWriter, r *http.Request) {
	p, ok := a.proven()
	if !ok {
		// Off, the agent has no such route.
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	var req models.InstanceReq
	if err := json.NewDecoder(io.LimitReader(r.Body, maxScopeBody)).Decode(&req); err != nil {
		a.refuse(w, r, &models.ScopeError{
			Status:  http.StatusBadRequest,
			Code:    models.ScopeErrBadRequest,
			Message: fmt.Sprintf("invalid instance request: %v", err),
			Remedy:  "send a JSON body as keploy's scope protocol v1 describes",
		})
		return
	}
	caller, err := callerPID(r)
	if err != nil {
		a.refuse(w, r, &models.ScopeError{
			Status:  http.StatusInternalServerError,
			Code:    models.ScopeErrNoCaller,
			Message: "keploy's agent could not tell which process called it: " + err.Error(),
			Remedy:  "run the test command on the machine keploy's agent runs on",
		})
		return
	}
	resp, serr := p.ClaimInstance(r.Context(), req, caller)
	if serr != nil {
		a.refuse(w, r, serr)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, resp)
}

func (a *Agent) provenBegin(w http.ResponseWriter, r *http.Request, p provenScope, req models.ScopeReq) {
	resp, serr := p.ProvenBegin(r.Context(), req)
	if serr != nil {
		a.refuse(w, r, serr)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, resp)
}

func (a *Agent) provenEnd(w http.ResponseWriter, r *http.Request, p provenScope, req models.ScopeReq) {
	resp, serr := p.ProvenEnd(r.Context(), req)
	if serr != nil {
		writeScopeError(w, r, serr)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, resp)
}

// provenAppStart refuses an app start under the wire: an integration of
// protocol v1 never sends one.
func provenAppStart(w http.ResponseWriter, r *http.Request) {
	writeScopeError(w, r, &models.ScopeError{
		Status:  http.StatusUnprocessableEntity,
		Code:    models.ScopeErrUpgradeIntegration,
		Message: "this test harness marks app starts, which keploy's scope protocol v1 does not use",
		Remedy:  "upgrade the keploy integration of your test framework (go-sdk keploytest, keploy-pytest, @keploy/jest)",
	})
}
