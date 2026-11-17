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
	return d.exit(ref, 0, "KILL-"+signal)
}

// ContainerStop of a container that is not running is the daemon's no-op.
func (d *stackDaemon) ContainerStop(_ context.Context, ref string, _ container.StopOptions) error {
	if !d.running(ref) {
		return nil
	}
	return d.exit(ref, 0, "STOP")
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
