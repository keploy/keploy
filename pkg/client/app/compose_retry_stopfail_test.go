package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// stackListFailsDocker is abortDocker whose daemon will not list the running
// containers of the project (keploy's stop of the stack's listing), after
// askDaemon's asks; every other listing answers. onList, when set, runs at
// each listing it will not make.
type stackListFailsDocker struct {
	*abortDocker
	onList func()
}

func (d *stackListFailsDocker) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	if l := o.Filters.Get("label"); !o.All && len(l) == 1 && l[0] == composeProjectLabelKey+"=proj" {
		if d.onList != nil {
			d.onList()
		}
		return nil, errdefs.NotFound(errors.New("the daemon will not list them"))
	}
	return d.abortDocker.ContainerList(ctx, o)
}

// A CRASH KEPLOY COULD NOT STOP THE STACK ON ENDS THE RUN. A dependency the
// app depends on crashes before the app started, and keploy's stop of the
// stack for the retry cannot list the project: the crash is the run's halt,
// naming the failed stop and the next step, the run retries nothing, and its
// end takes the stack down (compose down, the stop failing again). keploy no
// longer held every exit for its own stop while compose's up ran on, a
// runner's exit ending nothing. Record and test, both KEPLOY_PROVEN modes.
func TestADependencysCrashKeployCannotStopTheStackOnEndsTheRun(t *testing.T) {
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
			d.add("runner-id", "runner", runningState(t0))
			d.add("flaky-id", "flaky", stateAt(t0, true, 1, t0.Add(time.Second)))
			a := newAbortApp(d, "docker compose up --exit-code-from runner", composeDoc(t, doc))
			a.docker = &stackListFailsDocker{abortDocker: d}
			core, logs := observer.New(zapcore.WarnLevel)
			a.logger = zap.New(core)
			a.Mode = mode
			returned := a.composeUpRunning()
			if !a.composeDied(ctx, serviceDie("flaky-id", "flaky", "1", nil)) {
				t.Fatalf("%s: flaky's crash, with keploy's stop of the stack unable to list the project, ended nothing; want the run ended on it", what)
			}
			h := a.composeAppHalt()
			if h == nil || h.service != "flaky" || h.serviceCode != 1 || !h.beforeApp || !h.stackNotStopped {
				t.Fatalf("%s: halt %+v; want flaky's crash before the app started, with the stack not stopped", what, h)
			}
			for _, want := range []string{`compose service "flaky"`, "could not stop the stack to retry the bring-up", "Next step: check that the docker daemon answers"} {
				if !strings.Contains(h.Error(), want) {
					t.Fatalf("%s: the halt says %q; want it to say %q", what, h.Error(), want)
				}
			}
			if n := logs.FilterMessageSnippet("could not stop the application's compose stack to retry its bring-up").Len(); n != 1 {
				t.Fatalf("%s: %d warnings that keploy could not stop the stack for the retry; want 1", what, n)
			}
			if a.composeRetriesTheBringUp(nil, 1, utils.CmdError{}, func() []composeServiceState { return nil }) {
				t.Fatalf("%s: the run retries the bring-up on a crash it ended on", what)
			}
			downs := 0
			a.composeHaltEnd(ctx, make(chan struct{}), func() { downs++ })()
			if downs != 1 {
				t.Fatalf("%s: %d compose downs at the run's end; want the stack taken down once, its stop failing again", what, downs)
			}
			returned()
			sctx, cancel := context.WithTimeout(ctx, time.Minute)
			if !a.composeAwaitsTheStackStop(sctx) {
				t.Fatalf("%s: keploy's stop of the stack was not said ended once the up returned", what)
			}
			cancel()
		}
	}
}

// Each run begins with nothing an earlier run kept: keploy's stop of the
// stack for a crash among it, which this run's retry would otherwise wait on
// for good when that stop never ended. Both KEPLOY_PROVEN modes.
func TestARunBeginsWithNothingAnEarlierRunKept(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		a := newAbortApp(newAbortDocker(time.Now()), "docker compose up --exit-code-from runner", nil)
		never := make(chan struct{})
		project := "proj"
		up := make(chan struct{})
		a.composeApp.halted.Store(&composeHalt{exitCode: 1})
		a.composeApp.stopping.Store(true)
		a.composeApp.agentStopping.Store(true)
		a.composeApp.appKilledAt.Store(1)
		a.composeApp.project.Store(&project)
		a.composeApp.appStartedAt.Store(1)
		a.composeApp.depCrash.Store(&composeHalt{service: "flaky"})
		a.composeApp.upReturned.Store(&up)
		a.composeApp.stackStop.Store(&never)
		a.composeBeginsARun()
		if a.composeApp.halted.Load() != nil || a.composeApp.stopping.Load() || a.composeApp.agentStopping.Load() ||
			a.composeApp.appKilledAt.Load() != 0 || a.composeApp.project.Load() != nil || a.composeApp.appStartedAt.Load() != 0 ||
			a.composeApp.depCrash.Load() != nil || a.composeApp.upReturned.Load() != nil || a.composeApp.stackStop.Load() != nil {
			t.Fatalf("%s=%q: a field an earlier run kept is still set after the run's begin", provenswitch.Env, proven)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if !a.composeAwaitsTheStackStop(ctx) {
			t.Fatalf("%s=%q: the run's retry waits on an earlier run's stop of the stack that never ended", provenswitch.Env, proven)
		}
		cancel()
	}
}

// A HALT KEPT BEFORE A FAILED STACK STOP STANDS. A dependency the app depends
// on crashes before the app started; while keploy's stop of the stack for
// the retry tries to list the project, another exit's halt is kept (here
// runner's 3, which the user's --exit-code-from names). The stop fails: the
// run ends on runner's halt, its service and code, not on the crash, and the
// warning says so. Record and test, both KEPLOY_PROVEN modes.
func TestAHaltKeptBeforeAFailedStackStopForARetryStands(t *testing.T) {
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
			d.add("runner-id", "runner", runningState(t0))
			d.add("flaky-id", "flaky", stateAt(t0, true, 1, t0.Add(time.Second)))
			a := newAbortApp(d, "docker compose up --exit-code-from runner", composeDoc(t, doc))
			runner := &composeHalt{exitCode: 3, service: "runner", serviceCode: 3, flags: "--exit-code-from runner"}
			a.docker = &stackListFailsDocker{abortDocker: d, onList: func() { a.composeApp.halted.CompareAndSwap(nil, runner) }}
			core, logs := observer.New(zapcore.WarnLevel)
			a.logger = zap.New(core)
			a.Mode = mode
			returned := a.composeUpRunning()
			if !a.composeDied(ctx, serviceDie("flaky-id", "flaky", "1", nil)) {
				t.Fatalf("%s: flaky's crash, with keploy's stop of the stack unable to list the project, ended nothing; want the run ended", what)
			}
			if h := a.composeAppHalt(); h != runner || h.service != "runner" || h.exitCode != 3 || h.stackNotStopped {
				t.Fatalf("%s: halt %+v; want runner's 3, kept before the stack's stop failed, to stand", what, h)
			}
			if n := logs.FilterMessageSnippet("the run ends on the halt kept before it").Len(); n != 1 {
				t.Fatalf("%s: %d warnings that the run ends on the halt kept before the failed stop; want 1", what, n)
			}
			if n := logs.FilterMessageSnippet("it ends the run on that crash").Len(); n != 0 {
				t.Fatalf("%s: %d warnings that the run ends on flaky's crash; want none, runner's halt stands", what, n)
			}
			returned()
			sctx, cancel := context.WithTimeout(ctx, time.Minute)
			if !a.composeAwaitsTheStackStop(sctx) {
				t.Fatalf("%s: keploy's stop of the stack was not said ended once the up returned", what)
			}
			cancel()
		}
	}
}

// The halt of a failed stack stop for a retry is kept before that stop is
// said ended: the run's retry, which waits for the stop, finds the halt the
// moment its wait ends, so it never takes the crash for one to retry. The up
// returns as the stop's listing fails, so the stop is said ended as soon as
// it can be. Record and test, both KEPLOY_PROVEN modes.
func TestTheHaltOfAFailedStackStopIsKeptBeforeTheStopIsSaidEnded(t *testing.T) {
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
			for i := 0; i < 20; i++ {
				ctx := context.Background()
				t0 := time.Now().Add(-time.Hour)
				d := newAbortDocker(t0)
				d.containers["app-id"].state = runningState(t0.Add(2 * time.Second))
				d.add("flaky-id", "flaky", stateAt(t0, true, 1, t0.Add(time.Second)))
				d.add("runner-id", "runner", runningState(t0))
				a := newAbortApp(d, "docker compose up --exit-code-from runner", composeDoc(t, doc))
				a.logger = zap.NewNop()
				a.Mode = mode
				returned := a.composeUpRunning()
				a.docker = &stackListFailsDocker{abortDocker: d, onList: returned}
				seen := make(chan *composeHalt, 1)
				go func() {
					for a.composeApp.stackStop.Load() == nil {
						time.Sleep(50 * time.Microsecond)
					}
					sctx, cancel := context.WithTimeout(ctx, time.Minute)
					defer cancel()
					if !a.composeAwaitsTheStackStop(sctx) {
						seen <- nil
						return
					}
					seen <- a.composeAppHalt()
				}()
				if !a.composeDied(ctx, serviceDie("flaky-id", "flaky", "1", nil)) {
					t.Fatalf("%s: flaky's crash, with keploy's stop of the stack unable to list the project, ended nothing", what)
				}
				if h := <-seen; h == nil || h.service != "flaky" || !h.stackNotStopped {
					t.Fatalf("%s: the retry's wait for the stack's stop ended with halt %+v; want flaky's crash kept already, the stack not stopped", what, h)
				}
			}
		}
	}
}
