package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	"go.uber.org/zap/zaptest/observer"
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

// An up that returned because keploy stopped the app itself holds the run
// until keploy's teardown ends it, or the app keploy started again since
// halts: an up attached to the app alone returns as soon as the app has
// stopped, and the run, so ended, took the project down, keploy's agent with
// it, while the recording was draining it. An up that did not return on
// keploy's stop does not wait.
func TestComposeAwaitTeardownHoldsTheRunAfterKeploysStop(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan struct{})
	close(closed)
	exited := func(status string, code int) map[string]container.InspectResponse {
		return map[string]container.InspectResponse{"app": {ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: status, ExitCode: code}}}}
	}
	for _, tc := range []struct {
		name     string
		stopping bool
		halted   bool
		app      map[string]container.InspectResponse
		ctx      context.Context
		halt     <-chan struct{}
		want     composeTeardownWait
		ended    int
	}{
		{"keploy did not stop the app, which runs on", false, false, nil, cancelled, nil, composeNoWait, 0},
		{"keploy did not stop the app, which compose only created", false, false, exited("created", 0), cancelled, nil, composeNoWait, 0},
		{"the app exited on its own, before the watch took it", false, false, exited("exited", 3), context.Background(), nil, composeHaltCame, 1},
		{"the app halted", true, true, nil, cancelled, nil, composeNoWait, 1},
		{"keploy's teardown ends the run", true, false, nil, cancelled, nil, composeTeardownCame, 0},
		{"the app keploy started again halts", true, false, nil, context.Background(), closed, composeHaltCame, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{logger: zap.NewNop(), container: "app", docker: &inspectRecorder{inspects: tc.app}}
			a.composeApp.stopping.Store(tc.stopping)
			if tc.halted {
				a.composeApp.halted.Store(&composeHalt{exitCode: 1})
			}
			ended := 0
			if got := a.composeAwaitTeardown(tc.ctx, tc.halt, func() { ended++ }); got != tc.want || ended != tc.ended {
				t.Fatalf("composeAwaitTeardown = %d, ending the run %d times; want %d, %d", got, ended, tc.want, tc.ended)
			}
			if tc.app != nil && tc.ended > 0 {
				if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
					t.Fatalf("the halt kept is %+v; want the app's exit, code 3", h)
				}
			}
		})
	}
}

// The run of `docker compose up app` (attached to the app alone) under the
// switch: the up returns once keploy has stopped the app itself, and the run
// holds the project up, keploy's agent in it, until keploy's teardown ends
// the run (its context, which descends from keploy's own: a signal to keploy
// ends it too), or until the app keploy started again since halts. Only then
// is the project taken down. Before, the run ended with the up, and its
// deferred down took the agent down mid-drain.
func TestRunHoldsTheProjectAfterKeploysStopOfAnAppAttachedAlone(t *testing.T) {
	for _, end := range []string{"keploy's teardown", "a halt of the app keploy started again", "the app's own exit, before the watch takes it"} {
		t.Run(end, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			argvLog := stubDockerCLI(t, "", 0)
			upEnds := filepath.Join(t.TempDir(), "app-stopped")
			t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", upEnds)
			core, logs := observer.New(zap.DebugLevel)
			rec := &inspectRecorder{events: make(chan events.Message, 1)}
			a := &App{
				logger:          zap.New(core),
				docker:          rec,
				kind:            utils.DockerCompose,
				cmd:             "docker compose -f /tmp/keploy-compose.yaml up app",
				composeFile:     "/tmp/keploy-compose.yaml",
				keployContainer: "keploy-v3",
				container:       "app",
				composeServices: []string{"app", "keploy-agent"},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ran := make(chan models.AppError, 1)
			go func() { ran <- a.run(ctx) }()

			for !strings.Contains(recordedLog(t, argvLog), "ARG up") {
				time.Sleep(10 * time.Millisecond)
			}
			if end == "the app's own exit, before the watch takes it" {
				// The app exits on its own (no die event reaches the
				// watch), and the up attached to it returns: the container
				// says it exited, with its code.
				rec.mu.Lock()
				rec.inspects = map[string]container.InspectResponse{"app": {
					ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited", ExitCode: 3}},
					Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": "p1"}},
				}}
				rec.mu.Unlock()
				if err := os.WriteFile(upEnds, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				appErr := <-ran
				if appErr.ExitCode != 3 {
					t.Fatalf("the run ended %+v; want ended by the app's own exit, its exit code 3", appErr)
				}
				if !rec.listedProject("p1") {
					t.Fatal("the run did not stop the app's project, as the watch would on the halt")
				}
				if !strings.Contains(recordedLog(t, argvLog), "ARG down") {
					t.Fatalf("the project was not taken down once the run ended:\n%s", recordedLog(t, argvLog))
				}
				return
			}
			// keploy stops the app (StopComposeApp marks it stopping before
			// its SIGINT), and the up attached to it returns.
			a.composeApp.stopping.Store(true)
			if err := os.WriteFile(upEnds, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for logs.FilterMessageSnippet("the project stays up for keploy's teardown").Len() == 0 {
				select {
				case appErr := <-ran:
					t.Fatalf("the run ended with the up, before keploy's teardown (%+v):\n%s", appErr, recordedLog(t, argvLog))
				default:
				}
				time.Sleep(10 * time.Millisecond)
			}
			if strings.Contains(recordedLog(t, argvLog), "ARG down") {
				t.Fatalf("the project was taken down while the run held it for keploy's teardown:\n%s", recordedLog(t, argvLog))
			}

			var appErr models.AppError
			if end == "keploy's teardown" {
				cancel()
				appErr = <-ran
				if appErr.AppErrorType != models.ErrCtxCanceled {
					t.Fatalf("the run ended %+v; want ended by keploy's teardown", appErr)
				}
			} else {
				// keploy started the app again (StartComposeApp), and it
				// exits on its own: the watch ends the run on the halt.
				a.composeApp.stopping.Store(false)
				rec.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie,
					Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "3"}}}
				appErr = <-ran
				if appErr.ExitCode != 3 {
					t.Fatalf("the run ended %+v; want ended by the app's halt, its exit code 3", appErr)
				}
			}
			if !strings.Contains(recordedLog(t, argvLog), "ARG down") {
				t.Fatalf("the project was not taken down once the run ended:\n%s", recordedLog(t, argvLog))
			}
		})
	}
}

// The run's end on a halt runs once, though the watch and the run, on an up
// that returned on the app's own exit, both come to it.
func TestComposeHaltEndRunsOnce(t *testing.T) {
	rec := &inspectRecorder{inspects: map[string]container.InspectResponse{"app": {
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited", ExitCode: 3}},
		Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": "p1"}},
	}}}
	a := &App{logger: zap.NewNop(), container: "app", docker: rec}
	halt := make(chan struct{})
	downs := 0
	end := a.composeHaltEnd(context.Background(), halt, func() { downs++ })
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); end() }()
	}
	wg.Wait()
	select {
	case <-halt:
	default:
		t.Fatal("the halt was not said")
	}
	if !a.composeApp.stopping.Load() {
		t.Fatal("the app's exit is still taken for a halt after the run's end")
	}
	rec.mu.Lock()
	lists := len(rec.listedLabels)
	rec.mu.Unlock()
	if lists != 1 || downs != 0 {
		t.Fatalf("the project was stopped %d times (and taken down %d); want once, stopped", lists, downs)
	}
}

// fakeAppDocker answers the inspect of the app's container with running,
// and records its start (StartComposeApp), which makes it run. With locked
// set, it says each call it gets while the stop lock is free.
type fakeAppDocker struct {
	docker.Client
	mu      sync.Mutex
	running bool
	started int
	locked  func(call string)
}

// underStopLock is a fakeAppDocker's locked: it fails t when a docker call
// runs while a's stop lock is free, made outside it.
func underStopLock(t *testing.T, a *App) func(string) {
	return func(call string) {
		if a.composeApp.stopMu.TryLock() {
			a.composeApp.stopMu.Unlock()
			t.Errorf("%s ran outside the stop lock", call)
		}
	}
}

func (d *fakeAppDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	if d.locked != nil {
		d.locked("the inspect")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := &container.State{Status: "exited", Running: d.running}
	if d.running {
		st.Status = "running"
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: st}}, nil
}

func (d *fakeAppDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	if d.locked != nil {
		d.locked("the start")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running = true
	d.started++
	return nil
}

func (d *fakeAppDocker) startedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

// The watch decides an exit under the stop lock, as keploy's stops and starts
// are made: a die that comes as keploy's stop is under way (the lock held,
// stopping not yet set) is keploy's once the stop is through; one that
// comes as keploy starts the app again finds it running once the start is
// through. Unserialised, the first was a halt beside a clean stop, and the
// second a halt of an app keploy had just started.
func TestComposeAppDiedIsDecidedUnderTheStopLock(t *testing.T) {
	die := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "137"}}}
	for _, tc := range []struct {
		name  string
		while func(a *App, d *fakeAppDocker)
	}{
		{"keploy's stop under way", func(a *App, _ *fakeAppDocker) { a.composeApp.stopping.Store(true) }},
		{"keploy's start under way", func(a *App, d *fakeAppDocker) {
			d.mu.Lock()
			d.running = true
			d.mu.Unlock()
			a.composeApp.stopping.Store(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeAppDocker{}
			a := &App{logger: zap.NewNop(), docker: d, container: "app"}
			a.composeApp.stopMu.Lock()
			decided := make(chan bool, 1)
			go func() { decided <- a.composeAppDied(context.Background(), die) }()
			tc.while(a, d)
			a.composeApp.stopMu.Unlock()
			if <-decided || a.composeAppHalt() != nil {
				t.Fatal("an exit that came as keploy's stop or start was under way was taken for a halt")
			}
		})
	}
}

// keploy's start of the app, the watch's decision of an exit, and the run's
// reading of how its up ended are each made under the stop lock: the docker
// calls they make run while it is held.
func TestTheComposeAppsTransitionsAreMadeUnderTheStopLock(t *testing.T) {
	die := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "1"}}}
	t.Run("keploy's start", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		a.composeApp.stopping.Store(true)
		if err := a.StartComposeApp(context.Background()); err != nil || d.startedCount() != 1 || a.composeApp.stopping.Load() {
			t.Fatalf("the start: %v, started %d, stopping %v", err, d.startedCount(), a.composeApp.stopping.Load())
		}
	})
	t.Run("the watch's decision", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		if !a.composeAppDied(context.Background(), die) {
			t.Fatal("an exit keploy did not ask for was not a halt")
		}
	})
	t.Run("the run's reading", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		if got := a.composeAwaitTeardown(context.Background(), nil, func() {}); got != composeHaltCame {
			t.Fatalf("an up that returned on the app's own exit ended %d; want a halt (%d)", got, composeHaltCame)
		}
	})
}

// The run decides how its up ended under the stop lock too: an up that
// returned as keploy's stop of the app was under way (the app already gone,
// stopping not yet set) is keploy's stop once the stop is through, held for
// keploy's teardown, not a halt.
func TestComposeAwaitTeardownIsDecidedUnderTheStopLock(t *testing.T) {
	d := &fakeAppDocker{}
	a := &App{logger: zap.NewNop(), docker: d, container: "app"}
	ctx, cancel := context.WithCancel(context.Background())
	a.composeApp.stopMu.Lock()
	ended := make(chan composeTeardownWait, 1)
	go func() { ended <- a.composeAwaitTeardown(ctx, nil, func() {}) }()
	a.composeApp.stopping.Store(true)
	a.composeApp.stopMu.Unlock()
	cancel()
	if got := <-ended; got != composeTeardownCame {
		t.Fatalf("an up that returned on keploy's stop under way ended %d; want held for keploy's teardown (%d)", got, composeTeardownCame)
	}
}
