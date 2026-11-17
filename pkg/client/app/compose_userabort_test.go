package app

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	"gopkg.in/yaml.v3"
)

// abortDocker is a compose daemon holding the containers of project "proj",
// each by its ID, keploy's agent's by its name too. A stop leaves a
// container exited with its stopCode.
type abortDocker struct {
	*composeDocker
	mu         sync.Mutex
	containers map[string]*abortSvcContainer
	stops      []string
	// unreadable is a container the daemon lists but whose inspect says
	// it has no such container (removed in between).
	unreadable string
}

type abortSvcContainer struct {
	service, name string
	labels        map[string]string
	state         container.State
	stopCode      int
	// stopIgnored leaves the container running at keploy's stop.
	stopIgnored bool
}

func newAbortDocker(started time.Time) *abortDocker {
	d := &abortDocker{composeDocker: newComposeDocker(true, true), containers: map[string]*abortSvcContainer{}}
	d.add("app-id", "app", runningState(started))
	d.add("agent-id", composeAgentServiceKey, runningState(started)).name = haltAgent
	return d
}

func (d *abortDocker) add(id, service string, st container.State) *abortSvcContainer {
	c := &abortSvcContainer{service: service, state: st, stopCode: 143,
		labels: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: service}}
	d.mu.Lock()
	d.containers[id] = c
	d.mu.Unlock()
	return c
}

func (d *abortDocker) find(ref string) (string, *abortSvcContainer) {
	for id, c := range d.containers {
		if id == ref || (c.name != "" && c.name == ref) {
			return id, c
		}
	}
	return "", nil
}

func (d *abortDocker) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, c := d.find(ref)
	if c == nil || (id != "" && id == d.unreadable) {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
	}
	st := c.state
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, State: &st},
		Config:            &container.Config{Labels: c.labels},
	}, nil
}

func (d *abortDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []container.Summary
	for id, c := range d.containers {
		match := true
		for _, l := range opts.Filters.Get("label") {
			k, v, _ := strings.Cut(l, "=")
			if c.labels[k] != v {
				match = false
			}
		}
		if match && (opts.All || c.state.Running) {
			out = append(out, container.Summary{ID: id, Labels: c.labels})
		}
	}
	return out, nil
}

func (d *abortDocker) ContainerStop(_ context.Context, ref string, _ container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, c := d.find(ref)
	if c == nil {
		return errdefs.NotFound(errors.New("no such container"))
	}
	d.stops = append(d.stops, id)
	if c.state.Running && !c.stopIgnored {
		c.state = container.State{Status: "exited", ExitCode: c.stopCode, StartedAt: c.state.StartedAt, FinishedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	return nil
}

func newAbortApp(d *abortDocker, cmd string, compose *docker.Compose) *App {
	a := &App{logger: zap.NewNop(), docker: d, container: "app-1", kind: "docker-compose", keployContainer: haltAgent, composeService: "app"}
	a.setComposeUserAbort(cmd, compose)
	return a
}

func serviceDie(id, service, code string, more map[string]string) events.Message {
	attrs := map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: service, "exitCode": code}
	for k, v := range more {
		attrs[k] = v
	}
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: id, Attributes: attrs}}
}

func exitedAt(code int, started time.Time) container.State {
	st := exitedState(code, started)
	st.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return st
}

func composeDoc(t *testing.T, doc string) *docker.Compose {
	t.Helper()
	var c docker.Compose
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// keploy reads the abort flags of the user's `up` before it leaves them out
// of the command compose runs: which exits end the run, whose code is the
// run's, and which services compose attaches to (the up's services, --attach,
// --no-attach, `attach: false` (`attach: true` keeps it attached),
// --attach-dependencies through depends_on).
func TestTheUsersComposeAbortFlagsAreRead(t *testing.T) {
	doc := composeDoc(t, `
services:
  app:
    image: app
    depends_on:
      db:
        condition: service_healthy
  runner:
    image: runner
    depends_on: [app]
  db:
    image: db
  logs:
    image: logs
    attach: false
  metrics:
    image: metrics
    attach: true
`)
	type want struct {
		onExit, onFailure bool
		from              string
		attaches          map[string]bool
		flags             string
	}
	all := map[string]bool{"app": true, "runner": true, "db": true, "logs": false, "metrics": true, "worker": true}
	for _, c := range []struct {
		cmd  string
		want want
	}{
		{"docker compose --profile traffic up --exit-code-from runner", want{onExit: true, from: "runner", attaches: all, flags: "--exit-code-from runner"}},
		{"docker compose up --exit-code-from=runner --build", want{onExit: true, from: "runner", attaches: all, flags: "--exit-code-from runner"}},
		{"docker-compose up --abort-on-container-exit", want{onExit: true, attaches: all, flags: "--abort-on-container-exit"}},
		{"docker compose up --abort-on-container-failure --exit-code-from runner", want{onFailure: true, from: "runner", attaches: all, flags: "--abort-on-container-failure --exit-code-from runner"}},
		{"docker compose up --abort-on-container-exit --no-attach db", want{onExit: true, attaches: map[string]bool{"app": true, "runner": true, "db": false, "logs": false}, flags: "--abort-on-container-exit"}},
		{"docker compose up --attach runner --attach logs --abort-on-container-exit", want{onExit: true, attaches: map[string]bool{"app": false, "runner": true, "db": false, "logs": true}, flags: "--abort-on-container-exit"}},
		{"docker compose -p up up --timeout 5 --abort-on-container-exit runner", want{onExit: true, attaches: map[string]bool{"app": false, "runner": true, "db": false, "5": false}, flags: "--abort-on-container-exit"}},
		{"docker compose up --abort-on-container-exit --attach-dependencies runner", want{onExit: true, attaches: map[string]bool{"app": true, "runner": true, "db": true, "logs": false}, flags: "--abort-on-container-exit"}},
		{"docker compose up --exit-code-from db runner", want{onExit: true, from: "db", attaches: map[string]bool{"app": false, "runner": true, "db": true}, flags: "--exit-code-from db"}},
		{"docker compose up app && docker compose logs --abort-on-container-exit", want{}},
		{"docker compose up", want{}},
		{"docker compose -f x.yml config --abort-on-container-exit", want{}},
	} {
		r := composeUserAbort(c.cmd, doc)
		if r.onExit != c.want.onExit || r.onFailure != c.want.onFailure || r.exitCodeFrom != c.want.from || strings.Join(r.flags, " ") != c.want.flags {
			t.Fatalf("%q: rule %+v; want %+v", c.cmd, r, c.want)
		}
		if r.active() != (c.want.onExit || c.want.onFailure) {
			t.Fatalf("%q: active %v", c.cmd, r.active())
		}
		for service, attached := range c.want.attaches {
			if r.attaches(service) != attached {
				t.Fatalf("%q: attaches(%q) = %v; want %v", c.cmd, service, !attached, attached)
			}
		}
	}
	if (composeAbortRule{onExit: true}).endsOn(0) != true || (composeAbortRule{onFailure: true}).endsOn(0) || !(composeAbortRule{onFailure: true}).endsOn(-1) || (composeAbortRule{}).endsOn(1) {
		t.Fatal("endsOn: an exit ends an exit rule's run, a failure (and a code docker did not give) a failure rule's, nothing an empty rule's")
	}
}

// `up --exit-code-from runner`: the runner's exit, once the app has started,
// ends the run with the runner's code, record and test alike, in both modes,
// on the watch's stream; a 0 is the clean end compose's abort gave, any other
// code an unexpected one that names the service and the flag.
func TestTheUsersExitCodeFromEndsTheRunOnItsServicesExit(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			for _, code := range []int{0, 3} {
				started := time.Now().Add(-time.Minute)
				d := newAbortDocker(started)
				d.add("runner-id", "runner", exitedAt(code, started))
				a := newAbortApp(d, "docker compose --profile traffic up --exit-code-from runner", nil)
				a.Mode = mode
				ended := make(chan struct{}, 1)
				stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
				stream := <-d.streams
				<-d.errStreams
				stream <- serviceDie("runner-id", "runner", "ignored: the daemon's state decides", nil)
				select {
				case <-ended:
				case <-time.After(time.Minute):
					t.Fatalf("%s=%q %s: the runner's exit (%d) did not end the run", provenswitch.Env, proven, mode, code)
				}
				stop()
				// The halt is --exit-code-from's own service's: its code is
				// the run's as it stands, read again from no other.
				a.composeHaltCodeFrom(context.Background())
				h := a.composeAppHalt()
				if h == nil || h.service != "runner" || h.exitCode != code || h.serviceCode != code || h.agent != "" || h.codeFrom != "" {
					t.Fatalf("%s=%q %s: halt %+v; want the runner's, code %d", provenswitch.Env, proven, mode, h, code)
				}
				e := h.appError("")
				if e.Err != nil && strings.Contains(e.Err.Error(), "the run's exit code is that of") {
					t.Fatalf("%s=%q %s: %q names the runner's code as another's", provenswitch.Env, proven, mode, e.Err)
				}
				switch {
				case code == 0 && (e.AppErrorType != models.ErrAppStopped || e.ExitCode != 0):
					t.Fatalf("%s=%q %s: the run's end %+v; want the clean end", provenswitch.Env, proven, mode, e)
				case code != 0 && (e.AppErrorType != models.ErrUnExpected || e.ExitCode != code || e.Err == nil ||
					!strings.Contains(e.Err.Error(), `compose service "runner" exited with code 3`) || !strings.Contains(e.Err.Error(), "--exit-code-from runner ends the run")):
					t.Fatalf("%s=%q %s: the run's end %+v; want an unexpected one naming the runner and the flag", provenswitch.Env, proven, mode, e)
				}
			}
		}
	}
}

// An exit the user's flags do not end the run on ends nothing: with no flag
// (a one-shot's exit), a container compose does not attach to, a clean exit
// under --abort-on-container-failure, a one-off, another project's, keploy's
// agent's service, one running again, a die of an earlier run, an exit in
// keploy's own stop of the stack, and any exit before the app started. A
// failure under --abort-on-container-failure ends the run with its code.
func TestAnExitTheUsersFlagsDoNotEndTheRunOnEndsNothing(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		ctx := context.Background()
		started := time.Now().Add(-time.Minute)
		// rowCtx is the context the row's watch reads in; a setup may end it.
		var rowCtx context.Context
		nothing := func(name, cmd string, setup func(d *abortDocker, a *App), m events.Message) {
			t.Helper()
			rowCtx = ctx
			d := newAbortDocker(started)
			d.add("worker-id", "worker", exitedAt(0, started))
			a := newAbortApp(d, cmd, nil)
			if setup != nil {
				setup(d, a)
			}
			before := a.composeAppHalt()
			if a.composeDied(rowCtx, m) || a.composeAppHalt() != before {
				t.Fatalf("%s=%q: %s ended the run: %+v", provenswitch.Env, proven, name, a.composeAppHalt())
			}
		}
		exit := "docker compose up --abort-on-container-exit"
		die := serviceDie("worker-id", "worker", "0", nil)
		nothing("an exit with no abort flag", "docker compose up", nil, die)
		nothing("a container not attached", "docker compose up --abort-on-container-exit --no-attach worker", nil, die)
		nothing("a clean exit under --abort-on-container-failure", "docker compose up --abort-on-container-failure", nil, die)
		nothing("a one-off", exit, nil, serviceDie("worker-id", "worker", "0", map[string]string{composeOneOffLabelKey: "True"}))
		nothing("another project's", exit, nil, serviceDie("worker-id", "worker", "0", map[string]string{composeProjectLabelKey: "other"}))
		nothing("keploy's agent's service", exit, func(d *abortDocker, _ *App) {
			d.add("old-agent-id", composeAgentServiceKey, exitedAt(0, started))
		}, serviceDie("old-agent-id", composeAgentServiceKey, "0", nil))
		nothing("one running again", exit, func(d *abortDocker, _ *App) {
			d.containers["worker-id"].state = runningState(started)
		}, die)
		nothing("a die of an earlier run", exit, func(d *abortDocker, _ *App) {
			d.containers["worker-id"].state = exitedAt(0, time.Now().Add(time.Hour))
		}, die)
		nothing("an exit in keploy's own stop of the stack", exit, func(_ *abortDocker, a *App) {
			a.composeApp.agentStopping.Store(true)
		}, die)
		nothing("an exit before the app started", exit, func(d *abortDocker, _ *App) {
			d.containers["app-id"].state = container.State{Status: "created"}
		}, die)
		nothing("keploy's own stop of the app", exit, func(d *abortDocker, a *App) {
			d.containers["app-id"].state = exitedAt(0, started)
			a.composeApp.stopping.Store(true)
		}, serviceDie("app-id", "app", "0", nil))
		nothing("an exit with no service of the app known", exit, func(d *abortDocker, a *App) {
			d.containers["app-id"].name = "app-1"
			a.composeService = ""
		}, die)
		nothing("an exit of no project when the agent's cannot be read", exit, func(d *abortDocker, _ *App) {
			delete(d.containers, "agent-id")
		}, serviceDie("worker-id", "worker", "0", map[string]string{composeProjectLabelKey: ""}))
		nothing("an exit the watch reads as its context ends", exit, func(_ *abortDocker, a *App) {
			p := "proj"
			a.composeApp.project.Store(&p)
			c, cancel := context.WithCancel(ctx)
			cancel()
			rowCtx = c
		}, die)
		nothing("an exit once the app's own halt is kept", exit, func(_ *abortDocker, a *App) {
			a.composeApp.halted.Store(&composeHalt{exitCode: 9})
		}, die)
		nothing("a kill", exit, nil, events.Message{Type: events.ContainerEventType, Action: events.ActionKill, TimeNano: time.Now().UnixNano(),
			Actor: events.Actor{ID: "worker-id", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "worker"}}})

		d := newAbortDocker(started)
		d.add("worker-id", "worker", exitedAt(2, started))
		a := newAbortApp(d, "docker compose up --abort-on-container-failure", nil)
		if !a.composeDied(ctx, serviceDie("worker-id", "worker", "2", nil)) {
			t.Fatalf("%s=%q: a failure under --abort-on-container-failure did not end the run", provenswitch.Env, proven)
		}
		if h := a.composeAppHalt(); h == nil || h.service != "worker" || h.exitCode != 2 {
			t.Fatalf("%s=%q: halt %+v; want the worker's, code 2", provenswitch.Env, proven, h)
		}
		// An unread container's exit is the event's code.
		d = newAbortDocker(started)
		a = newAbortApp(d, exit, nil)
		if !a.composeDied(ctx, serviceDie("gone-id", "worker", "5", nil)) || a.composeAppHalt().exitCode != 5 {
			t.Fatalf("%s=%q: a removed container's exit 5: halt %+v", provenswitch.Env, proven, a.composeAppHalt())
		}
		// The app's own exit is the app's halt, whatever the flags say.
		d = newAbortDocker(started)
		d.containers["app-id"].state = exitedAt(4, started)
		a = newAbortApp(d, "docker compose up --exit-code-from runner", nil)
		if !a.composeDied(ctx, serviceDie("app-id", "app", "4", nil)) {
			t.Fatalf("%s=%q: the app's exit did not end the run", provenswitch.Env, proven)
		}
		if h := a.composeAppHalt(); h == nil || h.service != "" || h.exitCode != 4 {
			t.Fatalf("%s=%q: halt %+v; want the app's own, code 4", provenswitch.Env, proven, h)
		}
	}
}

// With --exit-code-from runner, another attached container's exit ends the
// run too (the flag implies --abort-on-container-exit), and the run's exit
// code is the runner's, read once keploy's end has stopped the stack, as
// compose's abort ended with it. Under --abort-on-container-failure the
// failure's code stands.
func TestTheRunsCodeIsTheExitCodeFromServicesOnceTheStackIsStopped(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, failure := range []bool{false, true} {
			ctx := context.Background()
			started := time.Now().Add(-time.Minute)
			d := newAbortDocker(started)
			d.add("runner-id", "runner", runningState(started)).stopCode = 137
			d.add("worker-id", "worker", exitedAt(1, started))
			cmd := "docker compose up --exit-code-from runner"
			if failure {
				cmd = "docker compose up --abort-on-container-failure --exit-code-from runner"
			}
			a := newAbortApp(d, cmd, nil)
			if !a.composeDied(ctx, serviceDie("worker-id", "worker", "1", nil)) {
				t.Fatalf("%s=%q failure=%v: the worker's exit did not end the run", provenswitch.Env, proven, failure)
			}
			halt := make(chan struct{})
			downs := 0
			a.composeHaltEnd(ctx, halt, func() { downs++ })()
			if downs != 0 || len(d.stops) == 0 {
				t.Fatalf("%s=%q failure=%v: the halt's end stopped %v and took the project down %d times", provenswitch.Env, proven, failure, d.stops, downs)
			}
			h := a.composeAppHalt()
			want := 137
			if failure {
				want = 1
			}
			if h == nil || h.service != "worker" || h.serviceCode != 1 || h.exitCode != want {
				t.Fatalf("%s=%q failure=%v: halt %+v; want the worker's exit, the run's code %d", provenswitch.Env, proven, failure, h, want)
			}
			e := h.appError("")
			if e.ExitCode != want || e.Err == nil || !strings.Contains(e.Err.Error(), `compose service "worker" exited with code 1`) {
				t.Fatalf("%s=%q failure=%v: the run's end %+v", provenswitch.Env, proven, failure, e)
			}
			if said := strings.Contains(e.Err.Error(), `the run's exit code is that of service "runner", 137`); said == failure {
				t.Fatalf("%s=%q failure=%v: %q", provenswitch.Env, proven, failure, e.Err)
			}
		}

		// A runner the stack's stop left running has no code to give: the
		// worker's exit's stands.
		ctx := context.Background()
		started := time.Now().Add(-time.Minute)
		d := newAbortDocker(started)
		d.add("runner-id", "runner", runningState(started)).stopIgnored = true
		d.add("worker-id", "worker", exitedAt(1, started))
		a := newAbortApp(d, "docker compose up --exit-code-from runner", nil)
		if !a.composeDied(ctx, serviceDie("worker-id", "worker", "1", nil)) {
			t.Fatalf("%s=%q: the worker's exit did not end the run", provenswitch.Env, proven)
		}
		a.composeHaltEnd(ctx, make(chan struct{}), func() {})()
		if h := a.composeAppHalt(); h == nil || h.service != "worker" || h.exitCode != 1 || h.codeFrom != "" {
			t.Fatalf("%s=%q: halt %+v; want the worker's code 1, a running runner's read as none", provenswitch.Env, proven, h)
		}

		// A service's SIGKILL (137) with keploy's agent's container down is
		// the service's halt, not the app's killed with its agent's
		// namespace.
		d = newAbortDocker(started)
		d.containers["agent-id"].state = exitedAt(0, started)
		d.add("worker-id", "worker", exitedAt(137, started))
		a = newAbortApp(d, "docker compose up --abort-on-container-exit", nil)
		if !a.composeDied(ctx, serviceDie("worker-id", "worker", "137", nil)) {
			t.Fatalf("%s=%q: the worker's 137 did not end the run", provenswitch.Env, proven)
		}
		a.composeHaltEnd(ctx, make(chan struct{}), func() {})()
		if h := a.composeAppHalt(); h == nil || h.service != "worker" || h.agent != "" || h.exitCode != 137 {
			t.Fatalf("%s=%q: halt %+v; want the worker's 137", provenswitch.Env, proven, h)
		}
	}
}

// An up of the user's services alone (`up --exit-code-from tests tests`, the
// app their dependency) returns once they exit, the app still up: the run
// takes the first exit the flags end the run on, as the watch would, ends the
// project once, and the run's code is that exit's. Not taken: a container
// compose does not attach to, a one-off, keploy's agent's, one not down, one
// docker cannot read, a clean exit under --abort-on-container-failure. With no
// abort flag, or no service of the app known, the up's end stands as before.
func TestAnUpOfTheUsersServicesAloneEndsOnTheirExit(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, c := range []struct {
			cmd, want string
			code      int
			noService bool
		}{
			{cmd: "docker compose up --exit-code-from tests tests", want: "tests", code: 6},
			{cmd: "docker compose up --abort-on-container-exit tests worker", want: "worker", code: 0},
			{cmd: "docker compose up --abort-on-container-failure tests worker", want: "worker", code: 2},
			{cmd: "docker compose up --abort-on-container-failure --no-attach app", want: "worker", code: 2},
			{cmd: "docker compose up tests"},
			{cmd: "docker compose up --exit-code-from tests tests", noService: true},
		} {
			ctx := context.Background()
			started := time.Now().Add(-time.Minute)
			at := func(code int, finished time.Duration) container.State {
				st := exitedState(code, started)
				st.FinishedAt = started.Add(finished).UTC().Format(time.RFC3339Nano)
				return st
			}
			d := newAbortDocker(started)
			d.add("tests-id", "tests", at(6, 30*time.Second))
			d.add("db-id", "db", at(0, time.Second))
			d.add("worker-id", "worker", at(2, 20*time.Second))
			d.add("worker-failed-id", "worker", at(0, 10*time.Second))
			d.add("busy-id", "tests", runningState(started))
			d.add("old-agent-id", composeAgentServiceKey, at(1, 2*time.Second))
			d.add("tests-run-id", "tests", at(1, 3*time.Second)).labels[composeOneOffLabelKey] = "True"
			d.add("gone-id", "tests", at(1, 4*time.Second)).name = "unreadable"
			d.containers["gone-id"].labels = map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "tests"}
			a := newAbortApp(d, c.cmd, nil)
			if c.noService {
				d.containers["app-id"].name = "app-1"
				a.composeService = ""
			}
			d.unreadable = "gone-id"
			ends := 0
			got := a.composeAwaitTeardown(ctx, make(chan struct{}), func() { ends++ })
			if c.want == "" {
				if got != composeNoWait || ends != 0 || a.composeAppHalt() != nil {
					t.Fatalf("%s=%q %q: %v, %d ends, halt %+v; want the up's own end", provenswitch.Env, proven, c.cmd, got, ends, a.composeAppHalt())
				}
				continue
			}
			if got != composeHaltCame || ends != 1 {
				t.Fatalf("%s=%q %q: %v after %d ends; want the halt, ended once", provenswitch.Env, proven, c.cmd, got, ends)
			}
			if h := a.composeAppHalt(); h == nil || h.service != c.want || h.exitCode != c.code {
				t.Fatalf("%s=%q %q: halt %+v; want %s's, code %d", provenswitch.Env, proven, c.cmd, h, c.want, c.code)
			}
		}
	}
}

// abortSetupDocker is a docker client for keploy's compose setup: the app is
// service "app" of the document it parses.
type abortSetupDocker struct {
	docker.Client
	compose *docker.Compose
}

func (d abortSetupDocker) FindContainerInCompose(c *docker.Compose, _ string) (*docker.ComposeServiceInfo, error) {
	return &docker.ComposeServiceInfo{AppServiceName: "app", Compose: c}, nil
}

func (d abortSetupDocker) FindContainerInComposeFiles([]string, string) (*docker.ComposeServiceInfo, error) {
	return &docker.ComposeServiceInfo{AppServiceName: "app", Compose: d.compose}, nil
}

func (abortSetupDocker) ModifyComposeForAgent(*docker.Compose, models.SetupOptions, string) error {
	return nil
}
func (abortSetupDocker) MarshalCompose(*docker.Compose) ([]byte, error) {
	return []byte("services: {}"), nil
}
func (abortSetupDocker) WriteComposeFile(*docker.Compose, string) error { return nil }

// keploy's setup keeps the rule of the user's abort flags, read with the
// compose document (a service set `attach: false`), and leaves the flags out
// of the command compose runs, in-memory and on-disk alike.
func TestTheSetupKeepsTheUsersAbortRule(t *testing.T) {
	const doc = "services:\n  app:\n    image: app\n  logs:\n    image: logs\n    attach: false\n"
	for _, inMemory := range []bool{false, true} {
		a := &App{logger: zap.NewNop(), container: "app-1", kind: "docker-compose",
			docker: abortSetupDocker{compose: composeDoc(t, doc)},
			cmd:    utils.NewAppCommand("docker compose -f docker-compose.yaml up --exit-code-from runner --build")}
		if inMemory {
			a.opts.InMemoryCompose = []byte(doc)
		}
		if err := a.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("in-memory=%v: %v", inMemory, err)
		}
		if strings.Contains(a.cmd.WholeUnredacted(), "--exit-code-from") || !strings.Contains(a.cmd.WholeUnredacted(), "--build") {
			t.Fatalf("in-memory=%v: compose runs %q", inMemory, a.cmd.WholeUnredacted())
		}
		if r := a.composeAbort; r.exitCodeFrom != "runner" || !r.onExit || r.attaches("logs") || !r.attaches("app") {
			t.Fatalf("in-memory=%v: rule %+v", inMemory, r)
		}
	}
}
