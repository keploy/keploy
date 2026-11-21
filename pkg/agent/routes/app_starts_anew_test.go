package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/service/agent"
)

// appStartHooks are hooks with the AppStartHook capability, noting each
// notice and answering err.
type appStartHooks struct {
	agent.AgentHook
	sets  []string
	first []time.Time
	err   error
}

func (h *appStartHooks) AppStartsAnew(_ context.Context, set string, first time.Time) error {
	h.sets, h.first = append(h.sets, set), append(h.first, first)
	return h.err
}

func useHooks(t *testing.T, h agent.AgentHooks) {
	t.Helper()
	prev := agent.ActiveHooks
	agent.ActiveHooks = h
	t.Cleanup(func() { agent.ActiveHooks = prev })
}

func appStartsAnew(a *Agent, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.HandleAppStartsAnew(rec, httptest.NewRequest(http.MethodPost, "/agent/hooks/app-starts-anew", strings.NewReader(body)))
	return rec
}

// The agent hands the hooks that keep the app's clock the set the application
// starts anew for and its earliest recorded test case, as sent.
func TestAppStartsAnewTellsTheHooksTheSetAndItsFirstRecordedCase(t *testing.T) {
	h := &appStartHooks{}
	useHooks(t, h)
	a := &Agent{logger: zap.NewNop()}
	if rec := appStartsAnew(a, `{"testSetID":"test-set-1","firstRecorded":"2026-10-06T10:00:01.25Z"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	want := time.Date(2026, 10, 6, 10, 0, 1, 250000000, time.UTC)
	if len(h.sets) != 1 || h.sets[0] != "test-set-1" || !h.first[0].Equal(want) {
		t.Fatalf("told %v at %v, want test-set-1 at %v", h.sets, h.first, want)
	}
}

// Hooks that cannot fail the notice's answer: the CLI says it.
func TestAppStartsAnewAnswersTheHooksError(t *testing.T) {
	useHooks(t, &appStartHooks{err: errors.New("clock file gone")})
	rec := appStartsAnew(&Agent{logger: zap.NewNop()}, `{"testSetID":"test-set-1"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "clock file gone") {
		t.Fatalf("status %d %q, want 500 with the hooks' error", rec.Code, rec.Body.String())
	}
}

// Hooks without the capability (OSS's own) keep no clock: the notice is
// answered, with nothing to do.
func TestAppStartsAnewWithoutTheCapabilityIsAnswered(t *testing.T) {
	useHooks(t, &agent.AgentHook{})
	if rec := appStartsAnew(&Agent{logger: zap.NewNop()}, `{"testSetID":"test-set-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppStartsAnewRefusesABodyItCannotRead(t *testing.T) {
	h := &appStartHooks{}
	useHooks(t, h)
	if rec := appStartsAnew(&Agent{logger: zap.NewNop()}, `{`); rec.Code != http.StatusBadRequest || len(h.sets) != 0 {
		t.Fatalf("status %d, %d notices; want 400 and none", rec.Code, len(h.sets))
	}
}
