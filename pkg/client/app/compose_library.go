package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/docker/compose/v2/pkg/api"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// composeStack is the subset of the compose library this package drives.
//
// It exists so the in-memory path is testable without a docker daemon: the
// shell-out it replaces could be intercepted with a stub `docker` on PATH, and
// losing that would leave the whole path covered only by live runs.
type composeStack interface {
	ProjectName() string
	Up(ctx context.Context, opts docker.ComposeUpOptions, out, errW io.Writer) error
	Down(ctx context.Context, timeout time.Duration) error
	Ps(ctx context.Context) ([]docker.ServiceState, error)
	ContainerIDsForService(ctx context.Context, service string) ([]string, error)
}

// composeRunner returns the library-backed runner for the in-memory compose
// document, building it on first use.
//
// It is built lazily rather than in SetupCompose because loading the project
// is the first thing that can fail on a malformed document, and doing it here
// keeps that failure on the same code path — and in the same error class —
// as the `docker compose up` that used to report it.
func (a *App) composeRunner(ctx context.Context) (composeStack, error) {
	a.composeRunnerMu.Lock()
	defer a.composeRunnerMu.Unlock()

	if a.composeRunnerVal != nil {
		return a.composeRunnerVal, nil
	}
	if len(a.composeContent) == 0 {
		return nil, fmt.Errorf("no in-memory compose content to run")
	}

	build := a.newComposeStack
	if build == nil {
		build = a.newLibraryComposeStack
	}
	runner, err := build(ctx)
	if err != nil {
		return nil, err
	}
	a.composeRunnerVal = runner
	return runner, nil
}

// newLibraryComposeStack is the production builder: the real compose library,
// scoped to the same project the equivalent `docker compose` invocation would
// have resolved.
func (a *App) newLibraryComposeStack(ctx context.Context) (composeStack, error) {
	projectName, projectDir := composeProjectScope(a.cmd)
	return docker.NewComposeRunner(ctx, a.docker, docker.ComposeRunnerOptions{
		Content:     a.composeContent,
		ProjectName: projectName,
		WorkingDir:  projectDir,
	})
}

// runComposeInProcess is the in-memory-compose replacement for
// utils.ExecuteCommand. It returns the SAME utils.CmdError contract:
//
//	Init    — the stack could never be started (project load, engine unreachable)
//	Runtime — the stack started and then failed or exited non-zero
//	zero    — a clean exit
//
// run() classifies on exactly that split (Init -> ErrCommandError,
// Runtime -> ErrUnExpected), and shouldAbortTestRun gives compose special
// leniency off it, so the split is behaviour rather than bookkeeping.
func (a *App) runComposeInProcess(ctx context.Context, composeDown func()) utils.CmdError {
	runner, err := a.composeRunner(ctx)
	if err != nil {
		return utils.CmdError{Type: utils.Init, Err: err}
	}

	opts := composeUpSemantics(a.cmd)
	a.logger.Info("Starting Application :",
		zap.String("executing_cmd", a.cmd),
		zap.String("composeProject", runner.ProjectName()),
		zap.String("exitCodeFrom", opts.ExitCodeFrom),
		zap.String("onExit", cascadeName(opts.OnExit)))

	// Up runs on a context this function owns, NOT the caller's.
	//
	// compose's own reaction to a cancelled context is a graceful stop of every
	// service, on a context it deliberately makes uncancellable, using each
	// service's full stop grace. That is precisely the teardown the shell-out
	// path was written to avoid: the eBPF agent is slow to stop, and waiting on
	// it overran utils.DrainErrGroup's 30s budget and failed otherwise-green
	// runs with "teardown drain timed out".
	//
	// So keploy's own bounded teardown has to happen FIRST — short grace for the
	// app's coverage flush, then a fast project-wide down — and only then is the
	// up context cancelled as a backstop.
	upCtx, cancelUp := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelUp()

	upDone := make(chan struct{})
	teardownDone := make(chan struct{})
	go func() {
		defer close(teardownDone)
		select {
		case <-ctx.Done():
			a.composeGraceThenDown(composeDown)
			cancelUp()
		case <-upDone:
		}
	}()

	err = runner.Up(upCtx, opts, os.Stdout, os.Stderr)
	close(upDone)

	// Wait for the teardown to finish before returning.
	//
	// Up returns as soon as the attached containers are gone, which the teardown
	// itself causes — so without this the function can return while its own
	// `down` and force-remove are still in flight. Callers treat "the app runner
	// returned" as "the stack is down": the replay loop starts the next
	// test-set's `up` right after, and it would race the daemon still reaping
	// these containers, which is the "container name already in use" stall the
	// teardown's reap barrier exists to prevent.
	//
	// It cannot hang: the goroutine closes teardownDone on both branches, and
	// composeGraceThenDown is bounded by graceBudget plus ComposeDown's own
	// per-call budgets.
	<-teardownDone

	if err != nil {
		return utils.CmdError{Type: utils.Runtime, Err: err}
	}
	return utils.CmdError{}
}

// cascadeName renders api.Cascade for logs. It is an int enum, so a plain
// string() conversion yields a one-rune string, not the name.
func cascadeName(c api.Cascade) string {
	switch c {
	case api.CascadeStop:
		return "stop"
	case api.CascadeFail:
		return "fail"
	case api.CascadeIgnore:
		return "ignore"
	}
	return "unknown"
}

// composeGraceThenDown is the in-process port of cmdCancel's compose branch.
//
// The grace is wall-clock-bounded, not iteration-counted: a saturated daemon
// must not let the per-tick inspect sum past the teardown budget. It ends early
// as soon as the app container has exited, because at that point the coverage
// flush is done (Go has written GOCOVERDIR, Java its jacoco .exec) and there is
// no reason to keep waiting on the slower sibling services.
func (a *App) composeGraceThenDown(composeDown func()) {
	graceDeadline := time.Now().Add(graceBudget)
	for time.Now().Before(graceDeadline) {
		time.Sleep(500 * time.Millisecond)

		ictx, icancel := context.WithTimeout(context.Background(), dockerInspectBudget)
		info, ierr := a.docker.ContainerInspect(ictx, a.container)
		icancel()
		if ierr == nil && (info.State.Status == "exited" || info.State.Status == "dead") {
			a.logger.Debug("app container stopped gracefully; tearing down the rest of the compose stack")
			break
		}
	}
	composeDown()
}

// joinComposeLabels renders a label map in the comma-separated "k=v,k=v" form
// `docker compose ps --format json` prints, so one classifier — isComposeOneOff
// — serves both the library and the shell-out path. Keys are sorted so the
// result is stable and testable; Go map iteration order is not.
func joinComposeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}
