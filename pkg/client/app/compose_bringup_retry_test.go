package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// The run's retry of the bring-up, decided on the App alone:
//   - a crash keploy stopped the stack on (depCrash) retries the bring-up on
//     its own, whatever the up's own end (compose's up, run without the
//     abort flags, returns 0 once keploy has stopped the stack), within
//     composeDepFailureRetries, and not once the run is cancelled;
//   - a halt kept takes no retry on any attempt: one kept while a retried up
//     ran ends the run, though the crash and compose's own failure would
//     take another;
//   - a new bring-up clears what the last one kept (the app's first start,
//     keploy's stop of the stack), unless a halt came in the backoff, which
//     then ends the run with nothing cleared;
//   - once the retries are spent, the crash still kept is the run's halt,
//     naming the service and the bring-ups; a halt kept already stands.
//
// Both KEPLOY_PROVEN modes.
func TestTheRunsRetryOfTheBringUpOnACrashKeployStoppedTheStackOn(t *testing.T) {
	crash := composeHalt{exitCode: 1, service: "flaky", serviceCode: 1, flags: "--abort-on-container-exit", beforeApp: true}
	returned0 := utils.CmdError{}
	failed := utils.CmdError{Type: utils.Runtime, Err: errors.New("exit status 1")}
	noStates := func() []composeServiceState { return nil }
	// compose's own abort on early's exit, the app never started: what
	// compose's failure alone would retry.
	earlyCrashed := func() []composeServiceState {
		return []composeServiceState{{Service: "app", State: "created"}, {Service: "early", State: "exited", ExitCode: 3}}
	}
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			crashed := func() *App {
				a := newAbortApp(newAbortDocker(time.Now()), "docker compose up --abort-on-container-exit", nil)
				c := crash
				a.composeApp.depCrash.Store(&c)
				return a
			}
			a := crashed()
			for attempt := 1; attempt <= composeDepFailureRetries; attempt++ {
				if !a.composeRetriesTheBringUp(nil, attempt, returned0, noStates) {
					t.Fatalf("bring-up %d ended 0 on a crash keploy stopped the stack on: no retry; want one, within %d", attempt, composeDepFailureRetries)
				}
			}
			if a.composeRetriesTheBringUp(nil, composeDepFailureRetries+1, returned0, noStates) {
				t.Fatalf("a retry past the bound of %d", composeDepFailureRetries)
			}
			if a.composeRetriesTheBringUp(context.Canceled, 1, returned0, noStates) {
				t.Fatal("a retry of a cancelled run")
			}
			if b := newAbortApp(newAbortDocker(time.Now()), "docker compose up --abort-on-container-exit", nil); b.composeRetriesTheBringUp(nil, 1, returned0, noStates) {
				t.Fatal("a retry with no crash kept and an up that ended 0")
			}
			if !a.composeRetriesTheBringUp(nil, 2, failed, earlyCrashed) {
				t.Fatal("no retry of a failed retried up beside a crash kept")
			}
			a.composeApp.halted.Store(&composeHalt{exitCode: 3, service: "early", serviceCode: 3, flags: "--abort-on-container-exit"})
			for attempt := 1; attempt <= composeDepFailureRetries; attempt++ {
				if a.composeRetriesTheBringUp(nil, attempt, failed, earlyCrashed) {
					t.Fatalf("a halt kept during bring-up %d took a retry; want the run ended on it", attempt)
				}
			}

			// A halt kept in the backoff: no new bring-up, nothing cleared.
			a.composeApp.stopping.Store(true)
			a.composeApp.agentStopping.Store(true)
			a.composeApp.appStartedAt.Store(42)
			if a.composeBeginsABringUpAgain() || a.composeApp.depCrash.Load() == nil || !a.composeApp.stopping.Load() ||
				!a.composeApp.agentStopping.Load() || a.composeApp.appStartedAt.Load() != 42 {
				t.Fatal("a new bring-up began over a halt kept in the backoff; want none, and nothing cleared")
			}
			b := crashed()
			b.composeApp.stopping.Store(true)
			b.composeApp.agentStopping.Store(true)
			b.composeApp.appStartedAt.Store(42)
			if !b.composeBeginsABringUpAgain() || b.composeApp.depCrash.Load() != nil || b.composeApp.stopping.Load() ||
				b.composeApp.agentStopping.Load() || b.composeApp.appStartedAt.Load() != 0 {
				t.Fatal("a new bring-up kept the last one's crash, stop or first start of the app")
			}

			spent := crashed()
			spent.composeKeepsASpentDepCrash(composeDepFailureRetries + 1)
			h := spent.composeAppHalt()
			if h == nil || h.service != "flaky" || h.serviceCode != 1 || h.bringUps != composeDepFailureRetries+1 ||
				!strings.Contains(h.Error(), `"flaky", which the application depends on, crashed (its last exit code 1) before the application started, on each of keploy's 4 bring-ups`) {
				t.Fatalf("the spent crash's halt is %+v (%v); want flaky's crash, named with the 4 bring-ups", h, h)
			}
			kept := crashed()
			first := &composeHalt{exitCode: 3, service: "early", serviceCode: 3}
			kept.composeApp.halted.Store(first)
			if kept.composeKeepsASpentDepCrash(2); kept.composeAppHalt() != first {
				t.Fatalf("the halt kept became %+v; want it to stand", kept.composeAppHalt())
			}
			none := newAbortApp(newAbortDocker(time.Now()), "docker compose up --abort-on-container-exit", nil)
			if none.composeKeepsASpentDepCrash(1); none.composeAppHalt() != nil {
				t.Fatalf("a halt %+v with no crash kept", none.composeAppHalt())
			}
		})
	}
}

// compose's up, run without the user's abort flags, returned with the app
// never started, on the crash of a service the app depends on: once the up
// has returned, the run's retry is behind it, so the crash ends the run (a
// halt, before the app), and no retry is kept for it. Record and test, both
// KEPLOY_PROVEN modes.
func TestAnUpThatReturnedOnADependencysCrashEndsTheRunOnIt(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			d := newAbortDocker(time.Now().Add(-time.Minute))
			d.containers["app-id"].state = container.State{Status: "created"}
			at := time.Now().Add(-30 * time.Second)
			d.add("flaky-id", "flaky", stateAt(at.Add(-time.Second), true, 1, at))
			a := newAbortApp(d, "docker compose up --abort-on-container-exit", composeDoc(t, depCrashDoc))
			a.Mode = mode
			ended := a.composeAbortExited(context.Background())
			h := a.composeAppHalt()
			if !ended || h == nil || h.service != "flaky" || h.serviceCode != 1 || !h.beforeApp || h.bringUps != 0 || a.composeApp.depCrash.Load() != nil {
				t.Fatalf("%s=%q %s: ended %v, halt %+v, crash kept %+v; want the run ended on flaky's crash before the app, no retry kept",
					provenswitch.Env, proven, mode, ended, h, a.composeApp.depCrash.Load())
			}
		}
	}
}

// startsAfterTheUpDocker gives keploy's watch of the project's starts a start
// already delivered as the run's up returns (returned is called as the
// stream is given), and says when the watch has ended (its context's end).
type startsAfterTheUpDocker struct {
	*abortDocker
	returned func()
	ended    chan (<-chan struct{})
}

func (d *startsAfterTheUpDocker) Events(ctx context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs := make(chan events.Message, 1)
	msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: "next-id", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app"}}}
	d.returned()
	d.ended <- ctx.Done()
	return msgs, make(chan error)
}

// A start the daemon delivers once the run's up has returned is no late
// starter of that up (the retry's bring-up, keploy's own start of the app):
// keploy's watch of the starts stops nothing then, whichever of the two
// ready cases its select takes. Many rounds, as the select takes either at
// random. Both KEPLOY_PROVEN modes.
func TestAStartDeliveredOnceTheUpReturnedIsNeverStopped(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			for round := 0; round < 64; round++ {
				d := &startsAfterTheUpDocker{abortDocker: newAbortDocker(time.Now().Add(-time.Minute)), ended: make(chan (<-chan struct{}), 1)}
				d.add("next-id", "app", runningState(time.Now()))
				a := newAbortApp(d.abortDocker, "docker compose up --abort-on-container-exit", nil)
				a.docker = d
				d.returned = a.composeUpRunning()
				a.stopComposeLateStarters(context.Background(), "proj")
				select {
				case done := <-d.ended:
					select {
					case <-done:
					case <-time.After(time.Minute):
						t.Fatalf("round %d: the watch of the starts did not end once the up had returned", round)
					}
				case <-time.After(time.Minute):
					t.Fatalf("round %d: keploy did not watch the starts while the up ran", round)
				}
				d.mu.Lock()
				stops := slices.Clone(d.stops)
				d.mu.Unlock()
				if len(stops) != 0 {
					t.Fatalf("round %d: stops %v; want nothing stopped once the up has returned", round, stops)
				}
			}
		})
	}
}

// keploy's stop of the app at a set's end keeps the app's first start in the
// run before it starts the app again, so the start keploy gives it next never
// stands for the first: an exit between the two came after the app started.
// Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheComposeAppKeepsItsFirstStart(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newComposeDocker(true, true)
			first := time.Now().Add(-time.Minute)
			d.mu.Lock()
			d.startedAt = first
			d.mu.Unlock()
			a := composeApp(d)
			if _, err := a.StopComposeApp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := a.composeApp.appStartedAt.Load(); got != first.UnixNano() {
				t.Fatalf("keploy's stop kept the first start %d; want %d", got, first.UnixNano())
			}
			if _, err := a.StartComposeApp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if a.composeExitBeforeApp(context.Background(), first.Add(time.Second).UnixNano()) {
				t.Fatal("an exit between the app's first start and keploy's start of it again was read as before the app")
			}
		})
	}
}

// keploy's stop of the stack on a dependency's crash, while the run's up
// runs, stops a container of the project that starts after the stop's
// listing (compose's up starting a service whose wait the stop met), so the
// up returns and the run does not hang on it. Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackStopsALateStarterOfTheRunsUpInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_STUB_UP_CODE", "1")
			var late atomic.Bool
			// compose's up returns once every container it attaches to is
			// down: the late starter keeps it up until it is stopped.
			r := startStackRun(t, proven, func(d *stackDaemon) bool {
				return late.Load() && !d.running(runAgentName) && !d.running("runner-1")
			}, func(a *App) {
				a.setComposeUserAbort("docker compose up --exit-code-from runner", composeDoc(t, `
services:
  app:
    image: app
    depends_on: [flaky]
  runner:
    image: runner
  flaky:
    image: flaky
`))
			})
			r.d.mu.Lock()
			r.d.cs[runAppName].status = "created"
			r.d.mu.Unlock()
			r.d.add(&abortContainer{name: "flaky-1", service: "flaky", status: "running"})
			stream := r.watchStream(t)
			if err := r.d.exit("flaky-1", 1, "EXIT"); err != nil {
				t.Fatal(err)
			}
			stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: "flaky-1", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "flaky", "exitCode": "1"}}}
			var starts chan events.Message
			select {
			case starts = <-r.d.streams:
			case <-time.After(30 * time.Second):
				t.Fatal("keploy's stop of the stack did not watch the project's starts while the up ran")
			}
			r.d.add(&abortContainer{name: "runner-1", service: "runner", status: "running"})
			late.Store(true)
			starts <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: "runner-1", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "runner"}}}
			eventually(t, "the late starter is stopped and the up returns", func() bool {
				trail := r.trail(t)
				return trailAt(trail, "STOP runner-1") >= 0 && trailAt(trail, "UP-DONE") >= 0
			})
		})
	}
}
