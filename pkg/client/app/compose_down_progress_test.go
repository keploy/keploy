package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// These cover how long keploy lets a `docker compose down` run: as long as
// the project makes progress on its way down (the daemon's events for it),
// never a number of seconds. A down keploy cut at 8 s on a loaded CI host
// left the app's dependencies, keploy's agent and the project's network
// behind ("signal: killed", "Error while Stopping"), for the next `up` to
// trip over.

// downTakes has the stand-in docker's `down` take d.
func downTakes(t *testing.T, d time.Duration) {
	t.Helper()
	t.Setenv("KEPLOY_STUB_DOWN_TENTHS", itoa10(int(d/(100*time.Millisecond))))
}

func itoa10(n int) string {
	if n < 10 {
		return itoa(n)
	}
	return itoa10(n/10) + itoa(n%10)
}

// shortDownStall sets the stall bound of a compose down for one test.
func shortDownStall(t *testing.T, d time.Duration) {
	t.Helper()
	old := composeDownStall
	composeDownStall = d
	t.Cleanup(func() { composeDownStall = old })
}

// projectMoves has the daemon report a container of the project stopping,
// another every 100 ms, until the test ends.
func projectMoves(t *testing.T, rec *inspectRecorder) {
	t.Helper()
	projectMovesWithAPause(t, rec, 0, 0)
}

// projectMovesWithAPause is projectMoves with no events for pause, once the
// project has moved for moveFor (pause 0: none).
func projectMovesWithAPause(t *testing.T, rec *inspectRecorder, moveFor, pause time.Duration) {
	t.Helper()
	rec.events = make(chan events.Message)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		start := time.Now()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if since := time.Since(start); pause > 0 && since >= moveFor && since < moveFor+pause {
					continue
				}
				n++
				select {
				case rec.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionStop, Actor: events.Actor{ID: "c" + itoa10(n)}}:
				case <-stop:
					return
				}
			}
		}
	}()
}

// waitDownStarted waits until the stand-in's `down` has begun.
func waitDownStarted(t *testing.T, argvLog string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(recordedLog(t, argvLog), "DOWN-STARTED") {
		if time.Now().After(deadline) {
			t.Fatalf("the down never started:\n%s", recordedLog(t, argvLog))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A down that takes longer than the 8 s it used to be given, while the
// project moves, runs to its end: nothing is cut, and so nothing is left to
// sweep.
func TestComposeDownRunsPastEightSecondsWhileTheProjectMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("a 10 s down")
	}
	a, rec, argvLog := keployStack(t, "", 0)
	projectMoves(t, rec)
	downTakes(t, 10*time.Second)

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "DOWN-DONE") {
		t.Fatalf("keploy cut a down that was making progress:\n%s", log)
	}
	if strings.Contains(log, "ARG ps") || len(rec.netLabels) != 0 {
		t.Fatalf("a down that ran to its end was swept after as a cut-short one (networks asked: %v):\n%s", rec.netLabels, log)
	}
}

// The project's events are what keep a down going past its stall bound: one
// that takes three of them, as the project moves, is not cut. They are asked
// for by the project's compose label.
func TestComposeDownIsKeptGoingByTheProjectsEvents(t *testing.T) {
	shortDownStall(t, time.Second)
	a, rec, argvLog := keployStack(t, "", 0)
	projectMoves(t, rec)
	downTakes(t, 3*time.Second)

	a.ComposeDown()

	if log := recordedLog(t, argvLog); !strings.Contains(log, "DOWN-DONE") {
		t.Fatalf("keploy cut a down whose project was moving, at the stall bound of %s:\n%s", composeDownStall, log)
	}
	if strings.Join(rec.eventLabels, ",") != "com.docker.compose.project=orderflow" {
		t.Fatalf("the down's progress was watched by labels %v; want the project's own, com.docker.compose.project=orderflow", rec.eventLabels)
	}
	sort.Strings(rec.eventActions)
	if strings.Join(rec.eventTypes, ",") != "container" || strings.Join(rec.eventActions, ",") != "destroy,die,kill,stop" {
		t.Fatalf("the down's progress was watched on types %v, events %v; want a container's steps out, container: destroy,die,kill,stop", rec.eventTypes, rec.eventActions)
	}
}

// What goes on in a project while its down is wedged is not progress: a
// running container's healthcheck (keploy's agent runs one every 5 s) and
// health status, and a restart loop's dying over and over. A down that gets
// only those is cut at the stall bound, whether the daemon filtered them out
// or sent them all the same (a container's health from ever more containers:
// only a step on a container's way out counts).
func TestComposeDownIsCutWhileOnlyHealthchecksAndRestartsGoOn(t *testing.T) {
	loop := []events.Message{
		{Type: events.ContainerEventType, Action: events.ActionExecStart + ": sh -c curl -f localhost:16789", Actor: events.Actor{ID: "agent"}},
		{Type: events.ContainerEventType, Action: events.ActionHealthStatus + ": healthy", Actor: events.Actor{ID: "agent"}},
		{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "looping-dep"}},
		{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{ID: "looping-dep"}},
		{Type: events.NetworkEventType, Action: events.ActionDisconnect, Actor: events.Actor{ID: "net"}},
	}
	for _, tc := range []struct {
		name  string
		noise func(i int) events.Message
	}{
		{"a healthcheck and a restart loop", func(i int) events.Message { return loop[i%len(loop)] }},
		{"health from ever more containers", func(i int) events.Message {
			return events.Message{Type: events.ContainerEventType, Action: events.ActionHealthStatus + ": healthy", Actor: events.Actor{ID: "dep-" + itoa10(i)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortDownStall(t, 500*time.Millisecond)
			a, rec, argvLog := keployStack(t, "", 0)
			rec.events = make(chan events.Message)
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for i := 0; ; i++ {
					select {
					case rec.events <- tc.noise(i):
					case <-stop:
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
			downTakes(t, 30*time.Second)

			a.ComposeDown()
			if log := recordedLog(t, argvLog); strings.Contains(log, "DOWN-DONE") {
				t.Fatalf("a wedged down was kept going by %s, past its stall bound of %s, to its end:\n%s", tc.name, composeDownStall, log)
			}
		})
	}
}

// Each event starts the stall again: a pause in the project's progress
// shorter than the stall bound does not cut a down, however long it has run.
func TestComposeDownStallStartsAgainAtEachEvent(t *testing.T) {
	shortDownStall(t, 1500*time.Millisecond)
	a, rec, argvLog := keployStack(t, "", 0)
	projectMovesWithAPause(t, rec, 2*time.Second, 600*time.Millisecond)
	downTakes(t, 4*time.Second)

	a.ComposeDown()

	if log := recordedLog(t, argvLog); !strings.Contains(log, "DOWN-DONE") {
		t.Fatalf("keploy cut a down at a pause of 600ms in its project's progress, under the stall bound of %s:\n%s", composeDownStall, log)
	}
}

// A down whose project does not move is cut at the stall bound, and what it
// left is swept: keploy's services' containers, then the project's networks.
func TestComposeDownIsCutWhenTheProjectStalls(t *testing.T) {
	shortDownStall(t, 500*time.Millisecond)
	ps := `{"ID":"id-db","Service":"db","State":"exited"}` + "\n"
	a, rec, argvLog := keployStack(t, ps, 0)
	rec.networks = []network.Summary{{ID: "net-1", Name: "orderflow_default"}}
	downTakes(t, 20*time.Second)
	core, logs := observer.New(zap.WarnLevel)
	a.logger = zap.New(core)

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if strings.Contains(log, "DOWN-DONE") {
		t.Fatalf("a down that made no progress ran to its end, not cut at the stall bound of %s:\n%s", composeDownStall, log)
	}
	// Cut at its stall bound, as keploy says, not at some other.
	cut := logs.FilterMessageSnippet("made no progress").All()
	if len(cut) != 1 || cut[0].ContextMap()["stall"] != composeDownStall {
		t.Fatalf("keploy said %v of the cut; want one, at the stall bound of %s", cut, composeDownStall)
	}
	if !rec.didRemove("id-db") {
		t.Fatalf("the cut-short down's leftover container was not swept; removed %v", rec.removedIDs())
	}
	if strings.Join(rec.netRemoved, ",") != "net-1" {
		t.Fatalf("the cut-short down's network was not swept; removed %v", rec.netRemoved)
	}
}

// A daemon that does not answer the subscription to the project's events
// does not hold the down up: it runs, and is cut at the stall bound.
func TestComposeDownDoesNotWaitForTheEventsSubscription(t *testing.T) {
	shortDownStall(t, 500*time.Millisecond)
	t.Run("a down that ends", func(t *testing.T) {
		a, rec, argvLog := keployStack(t, "", 0)
		rec.eventsHang = true
		a.ComposeDown()
		if !strings.Contains(recordedLog(t, argvLog), "DOWN-DONE") {
			t.Fatalf("the down never ran:\n%s", recordedLog(t, argvLog))
		}
	})
	t.Run("a down that stalls", func(t *testing.T) {
		a, rec, argvLog := keployStack(t, "", 0)
		rec.eventsHang = true
		downTakes(t, 30*time.Second)
		a.ComposeDown()
		if log := recordedLog(t, argvLog); strings.Contains(log, "DOWN-DONE") {
			t.Fatalf("the down was not cut at the stall bound with the events subscription unanswered:\n%s", log)
		}
	})
}

// A down that failed leaves the project's networks: compose removes them
// last. They are swept, by the project's label, after its containers; a down
// that succeeded is left alone.
func TestComposeDownSweepsTheProjectsNetworksAfterAFailedDown(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		a, rec, _ := keployStack(t, "", 1)
		rec.networks = []network.Summary{{ID: "net-1", Name: "orderflow_default"}, {ID: "net-2", Name: "orderflow_back"}}

		a.ComposeDown()

		if strings.Join(rec.netLabels, ",") != "com.docker.compose.project=orderflow" {
			t.Fatalf("the networks were listed by labels %v; want the project's own", rec.netLabels)
		}
		if strings.Join(rec.netRemoved, ",") != "net-1,net-2" {
			t.Fatalf("removed networks %v; want the project's two", rec.netRemoved)
		}
	})
	t.Run("succeeded", func(t *testing.T) {
		a, rec, _ := keployStack(t, "", 0)
		rec.networks = []network.Summary{{ID: "net-1", Name: "orderflow_default"}}

		a.ComposeDown()

		if len(rec.netLabels) != 0 || len(rec.netRemoved) != 0 {
			t.Fatalf("a down that succeeded was followed by a network sweep: listed %v, removed %v", rec.netLabels, rec.netRemoved)
		}
	})
	t.Run("no project", func(t *testing.T) {
		stubDockerCLI(t, "", 1)
		rec := &inspectRecorder{networks: []network.Summary{{ID: "net-1"}}}
		a := &App{logger: zap.NewNop(), docker: rec, composeFile: "/tmp/c.yaml", cmd: "docker compose up", container: "user-app"}

		a.ComposeDown()

		if len(rec.netLabels) != 0 || len(rec.netRemoved) != 0 || len(rec.eventLabels) != 0 {
			t.Fatalf("with no project known, keploy asked for networks %v (removed %v), events %v", rec.netLabels, rec.netRemoved, rec.eventLabels)
		}
	})
}

// The teardown's drain does not give up on a compose down still running: the
// down stops keploy's agent with the project, and holds the drains while it
// runs, as an agent's stop does.
func TestComposeDownHoldsTheDrains(t *testing.T) {
	a, rec, argvLog := keployStack(t, "", 0)
	projectMoves(t, rec)
	downTakes(t, 2*time.Second)

	held := make(chan bool, 1)
	go func() {
		waitDownStarted(t, argvLog)
		held <- utils.StopHoldsDrains()
	}()
	var g errgroup.Group
	g.Go(func() error { a.ComposeDown(); return nil })
	_, timedOut := utils.DrainErrGroupStatus(zap.NewNop(), "compose-down", &g, 300*time.Millisecond)

	if timedOut {
		t.Fatalf("the teardown's drain gave up on a compose down still running:\n%s", recordedLog(t, argvLog))
	}
	if !<-held {
		t.Fatal("the compose down did not hold the drains while it ran")
	}
	if utils.StopHoldsDrains() {
		t.Fatal("the compose down still holds the drains once it is over")
	}
}

// At the user's Ctrl+C, which the down offers once the run is interrupted,
// keploy cuts the down at once and sweeps what it left: a down that waits on
// progress can always be ended.
func TestComposeDownIsCutAtAPressOfCtrlC(t *testing.T) {
	shortDownStall(t, time.Hour)
	offer := utils.StopOffer
	utils.StopOffer = 0
	t.Cleanup(func() { utils.StopOffer = offer; utils.ClearInterrupted(); utils.ResetInterruptedAgain() })
	utils.MarkInterrupted()
	ps := `{"ID":"id-db","Service":"db","State":"exited"}` + "\n"
	a, rec, argvLog := keployStack(t, ps, 0)
	projectMoves(t, rec)
	downTakes(t, 30*time.Second)

	go func() {
		waitDownStarted(t, argvLog)
		for !utils.MarkInterruptedAgain() {
			time.Sleep(5 * time.Millisecond)
		}
	}()
	a.ComposeDown()

	if log := recordedLog(t, argvLog); strings.Contains(log, "DOWN-DONE") {
		t.Fatalf("the press of Ctrl+C did not cut the down; it ran to its end:\n%s", log)
	}
	if !rec.didRemove("id-db") {
		t.Fatalf("the down cut at the press was not swept after; removed %v", rec.removedIDs())
	}
}

// labelDocker answers the inspect of a container with its compose project
// label, from projects; a container it has none for is gone. The inspect of
// slow takes its context's whole bound, as on a saturated daemon.
type labelDocker struct {
	docker.Client
	projects map[string]string
	slow     string
}

func (d *labelDocker) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	if name == d.slow {
		<-ctx.Done()
		return container.InspectResponse{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return container.InspectResponse{}, err
	}
	p, ok := d.projects[name]
	if !ok {
		return container.InspectResponse{}, errors.New("no such container")
	}
	return container.InspectResponse{Config: &container.Config{Labels: map[string]string{"com.docker.compose.project": p}}}, nil
}

// The project a down watches is the one compose put on the app's container,
// or on keploy's agent's (the app's gone), or else the one compose takes
// from the compose command (-p) or from COMPOSE_PROJECT_NAME.
func TestComposeDownProject(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, env, want string
		projects             map[string]string
	}{
		{"the app's label", "docker compose -p other up", "env", "orderflow", map[string]string{"user-app": "orderflow", "keploy-v3": "agents"}},
		{"the agent's label", "docker compose -p other up", "env", "orderflow", map[string]string{"keploy-v3": "orderflow"}},
		{"-p", "docker compose -p orderflow up", "env", "orderflow", nil},
		{"-p=", "docker compose -p=orderflow up", "", "orderflow", nil},
		{"--project-name", "docker compose --project-name orderflow up", "", "orderflow", nil},
		{"--project-name=", "docker compose --project-name=orderflow up", "", "orderflow", nil},
		{"COMPOSE_PROJECT_NAME", "docker compose up", "orderflow", "orderflow", nil},
		{"COMPOSE_PROJECT_NAME in the command", "COMPOSE_PROJECT_NAME=orderflow docker compose up", "env", "orderflow", nil},
		{"another assignment first", "TAG=1 COMPOSE_PROJECT_NAME='orderflow' docker compose up", "", "orderflow", nil},
		{"an assignment after the command", "docker compose up COMPOSE_PROJECT_NAME=other", "", "", nil},
		{"none", "docker compose up", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMPOSE_PROJECT_NAME", tc.env)
			a := &App{logger: zap.NewNop(), docker: &labelDocker{projects: tc.projects}, cmd: tc.cmd, container: "user-app", keployContainer: "keploy-v3"}
			if got := a.composeDownProject(); got != tc.want {
				t.Fatalf("project %q; want %q", got, tc.want)
			}
		})
	}
	// In-process, the project is the one the library's up ran, which needs
	// no daemon; an assignment in the command, which no shell reads there,
	// is not it. Through the docker CLI, the shell reads the assignment.
	t.Run("the compose library's own", func(t *testing.T) {
		t.Setenv("COMPOSE_PROJECT_NAME", "")
		a := &App{logger: zap.NewNop(), docker: &labelDocker{}, cmd: "COMPOSE_PROJECT_NAME=x docker compose up",
			container: "user-app", keployContainer: "keploy-v3", composeContent: []byte("services: {}\n"), composeRunnerVal: &fakeComposeStack{}}
		want := "x"
		if docker.ComposeLibrarySupported {
			want = "fake"
		}
		if got := a.composeDownProject(); got != want {
			t.Fatalf("project %q; want %q (compose library linked: %v)", got, want, docker.ComposeLibrarySupported)
		}
	})
	t.Run("keploy's environment, in-process", func(t *testing.T) {
		t.Setenv("COMPOSE_PROJECT_NAME", "real")
		a := &App{logger: zap.NewNop(), docker: &labelDocker{}, cmd: "COMPOSE_PROJECT_NAME=x docker compose up",
			container: "user-app", keployContainer: "keploy-v3", composeContent: []byte("services: {}\n")}
		want := "x"
		if docker.ComposeLibrarySupported {
			want = "real"
		}
		if got := a.composeDownProject(); got != want {
			t.Fatalf("project %q; want %q (compose library linked: %v)", got, want, docker.ComposeLibrarySupported)
		}
	})
	// A saturated daemon takes the app's inspect's whole bound: the agent's
	// inspect gets its own, and still tells the project.
	t.Run("the app's inspect runs out", func(t *testing.T) {
		t.Setenv("COMPOSE_PROJECT_NAME", "")
		d := &labelDocker{projects: map[string]string{"keploy-v3": "orderflow"}, slow: "user-app"}
		a := &App{logger: zap.NewNop(), docker: d, cmd: "docker compose up", container: "user-app", keployContainer: "keploy-v3"}
		if got := a.composeDownProject(); got != "orderflow" {
			t.Fatalf("project %q; want orderflow, from the agent's label", got)
		}
	})
}
