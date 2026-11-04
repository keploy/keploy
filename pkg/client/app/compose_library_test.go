package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestComposeUpSemanticsAgreeWithTheInjectedFlags is the parity guard between
// the two halves of this feature.
//
// ensureInMemoryComposeFlags decides which `up` flags the run carries;
// composeUpSemantics is what the in-process path reads them back with. If the
// two ever drift, the library path silently runs with different abort/exit-code
// behaviour than the command string says — a stack that never aborts on app
// exit, or an exit code that stops propagating — and nothing fails loudly.
func TestComposeUpSemanticsAgreeWithTheInjectedFlags(t *testing.T) {
	tests := []struct {
		name               string
		service            string
		preferFailureAbort bool
		wantExitCodeFrom   string
		wantOnExit         docker.ComposeOnExit
	}{
		{
			name:             "abort on container exit carries the app service",
			service:          "app",
			wantExitCodeFrom: "app",
			wantOnExit:       docker.ComposeOnExitStop,
		},
		{
			name:               "a completion dependency switches to abort-on-failure",
			service:            "app",
			preferFailureAbort: true,
			wantExitCodeFrom:   "",
			wantOnExit:         docker.ComposeOnExitFail,
		},
		{
			name:             "no service means no --exit-code-from",
			service:          "",
			wantExitCodeFrom: "",
			wantOnExit:       docker.ComposeOnExitStop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ensureInMemoryComposeFlags("docker compose up", tt.service, tt.preferFailureAbort)
			got := composeUpSemantics(cmd)

			if got.ExitCodeFrom != tt.wantExitCodeFrom {
				t.Errorf("ExitCodeFrom = %q, want %q (command was %q)", got.ExitCodeFrom, tt.wantExitCodeFrom, cmd)
			}
			if got.OnExit != tt.wantOnExit {
				t.Errorf("OnExit = %v, want %v (command was %q)", got.OnExit, tt.wantOnExit, cmd)
			}
		})
	}
}

// TestComposeUpSemanticsDefaultsToCascadeIgnore pins the CLI's own default. It
// is NOT CascadeStop: with CascadeStop the whole stack is torn down the first
// time any container exits, which a one-shot migration or seed container does
// on purpose.
func TestComposeUpSemanticsDefaultsToCascadeIgnore(t *testing.T) {
	got := composeUpSemantics("docker compose -f - up")

	if got.OnExit != docker.ComposeOnExitIgnore {
		t.Fatalf("OnExit = %v with no abort flag, want CascadeIgnore; a stack with a one-shot "+
			"init container would be torn down as soon as it finished", got.OnExit)
	}
	if got.ExitCodeFrom != "" {
		t.Fatalf("ExitCodeFrom = %q with no flag, want empty", got.ExitCodeFrom)
	}
}

// TestComposeUpSemanticsReadsTheEqualsForm covers `--exit-code-from=app`, which
// a user may write by hand; ensureComposeExitOnAppFailure leaves any
// user-supplied flag untouched, so this form does reach the parser.
func TestComposeUpSemanticsReadsTheEqualsForm(t *testing.T) {
	got := composeUpSemantics("docker compose -f - up --abort-on-container-failure --exit-code-from=web")

	if got.ExitCodeFrom != "web" {
		t.Errorf("ExitCodeFrom = %q, want %q", got.ExitCodeFrom, "web")
	}
	if got.OnExit != docker.ComposeOnExitFail {
		t.Errorf("OnExit = %v, want CascadeFail", got.OnExit)
	}
}

// TestComposeProjectScopeReadsBothForms pins project resolution. Getting it
// wrong does not fail loudly: the library would simply address a DIFFERENT
// project, so `down` leaves this stack running and `ps` reports nothing.
func TestComposeProjectScopeReadsBothForms(t *testing.T) {
	tests := []struct {
		cmd      string
		wantName string
		wantDir  string
	}{
		{"docker compose -f - up", "", ""},
		{"docker compose -p orderflow -f - up", "orderflow", ""},
		{"docker compose --project-name orderflow -f - up", "orderflow", ""},
		{"docker compose --project-name=orderflow -f - up", "orderflow", ""},
		{"docker compose -p=orderflow -f - up", "orderflow", ""},
		{"docker compose --project-directory /srv/app -f - up", "", "/srv/app"},
		{"docker compose --project-directory=/srv/app -p flow -f - up", "flow", "/srv/app"},
		{`docker compose -p "quoted" --project-directory '/srv/q' -f - up`, "quoted", "/srv/q"},
		{`docker compose --project-name='quoted' --project-directory="/srv/q" -f - up`, "quoted", "/srv/q"},
	}
	for _, tt := range tests {
		name, dir := composeProjectScope(tt.cmd)
		if name != tt.wantName || dir != tt.wantDir {
			t.Errorf("composeProjectScope(%q) = (%q, %q), want (%q, %q)",
				tt.cmd, name, dir, tt.wantName, tt.wantDir)
		}
	}
}

// composeEnvFiles reads every --env-file the command gives, in order and in
// both forms, without quotes; none when it gives none.
func TestComposeEnvFilesReadsEveryEnvFile(t *testing.T) {
	for _, tt := range []struct {
		cmd  string
		want []string
	}{
		{"docker compose -f - up", nil},
		{"docker compose --env-file a.env -f - up", []string{"a.env"}},
		{`docker compose --env-file=a.env --env-file "/srv/b.env" -f - up --env-file='c.env'`, []string{"a.env", "/srv/b.env", "c.env"}},
		{"docker compose -f - up --env-file", nil},
	} {
		if got := composeEnvFiles(tt.cmd); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("composeEnvFiles(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// exitedInspector reports the app container as already exited, so the teardown
// grace loop ends on its first tick instead of burning the full budget.
type exitedInspector struct {
	docker.Client
}

func (exitedInspector) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited"}},
	}, nil
}

func (exitedInspector) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}

// TestRunComposeInProcessClassifiesFailures pins the Init/Runtime split that
// run() turns into ErrCommandError vs ErrUnExpected, and that
// shouldAbortTestRun gives compose special leniency off.
func TestRunComposeInProcessClassifiesFailures(t *testing.T) {
	t.Run("a stack that cannot be built is an Init failure", func(t *testing.T) {
		a := &App{
			logger:         zap.NewNop(),
			composeContent: []byte("services: {}"),
			cmd:            "docker compose -f - up",
			newComposeStack: func(context.Context) (composeStack, error) {
				return nil, errors.New("malformed compose document")
			},
		}

		got := a.runComposeInProcess(context.Background(), func() {})

		if got.Type != utils.Init {
			t.Fatalf("type = %v, want Init; run() maps Init to ErrCommandError", got.Type)
		}
	})

	t.Run("a stack that starts and then fails is a Runtime failure", func(t *testing.T) {
		stack := &fakeComposeStack{upErr: errors.New("service app exited")}
		a := &App{
			logger:          zap.NewNop(),
			composeContent:  []byte("services: {}"),
			cmd:             "docker compose -f - up",
			newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
		}

		got := a.runComposeInProcess(context.Background(), func() {})

		if got.Type != utils.Runtime {
			t.Fatalf("type = %v, want Runtime; run() maps Runtime to ErrUnExpected", got.Type)
		}
	})

	t.Run("a clean exit reports no error", func(t *testing.T) {
		stack := &fakeComposeStack{}
		a := &App{
			logger:          zap.NewNop(),
			composeContent:  []byte("services: {}"),
			cmd:             "docker compose -f - up",
			newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
		}

		if got := a.runComposeInProcess(context.Background(), func() {}); got.Err != nil {
			t.Fatalf("clean run reported %v", got.Err)
		}
	})
}

// TestRunComposeInProcessTearsDownOnCancel pins keploy's OWN bounded teardown.
//
// compose reacts to a cancelled context with a graceful stop of every service,
// on a context it deliberately makes uncancellable. That is the slow teardown
// the drain budget cannot absorb, so keploy's fast down must run FIRST: Up's
// context must still be live when the down starts. If this stops happening,
// runs fail with "teardown drain timed out" rather than anything that points
// here.
//
// And the run returns only once its down has finished: the replay loop starts
// the next test-set's `up` as soon as it returns, which would race the daemon
// still reaping these containers ("container name already in use").
func TestRunComposeInProcessTearsDownOnCancel(t *testing.T) {
	blockUntilCancel := make(chan struct{})
	upStarted := make(chan struct{})
	stack := &fakeComposeStack{upBlocksUntil: blockUntilCancel, upStarted: upStarted}

	downCalled := make(chan struct{})
	releaseDown := make(chan struct{})
	var upEndedBeforeDown atomic.Bool
	a := &App{
		logger:          zap.NewNop(),
		docker:          exitedInspector{},
		composeContent:  []byte("services: {}"),
		cmd:             "docker compose -f - up",
		container:       "user-app",
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan utils.CmdError, 1)
	go func() {
		done <- a.runComposeInProcess(ctx, func() {
			upEndedBeforeDown.Store(stack.upContext().Err() != nil)
			close(downCalled)
			close(blockUntilCancel)
			<-releaseDown
		})
	}()

	select {
	case <-upStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never called the runner's Up")
	}
	cancel()

	select {
	case <-downCalled:
	case <-time.After(graceBudget + 5*time.Second):
		close(releaseDown)
		t.Fatal("the compose stack was never brought down after the run was cancelled; compose's own " +
			"graceful stop then runs unbounded and overruns the teardown drain budget")
	}
	if upEndedBeforeDown.Load() {
		close(releaseDown)
		t.Fatal("the run's cancel reached the runner's Up before keploy's down started: compose's own " +
			"unbounded graceful stop runs first")
	}

	// The down is still running: the run must not have returned.
	select {
	case <-done:
		close(releaseDown)
		t.Fatal("runComposeInProcess returned while its own down was still running")
	case <-time.After(time.Second):
	}
	close(releaseDown)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runComposeInProcess did not return after the teardown")
	}
}

// TestACancelledInProcessRunStopsTheAppAsTheSwitchSays pins which teardown a
// cancelled in-process run takes. Under the proven switch keploy stops the app
// itself (StopComposeApp): it looks for the app's container through the
// compose project on its agent's container, and says so when it cannot find
// it. With the switch off the app gets its grace (composeAppGrace): keploy
// reads the app's container until it has exited and asks for no stop. Both
// end in the same down, so a run that took the wrong one is seen only here.
func TestACancelledInProcessRunStopsTheAppAsTheSwitchSays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proven string
		stops  bool
	}{
		{name: "under the switch keploy stops the app itself", proven: "1", stops: true},
		{name: "with the switch off the app gets its grace", proven: "", stops: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			blockUntilCancel := make(chan struct{})
			stack := &fakeComposeStack{upBlocksUntil: blockUntilCancel}
			exited := container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited"}},
			}
			// Every container has exited, and none carries a label.
			d := &inspectRecorder{inspects: map[string]container.InspectResponse{"user-app": exited, "keploy-v3": exited}}
			core, logs := observer.New(zap.WarnLevel)
			a := &App{
				logger:          zap.New(core),
				docker:          d,
				composeContent:  []byte("services: {}"),
				cmd:             "docker compose -f - up",
				container:       "user-app",
				composeService:  "app",
				keployContainer: "keploy-v3",
				newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
			}

			ctx, cancel := context.WithCancel(context.Background())
			downs := 0
			done := make(chan utils.CmdError, 1)
			go func() {
				done <- a.runComposeInProcess(ctx, func() {
					downs++
					close(blockUntilCancel)
				})
			}()
			cancel()

			select {
			case <-done:
			case <-time.After(graceBudget + 10*time.Second):
				t.Fatal("runComposeInProcess did not return after the run was cancelled")
			}
			if downs != 1 {
				t.Fatalf("the stack was brought down %d times, want once", downs)
			}
			const cannotStop = "keploy could not stop the application's container before taking its compose stack down"
			warned := logs.FilterMessage(cannotStop).Len() == 1
			if asked := d.askedAbout(a.keployContainer); asked != tc.stops || warned != tc.stops {
				t.Fatalf("keploy looked for the app's container through its agent's project: %v, and warned %q: %v; "+
					"want both %v (the switch %q)", asked, cannotStop, warned, tc.stops, tc.proven)
			}
			if !tc.stops && !d.askedAbout(a.container) {
				t.Fatalf("with the switch off keploy never read the app's container %q for its grace (inspected %v)", a.container, d.asked)
			}
		})
	}
}

// TestExitCodeFromErrReadsComposeStatusError pins exit-code propagation across
// the mechanism change. The in-process path runs no child process, so it can
// never produce an *exec.ExitError; its runner reports the --exit-code-from
// status as a *docker.ComposeExitError. Without this, every compose exit code collapses to -1 and
// a wrapped runner's status silently stops propagating.
func TestExitCodeFromErrReadsComposeStatusError(t *testing.T) {
	if got := exitCodeFromErr(fmt.Errorf("compose up: %w", &docker.ComposeExitError{Code: 7, Err: errors.New("app exited")})); got != 7 {
		t.Fatalf("exitCodeFromErr(*docker.ComposeExitError{7}) = %d, want 7", got)
	}
	if got := exitCodeFromErr(nil); got != -1 {
		t.Fatalf("exitCodeFromErr(nil) = %d, want -1", got)
	}
}

// TestComposeRunnerIsRebuiltWhenTheDocumentChanges pins the cache reset. The
// per-test-set replay loop regenerates the compose document between rounds; a
// cached project would keep addressing the PREVIOUS stack, so `down` would
// leave the current one running.
func TestComposeRunnerIsRebuiltWhenTheDocumentChanges(t *testing.T) {
	var built int
	a := &App{
		logger:          zap.NewNop(),
		composeContent:  []byte("services: {}"),
		cmd:             "docker compose -f - up",
		newComposeStack: func(context.Context) (composeStack, error) { built++; return &fakeComposeStack{}, nil },
	}

	if _, err := a.composeRunner(context.Background()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if _, err := a.composeRunner(context.Background()); err != nil {
		t.Fatalf("second build: %v", err)
	}
	if built != 1 {
		t.Fatalf("runner built %d times without a document change, want 1", built)
	}

	a.setComposeSource("", []byte("services:\n  app:\n    image: alpine\n"), nil)

	if _, err := a.composeRunner(context.Background()); err != nil {
		t.Fatalf("rebuild after a new document: %v", err)
	}
	if built != 2 {
		t.Fatalf("runner was not rebuilt after the compose document changed (built %d times); "+
			"teardown would target the previous test-set's stack", built)
	}
}

// A build that registers an in-process runner gets keploy's own compose
// document run through it, scoped as the command scopes it; with none
// registered, the same App keeps the `docker compose -f -` shell-out.
func TestARegisteredComposeStackRunsTheInMemoryDocument(t *testing.T) {
	newApp := func() *App {
		return &App{
			logger:         zap.NewNop(),
			kind:           utils.DockerCompose,
			cmd:            "docker compose -p proj --project-directory /work --env-file a.env up",
			composeContent: []byte("services: {app: {image: app}}"),
		}
	}
	if a := newApp(); a.useComposeLibrary() {
		t.Fatal("the in-memory document takes the in-process path with no runner registered")
	}

	var got []docker.ComposeStackSpec
	stack := &fakeComposeStack{ps: []docker.ServiceState{{ID: "agent-id", Service: keployAgentComposeService}}}
	docker.RegisterComposeStack(func(_ context.Context, spec docker.ComposeStackSpec) (docker.ComposeStack, error) {
		got = append(got, spec)
		return stack, nil
	})
	t.Cleanup(func() { docker.RegisterComposeStack(nil) })

	a := newApp()
	if !a.useComposeLibrary() {
		t.Fatal("the in-memory document keeps the shell-out with a runner registered")
	}
	if ids := a.composeAgentContainerIDs(context.Background()); !reflect.DeepEqual(ids, []string{"agent-id"}) {
		t.Fatalf("agent container ids %v; want the registered runner's [agent-id]", ids)
	}
	if len(got) != 1 {
		t.Fatalf("the registered builder ran %d times; want once", len(got))
	}
	want := docker.ComposeStackSpec{Content: a.composeContent, ProjectName: "proj", WorkingDir: "/work", EnvFiles: []string{"a.env"}}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the builder got %+v; want %+v", got[0], want)
	}

	noDoc := newApp()
	noDoc.composeContent = nil
	if noDoc.useComposeLibrary() {
		t.Fatal("a compose file on disk takes the in-process path; only keploy's own in-memory document may")
	}
}

// A run of keploy's own compose document, in a build with a runner, starts
// the stack through that runner and tears it down through it: no `docker`
// binary runs, which is what lets the agent image, which has no compose
// plugin, run a compose replay.
func TestARunDrivesTheInMemoryDocumentThroughTheRunner(t *testing.T) {
	forceComposeLibrary(t)
	argvLog := stubDockerCLI(t, "", 0)
	stack := &fakeComposeStack{}
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		kind:            utils.DockerCompose,
		cmd:             "docker compose -f - up",
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		keployContainer: "keploy-v3",
		container:       "app",
		composeServices: []string{"app", "keploy-agent"},
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	if appErr := a.run(context.Background()); appErr.AppErrorType != models.ErrAppStopped {
		t.Fatalf("the stack's up returned clean; run() reported %+v", appErr)
	}

	if n := stack.upCalls.Load(); n != 1 {
		t.Fatalf("the runner's Up ran %d times; want once", n)
	}
	if stack.downCall != 1 {
		t.Fatalf("the runner's Down ran %d times; want once", stack.downCall)
	}
	if log := recordedLog(t, argvLog); strings.Contains(log, "ARG") {
		t.Fatalf("the run shelled out to a `docker` binary:\n%s", log)
	}
}

// A state read on the in-process path asks the runner, which knows its own
// project: it asks the daemon nothing to resolve one, whose answer it would
// wait out however slow (composeProjectLabelAnswered) only to drop it.
func TestALibraryStateReadAsksTheDaemonNothing(t *testing.T) {
	forceComposeLibrary(t)
	rec := &inspectRecorder{}
	stack := &fakeComposeStack{ps: []docker.ServiceState{{ID: "id-db", Service: "db", State: "exited", ExitCode: 1}}}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		composeContent:  []byte("services:\n  db:\n    image: postgres\n"),
		cmd:             "docker compose -f - up",
		keployContainer: "keploy-v3",
		container:       "user-app",
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	states := a.composeServiceStates(context.Background())

	if len(states) != 1 || states[0].ID != "id-db" || states[0].ExitCode != 1 {
		t.Fatalf("states %+v; want the runner's id-db, exited 1", states)
	}
	if len(rec.asked) != 0 {
		t.Fatalf("a state read on the in-process path inspected %v on the daemon", rec.asked)
	}
}

// The dependency-abort retry reads the in-process runner's service states as
// it reads `docker compose ps`'s: an app compose created but never started,
// beside a dependency that crashed, is retried; an app that ran is not.
func TestALibraryStateReadFeedsTheDependencyAbortRetry(t *testing.T) {
	forceComposeLibrary(t)
	for _, tc := range []struct {
		name     string
		appState string
		retry    bool
	}{
		{name: "compose created the app and a dependency crashed", appState: "created", retry: true},
		{name: "the app ran and exited", appState: "exited", retry: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := &fakeComposeStack{ps: []docker.ServiceState{
				{ID: "id-app", Service: "app", State: tc.appState, ExitCode: 1},
				{ID: "id-db", Service: "db", State: "exited", ExitCode: 1},
				{ID: "id-agent", Service: keployAgentComposeService, State: "running"},
			}}
			a := &App{
				logger:          zap.NewNop(),
				docker:          &inspectRecorder{},
				kind:            utils.DockerCompose,
				composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
				cmd:             "docker compose -f - up",
				composeService:  "app",
				keployContainer: "keploy-v3",
				container:       "user-app",
				newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
			}

			got := shouldRetryComposeUp(a.kind, 1, composeDepFailureRetries, nil, errors.New("dependency failed to start"),
				utils.Runtime, a.composeService, func() []composeServiceState {
					return a.composeServiceStatesWithin(composePsStateBudget)
				})

			if got != tc.retry {
				t.Fatalf("with the app %s and db exited 1 in the runner's states, keploy retried the up: %v; want %v",
					tc.appState, got, tc.retry)
			}
			if stack.psCalls != 1 {
				t.Fatalf("the retry decision read the runner's states %d times; want once", stack.psCalls)
			}
		})
	}
}

// On the in-process path the app's recent output is read under the runner's
// project, where the runner's Up keeps it (StartAppOutputRun), and there is
// none before a runner is built.
func TestALibraryRunsRecentOutputIsReadUnderTheRunnersProject(t *testing.T) {
	forceComposeLibrary(t)
	docker.ForgetAppOutput()
	t.Cleanup(docker.ForgetAppOutput)
	a := &App{
		logger:          zap.NewNop(),
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -p orderflow -f - up",
		container:       "app",
		composeService:  "app",
		newComposeStack: func(context.Context) (composeStack, error) { return &fakeComposeStack{}, nil },
	}
	if got := a.recentAppOutput(); got != "" {
		t.Fatalf("no runner built yet: recent output %q; want none", got)
	}
	if _, err := a.composeRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := docker.StartAppOutputRun("fake")
	docker.RecordAppOutputOfRun("fake", run, "fake-app-1", "app", "panic: the app crashed")

	if got := a.recentAppOutput(); !strings.Contains(got, "panic: the app crashed") {
		t.Fatalf("recent output %q; want the app's line the runner kept under its project", got)
	}
}

// The library never runs the command in a shell, so an assignment in it does
// not name the project the down takes; keploy's own environment does.
func TestALibraryDownProjectIgnoresTheCommandsAssignment(t *testing.T) {
	forceComposeLibrary(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	a := &App{
		logger:         zap.NewNop(),
		docker:         &inspectRecorder{},
		composeContent: []byte("services:\n  app:\n    image: alpine\n"),
		cmd:            "COMPOSE_PROJECT_NAME=assigned docker compose -f - up",
	}
	if got := a.composeDownProject(context.Background()); got != "" {
		t.Fatalf("project %q; the library run never sees the command's assignment", got)
	}
	t.Setenv("COMPOSE_PROJECT_NAME", "Exported")
	if got := a.composeDownProject(context.Background()); got != "exported" {
		t.Fatalf("project %q; want keploy's own COMPOSE_PROJECT_NAME, normalized", got)
	}
}

// With no in-memory document there is nothing for a runner to run: none is
// built, and the caller hears why.
func TestNoRunnerIsBuiltWithoutAnInMemoryDocument(t *testing.T) {
	built := 0
	a := &App{
		logger:          zap.NewNop(),
		composeFile:     "/tmp/keploy-compose.yaml",
		newComposeStack: func(context.Context) (composeStack, error) { built++; return &fakeComposeStack{}, nil },
	}
	if r, err := a.composeRunner(context.Background()); err == nil || r != nil || built != 0 {
		t.Fatalf("composeRunner with no document = (%v, %v), builder ran %d times; want an error and no build", r, err, built)
	}
}
