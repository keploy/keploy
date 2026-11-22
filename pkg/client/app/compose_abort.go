package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The reads of a compose project's daemon that the end of an up takes: an up
// that reported 0 though a dependency's crash ended it before compose started
// the app (composeAbortReportedZero), and the project's states the retry of
// such an up decides on (composeFailedUpStates).
//
// Each read of the daemon the decision makes is read again until it
// succeeds or the watch ends (composeAbortRead): the die it is about may be
// the abort's last, and no later die comes to decide on. A read that fails
// for good (a refused request) is read until the up ends, at a pace. A daemon
// keploy's Engine API client cannot even open a connection to is the one
// exception (composeAbortLost): it gives the watch no events and no answers,
// so the watch stops, says so once, and the up runs unwatched, as before the
// watch, rather than re-dial that daemon at a pace for the whole up.

// readsComposeProject reports whether keploy can read a compose run's project
// from the daemon: a compose app keploy knows the service and the agent of.
func (a *App) readsComposeProject() bool {
	return a.kind == utils.DockerCompose && a.docker != nil && a.composeService != "" && a.keployContainer != ""
}

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
// composeAbortUnreachableAsks-th, it says, once for the read that lost the
// daemon, that keploy does not read what (the read's own words) from it, and
// returns errComposeAbortUnreachable: its caller ends.
func (a *App) composeAbortLost(what string, unreached *int, err error) error {
	if !composeAbortCannotDial(err) {
		*unreached = 0
		return nil
	}
	*unreached++
	if *unreached < composeAbortUnreachableAsks {
		return nil
	}
	a.logger.Warn("keploy cannot reach the docker daemon (its Engine API client uses DOCKER_HOST, else the default socket, which is not always the daemon docker compose uses), so it does not read "+what+" from it",
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
		if lost := a.composeAbortLost(what, &unreached, err); lost != nil {
			return lost
		}
		if capped {
			budget *= 2
		}
		a.logger.Debug("keploy could not read "+what+" from the docker daemon; it reads it again",
			zap.Duration("cap", budget), zap.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(composeAbortRetryPace):
		}
	}
}

// errComposeAbortReportedZero is what the error of an up that exited 0 though
// compose never started the app wraps (composeReportedZero).
var errComposeAbortReportedZero = errors.New("docker compose reported 0, but a service the application depends on crashed and compose never started the application")

// composeReportedZero is that error. states is the project as keploy read it,
// which the retry's decision and its removal of what crashed take from here: a
// read of their own could miss on the daemon this read waited for.
type composeReportedZero struct {
	states []composeServiceState
}

func (e *composeReportedZero) Error() string { return errComposeAbortReportedZero.Error() }

func (e *composeReportedZero) Unwrap() error { return errComposeAbortReportedZero }

// composeAbortReportedZero reads the project of an up that returned no error,
// and gives the Runtime error of an abort when a dependency's crash ended that
// up before compose started the app. Compose can report 0 for such an up:
// docker compose v5.1.3 stops the project on the dependency's exit in a
// goroutine that assigns the up's own err, and the up waits for that goroutine
// before it returns the failed start's err, so a stop that ends after the
// start failed turns the up's error into nil. Read as a clean end, that up
// passed the retry by, and the run ended on the crash the retry is for; keploy
// mock took it for a test command that succeeded.
//
// An app that ran never reads as an abort, nor does an up the run's own stop
// ended: the stop does not wait on this read, and the run's end reads that up.
func (a *App) composeAbortReportedZero(ctx context.Context, cmdErr utils.CmdError) utils.CmdError {
	if cmdErr.Err != nil || ctx.Err() != nil || !a.readsComposeProject() {
		return cmdErr
	}
	states := a.composeProjectStates(ctx)
	if !composeDependencyStoppedTheApp(a.composeService, states) {
		return cmdErr
	}
	a.logger.Info("docker compose reported 0 for an up a dependency's crash ended before it started the application; keploy reads it as that failed up",
		zap.String("service", a.composeService))
	return utils.CmdError{Type: utils.Runtime, Err: &composeReportedZero{states: states}}
}

// composeEndReadFailures is how many reads the daemon answers with a failure
// end composeEndRead. A count, not a time: no clock decides how an up ended,
// and a daemon that keeps failing a read, which nothing else would end now
// that the up has returned, must not hold the run.
const composeEndReadFailures = 3

// composeEndRead runs read, a read of how a compose up ended, until the daemon
// answers or ctx ends (composeAbortRead): one that runs past its cap is read
// again with a longer one, so a daemon slower than any cap is still read.
//
// It also ends, with what read last left, when there is no answer to wait
// for: the daemon refuses the read, which reading it again does not change,
// or fails it composeEndReadFailures times, or keploy's client cannot reach it
// (composeAbortLost). Its error is ctx's or a lost daemon's, nil otherwise, so
// a caller takes the answer from what read left.
func (a *App) composeEndRead(ctx context.Context, what string, read func(context.Context) error) error {
	failed := 0
	return a.composeAbortRead(ctx, what, func(rctx context.Context) error {
		err := read(rctx)
		switch {
		case err == nil, errors.Is(rctx.Err(), context.DeadlineExceeded), composeAbortCannotDial(err):
			// An answer; or none yet, past its cap, which composeAbortRead
			// reads again; or a failed dial, which it counts.
			return err
		case composeAbortDaemonRefuses(err):
			a.logger.Debug("the docker daemon refused keploy's read of "+what, zap.Error(err))
			return nil
		}
		if failed++; failed >= composeEndReadFailures {
			a.logger.Debug("the docker daemon keeps failing keploy's read of "+what, zap.Int("reads", failed), zap.Error(err))
			return nil
		}
		return err
	})
}

// composeProjectStates is the project of keploy's agent's container as the
// daemon says, one-offs aside, read until it answers (composeEndRead). It
// reads the daemon, not `docker compose ps`: ps loads the project again
// without what the user's command gave the up (an assignment, an --env-file),
// so a file that needs it fails every ps.
//
// nil when composeEndRead ended with no answer, or when keploy's agent's
// container does not exist. An up that reported 0 then stands, and the run's
// end decides from the stopped containers (ComposeEndAfterReady); a failed up
// is read by ps (composeFailedUpStates).
func (a *App) composeProjectStates(ctx context.Context) []composeServiceState {
	var states []composeServiceState
	// Its error is ctx's or a lost daemon's, said by then: no answer either
	// way, and a read that failed left no states.
	_ = a.composeEndRead(ctx, "the compose project's states", func(rctx context.Context) (err error) {
		states, err = a.readComposeProjectStates(rctx)
		return err
	})
	return states
}

// composeFailedUpStates is the project of an up that failed, for the retry's
// decision and its removal of what crashed. The daemon's answer
// (composeProjectStates), so no clock decides that retry and a file `docker
// compose ps` cannot load does not lose it; `docker compose ps` when the daemon
// gave none: one keploy's client does not reach may be the one compose uses,
// and an up that failed before it made keploy's agent's container has no
// project to read.
func (a *App) composeFailedUpStates(ctx context.Context) []composeServiceState {
	if a.readsComposeProject() {
		if states := a.composeProjectStates(ctx); len(states) > 0 {
			return states
		}
	}
	return a.composeServiceStatesWithin(composePsStateBudget)
}

// readComposeAbortProject reads the compose project of keploy's agent's
// container: its label, "" when the container is gone or carries none.
func (a *App) readComposeAbortProject(ctx context.Context) (string, error) {
	info, err := a.docker.ContainerInspect(ctx, a.keployContainer)
	if cerrdefs.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Config == nil {
		return "", nil
	}
	return strings.TrimSpace(info.Config.Labels[composeProjectLabelKey]), nil
}

// readComposeProjectStates is one read of composeProjectStates; it fails on
// any read of the daemon that fails, and gives no states then.
func (a *App) readComposeProjectStates(ctx context.Context) ([]composeServiceState, error) {
	project, err := a.readComposeAbortProject(ctx)
	if err != nil || project == "" {
		return nil, err
	}
	list, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", composeProjectLabelKey+"="+project),
	)})
	if err != nil {
		return nil, err
	}
	states := make([]composeServiceState, 0, len(list))
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
			return nil, err
		}
		if info.ContainerJSONBase == nil || info.State == nil {
			return nil, fmt.Errorf("the daemon gave no state of container %s", c.ID)
		}
		states = append(states, composeServiceState{
			Service:  c.Labels[composeServiceLabelKey],
			State:    info.State.Status,
			ExitCode: info.State.ExitCode,
			ID:       c.ID,
		})
	}
	return states, nil
}
