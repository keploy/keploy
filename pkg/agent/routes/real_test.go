package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type realSvc struct {
	agent.Service
	real     []string
	kept     bool
	run      models.RealRun
	test     models.RealTest
	used     []string
	usedFrom time.Time
	err      error
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

func (s *realSvc) KeepRealDeps(ctx context.Context, real []string) error {
	s.kept = true
	return s.SetRealDeps(ctx, real)
}

func (s *realSvc) RealTest(context.Context) (models.RealTest, error) { return s.test, s.err }

func (s *realSvc) RealUsedDuring(from, to time.Time) []string {
	if s.usedFrom = from; to.Before(from) {
		return nil
	}
	return s.used
}

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
	require.False(t, svc.kept)
	require.Equal(t, http.StatusOK, serve(a.HandleRealDeps, http.MethodPost, `{"real":["localhost:5432"],"keep":true}`).Code)
	require.True(t, svc.kept, "a later push of the same run keeps")
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
	require.Equal(t, http.StatusNotFound, serve(a.HandleRealTest, http.MethodGet, ``).Code)
}

// /agent/real/test ends keploy test's open test case for mock.real and
// answers its REAL rows and evidence; a service without REAL support has
// none to give.
func TestHandleRealTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &realSvc{test: models.RealTest{Real: []models.RealUse{{Dest: "db:5432", Conns: 1}},
		Incomplete: []models.CaptureIncomplete{{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "w"}}}}
	rec := serve((&Agent{logger: zap.NewNop(), svc: svc}).HandleRealTest, http.MethodGet, ``)
	require.Equal(t, http.StatusOK, rec.Code)
	var got models.RealTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, svc.test, got)
	require.Equal(t, http.StatusMethodNotAllowed, serve((&Agent{logger: zap.NewNop(), svc: svc}).HandleRealTest, http.MethodPost, ``).Code)
	require.Equal(t, http.StatusInternalServerError, serve((&Agent{logger: zap.NewNop(), svc: &realSvc{err: errors.New("x")}}).HandleRealTest, http.MethodGet, ``).Code)
	rec = serve((&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}).HandleRealTest, http.MethodGet, ``)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{}`, rec.Body.String())
}

// A recorded test case's window is its request and response, by its kind.
func TestTestCaseWindow(t *testing.T) {
	a, b := time.Unix(1, 0), time.Unix(2, 0)
	from, to := testCaseWindow(&models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: a}, HTTPResp: models.HTTPResp{Timestamp: b}})
	require.Equal(t, a, from)
	require.Equal(t, b, to)
	from, to = testCaseWindow(&models.TestCase{Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: a}, GrpcResp: models.GrpcResp{Timestamp: b}})
	require.Equal(t, a, from)
	require.Equal(t, b, to)
}

// beforeSimulateHooks notes a freeze and a move onto real time.
type beforeSimulateHooks struct {
	agent.AgentHook
	frozen, real []string
	file         string
}

func (h *beforeSimulateHooks) BeforeSimulate(_ context.Context, _ time.Time, _ string, tc string) error {
	h.frozen = append(h.frozen, tc)
	return nil
}
func (h *beforeSimulateHooks) BeginTestClock(context.Context, string, time.Time) error { return nil }
func (h *beforeSimulateHooks) EndTestClock(context.Context, string) error              { return nil }
func (h *beforeSimulateHooks) TimeFile() string                                        { return h.file }

type realBeforeSimulateHooks struct{ *beforeSimulateHooks }

func (h realBeforeSimulateHooks) BeginRealTestClock(_ context.Context, tc string) error {
	h.real = append(h.real, tc)
	return nil
}

// A test case that uses mock.real is put on the real clock after the rest
// of the hook, under the switch; a freezing build that cannot is refused
// (501). Without the switch, or without a list, it is frozen as before.
func TestHandleBeforeSimulateOfARealTestCase(t *testing.T) {
	prev := agent.ActiveHooks
	t.Cleanup(func() { agent.RegisterHooks(prev) })
	body := `{"timestamp":"2026-10-14T00:00:00Z","testSetID":"s","testCaseName":"tc","real":["db:5432"]}`

	t.Setenv(provenswitch.Env, "1")
	h := &beforeSimulateHooks{file: "/tmp/kclk1/t"}
	agent.RegisterHooks(realBeforeSimulateHooks{h})
	a := &Agent{logger: zap.NewNop()}
	require.Equal(t, http.StatusOK, serve(a.HandleBeforeSimulate, http.MethodPost, body).Code)
	require.Equal(t, []string{"tc"}, h.real)
	require.Equal(t, []string{"tc"}, h.frozen, "the rest of the hook runs first")
	h.frozen = nil
	require.Equal(t, http.StatusOK, serve(a.HandleBeforeSimulate, http.MethodPost, `{"timestamp":"2026-10-14T00:00:00Z","testSetID":"s","testCaseName":"plain"}`).Code)
	require.Equal(t, []string{"plain"}, h.frozen)

	// A mock.real case with no recorded request time is not frozen first.
	h.frozen, h.real = nil, nil
	require.Equal(t, http.StatusOK, serve(a.HandleBeforeSimulate, http.MethodPost, `{"testSetID":"s","testCaseName":"tc","real":["db:5432"]}`).Code)
	require.Empty(t, h.frozen)
	require.Equal(t, []string{"tc"}, h.real)

	agent.RegisterHooks(h) // freezes, cannot put a case on real time
	require.Equal(t, http.StatusNotImplemented, serve(a.HandleBeforeSimulate, http.MethodPost, body).Code)

	t.Setenv(provenswitch.Env, "")
	h.frozen = nil
	require.Equal(t, http.StatusOK, serve(a.HandleBeforeSimulate, http.MethodPost, body).Code)
	require.Equal(t, []string{"tc"}, h.frozen, "without the switch the list is not read")
}
