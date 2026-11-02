package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// A begin with a pid that keploy cannot narrow to the test's own mocks (a run
// without the suite and app-start marks) is the harness's to change, as a
// second open test is: 422, and the body says what to send instead.
func TestScopeBeginRefusesAPidBeginWithoutMarksWith422(t *testing.T) {
	err := fmt.Errorf("pkg::TestB began with the pid 7: %w.\n%s", agent.ErrPidBeginWithoutMarks, agent.PidBeginRemedy)
	a := &Agent{logger: zap.NewNop(), svc: &refusingScopeSvc{err: err}}
	rec := httptest.NewRecorder()
	a.HandleScopeBegin(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(`{"name":"pkg::TestB"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "pkg::TestB began with the pid 7: ") {
		t.Fatalf("the refusal does not name the test: %s", body)
	}
	if !strings.Contains(body, "\nEither add the marks, at record and at replay: ") || !strings.Contains(body, "POST /agent/app/start") ||
		!strings.Contains(body, "\nOr send the begin without a pid, and run the tests one at a time") {
		t.Fatalf("the refusal does not give both ways out, one line each: %s", body)
	}
}
