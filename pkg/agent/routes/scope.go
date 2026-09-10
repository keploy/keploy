package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
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
type scopeWindowReader interface {
	GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error)
}
type scopeTableSetter interface {
	SetScopeTable(ctx context.Context, table map[string][]string) error
}
type mockStatsReader interface {
	MockStats(ctx context.Context) (models.MockStats, error)
}
type capturedMockDrainer interface {
	DrainCapturedMocks(ctx context.Context) ([]*models.Mock, error)
}

// HandleScopeBegin marks the start of a named per-test scope.
func (a *Agent) HandleScopeBegin(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if s, ok := a.svc.(scopeBeginner); ok {
		if err := s.BeginScope(r.Context(), req.Name, req.Pid); err != nil {
			a.logger.Debug("scope begin failed", zap.String("name", req.Name), zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// HandleScopeEnd marks the end of a named per-test scope.
func (a *Agent) HandleScopeEnd(w http.ResponseWriter, r *http.Request) {
	var req models.ScopeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if s, ok := a.svc.(scopeEnder); ok {
		if err := s.EndScope(r.Context(), req.Name, req.Pid); err != nil {
			a.logger.Debug("scope end failed", zap.String("name", req.Name), zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if s, ok := a.svc.(scopeTableSetter); ok {
		if err := s.SetScopeTable(r.Context(), req.Mappings); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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
	stats := models.MockStats{}
	if s, ok := a.svc.(mockStatsReader); ok {
		got, err := s.MockStats(r.Context())
		if err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]string{"error": err.Error()})
			return
		}
		stats = got
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, stats)
}
