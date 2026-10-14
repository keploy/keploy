package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type realSvc struct {
	agent.Service
	real []string
	run  models.RealRun
	err  error
}

func (s *realSvc) SetRealDeps(_ context.Context, real []string) error {
	if s.err != nil {
		return s.err
	}
	if _, err := realdeps.Parse(real); err != nil {
		return err
	}
	s.real = real
	return nil
}

func (s *realSvc) RealRun(context.Context) (models.RealRun, error) { return s.run, s.err }

func serve(h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, "/agent/real", strings.NewReader(body)))
	return rec
}

// /agent/real installs the run's mock.real; a list the agent cannot read is
// 400, a service that cannot relay real destinations 501 to a list that
// names any and 200 to an empty one.
func TestHandleRealDeps(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &realSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	require.Equal(t, http.StatusOK, serve(a.HandleRealDeps, http.MethodPost, `{"real":["localhost:5432","auth.example.com:443"]}`).Code)
	require.Equal(t, []string{"localhost:5432", "auth.example.com:443"}, svc.real)
	require.Equal(t, http.StatusBadRequest, serve(a.HandleRealDeps, http.MethodPost, `{"real":["localhost"]}`).Code)
	require.Equal(t, http.StatusBadRequest, serve(a.HandleRealDeps, http.MethodPost, `{`).Code)
	require.Equal(t, http.StatusMethodNotAllowed, serve(a.HandleRealDeps, http.MethodGet, ``).Code)
	require.Equal(t, http.StatusOK, serve((&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}).HandleRealDeps, http.MethodPost, `{"real":[]}`).Code)
	require.Equal(t, http.StatusNotImplemented, serve((&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}).HandleRealDeps, http.MethodPost, `{"real":["a:1"]}`).Code)
	require.Equal(t, http.StatusNotImplemented, serve((&Agent{logger: zap.NewNop(), svc: &realSvc{err: models.ErrNoRealRelay}}).HandleRealDeps, http.MethodPost, `{"real":["a:1"]}`).Code)
	require.Equal(t, http.StatusInternalServerError, serve((&Agent{logger: zap.NewNop(), svc: &realSvc{err: errors.New("x")}}).HandleRealDeps, http.MethodPost, `{"real":["a:1"]}`).Code)
}

// /agent/real/run answers what the run saw of mock.real beyond its tests.
func TestHandleRealRun(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &realSvc{run: models.RealRun{Reached: []string{"db:5432"}}}
	rec := serve((&Agent{logger: zap.NewNop(), svc: svc}).HandleRealRun, http.MethodGet, ``)
	require.Equal(t, http.StatusOK, rec.Code)
	var got models.RealRun
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, svc.run, got)
	require.Equal(t, http.StatusMethodNotAllowed, serve((&Agent{logger: zap.NewNop(), svc: svc}).HandleRealRun, http.MethodPost, ``).Code)
	require.Equal(t, http.StatusNotImplemented, serve((&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}).HandleRealRun, http.MethodGet, ``).Code)
}

// Both are the proven model's: with the switch off they answer the 404 their
// paths had before they existed.
func TestTheRealRoutesAreAbsentWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a := &Agent{logger: zap.NewNop(), svc: &realSvc{}}
	require.Equal(t, http.StatusNotFound, serve(a.HandleRealDeps, http.MethodPost, `{"real":[]}`).Code)
	require.Equal(t, http.StatusNotFound, serve(a.HandleRealRun, http.MethodGet, ``).Code)
}
