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
	err := fmt.Errorf("pkg::TestB began with the pid 7: %w", agent.ErrPidBeginWithoutMarks)
	a := &Agent{logger: zap.NewNop(), svc: &refusingScopeSvc{err: err}}
	rec := httptest.NewRecorder()
	a.HandleScopeBegin(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(`{"name":"pkg::TestB"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Send the begin without a pid") || !strings.Contains(body, "/agent/app/start") {
		t.Fatalf("the refusal does not say what to send instead: %s", body)
	}
}
