package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// stackDaemon is an abortDaemon whose app and agent containers keploy can
// stop as the daemon does: a kill or a stop leaves a container exited, and
// is written, in order, to the stand-in docker CLI's log (log), so what
// keploy did on the daemon and what it ran through the CLI read as one
// trail. The up waits for the file upEnds (stubDockerCLI): upReturns says,
// after each stop, whether compose's up would have returned (every container
// it attaches to is down), and the daemon then makes the file. The watch's
// event streams are the test's to send on (streams).
type stackDaemon struct {
	*abortDaemon
	log, upEnds string
	upReturns   func(d *stackDaemon) bool
	streams     chan chan events.Message
	// started is when the containers' current runs began.
	started time.Time
	// stopCode is the code a running container exits with on a kill or a
	// stop (mu): its main process's answer to the signal, 0 for one that
	// handles it, 128 plus the signal's number for one the signal ends.
	stopCode int
	// stopHeld is, by container name, what a stop of it waits for before the
	// container exits (mu): its main process outlasting the stop's grace.
	stopHeld map[string]chan struct{}
}

const (
	runAppName   = "app-1"
	runAgentName = "keploy-v3-test"
)

func (d *stackDaemon) note(line string) {
	f, err := os.OpenFile(d.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

// running reports whether container name runs.
func (d *stackDaemon) running(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.cs[name]
	return ok && c.status == "running"
}

// exit leaves container name exited with code, and ends the up when it
// would have returned.
func (d *stackDaemon) exit(name string, code int, why string) error {
	d.mu.Lock()
	c, ok := d.cs[name]
	if !ok {
		d.mu.Unlock()
		return errdefs.NotFound(fmt.Errorf("No such container: %s", name))
	}
	if c.status != "running" {
		d.mu.Unlock()
		return errdefs.Conflict(errors.New("container is not running"))
	}
	c.status, c.exitCode = "exited", code
	d.mu.Unlock()
	d.note(why + " " + name)
	if d.upReturns != nil && d.upReturns(d) {
		_ = os.WriteFile(d.upEnds, nil, 0o600)
	}
	return nil
}

func (d *stackDaemon) ContainerKill(_ context.Context, ref, signal string) error {
	d.mu.Lock()
	code := d.stopCode
	d.mu.Unlock()
	return d.exit(ref, code, "KILL-"+signal)
}

// ContainerStop of a container that is not running is the daemon's no-op;
// a running one exits with stopCode.
func (d *stackDaemon) ContainerStop(_ context.Context, ref string, _ container.StopOptions) error {
	if !d.running(ref) {
		return nil
	}
	d.mu.Lock()
	code, held := d.stopCode, d.stopHeld[ref]
	d.mu.Unlock()
	if held != nil {
		<-held
	}
	return d.exit(ref, code, "STOP")
}

// ContainerWait answers once the container is not running (the only
// condition keploy asks for), or is gone.
func (d *stackDaemon) ContainerWait(ctx context.Context, ref string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	said, errs := make(chan container.WaitResponse, 1), make(chan error, 1)
	go func() {
		for {
			d.mu.Lock()
			c, ok := d.cs[ref]
			status, code := "", 0
			if ok {
				status, code = c.status, c.exitCode
			}
			d.mu.Unlock()
			switch {
			case !ok:
				errs <- errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
				return
			case status != "running":
				said <- container.WaitResponse{StatusCode: int64(code)}
				return
			}
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return said, errs
}

// ContainerInspect says when the container's run began, and, exited, when it
// ended.
func (d *stackDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	info, err := d.abortDaemon.ContainerInspect(ctx, ref)
	if err != nil {
		return info, err
	}
	st := *info.State
	if st.Status == "created" {
		// Made, never started: no start, no end.
		info.State = &st
		return info, nil
	}
	st.StartedAt = d.started.UTC().Format(time.RFC3339Nano)
	if !st.Running {
		st.Status = "exited"
		st.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	info.State = &st
	return info, nil
}

func (d *stackDaemon) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs := make(chan events.Message, 4)
	d.streams <- msgs
	return msgs, make(chan error)
}

// stackRun is run() of a compose app (service "app", container app-1) and
// keploy's agent (keploy-v3-test), through the stand-in docker CLI, its up
// waiting for d.upEnds. ran gives run()'s result once it returns.
type stackRun struct {
	a      *App
	d      *stackDaemon
	ran    chan models.AppError
	cancel context.CancelFunc
}

// startStackRun starts run() with the switch as proven says, on the App
// prep has set as an earlier run of it left it, and returns
// once its up has started and compose has made the app's and the agent's
// containers. The test's cleanup ends the up and the run.
func startStackRun(t *testing.T, proven string, upReturns func(d *stackDaemon) bool, prep ...func(a *App)) *stackRun {
	t.Helper()
	t.Setenv(provenswitch.Env, proven)
	log := stubDockerCLI(t, "", 0)
	ends := filepath.Join(t.TempDir(), "up-ends")
	t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", ends)
	d := &stackDaemon{abortDaemon: newAbortDaemon("proj"), log: log, upEnds: ends, upReturns: upReturns,
		streams: make(chan chan events.Message, 16), started: time.Now().Add(-time.Second)}
	a := &App{
		logger:          zap.NewNop(),
		docker:          d,
		kind:            utils.DockerCompose,
		cmd:             utils.NewAppCommand("docker compose -f /tmp/keploy-compose.yaml up"),
		composeFile:     "/tmp/keploy-compose.yaml",
		keployContainer: runAgentName,
		container:       runAppName,
		composeService:  "app",
		composeServices: []string{"app", keployAgentComposeService},
		opts:            models.SetupOptions{Mode: models.MODE_TEST},
	}
	for _, p := range prep {
		p(a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &stackRun{a: a, d: d, ran: make(chan models.AppError, 1), cancel: cancel}
	go func() { r.ran <- a.run(ctx) }()
	t.Cleanup(func() {
		_ = os.WriteFile(ends, nil, 0o600)
		cancel()
		select {
		case appErr := <-r.ran:
			r.ran <- appErr
		case <-time.After(time.Minute):
			t.Errorf("run() did not return within a minute of its context's end")
		}
	})
	// compose makes the containers once the up runs: keploy's checks before
	// the up (a prior agent's, its name's) find none.
	eventually(t, "the up starts", func() bool { return strings.Contains(recordedLog(t, log), "ARG up\n") })
	d.add(&abortContainer{name: runAppName, service: "app", status: "running"},
		&abortContainer{name: runAgentName, service: keployAgentComposeService, status: "running"})
	return r
}

// result is run()'s result within a minute, or the test fails.
func (r *stackRun) result(t *testing.T, what string) models.AppError {
	t.Helper()
	select {
	case appErr := <-r.ran:
		r.ran <- appErr
		return appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute of %s", what)
	}
	return models.AppError{}
}

// trail is the CLI's and the daemon's log, one line each.
func (r *stackRun) trail(t *testing.T) []string {
	return strings.Split(strings.TrimSpace(recordedLog(t, r.d.log)), "\n")
}

// trailAt is the index of the first line of trail that starts with prefix, -1
// when none does.
func trailAt(trail []string, prefix string) int {
	for i, l := range trail {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// The user's stop of a compose run (Ctrl+C), or keploy's teardown, cancels
// the run: keploy interrupts the application's container itself, so the app
// stops gracefully (its own shutdown, its coverage flush), and only then
// takes the stack down (docker compose down), with the switch on and off.
func TestACancelledComposeRunStopsTheAppBeforeTheDownInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, nil)
			r.cancel()
			if e := r.result(t, "its cancel"); e.AppErrorType != models.ErrCtxCanceled {
				t.Fatalf("run() ended %+v; want the cancel's end", e)
			}
			trail := r.trail(t)
			stop, down := trailAt(trail, "KILL-SIGINT "+runAppName), trailAt(trail, "DOWN-STARTED")
			if stop < 0 || down < 0 || stop > down {
				t.Fatalf("the app's interrupt at %d, the down at %d; want the interrupt first:\n%s", stop, down, strings.Join(trail, "\n"))
			}
		})
	}
}

// An up attached to the app alone (`docker compose up app`) returns as soon
// as keploy's own stop of the app (a set's end) has stopped it. With the
// switch off as on, the run holds until keploy's teardown, with the stack,
// keploy's agent in it, up: the set's end is read from the agent after the
// stop. Taken down then, the read found no agent.
func TestAnUpThatKeploysStopReturnedIsHeldUntilTheTeardownInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, func(d *stackDaemon) bool { return !d.running(runAppName) })
			if _, err := r.a.StopComposeApp(context.Background()); err != nil {
				t.Fatalf("keploy's stop of the app: %v", err)
			}
			eventually(t, "the up returns", func() bool { return strings.Contains(recordedLog(t, r.d.log), "UP-DONE") })
			// Held: no end of the run, no down, the agent up, for as long
			// as the teardown has not come.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case e := <-r.ran:
					r.ran <- e
					t.Fatalf("run() ended %+v when its up returned on keploy's stop; want it held until the teardown", e)
				case <-time.After(50 * time.Millisecond):
				}
				if trailAt(r.trail(t), "DOWN-STARTED") >= 0 || !r.d.running(runAgentName) {
					t.Fatalf("the stack was taken down before the teardown:\n%s", strings.Join(r.trail(t), "\n"))
				}
			}
			r.cancel()
			if e := r.result(t, "the teardown"); e.AppErrorType != models.ErrCtxCanceled {
				t.Fatalf("run() ended %+v; want the teardown's end", e)
			}
			if trailAt(r.trail(t), "DOWN-STARTED") < 0 {
				t.Fatalf("the teardown did not take the stack down:\n%s", strings.Join(r.trail(t), "\n"))
			}
		})
	}
}

// With the switch off as on, keploy's watch ends the run on the app's own
// exit (compose's abort flags left out, compose's up would keep the project
// up while keploy's agent runs), stops the project, and the run ends with
// the app's exit code.
func TestTheAppsOwnExitEndsAComposeRunWithItsCodeInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, func(d *stackDaemon) bool { return !d.running(runAppName) && !d.running(runAgentName) })
			stream := r.watchStream(t)
			if err := r.d.exit(runAppName, 3, "EXIT"); err != nil {
				t.Fatal(err)
			}
			stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: runAppName, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", "exitCode": "3"}}}
			e := r.result(t, "the app's exit")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) {
				t.Fatalf("run() ended %+v; want the app's halt, code 3", e)
			}
			if trail := r.trail(t); trailAt(trail, "STOP "+runAgentName) < 0 {
				t.Fatalf("the halt's end did not stop the project:\n%s", strings.Join(trail, "\n"))
			}
		})
	}
}

// An exit of `up --exit-code-from early`'s service before the app's container
// has started (early exits 3 at once) ends the run with 3, record and test, in
// both modes: keploy stops the stack, its agent with it, which fails the up
// (the app waited on the agent's health), and the run takes no retry of that
// up. Left to compose's up, which runs without the flag, the run never ended.
func TestAnExitBeforeTheAppStartedEndsAComposeRunWithItsCodeInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			t.Run("switch="+proven+"/"+string(mode), func(t *testing.T) {
				t.Setenv("KEPLOY_STUB_UP_CODE", "1")
				r := startStackRun(t, proven, func(d *stackDaemon) bool { return !d.running(runAgentName) }, func(a *App) {
					a.Mode, a.opts.Mode = mode, mode
					a.setComposeUserAbort("docker compose --profile early up --exit-code-from early", nil)
				})
				r.d.mu.Lock()
				r.d.cs[runAppName].status = "created"
				r.d.mu.Unlock()
				r.d.add(&abortContainer{name: "early-1", service: "early", status: "running"})
				stream := r.watchStream(t)
				if err := r.d.exit("early-1", 3, "EXIT"); err != nil {
					t.Fatal(err)
				}
				stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
					Actor: events.Actor{ID: "early-1", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "early", "exitCode": "3"}}}
				e := r.result(t, "early's exit")
				if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) ||
					!strings.Contains(e.Err.Error(), `compose service "early" exited with code 3 before the application started`) {
					t.Fatalf("run() ended %+v; want early's halt, code 3, before the application started", e)
				}
				trail := r.trail(t)
				if trailAt(trail, "STOP "+runAgentName) < 0 || trailAt(trail, "UP-RETRIED") >= 0 {
					t.Fatalf("want the agent stopped and the up not retried:\n%s", strings.Join(trail, "\n"))
				}
			})
		}
	}
}

// `up --exit-code-from runner` and the app's own crash (3) after it started:
// the run ends with the app's 3, not the code keploy's stop of the stack gave
// the runner, in both modes.
func TestTheAppsOwnExitUnderExitCodeFromKeepsItsCodeInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, func(d *stackDaemon) bool {
				return !d.running(runAppName) && !d.running(runAgentName) && !d.running("runner-1")
			},
				func(a *App) { a.setComposeUserAbort("docker compose up --exit-code-from runner", nil) })
			r.d.add(&abortContainer{name: "runner-1", service: "runner", status: "running"})
			stream := r.watchStream(t)
			if err := r.d.exit(runAppName, 3, "EXIT"); err != nil {
				t.Fatal(err)
			}
			stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
				Actor: events.Actor{ID: runAppName, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", "exitCode": "3"}}}
			e := r.result(t, "the app's exit")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) {
				t.Fatalf("run() ended %+v; want the app's halt, code 3", e)
			}
			if trail := r.trail(t); trailAt(trail, "STOP runner-1") < 0 {
				t.Fatalf("the halt's end did not stop the runner:\n%s", strings.Join(trail, "\n"))
			}
		})
	}
}

// An attached service that fails because keploy stopped the app at a set's
// end (`up --abort-on-container-failure`, a worker that exits 1 once the app
// is down) is keploy's own stop's exit: no halt, the run held, and keploy says
// so once, for the failure alone (a clean exit in the hold says nothing, and
// nor does the app's own exit in keploy's stop, 130 from an app keploy's
// interrupt ends: it is never one the user's flags read). Once keploy has
// started the app again, the worker's own crash still halts and ends the run
// with its code. In both modes.
func TestAnExitWhileKeployHoldsTheAppStoppedIsKeploysOwnInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			r := startStackRun(t, proven, func(d *stackDaemon) bool {
				return !d.running(runAppName) && !d.running(runAgentName) && !d.running("worker-1")
			}, func(a *App) {
				a.logger = zap.New(core)
				a.setComposeUserAbort("docker compose up --abort-on-container-failure", nil)
			})
			r.d.add(&abortContainer{name: "worker-1", service: "worker", status: "running"},
				&abortContainer{name: "tidy-1", service: "tidy", status: "running"})
			r.d.mu.Lock()
			r.d.stopCode = 130
			r.d.mu.Unlock()
			stream := r.watchStream(t)
			die := func(name, service string, code int) {
				stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
					Actor: events.Actor{ID: name, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: service, "exitCode": fmt.Sprint(code)}}}
			}
			if _, err := r.a.StopComposeApp(context.Background()); err != nil {
				t.Fatalf("keploy's stop of the app: %v", err)
			}
			die(runAppName, "app", 130)
			for _, c := range []struct {
				name, service string
				code          int
			}{{"tidy-1", "tidy", 0}, {"worker-1", "worker", 1}} {
				if err := r.d.exit(c.name, c.code, "EXIT"); err != nil {
					t.Fatal(err)
				}
				die(c.name, c.service, c.code)
			}
			own := func() []observer.LoggedEntry {
				return logs.FilterMessageSnippet("exited while keploy held the application stopped").All()
			}
			eventually(t, "keploy decides the worker's exit", func() bool { return len(own()) > 0 || r.a.composeAppHalt() != nil })
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				select {
				case e := <-r.ran:
					r.ran <- e
					t.Fatalf("run() ended %+v on an exit keploy's stop of the app gave; want it held", e)
				case <-time.After(50 * time.Millisecond):
				}
				if h := r.a.composeAppHalt(); h != nil {
					t.Fatalf("an exit keploy's stop of the app gave was kept as a halt: %+v", h)
				}
			}
			if said := own(); len(said) != 1 || said[0].ContextMap()["service"] != "worker" || said[0].ContextMap()["exitCode"] != int64(1) {
				t.Fatalf("said %v; want once, of the worker's exit 1 (the tidy exit 0 ends nothing, and the app's 130 in keploy's stop is not the flags')", said)
			}
			if _, err := r.a.StartComposeApp(context.Background(), noNotice); err != nil {
				t.Fatalf("keploy's start of the app: %v", err)
			}
			r.d.mu.Lock()
			r.d.cs["worker-1"].status = "running"
			r.d.mu.Unlock()
			if err := r.d.exit("worker-1", 2, "CRASH"); err != nil {
				t.Fatal(err)
			}
			die("worker-1", "worker", 2)
			e := r.result(t, "the worker's crash")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 2 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) ||
				!strings.Contains(e.Err.Error(), `compose service "worker" exited with code 2 after the application started`) {
				t.Fatalf("run() ended %+v; want the worker's halt, code 2, after the application started", e)
			}
			if len(own()) != 1 {
				t.Fatalf("said %v; want the crash outside the hold not taken for keploy's own", own())
			}
		})
	}
}

// ContainerStart starts container ref's run again.
func (d *stackDaemon) ContainerStart(_ context.Context, ref string, _ container.StartOptions) error {
	d.mu.Lock()
	c, ok := d.cs[ref]
	if ok {
		c.status, c.exitCode = "running", 0
	}
	d.mu.Unlock()
	if !ok {
		return errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	d.note("START " + ref)
	return nil
}

// watchStream is the stream of the run's watch of the app's container.
func (r *stackRun) watchStream(t *testing.T) chan events.Message {
	t.Helper()
	select {
	case stream := <-r.d.streams:
		return stream
	case <-time.After(time.Minute):
		t.Fatal("the run did not watch the app's container")
	}
	return nil
}

// runAgentDie is the die of keploy's agent's container with code.
func runAgentDie(code string) events.Message {
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: runAgentName, Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: keployAgentComposeService, "exitCode": code}}}
}

// keploy's own take-down of the stack while the app's run is live (a
// setup's failure: ComposeDownOnSetupFailure) stops keploy's agent: its exit
// in the down is keploy's own, not a halt, with the switch off and on.
func TestTheAgentsExitInKeploysDownOfALiveRunIsNoHaltInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_STUB_DOWN_TENTHS", "30")
			r := startStackRun(t, proven, nil)
			stream := r.watchStream(t)
			downed := make(chan struct{})
			go func() {
				defer close(downed)
				r.a.ComposeDown()
			}()
			eventually(t, "the down starts", func() bool { return trailAt(r.trail(t), "DOWN-STARTED") >= 0 })
			if err := r.d.exit(runAgentName, 0, "DOWN-STOPS"); err != nil {
				t.Fatal(err)
			}
			stream <- runAgentDie("0")
			deadline := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if h := r.a.composeAppHalt(); h != nil {
					t.Fatalf("the agent's exit in keploy's own down was taken for a halt: %+v", h)
				}
				time.Sleep(20 * time.Millisecond)
			}
			<-downed
		})
	}
}

// A run takes an exit of keploy's agent it did not ask for as a halt that
// names the agent, whatever an earlier run's take-down of the stack (the
// replay's between sets) marked: each run starts with no stop of the agent
// keploy's own.
func TestARunTakesTheAgentsExitAfterAnEarlierRunsDownInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			r := startStackRun(t, proven, func(d *stackDaemon) bool { return !d.running(runAppName) && !d.running(runAgentName) },
				func(a *App) { a.composeApp.agentStopping.Store(true) })
			stream := r.watchStream(t)
			if err := r.d.exit(runAgentName, 2, "EXIT"); err != nil {
				t.Fatal(err)
			}
			stream <- runAgentDie("2")
			e := r.result(t, "the agent's exit")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 2 || e.Err == nil || !strings.Contains(e.Err.Error(), "keploy's agent container") {
				t.Fatalf("run() ended %+v; want the agent's halt, code 2", e)
			}
		})
	}
}

// `up --exit-code-from runner`, the app `depends_on: [flaky]`, and flaky
// crashes (1) on its first start, before the app started: keploy stops the
// stack, as compose's abort stopped it, keeps no halt, removes flaky's
// crashed container and retries the bring-up, as before compose ran without
// the flag; the run does not end on flaky's crash. Record and test, both
// modes.
func TestADependencysCrashBeforeTheAppStartedRetriesTheBringUpInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			t.Run("switch="+proven+"/"+string(mode), func(t *testing.T) {
				t.Setenv("KEPLOY_STUB_UP_CODE", "1")
				var logs *observer.ObservedLogs
				r := startStackRun(t, proven, func(d *stackDaemon) bool { return !d.running(runAgentName) }, func(a *App) {
					a.Mode, a.opts.Mode = mode, mode
					var core zapcore.Core
					core, logs = observer.New(zap.InfoLevel)
					a.logger = zap.New(core)
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
				e := r.result(t, "the retried bring-up")
				if errors.Is(e.Err, models.ErrAppExitedOnItsOwn) || e.ExitCode == 1 {
					t.Fatalf("run() ended %+v; want no end on flaky's crash", e)
				}
				trail := r.trail(t)
				stop, retried := trailAt(trail, "STOP "+runAgentName), trailAt(trail, "UP-RETRIED")
				if stop < 0 || retried < 0 || retried < stop {
					t.Fatalf("want the agent stopped, then the up retried:\n%s", strings.Join(trail, "\n"))
				}
				said := len(logs.FilterMessageSnippet("crashed before the application started: keploy stops the stack").All())
				if took := len(logs.FilterMessageSnippet("retrying the bring-up").All()); said != 1 || took != 1 {
					t.Fatalf("said the crash %d times and took %d retries; want one each:\n%v", said, took, logs.All())
				}
				r.d.mu.Lock()
				_, kept := r.d.cs["flaky-1"]
				r.d.mu.Unlock()
				if kept {
					t.Fatal("flaky's crashed container was kept; want it removed before the retry")
				}
			})
		}
	}
}
