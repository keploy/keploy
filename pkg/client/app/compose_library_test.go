package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/cli/cli"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
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
		wantOnExit         api.Cascade
	}{
		{
			name:             "abort on container exit carries the app service",
			service:          "app",
			wantExitCodeFrom: "app",
			wantOnExit:       api.CascadeStop,
		},
		{
			name:               "a completion dependency switches to abort-on-failure",
			service:            "app",
			preferFailureAbort: true,
			wantExitCodeFrom:   "",
			wantOnExit:         api.CascadeFail,
		},
		{
			name:             "no service means no --exit-code-from",
			service:          "",
			wantExitCodeFrom: "",
			wantOnExit:       api.CascadeStop,
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

	if got.OnExit != api.CascadeIgnore {
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
	if got.OnExit != api.CascadeFail {
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
	}
	for _, tt := range tests {
		name, dir := composeProjectScope(tt.cmd)
		if name != tt.wantName || dir != tt.wantDir {
			t.Errorf("composeProjectScope(%q) = (%q, %q), want (%q, %q)",
				tt.cmd, name, dir, tt.wantName, tt.wantDir)
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
// the drain budget cannot absorb, so keploy's fast down must run FIRST. If this
// stops happening, runs fail with "teardown drain timed out" rather than
// anything that points here.
func TestRunComposeInProcessTearsDownOnCancel(t *testing.T) {
	blockUntilCancel := make(chan struct{})
	stack := &fakeComposeStack{upBlocksUntil: blockUntilCancel}

	downCalled := make(chan struct{})
	a := &App{
		logger:          zap.NewNop(),
		docker:          exitedInspector{},
		composeContent:  []byte("services: {}"),
		cmd:             "docker compose -f - up",
		container:       "user-app",
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan utils.CmdError, 1)
	go func() {
		done <- a.runComposeInProcess(ctx, func() {
			close(downCalled)
			close(blockUntilCancel)
		})
	}()

	cancel()

	select {
	case <-downCalled:
	case <-time.After(graceBudget + 5*time.Second):
		t.Fatal("the compose stack was never brought down after the run was cancelled; compose's own " +
			"graceful stop then runs unbounded and overruns the teardown drain budget")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runComposeInProcess did not return after the teardown")
	}
}

// TestExitCodeFromErrReadsComposeStatusError pins exit-code propagation across
// the mechanism change. The in-process path runs no child process, so it can
// never produce an *exec.ExitError; compose reports the --exit-code-from status
// as cli.StatusError. Without this, every compose exit code collapses to -1 and
// a wrapped runner's status silently stops propagating.
func TestExitCodeFromErrReadsComposeStatusError(t *testing.T) {
	if got := exitCodeFromErr(cli.StatusError{StatusCode: 7, Status: "app exited"}); got != 7 {
		t.Fatalf("exitCodeFromErr(cli.StatusError{7}) = %d, want 7", got)
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
