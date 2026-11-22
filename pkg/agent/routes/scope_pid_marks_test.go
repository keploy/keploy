package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A begin with a pid that keploy cannot narrow to the test's own mocks (a run
// without the suite mark or an app-start mark) is the harness's to change, as a
// second open test is: 422, and the body says what to send instead. The
// agent logs the refusal at WARN, naming the test, its pid and why: the test
// fails, and the replay with it, whatever the harness does with the answer.
func TestScopeBeginRefusesAPidBeginWithoutMarksWith422(t *testing.T) {
	t.Cleanup(starts.Default.Reset)
	err := fmt.Errorf("pkg::TestB began with the pid 7: %w.\n%s", agent.ErrPidBeginWithoutMarks, agent.PidBeginRemedy)
	core, logs := observer.New(zap.DebugLevel)
	a := &Agent{logger: zap.New(core), svc: &refusingScopeSvc{err: err}}
	rec := httptest.NewRecorder()
	a.HandleScopeBegin(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(`{"name":"pkg::TestB","pid":7}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "pkg::TestB began with the pid 7: ") {
		t.Fatalf("the refusal does not name the test: %s", body)
	}
	if !strings.Contains(body, "\nEither add the suite mark, at record and at replay: ") || !strings.Contains(body, "POST /agent/app/start") ||
		!strings.Contains(body, "\nOr send the begin without a pid, and run the tests one at a time") {
		t.Fatalf("the refusal does not give both ways out, one line each: %s", body)
	}
	refusals := logs.FilterMessageSnippet("keploy refused a test at its begin").All()
	if len(refusals) != 1 {
		t.Fatalf("want the refusal logged once, got %d: %v", len(refusals), logs.All())
	}
	e := refusals[0]
	if e.Level != zap.WarnLevel {
		t.Fatalf("the refusal is logged at %s, want WARN: where the agent's log is read, a Debug line is not", e.Level)
	}
	f := e.ContextMap()
	if f["test"] != "pkg::TestB" || f["pid"] != int64(7) || !strings.Contains(fmt.Sprint(f["error"]), agent.ErrPidBeginWithoutMarks.Error()) {
		t.Fatalf("the refusal's log does not name the test, its pid and why: %v", f)
	}
}
