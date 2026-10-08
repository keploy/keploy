package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// The scope API lets a user's own test runner mark per-test boundaries so the
// `keploy mock record|replay` flow can attribute / restrict mocks per test.
// Handlers reach the concrete capability via type-assertion so the
// agent.Service interface stays unchanged (same pattern as BeginTestErrorCapture).

type scopeBeginner interface {
	BeginScope(ctx context.Context, name string, pid int) error
}
type scopeEnder interface {
	EndScope(ctx context.Context, name string, pid int) error
}
type scopeBeginnerAt interface {
	BeginScopeAt(ctx context.Context, name string, pid int, at time.Time) error
}
type scopeEnderAt interface {
	EndScopeAt(ctx context.Context, name string, pid int, at time.Time) error
}

// beginScope hands the runner's time to a service that takes it and falls back to the old call.
func beginScope(ctx context.Context, svc any, req models.ScopeReq) error {
	if s, ok := svc.(scopeBeginnerAt); ok {
		return s.BeginScopeAt(ctx, req.Name, req.Pid, req.At)
	}
	if s, ok := svc.(scopeBeginner); ok {
		return s.BeginScope(ctx, req.Name, req.Pid)
	}
	return nil
}

// endScope is beginScope for the end of a scope.
func endScope(ctx context.Context, svc any, req models.ScopeReq) error {
	if s, ok := svc.(scopeEnderAt); ok {
		return s.EndScopeAt(ctx, req.Name, req.Pid, req.At)
	}
	if s, ok := svc.(scopeEnder); ok {
		return s.EndScope(ctx, req.Name, req.Pid)
	}
	return nil
}

type scopeNoter interface {
	NoteScope(name string, pid int, dir string, suite bool)
}
type scopeOutcomeNoter interface {
	NoteScopeOutcome(name string, pid int, outcome string)
}
type scopeGate interface {
	SetScopeGate(ctx context.Context, run []string, reason string) error
	ScopeRun(name string) (bool, string)
	NoteScopeGated(name string, pid int, at time.Time)
	NoteUngatable(name string)
}

type scopeWindowReader interface {
	GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error)
}
type scopeTableSetter interface {
	SetScopeTable(ctx context.Context, table map[string][]string) error
}
type mockStatsReader interface {
	MockStats(ctx context.Context) (models.MockStats, error)
}
type servedMockReader interface {
	ServedMocks(ctx context.Context) (map[string]models.MockState, error)
}
type capturedMockDrainer interface {
	DrainCapturedMocks(ctx context.Context) ([]*models.Mock, error)
}

// HandleScopeBegin marks the start of a named per-test scope.
func (a *Agent) HandleScopeBegin(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid scope-begin request: %v", err), http.StatusBadRequest)
		return
	}
	req.At = runnerAt(0, starts.Default.Start(uint32(req.Pid), req.Dir), req.At)
	// A test the replay gated out is told to skip before anything else: it
	// opens no window and narrows no pool, so skipping it leaves the replay
	// exactly as if it had never been begun — except for the record that it
	// was gated.
	if g, ok := a.svc.(scopeGate); ok && !req.Suite {
		if run, reason := g.ScopeRun(req.Name); !run {
			if req.CanSkip {
				g.NoteScopeGated(req.Name, req.Pid, markTime(req.At))
				render.Status(r, http.StatusOK)
				render.JSON(w, r, models.ScopeBeginResp{Status: "ok", Action: models.ScopeActionSkip, Reason: reason})
				return
			}
			// A harness that cannot skip runs the test anyway: begin it as
			// usual, so it is served its own mocks and its verdict counts.
			g.NoteUngatable(req.Name)
		}
	}
	if s, ok := a.svc.(scopeNoter); ok {
		s.NoteScope(req.Name, req.Pid, req.Dir, req.Suite)
	}
	starts.Default.Begin(uint32(req.Pid), req.Name, req.Dir, req.Suite, markTime(req.At))
	if err := beginScope(r.Context(), a.svc, req); err != nil {
		a.logger.Debug("scope begin failed", zap.String("name", req.Name), zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, models.ScopeBeginResp{Status: "ok"})
}

// HandleScopeEnd marks the end of a named per-test scope.
func (a *Agent) HandleScopeEnd(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid scope-end request: %v", err), http.StatusBadRequest)
		return
	}
	req.At = runnerAt(0, starts.Default.Start(uint32(req.Pid), req.Dir), req.At)
	starts.Default.End(uint32(req.Pid), req.Name, req.Suite, markTime(req.At))
	if s, ok := a.svc.(scopeOutcomeNoter); ok {
		if outcome := models.NormalizeScopeOutcome(req.Outcome); outcome != "" {
			s.NoteScopeOutcome(req.Name, req.Pid, outcome)
		}
	}
	if err := endScope(r.Context(), a.svc, req); err != nil {
		a.logger.Debug("scope end failed", zap.String("name", req.Name), zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// HandleScopeWindows returns the per-test windows collected during a record
// session (consumed by the CLI to build mappings.yaml).
func (a *Agent) HandleScopeWindows(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	windows := []models.ScopeWindow{}
	if s, ok := a.svc.(scopeWindowReader); ok {
		got, err := s.GetScopeWindows(r.Context())
		if err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]string{"error": err.Error()})
			return
		}
		windows = got
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, windows)
}

// HandleScopeTable installs the replay-time per-test name→mock-names table.
func (a *Agent) HandleScopeTable(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeTableReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid scope-table request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Sets != nil {
		starts.Default.SetTable(req.Root, req.Sets)
		var first time.Time
		for _, st := range req.Sets {
			if !st.Start.IsZero() && (first.IsZero() || st.Start.Before(first)) {
				first = st.Start
			}
		}
		if !first.IsZero() {
			if err := agent.ActiveHooks.SetFreezeAnchor(r.Context(), first); err != nil {
				a.logger.Debug("could not anchor the clock at the first recorded suite start", zap.Error(err))
			}
		}
	}
	if s, ok := a.svc.(scopeTableSetter); ok && req.Sets == nil {
		if err := s.SetScopeTable(r.Context(), req.Mappings); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// HandleScopeGate installs the replay's gate: the only tests that should run
// (models.ScopeGateReq). /agent/scope/begin answers every other test with
// ScopeActionSkip so its harness skips it. It is mounted outside /agent/scope/,
// the API a test runner's harness calls: which tests run is the replay CLI's
// decision, not the harness's.
func (a *Agent) HandleScopeGate(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeGateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid scope-gate request: %v", err), http.StatusBadRequest)
		return
	}
	g, ok := a.svc.(scopeGate)
	if !ok {
		http.Error(w, "this agent cannot gate which tests run", http.StatusNotImplemented)
		return
	}
	if err := g.SetScopeGate(r.Context(), req.Run, req.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// HandleCapturedMocks returns (and clears) the mocks captured on miss during a
// `--on-miss record` replay, gob-encoded like /storemocks.
func (a *Agent) HandleCapturedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")
	mocks := []*models.Mock{}
	if d, ok := a.svc.(capturedMockDrainer); ok {
		got, err := d.DrainCapturedMocks(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mocks = got
	}
	w.WriteHeader(http.StatusOK)
	if err := gob.NewEncoder(w).Encode(mocks); err != nil {
		a.logger.Debug("failed to encode captured mocks", zap.Error(err))
	}
}

// HandleMockStats returns a non-draining snapshot of the mock session.
func (a *Agent) HandleMockStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s, ok := a.svc.(mockStatsReader)
	if !ok {
		// 501, not 200 with a zero count — the same reason HandleServedMocks
		// gives below. A caller that treats "this agent cannot report" as
		// "nothing is stored" reads an unreportable agent as a replaced one,
		// and a caller that FAILS on a zero count then fails every run against
		// an agent build without the reader.
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": "this agent cannot report mock stats"})
		return
	}
	stats, err := s.MockStats(r.Context())
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, stats)
}

// HandleServedMocks reports which mocks have been served so far this session,
// keyed by mock name. Safe to poll: it reads the agent's never-drained
// persistent map, so unlike /consumedmocks it takes nothing away from the
// end-of-run outcome report or the --strict verdict.
func (a *Agent) HandleServedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s, ok := a.svc.(servedMockReader)
	if !ok {
		// 501, not 200 with an empty map: a caller polling for progress has to
		// be able to tell "this agent cannot report served mocks" from
		// "nothing has been served yet", or it renders the first as the
		// second and quietly reports every mock as unserved.
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": "this agent cannot report served mocks"})
		return
	}
	served, err := s.ServedMocks(r.Context())
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, served)
}

func (a *Agent) HandleAppStart(w http.ResponseWriter, r *http.Request) {
	var req models.AppStartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid app-start request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		http.Error(w, "invalid app-start request: port must be the port the app listens on, or left out right after the app is started", http.StatusBadRequest)
		return
	}
	if req.Pid <= 0 {
		http.Error(w, "invalid app-start request: pid must be the app's pid", http.StatusBadRequest)
		return
	}
	req.At = runnerAt(req.Pid, time.Time{}, req.At)
	var placed bool
	if req.Port == 0 {
		placed = starts.Default.Mark(uint32(req.Pid), markTime(req.At))
	} else {
		placed = starts.Default.Ready(uint32(req.Pid), uint16(req.Port), markTime(req.At))
	}
	if !placed {
		a.logger.Warn("the app could not be placed under a test process; mark the suite or test before starting the app, and start the app as a child process, or its calls are treated as a dependency's", zap.Int("pid", req.Pid))
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

var OnMark func(pid int, start time.Time) time.Duration

func runnerAt(pid int, start, at time.Time) time.Time {
	if OnMark == nil {
		return at
	}
	off := OnMark(pid, start)
	if off == 0 || at.IsZero() {
		return at
	}
	if s := at.Add(off); time.Since(s).Abs() < time.Since(at).Abs() {
		return s
	}
	return at
}

func markTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now()
	}
	return at
}
