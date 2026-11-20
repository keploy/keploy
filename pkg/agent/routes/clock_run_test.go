package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// clockRunHooks are hooks with the ClockRunTaker capability, noting each run
// id and answering err.
type clockRunHooks struct {
	agent.AgentHook
	runs []string
	err  error
}

func (h *clockRunHooks) TakeClockRun(_ context.Context, run string) error {
	h.runs = append(h.runs, run)
	return h.err
}

func clockRun(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a := &Agent{logger: zap.NewNop()}
	a.HandleClockRun(rec, httptest.NewRequest(http.MethodPost, "/agent/hooks/clock-run", strings.NewReader(body)))
	return rec
}

// The agent hands its hooks the run id its starter sent, whichever the switch.
func TestClockRunHandsTheHooksTheRunID(t *testing.T) {
	for _, env := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+env, func(t *testing.T) {
			t.Setenv(provenswitch.Env, env)
			h := &clockRunHooks{}
			useHooks(t, h)
			if rec := clockRun(`{"run":"run-1"}`); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(h.runs) != 1 || h.runs[0] != "run-1" {
				t.Fatalf("hooks took %v, want run-1", h.runs)
			}
		})
	}
}

// Hooks that take no run id keep no clock timeline (OSS's own, a native
// macOS or Windows agent's): the route answers them 501 naming that, never
// the 404 of an agent older than the route, so the starter does not call an
// agent of its own version older. Whichever the switch.
func TestClockRunWithoutTimelineHooksIsNotImplemented(t *testing.T) {
	for _, env := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+env, func(t *testing.T) {
			t.Setenv(provenswitch.Env, env)
			useHooks(t, &agent.AgentHook{})
			rec := clockRun(`{"run":"run-1"}`)
			if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "no timeline hooks in this agent") {
				t.Fatalf("status %d %q, want 501 naming no timeline hooks", rec.Code, rec.Body.String())
			}
		})
	}
}

// A run id the hooks refuse is answered 409 with their reason.
func TestClockRunAnswersTheHooksRefusal(t *testing.T) {
	useHooks(t, &clockRunHooks{err: errors.New("the clock already moved on run-0's id")})
	rec := clockRun(`{"run":"run-1"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "run-0's id") {
		t.Fatalf("status %d %q, want 409 with the hooks' reason", rec.Code, rec.Body.String())
	}
}

// A body that cannot be read whole, or one with no run id, is refused before
// the hooks are told anything: a run id read from a body with a value that
// does not decode is not taken.
func TestClockRunRefusesABodyWithNoRunID(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"run":""}`, `{"run":"run-1","run":1}`} {
		h := &clockRunHooks{}
		useHooks(t, h)
		if rec := clockRun(body); rec.Code != http.StatusBadRequest || len(h.runs) != 0 {
			t.Fatalf("%s: status %d, hooks took %v; want 400 and nothing taken", body, rec.Code, h.runs)
		}
	}
}
