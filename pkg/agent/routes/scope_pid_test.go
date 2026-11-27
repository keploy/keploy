package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type scopePIDSvc struct {
	agent.Service
	gotCaller int
	scoped    bool
	status    models.ScopeStatus
	serr      *models.ScopeError
}

func (s *scopePIDSvc) ScopePID(_ context.Context, caller int) (bool, *models.ScopeError) {
	s.gotCaller = caller
	return s.scoped, s.serr
}

func (s *scopePIDSvc) ScopeStatus(_ context.Context) models.ScopeStatus {
	return s.status
}

func TestHandleScopePIDRouteWorksWithProvenOffAndOn(t *testing.T) {
	wantPID := os.Getpid()
	for _, tc := range []struct {
		name      string
		provenVal string
		scoped    bool
	}{
		{"proven-off-scoped", "", true},
		{"proven-on-scoped", "1", true},
		{"proven-on-inactive-noop", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.provenVal)
			svc := &scopePIDSvc{scoped: tc.scoped}
			r := chi.NewRouter()
			(DefaultRoutes{}).New(r, svc, zap.NewNop())
			srv := httptest.NewServer(r)
			defer srv.Close()

			resp, err := srv.Client().Post(srv.URL+"/agent/scope-pid", "application/json", strings.NewReader(`{"pid":1}`))
			if err != nil {
				t.Fatalf("POST /agent/scope-pid: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			var got struct {
				Status string `json:"status"`
				Scoped bool   `json:"scoped"`
				PID    int    `json:"pid"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if svc.gotCaller != wantPID || got.PID != wantPID {
				t.Fatalf("caller=(svc=%d resp=%d), want %d (from TCP connection, never body)", svc.gotCaller, got.PID, wantPID)
			}
			if got.Scoped != tc.scoped {
				t.Fatalf("scoped=%v, want %v", got.Scoped, tc.scoped)
			}
		})
	}
}

func TestHandleScopePIDPropagatesScopeErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"not-in-run-422", http.StatusUnprocessableEntity, models.ScopeErrNotInRun},
		{"hook-failure-500", http.StatusInternalServerError, models.ScopeErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &scopePIDSvc{
				serr: &models.ScopeError{
					Status:  tc.status,
					Code:    tc.code,
					Message: "failed",
					Remedy:  "remedy",
				},
			}
			r := chi.NewRouter()
			(DefaultRoutes{}).New(r, svc, zap.NewNop())
			srv := httptest.NewServer(r)
			defer srv.Close()

			resp, err := srv.Client().Post(srv.URL+"/agent/scope-pid", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("POST /agent/scope-pid: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestHandleScopeStatusRouteReportsReadyAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   models.ScopeStatus
	}{
		{"unrequested", models.ScopeStatus{}},
		{"ready", models.ScopeStatus{Requested: true, Ready: true}},
		{"fallback", models.ScopeStatus{Requested: true, Ready: false, Reason: "read-only file system", Remedy: "run with sudo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &scopePIDSvc{status: tc.st}
			r := chi.NewRouter()
			(DefaultRoutes{}).New(r, svc, zap.NewNop())
			srv := httptest.NewServer(r)
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL + "/agent/scope-status")
			if err != nil {
				t.Fatalf("GET /agent/scope-status: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			var got models.ScopeStatus
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.st {
				t.Fatalf("got %+v, want %+v", got, tc.st)
			}
		})
	}
}
