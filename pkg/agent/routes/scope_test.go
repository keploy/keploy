package routes

// The /agent/scope/begin response is the contract a user's test runner reads.
// These tests pin two things at once: the new fields are present, AND the
// original `{"status":"ok"}` key survives so a runner written against the old
// contract keeps working.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// stubScopeSvc satisfies agent.Service via the embedded nil interface and
// implements only BeginScope, which is all HandleScopeBegin calls.
type stubScopeSvc struct {
	agent.Service
	ack models.ScopeAck
	err error
}

func (s *stubScopeSvc) BeginScope(_ context.Context, _ string, _ int) (models.ScopeAck, error) {
	return s.ack, s.err
}

func postScopeBegin(t *testing.T, svc agent.Service, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	a := &Agent{logger: zap.NewNop(), svc: svc}
	req := httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(body))
	rr := httptest.NewRecorder()
	a.HandleScopeBegin(rr, req)

	var decoded map[string]any
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
		}
	}
	return rr, decoded
}

// A scoped begin reports scoped=true with the pool size, and still carries
// status=ok.
func TestHandleScopeBeginRendersAck(t *testing.T) {
	svc := &stubScopeSvc{ack: models.ScopeAck{Scoped: true, Mocks: 3, Reason: models.ScopeReasonPoolRestricted}}
	rr, got := postScopeBegin(t, svc, `{"name":"alpha"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if got["status"] != "ok" {
		t.Fatalf("the original contract key must survive; got %v", got["status"])
	}
	if got["scoped"] != true {
		t.Fatalf("scoped: got %v, want true", got["scoped"])
	}
	if got["mocks"] != float64(3) {
		t.Fatalf("mocks: got %v, want 3", got["mocks"])
	}
	if got["reason"] != models.ScopeReasonPoolRestricted {
		t.Fatalf("reason: got %v", got["reason"])
	}
}

// The case the whole change exists for: the agent silently served the whole
// suite, and the runner can now see it. Before, this body was byte-identical to
// the scoped one above.
func TestHandleScopeBeginReportsUnscoped(t *testing.T) {
	svc := &stubScopeSvc{ack: models.ScopeAck{Reason: models.ScopeReasonUnmappedScope}}
	rr, got := postScopeBegin(t, svc, `{"name":"renamed"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("an unscoped begin is not an error; got %d", rr.Code)
	}
	if got["status"] != "ok" {
		t.Fatalf("status must stay ok for backward compatibility; got %v", got["status"])
	}
	if got["scoped"] != false {
		t.Fatalf("scoped: got %v, want false", got["scoped"])
	}
	if got["reason"] != models.ScopeReasonUnmappedScope {
		t.Fatalf("reason: got %v", got["reason"])
	}
}

// A service that predates the scope API (no BeginScope method) must still get a
// 200 with status=ok, and say that it has no scope support rather than claiming
// the test was isolated.
func TestHandleScopeBeginServiceWithoutScopeSupport(t *testing.T) {
	rr, got := postScopeBegin(t, nil, `{"name":"alpha"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if got["status"] != "ok" || got["scoped"] != false {
		t.Fatalf("got %v", got)
	}
	if got["reason"] != models.ScopeReasonUnsupported {
		t.Fatalf("reason: got %v, want %q", got["reason"], models.ScopeReasonUnsupported)
	}
}

// An error from BeginScope still fails the request — the ack reports "did the
// scoping take effect", not "did the call error".
func TestHandleScopeBeginPropagatesError(t *testing.T) {
	svc := &stubScopeSvc{err: context.DeadlineExceeded}
	rr, _ := postScopeBegin(t, svc, `{"name":"alpha"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rr.Code)
	}
}
