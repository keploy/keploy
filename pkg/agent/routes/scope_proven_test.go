package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// wireSvc is a service with the proven wire: it records what it was asked and
// answers what the test sets.
type wireSvc struct {
	agent.Service
	claimKey    string
	claimCaller int
	begun       []models.ScopeReq
	ended       []models.ScopeReq
	skips       map[string]string
	beginResp   models.ScopeBeginResp
	beginErr    *models.ScopeError
	endResp     models.ScopeEndResp
	oldBegun    string
}

func (s *wireSvc) ClaimInstance(_ context.Context, req models.InstanceReq, caller int) (models.InstanceResp, *models.ScopeError) {
	s.claimKey, s.claimCaller = req.Key, caller
	return models.InstanceResp{V: 1, Instance: models.InstanceRun}, nil
}

func (s *wireSvc) ProvenBegin(_ context.Context, req models.ScopeReq) (models.ScopeBeginResp, *models.ScopeError) {
	s.begun = append(s.begun, req)
	return s.beginResp, s.beginErr
}

func (s *wireSvc) ProvenEnd(_ context.Context, req models.ScopeReq) (models.ScopeEndResp, *models.ScopeError) {
	s.ended = append(s.ended, req)
	return s.endResp, nil
}

func (s *wireSvc) SetScopeSkips(_ context.Context, skip map[string]string) error {
	s.skips = skip
	return nil
}

func (s *wireSvc) SetScopeGate(context.Context, []string, string) error { return nil }
func (s *wireSvc) ScopeRun(string) (bool, string)                       { return true, "" }
func (s *wireSvc) NoteScopeGated(string, int, time.Time)                {}
func (s *wireSvc) NoteUngatable(string)                                 {}

// BeginScope is the old path, taken when the switch is off.
func (s *wireSvc) BeginScope(_ context.Context, name string, _ int) error {
	s.oldBegun = name
	return nil
}

func call(t *testing.T, handle http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handle(rec, httptest.NewRequest(http.MethodPost, "/agent/x", strings.NewReader(body)))
	return rec
}

func TestTheWireIsServedOnlyUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	svc := &wireSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeBegin, `{"name":"TestA","canSkip":true}`)
	if rec.Code != http.StatusOK || svc.oldBegun != "TestA" || len(svc.begun) != 0 {
		t.Fatalf("switch off: status %d, old begin %q, wire begins %d", rec.Code, svc.oldBegun, len(svc.begun))
	}
	if strings.Contains(rec.Body.String(), `"v"`) {
		t.Fatalf("switch off: the old answer carries no version, got %s", rec.Body.String())
	}
	// Off, /agent/instance is the plain 404 it was before the wire, for
	// every method.
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec := httptest.NewRecorder()
		a.HandleInstance(rec, httptest.NewRequest(method, "/agent/instance", strings.NewReader(`{"key":"orders"}`)))
		if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
			t.Fatalf("switch off: %s /agent/instance answered %d %q", method, rec.Code, rec.Body.String())
		}
	}
}

func TestBeginAndEndAnswerWithTheVersion(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &wireSvc{
		beginResp: models.ScopeBeginResp{V: 1, Status: "ok", Action: models.ScopeActionSkip, Reason: "flaky: replay_only"},
		endResp:   models.ScopeEndResp{V: 1, Status: "ok", Action: models.ScopeActionFail, Reason: "keploy: x"},
	}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeBegin, `{"name":"TestA","canSkip":true,"file":"tests/a.py"}`)
	var b models.ScopeBeginResp
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != http.StatusOK || b != svc.beginResp {
		t.Fatalf("begin: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if len(svc.begun) != 1 || svc.begun[0].File != "tests/a.py" || !svc.begun[0].CanSkip {
		t.Fatalf("begin request reached the service as %+v", svc.begun)
	}
	rec = call(t, a.HandleScopeEnd, `{"name":"TestA","outcome":"passed"}`)
	var e models.ScopeEndResp
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusOK || e != svc.endResp {
		t.Fatalf("end: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}

func TestAWireErrorIsAStatusAndAJSONBody(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &wireSvc{beginErr: &models.ScopeError{Status: http.StatusUnprocessableEntity, Code: models.ScopeErrOneTestAtATime, Message: "two tests", Remedy: "serialize"}}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeBegin, `{"name":"TestB","canSkip":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != models.ScopeErrOneTestAtATime || got["message"] != "two tests" || got["remedy"] != "serialize" {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestTheGateTakesTheSkipMapAndAcknowledgesIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &wireSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeGate, `{"run":null,"skip":{"orders::TestToken":"flaky: replay_only"}}`)
	if rec.Code != http.StatusOK || svc.skips["orders::TestToken"] != "flaky: replay_only" {
		t.Fatalf("gate: %d %s, skips %v", rec.Code, rec.Body.String(), svc.skips)
	}
	var ack models.ScopeGateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack.V != 1 || ack.Skips != 1 {
		t.Fatalf("ack %s (%v)", rec.Body.String(), err)
	}
	// A push without a map (an empty one is left out of the JSON) skips nothing.
	rec = call(t, a.HandleScopeGate, `{"run":null}`)
	if rec.Code != http.StatusOK || svc.skips != nil {
		t.Fatalf("empty gate: %d %s, skips %v", rec.Code, rec.Body.String(), svc.skips)
	}
}

// Under the switch a run list is not part of the gate.
func TestAProvenAgentRefusesARunList(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a := &Agent{logger: zap.NewNop(), svc: &wireSvc{}}
	rec := call(t, a.HandleScopeGate, `{"run":["TestA"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "run list") {
		t.Fatalf("gate: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, a.HandleScopeGate, `{`)
	var got map[string]string
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["error"] != models.ScopeErrBadRequest {
		t.Fatalf("an unreadable gate is a JSON error under the switch: %d %s", rec.Code, rec.Body.String())
	}
}

// An agent without the switch refuses a skip list it would ignore.
func TestAnAgentWithoutTheSwitchRefusesASkipList(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a := &Agent{logger: zap.NewNop(), svc: &wireSvc{}}
	rec := call(t, a.HandleScopeGate, `{"run":null,"skip":{"a::T":"r"}}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "KEPLOY_PROVEN") {
		t.Fatalf("gate: %d %s", rec.Code, rec.Body.String())
	}
}

// Under the wire every refusal is a JSON error, an unreadable body too.
func TestAnUnreadableBodyIsAJSONErrorUnderTheWire(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a := &Agent{logger: zap.NewNop(), svc: &wireSvc{}}
	for _, h := range []http.HandlerFunc{a.HandleScopeBegin, a.HandleScopeEnd, a.HandleInstance} {
		rec := call(t, h, `{`)
		var got map[string]string
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["error"] != models.ScopeErrBadRequest {
			t.Fatalf("unreadable body: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestAnAppStartNeedsANewIntegrationUnderTheWire(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a := &Agent{logger: zap.NewNop(), svc: &wireSvc{}}
	rec := call(t, a.HandleAppStart, `{"pid":42}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), models.ScopeErrUpgradeIntegration) {
		t.Fatalf("app start: %d %s", rec.Code, rec.Body.String())
	}
}

// refusingSvc records what it was told was refused.
type refusingSvc struct {
	wireSvc
	refusals []models.RunRefusal
}

func (s *refusingSvc) NoteRefusal(r models.RunRefusal) { s.refusals = append(s.refusals, r) }

// What the wire refuses is recorded for the run's verdict to name, and an
// integration can tell the agent what it refused; with the switch off the
// refusal path is the plain 404.
func TestRefusalsAreRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &refusingSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	// An httptest request comes from no connection: the agent cannot tell
	// the caller, and that refusal is recorded like any other.
	rec := call(t, a.HandleInstance, `{"key":"o"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	rec = call(t, a.HandleRefusal, `{"code":"go_test_parallel","message":"go test runs up to 8 test binaries at once","remedy":"add -p 1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"v":1`)
	require.Len(t, svc.refusals, 2)
	require.Equal(t, models.ScopeErrNoCaller, svc.refusals[0].Code)
	require.Equal(t, models.RunRefusal{Code: "go_test_parallel", Message: "go test runs up to 8 test binaries at once", Remedy: "add -p 1"}, svc.refusals[1])
	require.Equal(t, http.StatusBadRequest, call(t, a.HandleRefusal, `{"code":""}`).Code)
	t.Setenv(provenswitch.Env, "")
	off := call(t, a.HandleRefusal, `{"code":"x","message":"y"}`)
	require.Equal(t, http.StatusNotFound, off.Code)
	require.Equal(t, "404 page not found\n", off.Body.String())
}
