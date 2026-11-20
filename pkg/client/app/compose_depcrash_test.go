package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// depCrashDoc is a stack whose app depends on flaky (the list form), base
// (service_started, which itself depends on deep) and again (restart: true),
// waits on migrate's completion and on an optional cache's health; seed is a
// one-shot runner waits on, opt an optional dependency api waits on to be
// healthy, optmig an optional one-shot api waits on, and early a service
// nothing depends on.
const depCrashDoc = `
services:
  app:
    image: app
    depends_on:
      flaky:
        condition: service_started
      base:
        condition: service_started
      again:
        restart: true
      migrate:
        condition: service_completed_successfully
      optcache:
        condition: service_healthy
        required: false
  runner:
    image: runner
    depends_on:
      seed:
        condition: service_completed_successfully
  api:
    image: api
    depends_on:
      opt:
        condition: service_healthy
        required: false
      optmig:
        condition: service_completed_successfully
        required: false
  base:
    image: base
    depends_on: [deep]
  flaky: {image: flaky}
  deep: {image: deep}
  again: {image: again}
  migrate: {image: migrate}
  optcache: {image: optcache}
  seed: {image: seed}
  opt: {image: opt}
  optmig: {image: optmig}
  early: {image: early}
`

// stateAt is a container state that started at started and, with exited
// set, exited with code at finished, on the daemon's clock.
func stateAt(started time.Time, exited bool, code int, finished time.Time) container.State {
	if !exited {
		return runningState(started)
	}
	st := exitedState(code, started)
	st.FinishedAt = finished.UTC().Format(time.RFC3339Nano)
	return st
}

// What an exit the user's flags end the run on does, decided by the daemon's
// own instants (each container's StartedAt and FinishedAt, a fake clock):
//   - a crash, before the app started, of a service the app depends on, in
//     any depends_on form and to any depth (an optional dependency's too,
//     whose failure compose's up skips), keeps no halt: keploy stops the
//     stack, as compose's abort stopped it, and keeps the crash for the
//     run's retry of the bring-up (depCrash);
//   - a service nothing depends on (`early`), a 0, or a crash after the app
//     started, ends the run as the flags say;
//   - a one-shot's 0 that a service not yet started at the exit waits on
//     ends nothing, before or after the app started (compose's up starts
//     that service on it); its crash after the app started ends the run;
//   - an app started after the exit (compose's up started it on that exit,
//     before keploy read it) is the exit before the app; the app's first
//     start in the run is kept, so keploy's start of it again later, or a
//     daemon that no longer answers for the app's container, reads no exit
//     since as one before the app;
//   - only the app's project's containers say whether a dependent waits:
//     another project's container of it, only created, does not.
//
// Record and test, both KEPLOY_PROVEN modes.
func TestAnExitIsDecidedByTheDaemonsInstantsAndADependencysCrashRetriesTheBringUp(t *testing.T) {
	const exit, failure, codeFrom = "docker compose up --abort-on-container-exit", "docker compose up --abort-on-container-failure", "docker compose up --exit-code-from runner"
	type want int
	const (
		ends want = iota
		retries
		nothing
	)
	t0 := time.Now().Add(-time.Hour)
	before, at, after := t0.Add(-time.Second), t0, t0.Add(time.Second)
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			for _, c := range []struct {
				name, cmd, service string
				code               int
				// app is the app's container: nil, only created.
				app *time.Time
				// keptStart is the app's first start keploy kept; zero: none.
				keptStart time.Time
				setup     func(d *abortDocker)
				want      want
				// waiting is the service said to wait on the exit.
				waiting string
				// noInstant: the daemon gives no instant of the exit (no
				// FinishedAt, no time on its die event).
				noInstant bool
			}{
				{name: "a dependency in the list form crashes", cmd: codeFrom, service: "flaky", code: 1, want: retries},
				{name: "a service_started dependency crashes", cmd: exit, service: "base", code: 2, want: retries},
				{name: "a dependency's dependency crashes", cmd: failure, service: "deep", code: 1, want: retries},
				{name: "a restart: true dependency crashes", cmd: exit, service: "again", code: 137, want: retries},
				{name: "an optional dependency the app waits on crashes", cmd: exit, service: "optcache", code: 1, want: retries},
				{name: "a dependency crashes after the app started", cmd: codeFrom, service: "flaky", code: 1, app: &before, want: ends},
				{name: "a dependency crashes, and compose's up started the app after it", cmd: codeFrom, service: "flaky", code: 1, app: &after, want: retries},
				{name: "a dependency crashes after the app's first start, read after keploy started it again", cmd: codeFrom, service: "flaky", code: 1,
					app: &after, keptStart: before, want: ends},
				{name: "a dependency crashes after the app's first start, read when the daemon no longer answers for the app's container", cmd: codeFrom,
					service: "flaky", code: 1, app: &before, keptStart: before, want: ends, setup: func(d *abortDocker) { d.unreadable = "app-id" }},
				{name: "a dependency exits 0", cmd: exit, service: "flaky", code: 0, want: ends},
				{name: "a service nothing depends on exits 3", cmd: "docker compose --profile early up --exit-code-from early", service: "early", code: 3, want: ends},
				{name: "an optional dependency another service waits on crashes", cmd: exit, service: "opt", code: 1, want: ends,
					setup: func(d *abortDocker) { d.add("api-id", "api", container.State{Status: "created"}) }},
				{name: "an optional dependency another service waits on to be healthy exits 0 after the app started", cmd: exit, service: "opt", code: 0, app: &before,
					want: ends, setup: func(d *abortDocker) { d.add("api-id", "api", container.State{Status: "created"}) }},
				{name: "an optional one-shot a service not yet started waits on completes after the app started", cmd: exit, service: "optmig", code: 0, app: &before,
					want: nothing, waiting: "api", setup: func(d *abortDocker) { d.add("api-id", "api", container.State{Status: "created"}) }},
				{name: "a dependency crashes after the app started, and the daemon gives no instant of the exit", cmd: codeFrom, service: "flaky", code: 1, app: &before,
					noInstant: true, want: ends},
				{name: "a migration the app waits on completes, and compose's up started the app on it", cmd: exit, service: "migrate", code: 0, app: &after,
					want: nothing, waiting: "app"},
				{name: "a one-shot a service not yet started waits on completes after the app started", cmd: exit, service: "seed", code: 0, app: &before,
					want: nothing, waiting: "runner",
					setup: func(d *abortDocker) { d.add("runner-id", "runner", container.State{Status: "created"}) }},
				{name: "a one-shot completes after the app started, and compose's up started its waiter on it", cmd: exit, service: "seed", code: 0, app: &before,
					want: nothing, waiting: "runner",
					setup: func(d *abortDocker) { d.add("runner-id", "runner", runningState(after)) }},
				{name: "a one-shot whose waiter had started before it exits 0", cmd: exit, service: "seed", code: 0, app: &before, want: ends,
					setup: func(d *abortDocker) { d.add("runner-id", "runner", runningState(before)) }},
				{name: "a one-shot a service waits on fails after the app started", cmd: failure, service: "seed", code: 1, app: &before, want: ends,
					setup: func(d *abortDocker) { d.add("runner-id", "runner", container.State{Status: "created"}) }},
				{name: "a one-shot whose waiter had started completes beside another project's waiter only created", cmd: exit, service: "seed", code: 0, app: &before,
					want: ends,
					setup: func(d *abortDocker) {
						d.add("runner-id", "runner", runningState(before))
						d.add("runner-other", "runner", container.State{Status: "created"}).labels[composeProjectLabelKey] = "other"
					}},
			} {
				what := fmt.Sprintf("%s=%q %s: %s", provenswitch.Env, proven, mode, c.name)
				d := newAbortDocker(before)
				d.containers["app-id"].state = container.State{Status: "created"}
				if c.app != nil {
					d.containers["app-id"].state = runningState(*c.app)
				}
				exited := d.add(c.service+"-id", c.service, stateAt(before.Add(-time.Second), true, c.code, at))
				if c.noInstant {
					exited.state.FinishedAt = ""
				}
				if c.setup != nil {
					c.setup(d)
				}
				a := newAbortApp(d, c.cmd, composeDoc(t, depCrashDoc))
				a.Mode = mode
				if !c.keptStart.IsZero() {
					a.composeApp.appStartedAt.Store(c.keptStart.UnixNano())
				}
				core, logs := observer.New(zap.InfoLevel)
				a.logger = zap.New(core)
				// The event's own time is keploy's read, long after the exit:
				// the daemon's FinishedAt decides.
				m := serviceDie(c.service+"-id", c.service, "ignored: the daemon's state decides", nil)
				if c.noInstant {
					m.TimeNano = 0
				}
				ended := a.composeDied(context.Background(), m)
				h, dc := a.composeAppHalt(), a.composeApp.depCrash.Load()
				retried := logs.FilterMessageSnippet("crashed before the application started: keploy stops the stack").All()
				waited := logs.FilterMessageSnippet("docker compose up waits on that exit").All()
				switch c.want {
				case retries:
					if ended || h != nil || dc == nil || dc.service != c.service || dc.serviceCode != c.code || !dc.beforeApp || len(retried) != 1 ||
						!slices.Contains(d.stops, "agent-id") || !a.composeApp.stopping.Load() || !a.composeApp.agentStopping.Load() {
						t.Fatalf("%s: ended %v, halt %+v, crash %+v, stops %v, said %v; want no halt, the crash kept for the retry and the stack stopped",
							what, ended, h, dc, d.stops, logs.All())
					}
				case ends:
					if !ended || h == nil || h.service != c.service || h.serviceCode != c.code || dc != nil || len(retried) != 0 || len(waited) != 0 {
						t.Fatalf("%s: ended %v, halt %+v, crash %+v, said %v; want the exit's halt", what, ended, h, dc, logs.All())
					}
					first := c.keptStart
					if first.IsZero() && c.app != nil {
						first = *c.app
					}
					if wantBefore := first.IsZero() || first.After(at); h.beforeApp != wantBefore {
						t.Fatalf("%s: halt %+v; want it said before the app started: %v", what, h, wantBefore)
					}
				case nothing:
					if ended || h != nil || dc != nil || len(d.stops) != 0 || len(waited) != 1 || waited[0].ContextMap()["waitingService"] != c.waiting {
						t.Fatalf("%s: ended %v, halt %+v, crash %+v, stops %v, said %v; want nothing ended, %s said to wait on it",
							what, ended, h, dc, d.stops, logs.All(), c.waiting)
					}
				}
			}
		}
	}
}

// keploy's stop of the stack for a dependency's crash keeps the runs it takes
// down, so their exits, read once the run's retry of the bring-up has begun
// (keploy no longer holds the stack stopped), are keploy's own and end
// nothing: an attached service's (`--exit-code-from runner`, a runner killed
// at its stop grace's end, 137) and the app's (compose's up started it just
// after the crash) alike. A start the retried up gives a container is another
// run, whose crash ends the run as the flags say. Record and test, both
// KEPLOY_PROVEN modes.
func TestTheExitsOfKeploysStopOfTheStackStayItsOwnOnceTheRetryBegan(t *testing.T) {
	doc := `
services:
  app:
    image: app
    depends_on: [flaky]
  runner:
    image: runner
  flaky:
    image: flaky
`
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			what := fmt.Sprintf("%s=%q %s", provenswitch.Env, proven, mode)
			ctx := context.Background()
			t0 := time.Now().Add(-time.Hour)
			d := newAbortDocker(t0)
			d.containers["app-id"].state = runningState(t0.Add(2 * time.Second))
			d.add("runner-id", "runner", runningState(t0)).stopCode = 137
			d.add("flaky-id", "flaky", stateAt(t0, true, 1, t0.Add(time.Second)))
			a := newAbortApp(d, "docker compose up --exit-code-from runner", composeDoc(t, doc))
			a.Mode = mode
			if a.composeDied(ctx, serviceDie("flaky-id", "flaky", "1", nil)) || a.composeAppHalt() != nil || a.composeApp.depCrash.Load() == nil {
				t.Fatalf("%s: flaky's crash: halt %+v, crash %+v; want no halt and the crash kept for the retry", what, a.composeAppHalt(), a.composeApp.depCrash.Load())
			}
			if !slices.Contains(d.stops, "runner-id") || !slices.Contains(d.stops, "app-id") {
				t.Fatalf("%s: stops %v; want keploy's stop of the stack to take the runner and the app down", what, d.stops)
			}
			if !a.composeBeginsABringUpAgain() {
				t.Fatalf("%s: the retry's bring-up did not begin", what)
			}
			if a.composeDied(ctx, serviceDie("runner-id", "runner", "137", nil)) || a.composeAppHalt() != nil {
				t.Fatalf("%s: the runner's 137 from keploy's stop, read after the retry began, ended the run (halt %+v); want keploy's own stop", what, a.composeAppHalt())
			}
			if a.composeDied(ctx, serviceDie("app-id", "app", "143", nil)) || a.composeAppHalt() != nil {
				t.Fatalf("%s: the app's exit from keploy's stop, read after the retry began, ended the run (halt %+v); want keploy's own stop", what, a.composeAppHalt())
			}
			d.mu.Lock()
			d.containers["runner-id"].state = stateAt(t0.Add(time.Minute), true, 3, t0.Add(2*time.Minute))
			d.mu.Unlock()
			if !a.composeDied(ctx, serviceDie("runner-id", "runner", "3", nil)) {
				t.Fatalf("%s: the crash of the runner's run the retried up started ended nothing; want the run ended on it", what)
			}
			if h := a.composeAppHalt(); h == nil || h.service != "runner" || h.serviceCode != 3 {
				t.Fatalf("%s: halt %+v; want the runner's crash, code 3", what, h)
			}
		}
	}
}

// keploy's stop of the stack stops what compose's up starts after the stop's
// listing, until the up returns, as compose's own abort stops a late starter:
// a service whose service_completed_successfully wait a stopped one-shot's
// exit met started after the listing, and the up, waiting on it, never
// returned, so the run hung. The late starter's run is one keploy's stop took
// down, so its exit, keploy not holding the stack stopped, ends nothing. Once
// the up has returned, nothing more is stopped. Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackStopsALateStarterUntilTheUpReturns(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newAbortDocker(time.Now().Add(-time.Minute))
			a := newAbortApp(d, "docker compose up --abort-on-container-exit", nil)
			returned := a.composeUpRunning()
			defer returned()
			if !a.stopComposeProject(context.Background()) {
				t.Fatal("the stack's stop could not tell the project")
			}
			if !slices.Contains(d.stops, "agent-id") || !slices.Contains(d.stops, "app-id") {
				t.Fatalf("stops %v; want the running app and agent stopped", d.stops)
			}
			var stream chan events.Message
			select {
			case stream = <-d.streams:
			case <-time.After(time.Minute):
				t.Fatal("keploy's stop did not watch the project's starts")
			}
			d.add("runner-id", "runner", runningState(time.Now()))
			stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: "runner-id", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "runner"}}}
			eventually(t, "the late starter is stopped", func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return slices.Contains(d.stops, "runner-id")
			})
			if a.composeDied(context.Background(), serviceDie("runner-id", "runner", "143", nil)) || a.composeAppHalt() != nil {
				t.Fatalf("the late starter's exit from keploy's stop ended the run (halt %+v); want keploy's own stop", a.composeAppHalt())
			}
			returned()
			d.add("late-id", "late", runningState(time.Now()))
			select {
			case stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: "late-id", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "late"}}}:
			default:
			}
			time.Sleep(200 * time.Millisecond)
			d.mu.Lock()
			defer d.mu.Unlock()
			if slices.Contains(d.stops, "late-id") {
				t.Fatalf("stops %v; want nothing stopped once the up has returned", d.stops)
			}
		})
	}
}

// TestALateStarterKeployCannotStopIsSaidAtDebugUnlessItIsGone: keploy's stop
// of a late starter the daemon refuses is said at Debug, naming the
// container; one already gone (the daemon's NotFound) and one stopped say
// nothing.
func TestALateStarterKeployCannotStopIsSaidAtDebugUnlessItIsGone(t *testing.T) {
	const said = "could not stop a late-started container of the application's compose project"
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newAbortDocker(time.Now().Add(-time.Minute))
			a := newAbortApp(d, "docker compose up --abort-on-container-exit", nil)
			core, logs := observer.New(zap.DebugLevel)
			a.logger = zap.New(core)
			returned := a.composeUpRunning()
			defer returned()
			if !a.stopComposeProject(context.Background()) {
				t.Fatal("the stack's stop could not tell the project")
			}
			var stream chan events.Message
			select {
			case stream = <-d.streams:
			case <-time.After(time.Minute):
				t.Fatal("keploy's stop did not watch the project's starts")
			}
			d.add("refused-id", "refused", runningState(time.Now()))
			d.add("runner-id", "runner", runningState(time.Now()))
			d.mu.Lock()
			d.stopFails = map[string]error{"refused-id": errors.New("the daemon refuses the stop")}
			d.mu.Unlock()
			// gone-id started and was removed before keploy's stop reached it.
			for _, s := range []struct{ id, service string }{{"gone-id", "gone"}, {"refused-id", "refused"}, {"runner-id", "runner"}} {
				stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
					Actor: events.Actor{ID: s.id, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: s.service}}}
			}
			eventually(t, "keploy asked to stop each late starter", func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return slices.Contains(d.stopAsks, "gone-id") && slices.Contains(d.stopAsks, "refused-id") && slices.Contains(d.stopAsks, "runner-id")
			})
			eventually(t, "the refused stop is said at Debug", func() bool {
				return logs.FilterMessage(said).FilterField(zap.String("container", "refused-id")).Len() == 1
			})
			time.Sleep(200 * time.Millisecond)
			for _, e := range logs.FilterMessage(said).All() {
				if c := e.ContextMap()["container"]; c != "refused-id" {
					t.Fatalf("keploy said %q for %v; want it said only for the stop the daemon refused", said, c)
				}
			}
			if e := logs.FilterMessage(said).All()[0]; e.Level != zap.DebugLevel {
				t.Fatalf("the refused stop is said at %v; want Debug", e.Level)
			}
		})
	}
}
