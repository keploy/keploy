package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
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
// The in-process compose path (useComposeLibrary) is not watched: there a
// runner the build registered (docker.RegisterComposeStack) drives the up,
// not a compose process keploy could end. keploy itself registers none, so
// every up an OSS build runs is the shell-out this file watches.
//
// Each read of the daemon the decision makes is read again until it
// succeeds or the watch ends (composeAbortRead): the die it is about may be
// the abort's last, and no later die comes to decide on. A read that fails
// for good (a refused request) is read until the up ends, at a pace. A daemon
// keploy's Engine API client cannot even open a connection to is the one
// exception (composeAbortLost): it gives the watch no events and no answers,
// so the watch stops, says so once, and the up runs unwatched, as before the
// watch, rather than re-dial that daemon at a pace for the whole up.

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
// breaks is asked for again from its last event, so no die is missed, until
// composeAbortUnreachableAsks asks in a row cannot open a connection to the
// daemon (composeAbortCannotDial).
func (a *App) watchComposeAbort(ctx context.Context) (upCtx context.Context, stop func()) {
	upCtx, endUp := context.WithCancelCause(ctx)
	if !a.watchesComposeAbort() {
		return upCtx, func() { endUp(nil) }
	}
	// From the watch's start, not the goroutine's: a die before the goroutine
	// runs (a loaded host) is in the stream's replay. On the daemon's clock,
	// read before the up starts (composeAbortSince).
	since, reached := a.composeAbortSince(ctx)
	if !reached {
		// No events come from a daemon keploy cannot reach: the up runs
		// unwatched, as before the watch (composeAbortLost said so).
		return upCtx, func() { endUp(nil) }
	}
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var project string
		unreached := 0
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
					if a.composeAbortLost(&unreached, err) != nil {
						return
					}
					a.logger.Debug("the docker events stream of the compose project broke; asking for it again from its last event", zap.Error(err))
					break stream
				case m := <-msgs:
					if m.TimeNano > 0 {
						since = time.Unix(0, m.TimeNano)
					}
					var err error
					if project == "" {
						project, err = a.composeAbortProject(wctx)
					}
					ended := false
					if err == nil {
						ended, err = a.composeAbortEnds(wctx, project, m)
					}
					if err != nil {
						// The watch's end, or a daemon keploy lost
						// (composeAbortLost said so).
						return
					}
					if ended {
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
			case <-time.After(composeAbortRetryPace):
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
// is never "created" again, so once it ran no read is the abort. The error
// is the read's that did not end with an answer (composeAbortedBeforeTheApp).
func (a *App) composeAbortEnds(ctx context.Context, project string, m events.Message) (bool, error) {
	attrs := m.Actor.Attributes
	if project == "" || attrs[composeProjectLabelKey] != project || isComposeOneOffLabel(attrs) {
		return false, nil
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
// that will not say its time: one that refuses the read, gives no time, or
// fails the read composeAbortSinceReads times, leaves keploy's clock, which
// is as right as the daemon's is close to it. One keploy's Engine API client
// cannot open a connection to (DOCKER_HOST, else the default socket, which
// is not always the daemon the compose CLI uses) composeAbortUnreachableAsks
// times in a row gives the watch no events either: reached is false, and
// that up runs unwatched, as it did before the watch.
func (a *App) composeAbortSince(ctx context.Context) (since time.Time, reached bool) {
	var unsaid error
	reads := 0
	err := a.composeAbortRead(ctx, "the docker daemon's clock", func(rctx context.Context) error {
		reads++
		info, err := a.docker.Info(rctx)
		switch {
		case err == nil:
			if since, err = time.Parse(time.RFC3339Nano, info.SystemTime); err != nil || since.IsZero() {
				since, unsaid = time.Time{}, fmt.Errorf("the daemon gave the time %q", info.SystemTime)
			}
			return nil
		case composeAbortDaemonRefuses(err):
			unsaid = err
			return nil
		case composeAbortCannotDial(err):
			// composeAbortRead reads it again, and ends on
			// composeAbortUnreachableAsks of these in a row.
			return err
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
	if errors.Is(err, errComposeAbortUnreachable) {
		return time.Time{}, false
	}
	if unsaid != nil {
		a.logger.Warn("keploy could not read the docker daemon's time; it reads the daemon's events for compose aborting the up before the application started from keploy's clock instead",
			zap.Int("reads", reads), zap.Error(unsaid))
	}
	if since.IsZero() {
		return time.Now(), true
	}
	return since, true
}

// composeAbortSinceReads is the count of reads of the daemon's time from
// which a read that fails makes the watch take keploy's clock; one that
// fails before it is read again. Every read counts, failed dials too, so a
// failed dial, another, then a 500 take keploy's clock at the third read.
// Two failures are apart from it: a refusal takes keploy's clock at any
// read, and a failed dial is read again at any read, until
// composeAbortUnreachableAsks of them in a row end the watch.
const composeAbortSinceReads = 3

// composeAbortUnreachableAsks is how many asks of the daemon in a row that
// cannot open a connection to it (composeAbortCannotDial) end the watch. A
// count, not a time, and in a row: one dial that fails (a daemon whose
// accept queue is full) does not end it, and any ask whose connection the
// daemon accepted starts the count again, even one it then dropped before
// any answer.
const composeAbortUnreachableAsks = 3

// errComposeAbortUnreachable is the error a read of the daemon ends with,
// and the watch on, once composeAbortUnreachableAsks asks in a row could not
// open a connection to the daemon.
var errComposeAbortUnreachable = errors.New("keploy's docker client could not connect to the docker daemon")

// composeAbortLost takes err, the failure of an ask of the daemon, with
// *unreached the count of the asks before it, in a row, that could not open
// a connection to the daemon (composeAbortCannotDial). It counts such an err
// and returns nil while the count is under composeAbortUnreachableAsks; any
// other failure starts the count again. On the
// composeAbortUnreachableAsks-th, it says, once for the up, that keploy does
// not watch the rest of the up, and returns errComposeAbortUnreachable: its
// caller ends the watch.
func (a *App) composeAbortLost(unreached *int, err error) error {
	if !composeAbortCannotDial(err) {
		*unreached = 0
		return nil
	}
	*unreached++
	if *unreached < composeAbortUnreachableAsks {
		return nil
	}
	a.logger.Warn("keploy cannot reach the docker daemon (its Engine API client uses DOCKER_HOST, else the default socket, which is not always the daemon docker compose uses), so it does not watch the rest of this docker compose up for compose stopping every container before it started the application",
		zap.String("service", a.composeService), zap.Int("asks", *unreached), zap.Error(err))
	return fmt.Errorf("%w (%d asks in a row): %w", errComposeAbortUnreachable, *unreached, err)
}

// composeAbortCannotDial reports whether err, the failure of an ask of the
// daemon, is keploy's Engine API client failing to open a connection to it:
// no socket at its address, nobody listening there, one keploy's user may
// not open, a host that does not resolve, a Windows named pipe that does not
// exist. Any other failure is not: one the daemon answered, even with a
// failure; a connection the daemon (or a socket proxy in front of it)
// accepted and then dropped before any answer, which
// client.IsErrConnectionFailed also reports; a dial the read's context
// ended (its cap, composeAbortRead). That daemon may well be there, and the
// watch goes on: a watch that stopped on it could leave an aborted up
// waiting for ever again.
//
// The client (docker v28) keeps a failed dial's *net.OpError (op "dial";
// under an HTTP proxy's "proxyconnect" when the dial was the proxy's) in the
// chain for most failed dials, and go-winio's *os.PathError for a named
// pipe's, its only source in the client's transport. For a refused
// connection and a unix socket's failed dial it keeps no cause, only its own
// "Cannot connect to the Docker daemon" error, which it gives for nothing
// but those and a net timeout that is not the context's (keploy's client
// sets none).
func composeAbortCannotDial(err error) bool {
	if !client.IsErrConnectionFailed(err) {
		return false
	}
	var op *net.OpError
	for e := err; errors.As(e, &op); e = op.Err {
		if op.Op == "dial" {
			return true
		}
	}
	var pipe *os.PathError
	return errors.As(err, &pipe) || strings.Contains(err.Error(), "Cannot connect to the Docker daemon")
}

// composeAbortDaemonRefuses reports whether err is the daemon's answer that
// it will not serve a request, which reading it again does not change: one
// it forbids (a socket proxy's rule), does not know, or does not take. Any
// other failure (no answer, a slow one, a server error) is one a later read
// may not have.
func composeAbortDaemonRefuses(err error) bool {
	return cerrdefs.IsPermissionDenied(err) || cerrdefs.IsUnauthorized(err) || cerrdefs.IsNotFound(err) ||
		cerrdefs.IsNotImplemented(err) || cerrdefs.IsInvalidArgument(err)
}

// composeAbortRetryPace is how long the watch waits before it reads the
// daemon again after a read failed: a pace, so a daemon that fails at once is
// not asked in a busy loop. It ends no read.
const composeAbortRetryPace = 200 * time.Millisecond

// composeAbortFirstReadCap is the cap on composeAbortRead's first read; each
// read that runs past its cap is read again with twice the cap.
const composeAbortFirstReadCap = 2 * time.Second

// composeAbortRead runs read, a read of the daemon for the die the watch is
// on, until it succeeds or ctx (the watch's) ends, and reports ctx's error
// then. A read that fails is read again; one that runs past its cap
// (composeAbortFirstReadCap at first) is read again with twice the cap, so a
// daemon slower than any one cap is still read. Neither an error nor a cap
// ends the decision on that die; composeAbortUnreachableAsks reads in a row
// whose dial of the daemon failed (composeAbortCannotDial) end it, with
// errComposeAbortUnreachable (composeAbortLost).
func (a *App) composeAbortRead(ctx context.Context, what string, read func(context.Context) error) error {
	budget := composeAbortFirstReadCap
	unreached := 0
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
		if lost := a.composeAbortLost(&unreached, err); lost != nil {
			return lost
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
// project, so the die is another project's. The error is composeAbortRead's.
func (a *App) composeAbortProject(ctx context.Context) (project string, err error) {
	err = a.composeAbortRead(ctx, "keploy's agent's container", func(rctx context.Context) error {
		info, err := a.docker.ContainerInspect(rctx, a.keployContainer)
		if cerrdefs.IsNotFound(err) {
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
	return project, err
}

// composeAbortedBeforeTheApp reads the containers of project, one-offs aside,
// until the daemon answers (composeAbortRead), and reports whether none runs,
// keploy's agent's have all stopped, and the app's service has exactly one,
// which has never started. Each agent container must have stopped: one still
// "created" is one compose may yet start, as while compose recreates a
// running agent (it makes the new one, then stops the old one: that die is
// not the abort). The error is composeAbortRead's.
func (a *App) composeAbortedBeforeTheApp(ctx context.Context, project string) (aborted bool, err error) {
	err = a.composeAbortRead(ctx, "the compose project's containers", func(rctx context.Context) (err error) {
		aborted, err = a.readComposeAbortedBeforeTheApp(rctx, project)
		return err
	})
	return aborted, err
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
		if cerrdefs.IsNotFound(err) {
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
