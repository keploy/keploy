package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.uber.org/zap"
)

// An agent whose recording client has closed its streams stops by itself (the
// signal that reached it too, held until then), and shuts its HTTP server: the
// client's stop request then fails with EOF, or is refused. From an agent that
// was up (agentReached, which stopAgentProcess checks), that is the agent on
// its way out, not one to signal.
func TestAgentGoingAwayIsHowAStoppingAgentAnswers(t *testing.T) {
	// A server that closes the connection without answering, as a server
	// shutting down does to a request it had not started on.
	eof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer eof.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close() // nothing listens there any more

	for name, uri := range map[string]string{"server closing": eof.URL, "nothing listening": goneURL} {
		a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: uri + "/agent"}}}
		if err := a.requestAgentStop(); err == nil || !agentGoingAway(err) {
			t.Errorf("%s: stop request error %v not taken for the agent going away", name, err)
		}
	}
	if agentGoingAway(errors.New("agent stop http 500: boom")) {
		t.Error("an agent that answered with an error was taken for one going away")
	}
}

// The stop of a native agent: one that answers is not signalled; one that was
// up and is already going away is waited for, and signalled only if it does
// not exit; one whose stop request fails otherwise, or that never answered
// (still starting: refused, as nothing listens yet), is signalled at once.
func TestStopAgentProcessSignalsOnlyAnAgentThatWillNotGo(t *testing.T) {
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)

	exited := make(chan struct{})
	close(exited)
	never := make(chan struct{})
	goingAway := fmt.Errorf(`Post "http://localhost:1/agent/stop": %w`, io.EOF)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	for _, tc := range []struct {
		name     string
		reached  bool
		stopErr  error
		procDone chan struct{}
		grace    time.Duration
		killed   bool
	}{
		{"it answers", true, nil, never, time.Hour, false},
		{"it is going away, and exits", true, goingAway, exited, time.Hour, false},
		{"it is not listening any more, and exits", true, refused, exited, time.Hour, false},
		{"it is going away, but never exits", true, goingAway, never, 50 * time.Millisecond, true},
		{"its stop request fails otherwise", true, errors.New("agent stop http 500: boom"), exited, time.Hour, true},
		// Its process would exit (procDone closed): one taken for going away
		// is waited for and not signalled.
		{"it never answered: still starting", false, refused, exited, time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentStoppingGrace = tc.grace
			a := &AgentClient{logger: zap.NewNop()}
			reached := new(atomic.Bool)
			reached.Store(tc.reached)
			var killed atomic.Bool
			a.stopAgentProcess(reached, func() error { return tc.stopErr }, tc.procDone, func() { killed.Store(true) })
			if killed.Load() != tc.killed {
				t.Fatalf("signalled=%v, want %v", killed.Load(), tc.killed)
			}
		})
	}
}

// The stop request, the grace for an agent on its way out, and the signal's
// waits, before the kill and after it, end before the recorders' teardown
// stops waiting on the goroutine that stops the agent (30 s): otherwise an
// agent that hangs as it goes is killed only after that wait gave up, and the
// teardown says a goroutine ignored its cancellation.
func TestAgentStoppingGraceEndsBeforeTheTeardownWait(t *testing.T) {
	if total := agentStopRequestTimeout + agentStoppingGrace + agentUtils.StopCommandWait; total >= 30*time.Second {
		t.Fatalf("the stop request (%v), the grace (%v) and the signal's waits (%v) take %v, not under the teardown's 30 s wait",
			agentStopRequestTimeout, agentStoppingGrace, agentUtils.StopCommandWait, total)
	}
}

// Each agent a setup starts has not answered yet, whatever the one before it
// did: a refused stop request to it is an agent still starting, signalled at
// once (stopAgentProcess), not one on its way out. And the stop of the one
// before it, which a setup retry leaves to its grace, still reads that one as
// having answered: it is waited for, not signalled at once.
func TestAStartedAgentIsNotReachedUntilItAnswers(t *testing.T) {
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{}}
	before := new(atomic.Bool)
	before.Store(true) // the agent before it answered
	a.agentReached.Store(before)
	_, _ = a.startAgent(context.Background(), false, models.SetupOptions{})
	if r := a.agentReached.Load(); r == nil || r.Load() {
		t.Fatal("a new agent inherits the one before it having answered")
	}
	if !before.Load() {
		t.Fatal("starting a new agent took back that the one before it answered: its stop signals it at once")
	}
}

// An agent that has exited is not signalled: its PID may be another
// process's by then.
func TestAnAgentThatExitedIsNotSignalled(t *testing.T) {
	procDone := make(chan struct{})
	var killed atomic.Int32
	kill := unlessExited(procDone, func() { killed.Add(1) })
	kill()
	if killed.Load() != 1 {
		t.Fatal("an agent still running was not signalled")
	}
	close(procDone)
	kill()
	if killed.Load() != 1 {
		t.Fatal("an agent that had exited was signalled")
	}
}

// The stop request goes on a connection of its own, never one kept from an
// earlier request: the agent may close such a connection for being idle as
// the request reaches it, and a POST is not resent, so the request failed
// ("server closed idle connection") with an agent that was not going away,
// which was then signalled at once.
func TestTheStopRequestIsSentOnAConnectionOfItsOwn(t *testing.T) {
	for _, tok := range []string{"", "secret"} {
		var conns atomic.Int32
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				conns.Add(1)
			}
		}
		srv.Start()
		defer srv.Close()
		a := &AgentClient{logger: zap.NewNop(), client: agentHTTPClient(tok), conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL + "/agent"}}}
		res, err := a.client.Get(srv.URL + "/agent/health") // leaves its connection idle, kept
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if err := a.requestAgentStop(); err != nil {
			t.Fatalf("token %q: stop request failed: %v", tok, err)
		}
		if n := conns.Load(); n != 2 {
			t.Fatalf("token %q: %d connections for a request and the stop after it: the stop went on the kept one", tok, n)
		}
	}
}
