package http_test

// GetPIDMapRefusals must tell an agent that ANSWERS "no insert was refused"
// from one that cannot answer at all.
//
// `keploy mock record|replay` and `keploy test` fail a session in which the
// count grew (the kernel refused a process a place in the agent's PID map, so
// its calls were not intercepted) and pass one in which it did not. An agent
// older than GET /agent/pidmaps (404), or one whose hooks do not count (501),
// must therefore not read as a count of zero, which would vouch for a capture
// nobody checked; nor as a failure, which would fail every run against it. It
// reads as models.ErrPIDMapRefusalsUnsupported: "unknown".

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// pidMapSvc is an agent service that answers only PIDMapRefusals. The embedded
// nil interface satisfies the type; any other call would panic.
type pidMapSvc struct {
	agentsvc.Service
	refusals models.PIDMapRefusals
	err      error
}

func (s pidMapSvc) PIDMapRefusals(context.Context) (models.PIDMapRefusals, error) {
	return s.refusals, s.err
}

// agentWith serves the agent's real routes over svc.
func agentWith(t *testing.T, svc agentsvc.Service) string {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL + "/agent"
}

// The client and the agent's own route agree: what the hooks counted is what
// the CLI reads, field for field.
func TestGetPIDMapRefusalsReadsWhatTheAgentCounted(t *testing.T) {
	for _, want := range []models.PIDMapRefusals{
		{TargetCapacity: 10240, ExcludedCapacity: 10240}, // none refused
		{Target: 271, Excluded: 3, LastTargetTGID: 1481467, TargetCapacity: 10240, ExcludedCapacity: 10240},
	} {
		got, err := newClient(t, agentWith(t, pidMapSvc{refusals: want})).GetPIDMapRefusals(context.Background())
		if err != nil {
			t.Fatalf("an agent that counts must answer: %v", err)
		}
		if got != want {
			t.Fatalf("read %+v, want %+v", got, want)
		}
	}
}

func TestGetPIDMapRefusalsFromAnAgentThatCannotCountIsUnknown(t *testing.T) {
	older := httptest.NewServer(http.NotFoundHandler()) // an agent older than the route
	defer older.Close()
	for _, tc := range []struct {
		name     string
		agentURI string
	}{
		{"an agent older than the route (404)", older.URL + "/agent"},
		{"an agent whose service has no count (501)", agentWith(t, struct{ agentsvc.Service }{})},
		{"an agent whose hooks do not count (501)", agentWith(t, pidMapSvc{err: models.ErrPIDMapRefusalsUnsupported})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newClient(t, tc.agentURI).GetPIDMapRefusals(context.Background())
			if err == nil {
				t.Fatalf("read %+v from an agent that cannot count: a usable zero would vouch for a capture nobody checked", got)
			}
			if !errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
				t.Fatalf("want ErrPIDMapRefusalsUnsupported, so that the session is left as it was; got %v", err)
			}
		})
	}
}

// A read the agent could not make is a real failure, which the CLI warns
// about; were it "unsupported" too, a broken counter would go by at Debug.
func TestGetPIDMapRefusalsKeepsARealFailureAnError(t *testing.T) {
	uri := agentWith(t, pidMapSvc{err: errors.New("read pid_map_insert_failures[target_namespace_pids]: bad file descriptor")})
	got, err := newClient(t, uri).GetPIDMapRefusals(context.Background())
	if err == nil {
		t.Fatalf("read %+v from an agent whose read failed", got)
	}
	if errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
		t.Fatalf("a failed read is not \"this agent cannot count\": %v", err)
	}
}
