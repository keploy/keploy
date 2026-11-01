package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"go.keploy.io/server/v3/pkg/models"
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
// have resolved. composeRunner calls it holding composeRunnerMu, which guards
// composeAPI.
func (a *App) newLibraryComposeStack(ctx context.Context) (composeStack, error) {
	projectName, projectDir := composeProjectScope(a.cmd)
	if a.composeAPI == nil {
		apiClient, err := docker.NewComposeAPIClient()
		if err != nil {
			return nil, fmt.Errorf("configure the compose Engine API client: %w", err)
		}
		a.composeAPI = apiClient
	}
	return docker.NewComposeRunner(ctx, a.composeAPI, docker.ComposeRunnerOptions{
		Content:     a.composeContent,
		ProjectName: projectName,
		WorkingDir:  projectDir,
		EnvFiles:    composeEnvFiles(a.cmd),
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

	// RESIDUAL RACE, stated rather than papered over. If ctx ends while Up is
	// still CREATING or PULLING, this `down` lists what exists at that instant
	// and removes it, and Up can create another container immediately after —
	// which nothing then removes, because downOnce has already fired and makes
	// the deferred composeDown a no-op. cancelUp() below stops Up as fast as it
	// can, so the window is the gap between the listing and the cancel taking
	// effect, but it is not zero.
	//
	// The alternative — cancel first, then down — is worse in the case that
	// actually happens: cancelling Up mid-run kills the application before its
	// coverage flush, which is the whole reason the grace exists. A correct fix
	// needs compose to tell us when creation has finished, which its API does
	// not expose. Left as the smaller hole, deliberately.
	upDone := make(chan struct{})
	teardownDone := make(chan struct{})
	go func() {
		defer close(teardownDone)
		select {
		case <-ctx.Done():
			if composeStopsTheApp() {
				// Under the switch the app's stop is keploy's own, and says
				// when it had to kill (compose_appstop.go).
				// It says a recording's kill itself.
				// An app that exited on its own needs no stop.
				if _, err := a.StopComposeApp(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, models.ErrAppExitedOnItsOwn) {
					a.logger.Warn("keploy could not stop the application's container before taking its compose stack down", zap.Error(err))
				}
				composeDown()
			} else {
				a.composeGraceThenDown(composeDown)
			}
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
	// composeGraceThenDown is bounded by graceBudget, ComposeDown by the
	// project's progress (keploy cuts a down that made none) and the per-call
	// budgets of its sweeps.
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

// composeGraceThenDown is the in-process port of cmdCancel's compose branch:
// the app's grace (composeAppGrace), then the down.
func (a *App) composeGraceThenDown(composeDown func()) {
	a.composeAppGrace(nil)
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
