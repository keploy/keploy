package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// readySvc implements only AppReadiness; the embedded nil Service makes any
// other call panic loudly.
type readySvc struct {
	agent.Service
	st    models.AppReadiness
	err   error
	since *uint64
}

func (s readySvc) AppReadiness(_ context.Context, since uint64) (models.AppReadiness, error) {
	if s.since != nil {
		*s.since = since
	}
	return s.st, s.err
}

// GET /agent/app/readiness over the real routes: 200 with the answer, 501
// from an agent that cannot watch (the CLI then leaves it to the first
// test's own connect), 500 for a read that failed (no progress seen, never
// "cannot watch").
func TestHandleAppReadinessAnswersOrSaysWhyNot(t *testing.T) {
	st := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
	}{
		{"the answer", readySvc{st: st}, http.StatusOK},
		{"cannot watch", readySvc{err: fmt.Errorf("%w: off", models.ErrReadinessUnsupported)}, http.StatusNotImplemented},
		{"a service without the watch", struct{ agent.Service }{}, http.StatusNotImplemented},
		{"a failed read", readySvc{err: errors.New("bad file descriptor")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/app/readiness", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var got models.AppReadiness
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Listens) != 1 || got.Listens[0] != st.Listens[0] {
				t.Fatalf("body %s (%v), want %+v", rec.Body.String(), err, st)
			}
		})
	}
}

// The listens asked for begin at ?since=N, the count a set's mark is; a since
// that is no count is refused.
func TestHandleAppReadinessReadsFromTheCountAskedFor(t *testing.T) {
	var got uint64
	r := chi.NewRouter()
	DefaultRoutes{}.New(r, readySvc{since: &got}, zap.NewNop())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/app/readiness?since=7", nil))
	if rec.Code != http.StatusOK || got != 7 {
		t.Fatalf("status %d, since %d; want 200 from 7", rec.Code, got)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/app/readiness?since=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d for since=x, want 400", rec.Code)
	}
}

// markSvc implements only MarkReadiness.
type markSvc struct {
	agent.Service
	st  models.AppReadiness
	err error
}

func (s markSvc) MarkReadiness(context.Context) (models.AppReadiness, error) { return s.st, s.err }

// POST /agent/app/readiness/mark: 200 with the counts, 501 from an agent that
// cannot watch, 500 for a mark that failed.
func TestHandleAppReadinessMarkAnswersTheCountsOrWhyNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
	}{
		{"the counts", markSvc{st: models.AppReadiness{ListenCount: 3}}, http.StatusOK},
		{"cannot watch", markSvc{err: fmt.Errorf("%w: off", models.ErrReadinessUnsupported)}, http.StatusNotImplemented},
		{"a service without the mark", struct{ agent.Service }{}, http.StatusNotImplemented},
		{"a failed mark", markSvc{err: errors.New("bad file descriptor")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent/app/readiness/mark", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code == http.StatusOK {
				var got models.AppReadiness
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ListenCount != 3 {
					t.Fatalf("body %s (%v), want the count 3", rec.Body.String(), err)
				}
			}
		})
	}
}
