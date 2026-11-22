package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// pidMapSvc implements only the piece HandlePIDMapRefusals needs. The embedded
// nil Service makes any other call panic loudly rather than silently pass.
type pidMapSvc struct {
	agent.Service // nil: any call other than PIDMapRefusals panics loudly
	refusals      models.PIDMapRefusals
	err           error
}

func (s pidMapSvc) PIDMapRefusals(context.Context) (models.PIDMapRefusals, error) {
	return s.refusals, s.err
}

// getPIDMaps asks GET /agent/pidmaps of the real routes over svc.
func getPIDMaps(t *testing.T, svc agent.Service) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	DefaultRoutes{}.New(r, svc, zap.NewNop())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/pidmaps", nil))
	return rec
}

// A session fails on a refused insert, so "none refused" is a statement only
// an agent that counts may make. One that cannot count answers 501: a 200 with
// zeros would pass every session against it as completely captured.
func TestHandlePIDMapRefusalsSaysWhenItCannotCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  agent.Service
	}{
		{"a service without the count", struct{ agent.Service }{}},
		{"hooks that do not count", pidMapSvc{err: models.ErrPIDMapRefusalsUnsupported}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getPIDMaps(t, tc.svc)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501: the caller must be able to tell \"cannot count\" from \"none refused\"\n%s", rec.Code, rec.Body)
			}
			var body map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["error"] == "" {
				t.Fatal("the 501 does not say why")
			}
		})
	}
}

// An agent that counts answers what it counted, whole: the CLI's verdict and
// its reason are made of every field.
func TestHandlePIDMapRefusalsAnswersWhatTheHooksCounted(t *testing.T) {
	for _, want := range []models.PIDMapRefusals{
		{TargetCapacity: 10240, ExcludedCapacity: 10240}, // none refused
		{Target: 271, Excluded: 3, LastTargetTGID: 1481467, TargetCapacity: 10240, ExcludedCapacity: 10240},
	} {
		rec := getPIDMaps(t, pidMapSvc{refusals: want})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body)
		}
		var got models.PIDMapRefusals
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got != want {
			t.Fatalf("answered %+v, want %+v", got, want)
		}
	}
}

// A read that failed is neither: not "none refused" (200), and not "this agent
// cannot count" (501), which the CLI takes quietly. It is a 500, which the CLI
// warns about.
func TestHandlePIDMapRefusalsReportsAReadThatFailed(t *testing.T) {
	rec := getPIDMaps(t, pidMapSvc{err: errors.New("read pid_map_insert_failures[target_namespace_pids]: bad file descriptor")})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500\n%s", rec.Code, rec.Body)
	}
}
