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
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// An agent whose recording client has closed its streams stops by itself (the
// signal that reached it too, held until then), and shuts its HTTP server: the
// client's stop request then fails with EOF, or is refused. From an agent that
// was up (agentRun.ready, which endAgent checks), that is the agent on its way
// out, not one to signal at once.
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

// endAgent's decision on the CLI's stop. The agent is asked first, and one
// that answers is not signalled. One that was up and is already going away is
// waited for, and signalled only if it does not exit within the grace; the
// user's second interrupt ends that wait without a signal (with a grace of an
// hour, that case would not return otherwise). One whose stop request fails
// otherwise, or that never answered (still starting: refused, as nothing
// listens yet), is signalled at once. A signalled agent is then waited for, never killed (the
// terminate func stands for SIGTERM, on which it exits). One already reaped is
// never signalled: its pid may be another process's by then.
func TestEndAgent(t *testing.T) {
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)
	goingAway := fmt.Errorf(`Post "http://localhost:1/agent/stop": %w`, io.EOF)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	failed := errors.New("agent stop http 500: boom")
	for _, tc := range []struct {
		name  string
		ask   error
		ready bool
		// exits: the agent exits by itself once endAgent waits for it to,
		// before any signal; reaped: it had already exited before the stop.
		exits, reaped bool
		grace         time.Duration
		interrupt     bool
		wantSignal    bool
	}{
		{name: "it answers", ready: true, grace: time.Hour},
		{name: "it answers, never answered before", grace: time.Hour},
		{name: "it is going away, and exits", ask: goingAway, ready: true, exits: true, grace: time.Hour},
		{name: "it is not listening any more, and exits", ask: refused, ready: true, exits: true, grace: time.Hour},
		{name: "it is going away, but does not exit", ask: goingAway, ready: true, grace: 50 * time.Millisecond, wantSignal: true},
		{name: "it is going away, and the user interrupts again", ask: goingAway, ready: true, grace: time.Hour, interrupt: true},
		{name: "its stop request fails otherwise", ask: failed, ready: true, grace: time.Hour, wantSignal: true},
		{name: "it never answered: still starting", ask: refused, grace: time.Hour, wantSignal: true},
		{name: "it never answered, and fails otherwise", ask: failed, grace: time.Hour, wantSignal: true},
		{name: "it was already reaped", ask: failed, ready: true, reaped: true, grace: time.Hour},
		{name: "it was already reaped, going away", ask: goingAway, ready: true, reaped: true, grace: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(utils.ResetInterruptedAgain)
			if tc.interrupt {
				utils.MarkInterruptedAgain()
			}
			agentStoppingGrace = tc.grace
			core, logs := observer.New(zap.DebugLevel)
			a := &AgentClient{logger: zap.New(core)}
			run := newAgentRun()
			run.ready.Store(tc.ready)
			if tc.reaped {
				run.exited()
			}
			var signalled atomic.Bool
			terminate := func() error {
				signalled.Store(true)
				run.exited() // the agent exits on SIGTERM
				return nil
			}
			returned := make(chan struct{})
			if tc.exits {
				// On its way out: it exits while endAgent waits for it, and
				// only then, so a stop that signals before waiting is caught.
				go func() {
					for logs.FilterMessageSnippet("already stopping; waiting for it to exit").Len() == 0 {
						select {
						case <-returned:
							return
						case <-time.After(time.Millisecond):
						}
					}
					run.exited()
				}()
			}
			go func() { a.endAgent(run, 1, func() error { return tc.ask }, terminate); close(returned) }()
			select {
			case <-returned:
			case <-time.After(time.Minute):
				t.Fatal("endAgent did not return")
			}
			if signalled.Load() != tc.wantSignal {
				t.Fatalf("signalled=%v, want %v", signalled.Load(), tc.wantSignal)
			}
		})
	}
}

// Each start of the agent waits for the agent before it to exit -- one agent
// at a time -- and gets a run of its own that has not answered, whatever the
// one before it did; the run before it keeps its own, which its stop reads.
func TestBeginRunWaitsForThePreviousAgentAndStartsFresh(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	prev := newAgentRun()
	prev.ready.Store(true)
	a.run = prev

	ended, end := context.WithCancel(context.Background())
	end()
	if _, _, _, err := a.beginRun(ended); err == nil {
		t.Fatal("began a new run while the previous agent was still running")
	}
	if a.currentRun() != prev {
		t.Fatal("a start that did not begin replaced the previous run")
	}

	prev.exited()
	_, stop, run, err := a.beginRun(context.Background())
	if err != nil {
		t.Fatalf("after the previous agent exited: %v", err)
	}
	defer stop()
	if run == prev || a.currentRun() != run {
		t.Fatal("the new start did not get a run of its own")
	}
	if run.ready.Load() {
		t.Fatal("a new agent inherits the one before it having answered")
	}
	if !prev.ready.Load() {
		t.Fatal("beginning a new run took back that the agent before it answered")
	}
}

// The stop request and the grace for an agent on its way out end before the
// recorders' teardown stops waiting on the goroutine that stops the agent
// (30 s): otherwise an agent that hangs as it goes is signalled only after
// that wait gave up, if at all.
func TestAgentStoppingGraceEndsBeforeTheTeardownWait(t *testing.T) {
	if total := agentStopRequestTimeout + agentStoppingGrace; total >= 30*time.Second {
		t.Fatalf("the stop request (%v) and the grace (%v) take %v, not under the teardown's 30 s wait",
			agentStopRequestTimeout, agentStoppingGrace, total)
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

// --from-container: the user's container is restored only once the agent
// container, which holds the app's ports, has exited -- it is waited for, not
// killed -- or once the user's second interrupt says not to wait.
func TestRestoreFromContainerWaitsForTheAgentContainer(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		utils.ResetInterruptedAgain()
		t.Cleanup(utils.ResetInterruptedAgain)
		fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
		a := restoreAgentWith(fake, demoSpec())
		a.fromContainer = "demo-app"
		a.fromContainerWasRunning = true
		run := newAgentRun()
		a.run = run
		t.Cleanup(run.exited)

		restored := make(chan struct{})
		go func() { a.RestoreFromContainer(); close(restored) }()
		// Only that it has NOT restored yet: too short a look under load can
		// miss a wrong restore, never fail a right one.
		select {
		case <-restored:
			t.Fatalf("interrupt=%v: restored the user's container while the agent container still ran", interrupt)
		case <-time.After(200 * time.Millisecond):
		}
		if fake.callIndex("start:") >= 0 {
			t.Fatalf("interrupt=%v: started the user's container while the agent container still ran", interrupt)
		}
		if interrupt {
			utils.MarkInterruptedAgain()
		} else {
			run.exited()
		}
		select {
		case <-restored:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("interrupt=%v: the restore never ran", interrupt)
		}
		if fake.callIndex("start:") < 0 {
			t.Fatalf("interrupt=%v: the user's container was not started again", interrupt)
		}
	}
}
