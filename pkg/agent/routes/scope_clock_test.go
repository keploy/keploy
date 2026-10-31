package routes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type beginsSvc struct {
	agent.Service
	begins map[string]time.Time
	real   map[string][]string
	err    error
}

func (s *beginsSvc) SetTestBegins(_ context.Context, req models.TestBeginsReq) error {
	if s.err != nil {
		return s.err
	}
	s.begins, s.real = req.Begins, req.Real
	return nil
}

// /agent/replay/clock hands the service the tests' recorded begins; a service
// that cannot take them answers 501, and a body it cannot read 400.
func TestHandleTestBegins(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &beginsSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	send := func(a *Agent, body string) int {
		rec := httptest.NewRecorder()
		a.HandleTestBegins(rec, httptest.NewRequest(http.MethodPost, "/agent/replay/clock", strings.NewReader(body)))
		return rec.Code
	}
	if code := send(a, `{"begins":{"pkg::TestA":"2026-10-06T09:01:00Z"}}`); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	if !svc.begins["pkg::TestA"].Equal(want) {
		t.Fatalf("the service got begins %v", svc.begins)
	}
	if code := send(a, `{"begins":{},"real":{"pkg::TestB":["db.example.com:5432"]}}`); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got := svc.real["pkg::TestB"]; len(got) != 1 || got[0] != "db.example.com:5432" {
		t.Fatalf("the service got the REAL tests %v", svc.real)
	}
	if code := send(a, `{`); code != http.StatusBadRequest {
		t.Fatalf("an unreadable body: status %d", code)
	}
	if code := send(&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}, `{"begins":{}}`); code != http.StatusNotImplemented {
		t.Fatalf("a service without the capability: status %d", code)
	}
	// A build that freezes the clock but cannot move it per test.
	if code := send(&Agent{logger: zap.NewNop(), svc: &beginsSvc{err: agent.ErrNoTestClock}}, `{"begins":{}}`); code != http.StatusNotImplemented {
		t.Fatalf("a build that cannot move its clock per test: status %d", code)
	}
	if code := send(&Agent{logger: zap.NewNop(), svc: &beginsSvc{err: agent.ErrNoRealTestClock}}, `{"begins":{}}`); code != http.StatusNotImplemented {
		t.Fatalf("a build that cannot put its clock on real time: status %d", code)
	}
	if code := send(&Agent{logger: zap.NewNop(), svc: &beginsSvc{err: errors.New("x")}}, `{"begins":{}}`); code != http.StatusInternalServerError {
		t.Fatalf("another failure: status %d", code)
	}
}

// refusingScopeSvc refuses every begin with err.
type refusingScopeSvc struct {
	agent.Service
	err error
}

func (s *refusingScopeSvc) BeginScopeAt(context.Context, string, int, time.Time) error { return s.err }

// A test begun while another holds the run's clock is refused with 422
// (release1-cut §2.1, one_test_at_a_time); any other failure is a 500.
func TestScopeBeginRefusesASecondOpenTestWith422(t *testing.T) {
	for err, want := range map[error]int{
		fmt.Errorf("pkg::TestB began while pkg::TestA was open: %w", agent.ErrOneTestAtATime): http.StatusUnprocessableEntity,
		errors.New("disk full"): http.StatusInternalServerError,
	} {
		a := &Agent{logger: zap.NewNop(), svc: &refusingScopeSvc{err: err}}
		rec := httptest.NewRecorder()
		a.HandleScopeBegin(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(`{"name":"pkg::TestB"}`)))
		if rec.Code != want {
			t.Fatalf("%v: status %d, want %d: %s", err, rec.Code, want, rec.Body.String())
		}
	}
}

// Through the agent's router: with the proven switch off /agent/replay/clock
// answers every method with the 404 an unknown path gets, as before the route
// existed, and stores nothing; with it on, a POST is taken and another method
// is 405.
func TestReplayClockRouteIsTheSwitchs(t *testing.T) {
	serve := func(svc agent.Service, method string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		DefaultRoutes{}.New(r, svc, zap.NewNop())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/agent/replay/clock", strings.NewReader(`{"begins":{"pkg::TestA":"2026-10-06T09:01:00Z"}}`)))
		return rec
	}
	unknown := func() *httptest.ResponseRecorder {
		r := chi.NewRouter()
		DefaultRoutes{}.New(r, &beginsSvc{}, zap.NewNop())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent/no-such-route", nil))
		return rec
	}()

	t.Setenv(provenswitch.Env, "")
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
		svc := &beginsSvc{}
		rec := serve(svc, m)
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Fatalf("switch off, %s: %d %q, want the unknown path's %d %q", m, rec.Code, rec.Body.String(), unknown.Code, unknown.Body.String())
		}
		if svc.begins != nil {
			t.Fatalf("switch off, %s: the service got begins %v", m, svc.begins)
		}
	}

	t.Setenv(provenswitch.Env, "1")
	svc := &beginsSvc{}
	if rec := serve(svc, http.MethodPost); rec.Code != http.StatusOK || len(svc.begins) != 1 {
		t.Fatalf("switch on, POST: %d, begins %v", rec.Code, svc.begins)
	}
	if rec := serve(&beginsSvc{}, http.MethodGet); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("switch on, GET: %d", rec.Code)
	}
}
