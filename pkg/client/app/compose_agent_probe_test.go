package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// agentHealth is a stand-in for keploy's agent's health check: it answers
// /agent/health with code, and counts the asks. uri is the agent's address
// as keploy holds it (AgentURI).
func agentHealth(t *testing.T, code int) (uri string, asks *atomic.Int32) {
	t.Helper()
	asks = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/health" {
			http.NotFound(w, r)
			return
		}
		asks.Add(1)
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/agent", asks
}

// refusedAgentURI is the address of an agent that has exited: nothing
// listens there, so every ask is refused.
func refusedAgentURI(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr + "/agent"
}

// An app killed with SIGKILL while keploy's agent runs on (killed from
// outside docker) keeps its own halt as soon as the agent answers its health
// check, not after keploy's stall bound (here ten minutes): an answer proves
// the agent was not what killed it. An agent that answers nothing (refused,
// or no address known) or answers something other than its 200 is waited
// for as before, and named once docker says its container is down.
func TestAnAppKilledFromOutsideDockerKeepsItsHaltAtOnce(t *testing.T) {
	stallBudget(t, 10*time.Minute)
	started := time.Now().Add(-time.Minute)
	for name, tc := range map[string]struct {
		code      int
		refused   bool
		noURI     bool
		downAfter int
		wantAgent bool
	}{
		"the agent answers":                           {code: http.StatusOK},
		"its health is refused, then it is down":      {refused: true, downAfter: 3, wantAgent: true},
		"its health answers 503, then it is down":     {code: http.StatusServiceUnavailable, downAfter: 3, wantAgent: true},
		"no address for it is known, then it is down": {noURI: true, downAfter: 3, wantAgent: true},
	} {
		d := &agentHaltDocker{composeDocker: newComposeDocker(false, true), app: exitedState(137, started),
			agent: runningState(started), downAfter: tc.downAfter, downCode: 139}
		a := newAgentHaltApp(d)
		core, logs := observer.New(zap.InfoLevel)
		a.logger = zap.New(core)
		var asks *atomic.Int32
		switch {
		case tc.refused:
			a.opts.AgentURI = refusedAgentURI(t)
		case !tc.noURI:
			a.opts.AgentURI, asks = agentHealth(t, tc.code)
		}
		kept := &composeHalt{exitCode: 137, startedAt: started.UnixNano()}
		a.composeApp.halted.Store(kept)
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.composeAgentNamesTheHalt(context.Background())
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the halt was not decided within 10 s (the stall bound is 10 min)", name)
		}
		got := a.composeAppHalt()
		if tc.wantAgent {
			if got == nil || got.agent != haltAgent || got.exitCode != 139 {
				t.Fatalf("%s: halt %+v; want the agent's, with its code 139", name, got)
			}
			if d.reads() != tc.downAfter {
				t.Fatalf("%s: the agent was read %d times, want %d", name, d.reads(), tc.downAfter)
			}
		} else if got != kept {
			t.Fatalf("%s: halt %+v; want the app's own, %+v", name, got, kept)
		}
		// The app's own halt says once why it stands; the agent's says nothing of it.
		if said, want := logs.FilterMessageSnippet("something outside docker killed it, and its exit ends the run").Len(), map[bool]int{false: 1, true: 0}[tc.wantAgent]; said != want {
			t.Fatalf("%s: said the outside kill %d times; want %d", name, said, want)
		}
		if asks != nil && asks.Load() == 0 {
			t.Fatalf("%s: the agent's health was never asked", name)
		}
	}
}

// appKillDie is the die of the app's container, run ref, with code.
func appKillDie(ref, code string) events.Message {
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: ref, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", "exitCode": code}}}
}

// A compose app SIGKILLed from outside docker, its agent up and answering,
// ends the run at once, with its own exit (137), not after keploy's stall
// bound; an app killed with its agent, which exits a moment later, ends it
// naming the agent and its code, as soon as docker says the agent is down;
// with the switch off and on.
func TestASIGKILLedComposeAppEndsTheRunAtOnceInEveryMode(t *testing.T) {
	bothDown := func(d *stackDaemon) bool { return !d.running(runAppName) && !d.running(runAgentName) }
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven+"/killed from outside docker", func(t *testing.T) {
			uri, asks := agentHealth(t, http.StatusOK)
			r := startStackRun(t, proven, bothDown, func(a *App) { a.opts.AgentURI = uri })
			stream := r.watchStream(t)
			if err := r.d.exit(runAppName, 137, "EXIT"); err != nil {
				t.Fatal(err)
			}
			begun := time.Now()
			stream <- appKillDie(runAppName, "137")
			e := r.result(t, "the app's SIGKILL")
			if took := time.Since(begun); took > 10*time.Second {
				t.Fatalf("the run ended %s after the app's die; want at once", took)
			}
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 137 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) ||
				strings.Contains(e.Err.Error(), "keploy's agent container") {
				t.Fatalf("run() ended %+v; want the app's own halt, code 137", e)
			}
			if asks.Load() == 0 {
				t.Fatal("the agent's health was never asked")
			}
		})
		t.Run("switch="+proven+"/killed with its agent", func(t *testing.T) {
			r := startStackRun(t, proven, bothDown, func(a *App) { a.opts.AgentURI = refusedAgentURI(t) })
			stream := r.watchStream(t)
			if err := r.d.exit(runAppName, 137, "EXIT"); err != nil {
				t.Fatal(err)
			}
			begun := time.Now()
			stream <- appKillDie(runAppName, "137")
			time.Sleep(300 * time.Millisecond)
			if err := r.d.exit(runAgentName, 2, "EXIT"); err != nil {
				t.Fatal(err)
			}
			e := r.result(t, "the agent's exit")
			if took := time.Since(begun); took > 10*time.Second {
				t.Fatalf("the run ended %s after the app's die; want as soon as the agent is down", took)
			}
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 2 || e.Err == nil || !strings.Contains(e.Err.Error(), "keploy's agent container") {
				t.Fatalf("run() ended %+v; want the agent's halt, code 2", e)
			}
		})
	}
}

// Each run starts with no kill of the app kept: a kill an earlier run kept
// (appKilledAt) does not stop a SIGKILL of this run's app, whose start the
// daemon cannot say (its container unread), from naming the agent that
// exited with it, with the switch off and on.
func TestARunNamesTheAgentWhateverKillAnEarlierRunKeptInEveryMode(t *testing.T) {
	bothDown := func(d *stackDaemon) bool { return !d.running(runAppName) && !d.running(runAgentName) }
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, bothDown, func(a *App) { a.composeApp.appKilledAt.Store(1) })
			stream := r.watchStream(t)
			if err := r.d.exit(runAgentName, 2, "EXIT"); err != nil {
				t.Fatal(err)
			}
			if err := r.d.exit(runAppName, 137, "EXIT"); err != nil {
				t.Fatal(err)
			}
			// The daemon cannot read the run of the app that died (an ID it
			// no longer has), so its halt says no start.
			stream <- appKillDie("app-run-gone", "137")
			e := r.result(t, "the app's SIGKILL")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 2 || e.Err == nil || !strings.Contains(e.Err.Error(), "keploy's agent container") {
				t.Fatalf("run() ended %+v; want the agent's halt, code 2", e)
			}
		})
	}
}

// An app's container that ran out of memory says so in its halt.
func TestTheHaltOfAnAppOutOfMemorySaysSo(t *testing.T) {
	if msg := (composeHalt{exitCode: 137, oomKilled: true}).Error(); !strings.Contains(msg, "code 137 (out of memory") {
		t.Fatalf("halt %q; want it to say the app ran out of memory", msg)
	}
	if msg := (composeHalt{exitCode: 137}).Error(); strings.Contains(msg, "memory") {
		t.Fatalf("halt %q of an app not out of memory says memory", msg)
	}
}
