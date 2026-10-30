package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A `docker compose up` whose services are attached ends, under the abort
// flags keploy adds (--abort-on-container-exit), when the containers it
// watches are all gone: each one's die, or its removal. When a dependency the
// app depends_on exits before it is healthy, compose aborts: it stops every
// container, keploy's agent with it, and the app's, which it never started,
// stays "created". A stop of a container that does not run is no event, so
// that container is never gone, and compose's up waits for it: forever,
// unless the start that failed on the dependency returned its error first
// (docker compose v5.6.1, about one up in two; the compose-dep-retry lane's
// all-attached case). keploy waited on that up, so it neither retried the
// bring-up nor saw the run end: the record timer ran out on a stack that no
// longer ran.
//
// watchComposeAbort is keploy's view of that end, from the daemon's events:
// once every container of the project has stopped, keploy's agent among
// them, and the app's container has never started, nothing can start the app
// in that up any more (it waits on the agent's health, and an up does not
// start a stopped container again), so the up is over whatever compose
// does. keploy then kills the up's process, as it kills an up that outlives
// its stop: a kill ends it whatever compose's own handling of a signal does
// (its SIGINT teardown happens to cancel the wait, a compose internal), and
// keploy takes nothing down, so the project stays as compose left it: the
// retry's decision reads it as for an up that returned on its own, and the
// retry (or the run's end, with its reason) follows.
//
// The in-process compose path (useComposeLibrary) is not watched: the
// compose library keploy vendors (v5.5.1) does not mark its up terminated on
// that abort, so the failed start's error ends the up on its own (the wait
// came with v5.6.0; docker/compose#14426). TestTheVendoredComposeHasNoAbortWait
// fails once the vendored library is v5.6.0 or later, so a bump brings this
// watch to that path first. The live row
// TestLiveAnInProcessUpThatComposeAbortedBeforeTheAppStartedIsRetried can
// fail on a library with the wait too, but only when compose's race goes that
// way (1 of 4 runs measured).
//
// Each read of the daemon the decision makes is read again until it
// succeeds or the watch ends (composeAbortRead): the die it is about may be
// the abort's last, and no later die comes to decide on. A read that fails
// for good (a refused request) is read until the up ends, at a pace.

// errComposeUpAborted is the cause the watch ends an up attempt's context
// with.
var errComposeUpAborted = errors.New("docker compose stopped every container of the project before it started the application")

// watchesComposeAbort reports whether a compose run's up attempts are
// watched for an abort before the app started: a compose app keploy knows
// the service and the agent of, run as a `docker compose` process, outside
// the proven switch, under which keploy leaves compose's abort flags out and
// ends the run on the app's exit itself (compose_appstop.go).
func (a *App) watchesComposeAbort() bool {
	return a.kind == utils.DockerCompose && !composeStopsTheApp() &&
		a.docker != nil && a.composeService != "" && a.keployContainer != ""
}

// watchComposeAbort watches one up attempt, from now until the returned stop
// is called (when the up has returned) or ctx ends, for its abort before the
// app started (see above). upCtx is the up's context, made from ctx: the
// watch ends it, with the cause errComposeUpAborted, on that abort. The
// stream is the daemon's die events of compose's containers; a stream that
// breaks is asked for again from its last event, so no die is missed.
func (a *App) watchComposeAbort(ctx context.Context) (upCtx context.Context, stop func()) {
	upCtx, endUp := context.WithCancelCause(ctx)
	if !a.watchesComposeAbort() {
		return upCtx, func() { endUp(nil) }
	}
	// From the watch's start, not the goroutine's: a die before the goroutine
	// runs (a loaded host) is in the stream's replay. On the daemon's clock,
	// read before the up starts (composeAbortSince).
	since := a.composeAbortSince(ctx)
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var project string
		for wctx.Err() == nil {
			msgs, errs := a.docker.Events(wctx, events.ListOptions{
				Since: fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
				Filters: filters.NewArgs(
					filters.Arg("type", string(events.ContainerEventType)),
					filters.Arg("event", string(events.ActionDie)),
					filters.Arg("label", composeProjectLabelKey),
				),
			})
		stream:
			for {
				select {
				case <-wctx.Done():
					return
				case err := <-errs:
					if wctx.Err() != nil {
						return
					}
					a.logger.Debug("the docker events stream of the compose project broke; asking for it again from its last event", zap.Error(err))
					break stream
				case m := <-msgs:
					if m.TimeNano > 0 {
						since = time.Unix(0, m.TimeNano)
					}
					if project == "" {
						project = a.composeAbortProject(wctx)
					}
					if a.composeAbortEnds(wctx, project, m) {
						a.logger.Info("docker compose stopped every container of the project, keploy's agent too, before it started the application (a container it depends on exited); keploy ends compose's up, which would wait for the application's container forever",
							zap.String("service", a.composeService))
						endUp(errComposeUpAborted)
						return
					}
				}
			}
			select {
			case <-wctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return upCtx, func() {
		cancel()
		<-done
		endUp(nil)
	}
}

// composeAbortEnds takes m, a die of a container of some compose project, and
// reports whether the up aborted before the app started: the die is one of
// the project's own (not another project's, nor a one-off's), and the
// project's containers have all stopped, keploy's agent's among them, with
// the app's never started. The app's own die is read as any other, not the
// watch's end: compose recreating a running app stops the old one and goes
// on, and an abort later in that up must still be seen. An app that started
// is never "created" again, so once it ran no read is the abort.
func (a *App) composeAbortEnds(ctx context.Context, project string, m events.Message) bool {
	attrs := m.Actor.Attributes
	if project == "" || attrs[composeProjectLabelKey] != project || isComposeOneOffLabel(attrs) {
		return false
	}
	return a.composeAbortedBeforeTheApp(ctx, project)
}

// composeAbortSince is the moment the watch reads the daemon's events from,
// on the daemon's own clock, read before the up starts: the daemon stamps
// each event by its clock and replays to a new stream the events stamped at
// or after its since, so on that clock every die of this up is later than
// it and every die of the attempt before (whose up has returned) earlier.
// From keploy's clock, a daemon ahead of it by more than the retry's gap
// replays the last attempt's dies to the retried up's watch, and the project
// as that abort left it reads as the abort: keploy killed every retried up
// before compose started anything.
//
// The up starts only once this read is done, so it never waits on a daemon
// that will not say its time: one that refuses the read, gives no time,
// cannot be reached by keploy's Engine API client (DOCKER_HOST, else the
// default socket, which is not always the daemon the compose CLI uses), or
// fails the read composeAbortSinceReads times, leaves keploy's clock, which
// is as right as the daemon's is close to it. A daemon keploy cannot reach
// gives the watch no events either, so that up runs unwatched, as it did
// before the watch.
func (a *App) composeAbortSince(ctx context.Context) time.Time {
	var since time.Time
	var unsaid error
	reads := 0
	_ = a.composeAbortRead(ctx, "the docker daemon's clock", func(rctx context.Context) error {
		reads++
		info, err := a.docker.Info(rctx)
		switch {
		case err == nil:
			if since, err = time.Parse(time.RFC3339Nano, info.SystemTime); err != nil || since.IsZero() {
				since, unsaid = time.Time{}, fmt.Errorf("the daemon gave the time %q", info.SystemTime)
			}
			return nil
		case composeAbortDaemonRefuses(err), client.IsErrConnectionFailed(err):
			unsaid = err
			return nil
		}
		// Every read before this one failed too. Bounded by a count, not a
		// time: a daemon that keeps failing this one request (a 5xx) must
		// not hold the up's start.
		if reads >= composeAbortSinceReads {
			unsaid = err
			return nil
		}
		return err
	})
	if unsaid != nil {
		a.logger.Warn("keploy could not read the docker daemon's time; it reads the daemon's events for compose aborting the up before the application started from keploy's clock instead",
			zap.Int("reads", reads), zap.Error(unsaid))
	}
	if since.IsZero() {
		return time.Now()
	}
	return since
}

// composeAbortSinceReads is how many failed reads of the daemon's time
// (other than a refusal or no connection, each of which ends the reads at
// once) the watch makes before it takes keploy's clock.
const composeAbortSinceReads = 3

// composeAbortDaemonRefuses reports whether err is the daemon's answer that
// it will not serve a request, which reading it again does not change: one
// it forbids (a socket proxy's rule), does not know, or does not take. Any
// other failure (no answer, a slow one, a server error) is one a later read
// may not have.
func composeAbortDaemonRefuses(err error) bool {
	return errdefs.IsForbidden(err) || errdefs.IsUnauthorized(err) || errdefs.IsNotFound(err) ||
		errdefs.IsNotImplemented(err) || errdefs.IsInvalidParameter(err)
}

// composeAbortRetryPace is how long the watch waits before it reads the
// daemon again after a read failed: a pace, so a daemon that fails at once is
// not asked in a busy loop. It ends no read.
const composeAbortRetryPace = 200 * time.Millisecond

// composeAbortRead runs read, a read of the daemon for the die the watch is
// on, until it succeeds or ctx (the watch's) ends, and reports ctx's error
// then. A read that fails is read again; one that runs past its cap
// (dockerInspectBudget at first) is read again with twice the cap, so a
// daemon slower than any one cap is still read. Neither an error nor a cap
// ends the decision on that die.
func (a *App) composeAbortRead(ctx context.Context, what string, read func(context.Context) error) error {
	budget := dockerInspectBudget
	for {
		rctx, cancel := context.WithTimeout(ctx, budget)
		err := read(rctx)
		capped := errors.Is(rctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if capped {
			budget *= 2
		}
		a.logger.Debug("keploy could not read "+what+" for the compose abort's decision; it reads it again",
			zap.Duration("cap", budget), zap.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(composeAbortRetryPace):
		}
	}
}

// composeAbortProject is the compose project of keploy's agent's container,
// read until the daemon answers (composeAbortRead): "" when that container
// does not exist, which the up makes before it starts any container of the
// project, so the die is another project's.
func (a *App) composeAbortProject(ctx context.Context) (project string) {
	_ = a.composeAbortRead(ctx, "keploy's agent's container", func(rctx context.Context) error {
		info, err := a.docker.ContainerInspect(rctx, a.keployContainer)
		if errdefs.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Config != nil {
			project = strings.TrimSpace(info.Config.Labels[composeProjectLabelKey])
		}
		return nil
	})
	return project
}

// composeAbortedBeforeTheApp reads the containers of project, one-offs aside,
// until the daemon answers (composeAbortRead), and reports whether none runs,
// keploy's agent's have all stopped, and the app's service has exactly one,
// which has never started. Each agent container must have stopped: one still
// "created" is one compose may yet start, as while compose recreates a
// running agent (it makes the new one, then stops the old one: that die is
// not the abort).
func (a *App) composeAbortedBeforeTheApp(ctx context.Context, project string) (aborted bool) {
	_ = a.composeAbortRead(ctx, "the compose project's containers", func(rctx context.Context) (err error) {
		aborted, err = a.readComposeAbortedBeforeTheApp(rctx, project)
		return err
	})
	return aborted
}

// readComposeAbortedBeforeTheApp is one read of composeAbortedBeforeTheApp;
// it fails on any read of the daemon that fails.
func (a *App) readComposeAbortedBeforeTheApp(ctx context.Context, project string) (bool, error) {
	list, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", composeProjectLabelKey+"="+project),
	)})
	if err != nil {
		return false, err
	}
	apps, appCreated := 0, false
	agents, agentsStopped := 0, true
	for _, c := range list {
		if isComposeOneOffLabel(c.Labels) {
			continue
		}
		info, err := a.docker.ContainerInspect(ctx, c.ID)
		if errdefs.IsNotFound(err) {
			// Removed since the list.
			continue
		}
		if err != nil {
			return false, err
		}
		if info.ContainerJSONBase == nil || info.State == nil {
			return false, fmt.Errorf("the daemon gave no state of container %s", c.ID)
		}
		status := info.State.Status
		switch status {
		case "created", "exited", "dead":
		default:
			return false, nil
		}
		switch c.Labels[composeServiceLabelKey] {
		case a.composeService:
			apps++
			appCreated = status == "created"
		case keployAgentComposeService:
			agents++
			agentsStopped = agentsStopped && status != "created"
		}
	}
	return apps == 1 && appCreated && agents > 0 && agentsStopped, nil
}

// endsAbortedComposeUp is the up's cancel (cmd.Cancel, through
// ExecuteCommand): when the watch ended upCtx, it kills the up's process
// group, which has nothing left to stop; when the run's end did, it is the
// run's own stop of the app, cancel.
func endsAbortedComposeUp(upCtx context.Context, logger *zap.Logger, cancel func(cmd *exec.Cmd) func() error) func(cmd *exec.Cmd) func() error {
	return func(cmd *exec.Cmd) func() error {
		stop := cancel(cmd)
		return func() error {
			if !errors.Is(context.Cause(upCtx), errComposeUpAborted) {
				return stop()
			}
			if err := utils.SendSignal(logger, -cmd.Process.Pid, syscall.SIGKILL); err != nil {
				// No process groups to signal (Windows): the up's own process.
				return cmd.Process.Kill()
			}
			return nil
		}
	}
}

// composeAbortCmdError says, on the up's error, that keploy ended the up
// because compose had aborted it before the app started; it keeps its type,
// a Runtime error, which the retry's decision reads. An up that exited 0 as
// keploy ended it is that Runtime error too: the app never started, so the
// up did not end clean.
func composeAbortCmdError(upCtx context.Context, cmdErr utils.CmdError) utils.CmdError {
	if !errors.Is(context.Cause(upCtx), errComposeUpAborted) {
		return cmdErr
	}
	const ended = "keploy ended docker compose's up, which would have waited for the application's container forever"
	if cmdErr.Err == nil {
		return utils.CmdError{Type: utils.Runtime, Err: fmt.Errorf("%s: %w", ended, errComposeUpAborted)}
	}
	cmdErr.Err = fmt.Errorf("%s: %w (%w)", ended, errComposeUpAborted, cmdErr.Err)
	return cmdErr
}
