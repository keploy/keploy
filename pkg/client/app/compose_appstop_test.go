package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// composeDocker is a docker daemon holding one app container: a kill with
// SIGINT stops it when exitsOnInt, a SIGKILL always does; a start runs it;
// events go out on the stream each Events call opens.
type composeDocker struct {
	docker.Client
	mu         sync.Mutex
	running    bool
	exitsOnInt bool
	signals    []string
	starts     int
	exitedCh   chan struct{}
	streams    chan chan events.Message
	errStreams chan chan error
	// inspected gets one value for each inspect, once it has answered.
	inspected chan struct{}
	// project, the app's compose project; others, its other running
	// containers; stopped, those a ContainerStop stopped, with the grace.
	project string
	others  []string
	stopped map[string]int
	// pid and id are the app container's main process and id, as the
	// daemon gives them (0 and "": its processes cannot be read).
	pid int
	id  string
	// exitCode is the stopped container's.
	exitCode int
	// intDelay, set: a SIGINT stops the container this long after it,
	// not at once (an app whose shutdown takes a while).
	intDelay time.Duration
}

func (d *composeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !opts.Filters.ExactMatch("label", "com.docker.compose.project="+d.project) {
		return nil, nil
	}
	var out []container.Summary
	for _, id := range d.others {
		if _, done := d.stopped[id]; !done {
			out = append(out, container.Summary{ID: id})
		}
	}
	return out, nil
}

func (d *composeDocker) ContainerStop(_ context.Context, id string, opts container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		d.stopped = map[string]int{}
	}
	d.stopped[id] = -1
	if opts.Timeout != nil {
		d.stopped[id] = *opts.Timeout
	}
	return nil
}

func newComposeDocker(running, exitsOnInt bool) *composeDocker {
	return &composeDocker{running: running, exitsOnInt: exitsOnInt, exitedCh: make(chan struct{}),
		streams: make(chan chan events.Message, 8), errStreams: make(chan chan error, 8), inspected: make(chan struct{}, 8)}
}

func (d *composeDocker) stop() {
	if d.running {
		d.running = false
		close(d.exitedCh)
	}
}

func (d *composeDocker) ContainerKill(_ context.Context, _ string, signal string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signals = append(d.signals, signal)
	if !d.running {
		return errdefs.Conflict(errors.New("container is not running"))
	}
	switch {
	case signal == "SIGINT" && d.intDelay > 0:
		go func() {
			time.Sleep(d.intDelay)
			d.mu.Lock()
			d.stop()
			d.mu.Unlock()
		}()
	case signal == "SIGKILL" || (signal == "SIGINT" && d.exitsOnInt):
		d.stop()
	}
	return nil
}

func (d *composeDocker) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	exited := d.exitedCh
	running := d.running
	d.mu.Unlock()
	out, errs := make(chan container.WaitResponse, 1), make(chan error, 1)
	if !running {
		out <- container.WaitResponse{}
		return out, errs
	}
	go func() {
		select {
		case <-exited:
			out <- container.WaitResponse{}
		case <-ctx.Done():
			errs <- ctx.Err()
		}
	}()
	return out, errs
}

func (d *composeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	if !d.running {
		d.running = true
		d.exitedCh = make(chan struct{})
	}
	return nil
}

func (d *composeDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	d.mu.Lock()
	running, project, pid, id, code := d.running, d.project, d.pid, d.id, d.exitCode
	d.mu.Unlock()
	defer func() { d.inspected <- struct{}{} }()
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, State: &container.State{Running: running, Pid: pid, ExitCode: code}},
		Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": project}},
	}, nil
}

func (d *composeDocker) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs, errs := make(chan events.Message, 4), make(chan error, 1)
	d.streams <- msgs
	d.errStreams <- errs
	return msgs, errs
}

func (d *composeDocker) state() (running bool, signals []string, starts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running, append([]string(nil), d.signals...), d.starts
}

func composeApp(d *composeDocker) *App {
	return &App{logger: zap.NewNop(), docker: d, container: "app-1", kind: "docker-compose"}
}

// stallBudget sets utils.StopStallBudget to d for the test.
func stallBudget(t *testing.T, d time.Duration) {
	t.Helper()
	budget := utils.StopStallBudget
	utils.StopStallBudget = d
	t.Cleanup(func() { utils.StopStallBudget = budget })
}

// keploy's stop of a compose app is SIGINT, then a wait for the container's
// stop on its events, then SIGKILL once nothing came for the stall bound
// (here shortened), which it says; a container that is not running is
// nothing to stop. Its exit is no halt until keploy starts it again. Here the
// daemon gives no process of the container to read (see the Linux tests for
// one whose processes keploy reads).
func TestTheComposeAppsStopInterruptsWaitsThenKills(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	for _, tc := range []struct {
		name                string
		running, exitsOnInt bool
		signals             string
		killed              bool
	}{
		{"it exits on the interrupt", true, true, "SIGINT", false},
		{"it does not", true, false, "SIGINT,SIGKILL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newComposeDocker(tc.running, tc.exitsOnInt)
			a := composeApp(d)
			killed, err := a.StopComposeApp(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			running, signals, _ := d.state()
			if running || strings.Join(signals, ",") != tc.signals || (killed != "") != tc.killed {
				t.Fatalf("running %v after signals %v, said %q; want stopped after %s, a kill said %v", running, signals, killed, tc.signals, tc.killed)
			}
			if tc.killed && !strings.Contains(killed, "keploy could not read its container's processes, and the docker daemon did not say it had stopped, for 300ms") {
				t.Fatalf("the kill is said as %q, want the bound named", killed)
			}
			if !a.composeApp.stopping.Load() {
				t.Fatal("after keploy's stop the app's exit would be taken for a halt")
			}
			if err := a.StartComposeApp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if running, _, starts := d.state(); !running || starts != 1 || a.composeApp.stopping.Load() {
				t.Fatalf("after the start: running %v, %d starts, stopping %v; want running again, watched", running, starts, a.composeApp.stopping.Load())
			}
		})
	}
}

// A compose app that is not running when keploy comes to stop it, which keploy
// had not asked to stop, exited on its own: a halt, which the stop answers
// (models.ErrAppExitedOnItsOwn) with the container's exit code, and keeps,
// though its watch has not seen the die event yet. One keploy had stopped
// already is nothing to stop.
func TestTheComposeAppsStopAnswersAHaltItFinds(t *testing.T) {
	d := newComposeDocker(false, false)
	d.exitCode = 3
	a := composeApp(d)
	killed, err := a.StopComposeApp(context.Background())
	_, signals, _ := d.state()
	if killed != "" || !errors.Is(err, models.ErrAppExitedOnItsOwn) || len(signals) != 0 {
		t.Fatalf("said %q (err %v) after %v; want the halt answered, no signal", killed, err, signals)
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
		t.Fatalf("the halt kept: %v, want exit code 3", h)
	}
	if a.composeApp.stopping.Load() {
		t.Fatal("a halt found at the stop left the app marked as stopping: its watch would take the die for keploy's stop, and not end the run")
	}
	if _, err := a.StopComposeApp(context.Background()); !errors.Is(err, models.ErrAppExitedOnItsOwn) {
		t.Fatalf("a later stop: err %v, want the halt again", err)
	}

	stopped := newComposeDocker(true, true)
	b := composeApp(stopped)
	if _, err := b.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if killed, err := b.StopComposeApp(context.Background()); killed != "" || err != nil || b.composeAppHalt() != nil {
		t.Fatalf("one keploy had stopped: said %q (err %v), halt %v; want nothing to stop", killed, err, b.composeAppHalt())
	}
}

// keploy's stops of a compose app come one at a time: the stop at a set's end
// and the teardown's can come at once (an interrupt as a set ends), and the
// second finds the container stopped by the first, rather than interrupt it
// again and cut its shutdown short.
func TestTheComposeAppsStopsComeOneAtATime(t *testing.T) {
	d := newComposeDocker(true, false)
	d.intDelay = 300 * time.Millisecond
	a := composeApp(d)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = a.StopComposeApp(context.Background())
		}(i)
	}
	wg.Wait()
	_, signals, _ := d.state()
	if errs[0] != nil || errs[1] != nil || strings.Join(signals, ",") != "SIGINT" {
		t.Fatalf("two stops at once: errors %v, signals %v; want one SIGINT, no error", errs, signals)
	}
}

// die is a die event of the app's container with exit code code.
func die(code string) events.Message {
	return events.Message{Action: events.ActionDie, Actor: events.Actor{Attributes: map[string]string{"exitCode": code}}, TimeNano: time.Now().UnixNano()}
}

// An exit of the app's container keploy did not ask for ends the run, once,
// with the app's exit code; one keploy asked for, or a stale one of a
// container running again, does not. A stream that breaks is asked for
// again, and an exit on the new one is still seen.
func TestTheComposeAppsWatchEndsTheRunOnAnExitKeployDidNotAskFor(t *testing.T) {
	d := newComposeDocker(true, true)
	a := composeApp(d)
	var ends atomic.Int32
	ended := make(chan struct{}, 1)
	stop := a.watchComposeApp(context.Background(), func() { ends.Add(1); ended <- struct{}{} })
	defer stop()
	stream := <-d.streams
	errs := <-d.errStreams

	// keploy's own stop, the container down: no halt.
	if _, err := a.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream <- die("0")
	<-d.inspected // the watch has read the event, the container down
	// A stale event of a container keploy has started again: no halt.
	if err := a.StartComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream <- die("137")
	<-d.inspected
	// The stream breaks; the watch asks for it again.
	errs <- errors.New("unexpected EOF")
	stream = <-d.streams
	<-d.errStreams
	select {
	case <-ended:
		t.Fatal("the run was ended for an exit keploy asked for, or for a container running again")
	default:
	}

	d.mu.Lock()
	d.stop()
	d.mu.Unlock()
	stream <- die("3")
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("an exit keploy did not ask for did not end the run")
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 || ends.Load() != 1 {
		t.Fatalf("halt %+v after %d ends; want exit code 3, once", h, ends.Load())
	}
}

// A halt ends the run as compose's abort did: APP_HALTED, with the app's exit
// code, and no error for a clean exit.
func TestAComposeHaltIsTheAppsExit(t *testing.T) {
	if e := (composeHalt{exitCode: 3}).appError("logs"); e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 || e.Err == nil || e.AppLogs != "logs" {
		t.Fatalf("exit 3: %+v", e)
	}
	if e := (composeHalt{exitCode: 0}).appError(""); e.AppErrorType != models.ErrAppStopped || e.ExitCode != 0 || e.Err != nil {
		t.Fatalf("exit 0: %+v", e)
	}
}

// Under the proven switch compose runs without the flags that end the stack
// on a container's exit, the user's own too, the rest of the command as
// written; with the switch off they are added as before.
func TestUnderTheSwitchComposeRunsWithoutItsAbortFlags(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for in, want := range map[string]string{
		"docker compose up": "docker compose up",
		"docker compose up --abort-on-container-exit --exit-code-from app":       "docker compose up",
		"docker compose -f a.yml up --exit-code-from=app --build":                "docker compose -f a.yml up --build",
		"docker compose up --abort-on-container-failure  --build":                "docker compose up  --build",
		"docker compose -p abort-on-container-exit up --abort-on-container-exit": "docker compose -p abort-on-container-exit up",
	} {
		if got := ensureComposeExitOnAppFailure(in, "app", false); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	t.Setenv(provenswitch.Env, "")
	if got := ensureComposeExitOnAppFailure("docker compose up", "app", false); got != "docker compose up --abort-on-container-exit --exit-code-from app" {
		t.Errorf("switch off: %q", got)
	}
}

// An exit keploy did not ask for stops the rest of the app's compose project
// as compose's abort did: each container still running, keploy's agent among
// them, is asked to stop with its own grace, so the agent goes once keploy has
// drained what it captured, not on a clock of keploy's, and compose up
// returns.
func TestAHaltStopsTheProjectAsComposesAbortDid(t *testing.T) {
	d := newComposeDocker(false, true)
	d.project, d.others = "proj", []string{"keploy-agent", "dep"}
	a := composeApp(d)
	if !a.stopComposeProject(context.Background()) {
		t.Fatal("the project was not stopped")
	}
	if len(d.stopped) != 2 || d.stopped["keploy-agent"] != -1 || d.stopped["dep"] != -1 {
		t.Fatalf("stopped %v; want the agent and the dependency, each with its own stop grace (-1: none set by keploy)", d.stopped)
	}
	d.project = ""
	if a.stopComposeProject(context.Background()) {
		t.Fatal("a project keploy could not tell was taken for stopped")
	}
}

// keploy record's stop of a compose app, under the switch, says a kill in the
// recording, as for any app it records (P101-b-k); a replay's does not.
func TestAComposeRecordingsStopSaysItsKill(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stallBudget(t, 300*time.Millisecond)
	for _, tc := range []struct {
		mode models.Mode
		want string
	}{
		{models.MODE_RECORD, "keploy stopped the application at the recording's end and keploy could not read its container's processes"},
		{models.MODE_TEST, ""},
	} {
		d := newComposeDocker(true, false)
		a := composeApp(d)
		a.opts.Mode = tc.mode
		if _, err := a.StopComposeApp(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := a.takeSetEndKill(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Fatalf("%s: the run says %q, want %q", tc.mode, got, tc.want)
		}
	}
}
