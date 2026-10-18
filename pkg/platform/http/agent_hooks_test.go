package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// hookCalls is every /hooks/* call, each made with the ctx it is given.
var hookCalls = []struct {
	name string
	call func(context.Context, *AgentClient) error
}{
	{"BeforeSimulate", func(ctx context.Context, a *AgentClient) error {
		now := time.Now()
		return a.BeforeSimulate(ctx, &now, "test-set-0", "test-1")
	}},
	{"AfterSimulate", func(ctx context.Context, a *AgentClient) error {
		return a.AfterSimulate(ctx, "test-1", "test-set-0")
	}},
	{"BeforeTestRun", func(ctx context.Context, a *AgentClient) error {
		return a.BeforeTestRun(ctx, "test-run-0")
	}},
	{"BeforeTestSetCompose", func(ctx context.Context, a *AgentClient) error {
		return a.BeforeTestSetCompose(ctx, "test-run-0", "test-set-0", true)
	}},
	{"AfterTestRun", func(ctx context.Context, a *AgentClient) error {
		return a.AfterTestRun(ctx, "test-run-0", []string{"test-set-0"}, models.TestCoverage{})
	}},
}

// slowAgent is an agent whose hook handler, once it is serving a call (served),
// answers only when the test says so: answer(status) answers it with that
// status, and answer(0) closes the connection without an answer. The handler
// is released when the test ends, if it is still waiting.
type slowAgent struct {
	served chan struct{}
	answer chan int
}

func newSlowAgent(t *testing.T) (*slowAgent, *AgentClient) {
	t.Helper()
	ag := &slowAgent{served: make(chan struct{}, 1), answer: make(chan int, 1)}
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ag.served <- struct{}{}
		select {
		case status := <-ag.answer:
			if status == 0 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(status)
		case <-stop:
		}
	}))
	// Cleanups run last first: the handler is released before the server's
	// Close waits for it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	return ag, &AgentClient{
		logger: zap.NewNop(),
		client: agentHTTPClient(""),
		conf:   &config.Config{Agent: config.Agent{AgentURI: srv.URL}},
	}
}

// requestDeadlines is a RoundTripper that notes whether each request it carries
// has a deadline.
type requestDeadlines struct {
	base http.RoundTripper
	mu   sync.Mutex
	had  []bool
}

func (d *requestDeadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	_, ok := r.Context().Deadline()
	d.mu.Lock()
	d.had = append(d.had, ok)
	d.mu.Unlock()
	return d.base.RoundTrip(r)
}

// A hook call waits for the agent's answer for as long as the agent serves it:
// keploy puts no deadline of its own on it. It had one, 50 s, and returned nil
// once it passed while the agent still served the call, so the run went on as
// if the hook were done.
func TestAgentHooks_WaitForTheAgentsAnswer(t *testing.T) {
	require.Zero(t, New(zap.NewNop(), nil, &config.Config{}).client.Timeout,
		"the agent client has a deadline of its own, which the hook calls run under")
	for _, h := range hookCalls {
		t.Run(h.name, func(t *testing.T) {
			ag, a := newSlowAgent(t)
			deadlines := &requestDeadlines{base: a.client.Transport}
			a.client.Transport = deadlines
			done := make(chan error, 1)
			go func() { done <- h.call(context.Background(), a) }()
			<-ag.served
			select {
			case err := <-done:
				t.Fatalf("the call returned (%v) while the agent was still serving it", err)
			default:
			}
			ag.answer <- http.StatusOK
			require.NoError(t, <-done)
			deadlines.mu.Lock()
			defer deadlines.mu.Unlock()
			require.Equal(t, []bool{false}, deadlines.had, "the hook request carries a deadline")
		})
	}
}

// An agent this client started that exits while it serves a hook call ends the
// call, with an error that says so.
func TestAgentHooks_EndWhenTheAgentExits(t *testing.T) {
	for _, h := range hookCalls {
		t.Run(h.name, func(t *testing.T) {
			ag, a := newSlowAgent(t)
			run := newAgentRun()
			a.run = run
			done := make(chan error, 1)
			go func() { done <- h.call(context.Background(), a) }()
			<-ag.served
			run.exited()
			err := <-done
			require.ErrorIs(t, err, errAgentExited)
			require.Contains(t, err.Error(), "keploy's agent exited before it answered /hooks/")
		})
	}
	t.Run("an agent gone before the call", func(t *testing.T) {
		_, a := newSlowAgent(t)
		run := newAgentRun()
		run.exited()
		a.run = run
		err := a.BeforeTestRun(context.Background(), "test-run-0")
		require.ErrorIs(t, err, errAgentExited)
	})
}

// A hook call ends when its ctx does (the user stopped the run), and says so.
func TestAgentHooks_EndWhenTheRunIsStopped(t *testing.T) {
	for _, h := range hookCalls {
		t.Run(h.name, func(t *testing.T) {
			ag, a := newSlowAgent(t)
			a.run = newAgentRun() // running throughout
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- h.call(ctx, a) }()
			<-ag.served
			cancel()
			err := <-done
			require.True(t, errors.Is(err, context.Canceled), "got %v, want context.Canceled", err)
		})
	}
}

// A hook call the agent never answers is an error, as a refusal is: it used to
// be logged at Debug and reported done.
func TestAgentHooks_AnUnansweredCallIsAnError(t *testing.T) {
	for _, h := range hookCalls {
		t.Run(h.name, func(t *testing.T) {
			ag, a := newSlowAgent(t)
			done := make(chan error, 1)
			go func() { done <- h.call(context.Background(), a) }()
			<-ag.served
			ag.answer <- 0 // the connection closes with no answer
			err := <-done
			require.Error(t, err)
			require.Contains(t, err.Error(), "keploy's agent did not answer /hooks/")
		})
	}
	t.Run("a refusal", func(t *testing.T) {
		ag, a := newSlowAgent(t)
		ag.answer <- http.StatusInternalServerError
		require.EqualError(t, a.AfterTestRun(context.Background(), "test-run-0", nil, models.TestCoverage{}), "agent hook failed: 500")
	})
}

// waitForLog waits until the logger has written msg at least n times, for as
// long as that takes.
func waitForLog(logs *observer.ObservedLogs, msg string, n int) []observer.LoggedEntry {
	for {
		if got := logs.FilterMessage(msg).All(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A hook call that waits for the agent says so, names the hook, says it again
// while it waits, and says when the answer came: a wait with no end is not a
// silent one.
func TestAgentHooks_SayWhileTheyWait(t *testing.T) {
	prevNotice, prevStill := hookNoticeAfter, hookStillWaitingEvery
	hookNoticeAfter, hookStillWaitingEvery = 0, 10*time.Millisecond
	t.Cleanup(func() { hookNoticeAfter, hookStillWaitingEvery = prevNotice, prevStill })
	core, logs := observer.New(zapcore.InfoLevel)
	ag, a := newSlowAgent(t)
	a.logger = zap.New(core)
	done := make(chan error, 1)
	go func() { done <- a.AfterTestRun(context.Background(), "test-run-0", nil, models.TestCoverage{}) }()
	<-ag.served
	first := waitForLog(logs, "waiting for keploy's agent to answer a hook; the run goes on once it has", 1)
	require.Equal(t, "/hooks/after-test-run", first[0].ContextMap()["endpoint"])
	waitForLog(logs, "still waiting for keploy's agent to answer a hook", 2)
	ag.answer <- http.StatusOK
	require.NoError(t, <-done)
	waitForLog(logs, "keploy's agent answered the hook", 1)
}

// A set's hook is the one a restarted agent is given again, whether or not the
// agent it was sent to answered it: not the previous set's.
func TestBeforeTestSetCompose_ARestartedAgentGetsThisSetsHook(t *testing.T) {
	var mu sync.Mutex
	var sets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.BeforeTestSetCompose
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sets = append(sets, req.TestSetID)
		mu.Unlock()
		if req.TestSetID == "test-set-1" && len(sets) == 2 {
			// The agent dies before it answers test-set-1's hook.
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sessions.Delete(srv.URL) })
	a := &AgentClient{logger: zap.NewNop(), client: agentHTTPClient(""), conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	a.session().configure(true)

	require.NoError(t, a.BeforeTestSetCompose(context.Background(), "test-run-0", "test-set-0", true))
	require.Error(t, a.BeforeTestSetCompose(context.Background(), "test-run-0", "test-set-1", false))

	s := a.session()
	s.mu.Lock()
	var apply func(context.Context) error
	for _, step := range s.steps {
		if step.kind == stepTestSetHook {
			apply = step.apply
		}
	}
	s.mu.Unlock()
	require.NotNil(t, apply, "no set hook recorded for a restarted agent")
	require.NoError(t, apply(context.Background()))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"test-set-0", "test-set-1", "test-set-1"}, sets, "a restarted agent was given another set's hook")
}
