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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

const haltAgent = "keploy-v3-halt"

// agentHaltDocker is a compose daemon holding the app's container and
// keploy's agent's, each read by its own name; every container is of the
// project "proj". The agent's container goes down after downAfter of its
// reads, when set.
type agentHaltDocker struct {
	*composeDocker
	mu         sync.Mutex
	app, agent container.State
	agentReads int
	appReads   int
	downAfter  int
	downCode   int
	// agentGone: the daemon has no agent container.
	agentGone bool
}

func (d *agentHaltDocker) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.app
	if ref == haltAgent {
		d.agentReads++
		if d.agentGone {
			return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
		}
		if d.downAfter > 0 && d.agentReads >= d.downAfter {
			d.agent = container.State{Status: "exited", ExitCode: d.downCode, StartedAt: d.agent.StartedAt}
		}
		st = d.agent
	} else {
		d.appReads++
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: ref, State: &st},
		Config:            &container.Config{Labels: map[string]string{composeProjectLabelKey: "proj"}},
	}, nil
}

func (d *agentHaltDocker) reads() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.agentReads
}

func (d *agentHaltDocker) appReadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.appReads
}

func runningState(started time.Time) container.State {
	return container.State{Running: true, Status: "running", StartedAt: started.UTC().Format(time.RFC3339Nano)}
}

func exitedState(code int, started time.Time) container.State {
	return container.State{Status: "exited", ExitCode: code, StartedAt: started.UTC().Format(time.RFC3339Nano)}
}

func newAgentHaltApp(d *agentHaltDocker) *App {
	return &App{logger: zap.NewNop(), docker: d, container: "app-1", kind: "docker-compose", keployContainer: haltAgent}
}

func agentDie(code string) events.Message {
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: "agent-id", Attributes: map[string]string{"name": haltAgent, "exitCode": code}}}
}

// keploy's agent's container exiting after the app started, keploy not
// having stopped it, ends the run, in either mode: the watch takes its die
// off the app's stream, and the halt names the agent, its exit code and the
// next step. It is never a clean stop of the app, whatever its code.
func TestAnAgentThatExitsAfterTheAppStartedEndsTheRunNamingIt(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, code := range []int{2, 0} {
			started := time.Now().Add(-time.Minute)
			d := &agentHaltDocker{composeDocker: newComposeDocker(true, true), app: runningState(started), agent: exitedState(code, started)}
			a := newAgentHaltApp(d)
			ended := make(chan struct{}, 1)
			stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
			stream := <-d.streams
			<-d.errStreams
			stream <- agentDie("ignored: the daemon's state decides")
			select {
			case <-ended:
			case <-time.After(time.Minute):
				t.Fatalf("%s=%q: the agent's exit (%d) after the app started did not end the run", provenswitch.Env, proven, code)
			}
			stop()
			h := a.composeAppHalt()
			if h == nil || h.agent != haltAgent || h.exitCode != code {
				t.Fatalf("%s=%q: halt %+v; want the agent's, code %d", provenswitch.Env, proven, h, code)
			}
			e := h.appError("logs")
			if e.AppErrorType != models.ErrUnExpected || e.ExitCode != code || e.Err == nil || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) {
				t.Fatalf("%s=%q: the run's end %+v; want an unexpected end with the agent's code %d", provenswitch.Env, proven, e, code)
			}
			for _, want := range []string{"keploy's agent container \"" + haltAgent + "\"", "exited with code", "Next step:"} {
				if !strings.Contains(e.Err.Error(), want) {
					t.Fatalf("%s=%q: %q does not say %q", provenswitch.Env, proven, e.Err, want)
				}
			}
		}
	}
}

// An exit of the agent's container ends nothing when keploy has begun to stop
// the stack (a halt's stop, a down), when the app has not started in this
// run (compose's up decides: it fails on the agent's health, and a retry may
// recreate the agent), when the container runs again, when a halt is kept
// already (it stands, its project stop under way), when the watch has ended
// (keploy then asks the daemon nothing of the app: this daemon answers even
// on an ended context, so the watch's end alone decides), or when the event
// is the agent's kill, not its exit. One the daemon gives by the agent's ID
// alone is the agent's too, and one of an agent the daemon no longer has
// takes its exit code from the event.
func TestAnAgentExitKeployAskedForOrBeforeTheAppStartedEndsNothing(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	byID := agentDie("1")
	byID.Actor = events.Actor{ID: haltAgent, Attributes: map[string]string{"exitCode": "1"}}
	kill := agentDie("")
	kill.Action = events.ActionKill
	for name, tc := range map[string]struct {
		app, agent container.State
		gone       bool
		set        func(a *App)
		halt       *composeHalt
		ctx        context.Context
		msg        *events.Message
		wantCode   int
		ends       bool
		// noAppRead: keploy reads nothing of the app's container.
		noAppRead bool
	}{
		"keploy stops the stack":  {app: runningState(started), agent: exitedState(0, started), set: func(a *App) { a.composeApp.agentStopping.Store(true) }},
		"the app never started":   {app: container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}, agent: exitedState(1, started)},
		"the app has no start":    {app: container.State{Status: "exited"}, agent: exitedState(1, started)},
		"the agent runs again":    {app: runningState(started), agent: runningState(started)},
		"the app's own halt kept": {app: exitedState(3, started), agent: exitedState(0, started), halt: &composeHalt{exitCode: 3}},
		"another phase's agent":   {app: runningState(started), agent: exitedState(1, started), set: func(a *App) { a.keployContainer = "keploy-v3-next" }},
		"the watch has ended":     {app: runningState(started), agent: exitedState(1, started), ctx: cancelled, noAppRead: true},
		"the agent's kill":        {app: runningState(started), agent: exitedState(1, started), msg: &kill},
		"by the agent's ID":       {app: runningState(started), agent: exitedState(4, started), msg: &byID, ends: true, wantCode: 4},
		"the agent is gone":       {app: runningState(started), gone: true, msg: &byID, ends: true, wantCode: 1},
	} {
		d := &agentHaltDocker{composeDocker: newComposeDocker(true, true), app: tc.app, agent: tc.agent, agentGone: tc.gone}
		a := newAgentHaltApp(d)
		if tc.set != nil {
			tc.set(a)
		}
		if tc.halt != nil {
			a.composeApp.halted.Store(tc.halt)
		}
		ctx, m := context.Background(), agentDie("1")
		if tc.ctx != nil {
			ctx = tc.ctx
		}
		if tc.msg != nil {
			m = *tc.msg
		}
		if got := a.composeDied(ctx, m); got != tc.ends {
			t.Fatalf("%s: the agent's exit ended the run: %v, want %v", name, got, tc.ends)
		}
		h := a.composeAppHalt()
		if tc.ends {
			if h == nil || h.agent != haltAgent || h.exitCode != tc.wantCode {
				t.Fatalf("%s: halt %+v; want the agent's, code %d", name, h, tc.wantCode)
			}
		} else if h != tc.halt {
			t.Fatalf("%s: halt %+v, want %+v", name, h, tc.halt)
		}
		if n := d.appReadCount(); tc.noAppRead && n != 0 {
			t.Fatalf("%s: keploy read the app's container %d times on an ended watch, want none", name, n)
		}
	}
}

// The app's die comes before its agent's: the kernel ends the pid namespace's
// other processes before its first. So an app killed with SIGKILL, not for
// its memory and with no kill of this run of it in the daemon's log, is read
// as the agent's end once docker says the agent's container is down (or that
// it has none), however many reads that takes; an app killed through docker,
// out of memory, or with a code of its own keeps its halt, at once, as does
// a halt that is the agent's already, or a run with no agent known; and an
// agent that stays up for the stall bound leaves the app's halt standing.
func TestAnAppKilledWithItsAgentsNamespaceNamesTheAgent(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	started := time.Now().Add(-time.Minute)
	zap137 := composeHalt{exitCode: 137, startedAt: started.UnixNano()}
	for name, tc := range map[string]struct {
		halt      *composeHalt
		killedAt  int64
		downAfter int
		agentUp   bool
		gone      bool
		noAgent   bool
		wantAgent bool
		wantCode  int
		wantReads int
	}{
		"the agent is down":          {halt: &zap137, wantAgent: true, wantCode: 139, wantReads: 1},
		"the agent goes down later":  {halt: &zap137, agentUp: true, downAfter: 3, wantAgent: true, wantCode: 139, wantReads: 3},
		"the agent is gone":          {halt: &zap137, gone: true, wantAgent: true, wantCode: -1, wantReads: 1},
		"no kill and no start known": {halt: &composeHalt{exitCode: 137}, wantAgent: true, wantCode: 139, wantReads: 1},
		"an earlier run's kill":      {halt: &zap137, killedAt: started.UnixNano() - 1, wantAgent: true, wantCode: 139, wantReads: 1},
		"a kill of this run":         {halt: &zap137, killedAt: started.UnixNano()},
		"a kill with no start known": {halt: &composeHalt{exitCode: 137}, killedAt: 1},
		"out of its own memory":      {halt: &composeHalt{exitCode: 137, oomKilled: true}},
		"a code of its own":          {halt: &composeHalt{exitCode: 3}},
		"SIGTERM":                    {halt: &composeHalt{exitCode: 143}},
		"the agent's halt already":   {halt: &composeHalt{exitCode: 137, agent: haltAgent}},
		"no halt":                    {},
		"no agent known":             {halt: &zap137, noAgent: true},
		"the agent stays up":         {halt: &zap137, agentUp: true},
	} {
		d := &agentHaltDocker{composeDocker: newComposeDocker(false, true), app: exitedState(137, started),
			agent: exitedState(139, started), downAfter: tc.downAfter, downCode: 139, agentGone: tc.gone}
		if tc.agentUp {
			d.agent = runningState(started)
		}
		a := newAgentHaltApp(d)
		if tc.noAgent {
			a.keployContainer = ""
		}
		var kept *composeHalt
		if tc.halt != nil {
			h := *tc.halt
			kept = &h
			a.composeApp.halted.Store(kept)
		}
		a.composeApp.appKilledAt.Store(tc.killedAt)
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.composeAgentNamesTheHalt(context.Background())
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the read of the agent did not end with the agent up past the stall bound", name)
		}
		got := a.composeAppHalt()
		if tc.wantAgent {
			if got == nil || got.agent != haltAgent || got.exitCode != tc.wantCode {
				t.Fatalf("%s: halt %+v; want the agent's, with its code %d", name, got, tc.wantCode)
			}
		} else if got != kept {
			t.Fatalf("%s: halt %+v; want the one kept, %+v", name, got, kept)
		}
		if tc.agentUp && !tc.wantAgent {
			if d.reads() < 2 {
				t.Fatalf("%s: the agent was read %d times; want reads until the stall bound", name, d.reads())
			}
		} else if d.reads() != tc.wantReads {
			t.Fatalf("%s: the agent was read %d times, want %d", name, d.reads(), tc.wantReads)
		}
	}
}

// The halt's end names the agent before it says the halt (the run reads it
// then) and before keploy stops the stack (after which the agent's exit is
// keploy's own): an app the agent's end killed is the agent's halt, and the
// agent's exit in keploy's stop that follows ends nothing.
func TestTheHaltsEndNamesTheAgentBeforeItStopsTheStack(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	d := &agentHaltDocker{composeDocker: newComposeDocker(false, true), app: exitedState(137, started), agent: exitedState(2, started)}
	d.project = "proj"
	a := newAgentHaltApp(d)
	a.composeApp.halted.Store(&composeHalt{exitCode: 137, startedAt: started.UnixNano()})
	halt := make(chan struct{})
	a.composeHaltEnd(context.Background(), halt, func() {})()
	select {
	case <-halt:
	default:
		t.Fatal("the halt was not said")
	}
	if h := a.composeAppHalt(); h.agent != haltAgent || h.exitCode != 2 {
		t.Fatalf("halt %+v; want the agent's, code 2", h)
	}
	if !a.composeApp.agentStopping.Load() || a.composeDied(context.Background(), agentDie("0")) {
		t.Fatal("the agent's exit in keploy's own stop of the stack was taken for a halt")
	}
}

// An app's crash ends the run with its exit code, and a one-shot container's
// exit (a migration's, exit 0) ends nothing, in either mode. With keploy's
// agent known, the watch asks the daemon for every container's die and kill,
// with no label (the daemon ANDs them), and tells the app's container by its
// service and project labels; a kill of the app's container is kept, and ends
// nothing.
func TestAnAppCrashEndsTheRunAndAOneShotExitEndsNothing(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		started := time.Now().Add(-time.Minute)
		d := &agentHaltDocker{composeDocker: newComposeDocker(true, true), app: exitedState(3, started), agent: runningState(started)}
		a := newAgentHaltApp(d)
		a.composeService = "app"
		f := a.composeAppDieFilter()
		if f.Contains("label") || f.Contains("container") || !f.ExactMatch("event", "die") || !f.ExactMatch("event", "kill") {
			t.Fatalf("%s=%q: the watch asks for %v", provenswitch.Env, proven, f)
		}
		labels := func(service string, more map[string]string) map[string]string {
			m := map[string]string{composeServiceLabelKey: service, composeProjectLabelKey: "proj"}
			for k, v := range more {
				m[k] = v
			}
			return m
		}
		ctx := context.Background()
		oneShot := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
			Actor: events.Actor{ID: "migrate-id", Attributes: labels("migrate", map[string]string{"name": "proj-migrate-1", "exitCode": "0"})}}
		if a.composeDied(ctx, oneShot) || a.composeAppHalt() != nil {
			t.Fatalf("%s=%q: a one-shot container's exit 0 ended the run", provenswitch.Env, proven)
		}
		kill := events.Message{Type: events.ContainerEventType, Action: events.ActionKill, TimeNano: started.UnixNano() + 5,
			Actor: events.Actor{ID: "app-id", Attributes: labels("app", map[string]string{"signal": "9"})}}
		if a.composeDied(ctx, kill) || a.composeApp.appKilledAt.Load() != started.UnixNano()+5 {
			t.Fatalf("%s=%q: a kill of the app's container: ended the run, or kept at %d", provenswitch.Env, proven, a.composeApp.appKilledAt.Load())
		}
		crash := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
			Actor: events.Actor{ID: "app-id", Attributes: labels("app", map[string]string{"exitCode": "3"})}}
		if !a.composeDied(ctx, crash) {
			t.Fatalf("%s=%q: the app's crash did not end the run", provenswitch.Env, proven)
		}
		if h := a.composeAppHalt(); h == nil || h.agent != "" || h.exitCode != 3 || h.startedAt != started.UnixNano() {
			t.Fatalf("%s=%q: halt %+v; want the app's, code 3, with its start", provenswitch.Env, proven, h)
		}
		if e := a.composeAppHalt().appError(""); e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 {
			t.Fatalf("%s=%q: the run's end %+v", provenswitch.Env, proven, e)
		}
	}
}

// The watch's filter is the app's container by name, and the agent's, with no
// service known; the app's service by label with no agent known; and every
// container's die and kill with both. A kill the daemon gives no time is kept
// at keploy's; a kill of another container is not the app's; and once the
// agent's halt is kept, the app's die that follows ends nothing more and
// leaves it standing.
func TestTheComposeWatchAsksForTheAgentAndKeepsItsHalt(t *testing.T) {
	for name, tc := range map[string]struct {
		service, agent   string
		label, container []string
	}{
		"no service known": {agent: haltAgent, container: []string{"app-1", haltAgent}},
		"no agent known":   {service: "app", label: []string{composeServiceLabelKey + "=app"}},
		"neither known":    {container: []string{"app-1"}},
	} {
		a := &App{logger: zap.NewNop(), container: "app-1", composeService: tc.service, keployContainer: tc.agent}
		f := a.composeAppDieFilter()
		if got := f.Get("label"); len(got) != len(tc.label) || (len(got) == 1 && got[0] != tc.label[0]) {
			t.Fatalf("%s: labels %v, want %v", name, got, tc.label)
		}
		if got := f.Get("container"); len(got) != len(tc.container) {
			t.Fatalf("%s: containers %v, want %v", name, got, tc.container)
		}
		for _, c := range tc.container {
			if !f.ExactMatch("container", c) {
				t.Fatalf("%s: the filter does not ask for %q", name, c)
			}
		}
	}

	started := time.Now().Add(-time.Minute)
	d := &agentHaltDocker{composeDocker: newComposeDocker(true, true), app: exitedState(137, started), agent: exitedState(2, started)}
	a := newAgentHaltApp(d)
	a.composeService = "app"
	ctx := context.Background()
	appLabels := map[string]string{composeServiceLabelKey: "app", composeProjectLabelKey: "proj"}
	kill := events.Message{Type: events.ContainerEventType, Action: events.ActionKill, Actor: events.Actor{ID: "migrate-id",
		Attributes: map[string]string{composeServiceLabelKey: "migrate", composeProjectLabelKey: "proj"}}}
	if a.composeDied(ctx, kill) || a.composeApp.appKilledAt.Load() != 0 {
		t.Fatalf("a kill of another container was kept for the app's: %d", a.composeApp.appKilledAt.Load())
	}
	kill.Actor = events.Actor{ID: "app-id", Attributes: appLabels}
	before := time.Now().UnixNano()
	if a.composeDied(ctx, kill) || a.composeApp.appKilledAt.Load() < before {
		t.Fatalf("a kill of the app the daemon gave no time was kept at %d, want keploy's time (%d on)", a.composeApp.appKilledAt.Load(), before)
	}
	a.composeApp.appKilledAt.Store(0)
	agentHalt := &composeHalt{exitCode: 2, agent: haltAgent}
	a.composeApp.halted.Store(agentHalt)
	die := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: "app-id", Attributes: appLabels}}
	if a.composeDied(ctx, die) || a.composeAppHalt() != agentHalt {
		t.Fatalf("the app's die after the agent's halt: halt %+v, want the agent's", a.composeAppHalt())
	}
}
