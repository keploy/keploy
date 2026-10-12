package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// PushScopeGate posts the gate to /replay/gate, keeping a nil list (no gate)
// apart from an empty one (run nothing); an agent that cannot gate — older
// (404) or without the capability (501) — is reported as such, not as success.
func TestAgentClient_PushScopeGate(t *testing.T) {
	var bodies []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/replay/gate" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()

	if err := a.PushScopeGate(ctx, nil, ""); err != nil {
		t.Fatalf("nil gate: %v", err)
	}
	if err := a.PushScopeGate(ctx, []string{}, "nothing proven"); err != nil {
		t.Fatalf("empty gate: %v", err)
	}
	if err := a.PushScopeGate(ctx, []string{"TestA"}, "not yet proven"); err != nil {
		t.Fatalf("gate: %v", err)
	}
	want := []string{`{"run":null}`, `{"run":[],"reason":"nothing proven"}`, `{"run":["TestA"],"reason":"not yet proven"}`}
	if len(bodies) != len(want) {
		t.Fatalf("posted %v, want %v", bodies, want)
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Fatalf("post %d = %s, want %s", i, bodies[i], want[i])
		}
	}

	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		status = s
		if err := a.PushScopeGate(ctx, []string{"TestA"}, "x"); !errors.Is(err, models.ErrScopeGateUnsupported) {
			t.Fatalf("status %d: got %v, want ErrScopeGateUnsupported", s, err)
		}
	}
	status = http.StatusInternalServerError
	if err := a.PushScopeGate(ctx, nil, ""); err == nil || errors.Is(err, models.ErrScopeGateUnsupported) {
		t.Fatalf("a failing agent must be an error of its own, got %v", err)
	}
}

// PushScopeSkips posts the proven gate to /replay/gate as a skip map, an empty
// map included, and requires the agent's versioned acknowledgement: any other
// answer is a failure, an older agent's 200 without one too, since a replay
// that cannot skip what CI skips must refuse, not run the list.
func TestAgentClient_PushScopeSkips(t *testing.T) {
	var bodies []string
	status, answer := http.StatusOK, `{"v":1,"status":"ok","skips":1}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/replay/gate" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()

	if err := a.PushScopeSkips(ctx, map[string]string{"orders::TestToken": "flaky: replay_only"}); err != nil {
		t.Fatalf("skips: %v", err)
	}
	answer = `{"v":1,"status":"ok","skips":0}`
	if err := a.PushScopeSkips(ctx, map[string]string{}); err != nil {
		t.Fatalf("no skips: %v", err)
	}
	if err := a.PushScopeSkips(ctx, map[string]string{"a": "b"}); err == nil {
		t.Fatal("an agent that applies fewer entries than were sent did not acknowledge the list")
	}
	bodies = bodies[:2]
	want := []string{`{"run":null,"skip":{"orders::TestToken":"flaky: replay_only"}}`, `{"run":null}`}
	if len(bodies) != 2 || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("posted %v, want %v", bodies, want)
	}
	answer = `{"status":"ok"}`
	if err := a.PushScopeSkips(ctx, map[string]string{"a": "b"}); err == nil {
		t.Fatal("an agent that answers without the wire's version did not acknowledge the list")
	}
	answer = `{"v":1,"status":"ok","skips":1}`
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		status = s
		if err := a.PushScopeSkips(ctx, map[string]string{"a": "b"}); err == nil {
			t.Fatalf("status %d: a refused list must be an error", s)
		}
	}
}
