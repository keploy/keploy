package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// KEPLOY STOPS A DOCKER COMPOSE APP ITSELF (P101-c), IN EVERY MODE.
//
// keploy runs its agent as a service of the app's compose stack, the app in
// the agent's network and pid namespaces. With --abort-on-container-exit the
// app's exit stopped the whole stack, keploy's agent with it, so a stop of the
// app left nothing to read of what it did as it stopped, and an exit of any
// other container (a one-shot migration's, exit 0 too) took the run down.
//
// So compose runs without the abort flags, the user's own too
// (withoutComposeAbortFlags), and keploy watches the app's container itself
// (watchComposeApp): an exit keploy did not ask for ends the run, as the
// abort did, with the app's exit code (APP_HALTED); so does an exit of
// keploy's agent's container after the app started, which names the agent
// (composeAgentDied); another container's exit (a one-shot migration's, exit
// 0 too) is compose's to show, and ends nothing. keploy stops the app
// (StopComposeApp: an interrupt, then a wait for its exit on events, a kill
// only when it made no progress on its way out, and the kill is said) and
// starts it again (StartComposeApp), while the agent and the app's
// dependencies run on.
//
// The app's container is the one compose made for the app's service in the
// project of keploy's agent (composeAppContainer), not the one a name reaches
// (P101-b-n), and keploy's interrupt is one the docker daemon takes for a
// stop (composeInterrupt), so its restart policy does not start it again
// (P101-c-A2 SF-3(b)).

// The labels compose puts on each container it makes for a service.
const (
	composeProjectLabelKey = "com.docker.compose.project"
	composeServiceLabelKey = "com.docker.compose.service"
	composeOneOffLabelKey  = "com.docker.compose.oneoff"
)

// composeAppState is what keploy knows of a compose app's container.
type composeAppState struct {
	// stopMu makes keploy's stops of the container one at a time: the stop
	// at a set's end and the teardown's can come at once (an interrupt as a
	// set ends), and a second interrupt would cut the app's shutdown short. The
	// second finds the container stopped by the first.
	stopMu sync.Mutex
	// stopping: keploy has asked the app to stop, so its exit is no halt.
	stopping atomic.Bool
	// halted is how the app's container exited when keploy had not asked
	// it to (watchComposeApp); nil while it has not.
	halted atomic.Pointer[composeHalt]
	// agentStopping: keploy has begun to stop the stack its agent runs in
	// (a halt's stop of the project, a down), so the agent's exit is
	// keploy's own, and no halt.
	agentStopping atomic.Bool
	// appKilledAt is when the daemon last logged a kill of the app's
	// container (a `docker kill` or `docker stop`, keploy's own among them),
	// on the daemon's clock, in Unix nanoseconds; 0: none since the run
	// began.
	appKilledAt atomic.Int64
	// project is the app's compose project, kept once read
	// (composeAppProject); nil while it has not been. Each run reads it
	// afresh.
	project atomic.Pointer[string]
	// appStartedAt is when the app's container first started in this run
	// (in this bring-up: a retry of the up begins anew), on the daemon's
	// clock, in Unix nanoseconds, kept once read (composeAppFirstStart);
	// 0 while keploy has not seen it start. keploy's own start of it again
	// between sets moves the container's StartedAt, never this.
	appStartedAt atomic.Int64
	// depCrash is the crash, before the app started, of a service the app
	// depends on, for which keploy stopped the stack as compose's abort
	// stopped it (composeAbortDied): the run's retry of the up reads it,
	// as it read compose's abort; nil when there is none.
	depCrash atomic.Pointer[composeHalt]
	// upReturned is closed when the run's `compose up` (this bring-up's)
	// has returned; nil while none runs. keploy's stops of the stack stop
	// what compose starts after them until then (stopComposeLateStarters).
	upReturned atomic.Pointer[chan struct{}]
	// stackStop is closed when keploy's stop of the stack for a crash
	// (depCrash) has taken every container it stops down; nil while there
	// has been none. The run's retry of the bring-up waits for it
	// (composeAwaitsTheStackStop).
	stackStop atomic.Pointer[chan struct{}]
	// stoppedRuns are, by container ID, the runs keploy's stops of the
	// stack took down, each by its StartedAt on the daemon's clock, in Unix
	// nanoseconds (composeStopsARun).
	stoppedRuns sync.Map
}

// THE APP'S CONTAINER IS THE ONE COMPOSE MADE FOR THE APP'S SERVICE (P101-b-n).
//
// keploy is given the app by a name (--container-name), which its setup finds
// in the compose file as a service's container_name or as the service's key
// (findContainerInServices). The key names no container: compose names a
// service's container <project>-<service>-<n> unless it sets container_name.
// And a name can reach a container that is not the app's: another stack's
// with that name, or one whose ID begins with it (the daemon takes a unique
// ID prefix for a name). keploy's stop at a set's end, read by that name,
// found no container and took that for an app it had stopped already, so the
// set's end was read with the app running and a call it made after the read
// was judged by no one, while the set passed; keploy record's stop missed it
// the same way, so its recording lacked the app's shutdown too. Or keploy
// interrupted the other container in the app's place, or took its exit for
// the app's halt.
//
// So keploy stops, starts and watches the container compose made for the
// app's service (composeService, the service keploy's setup wired to its
// agent) in the project of keploy's agent, as compose labels them. The
// agent's container is named by keploy, one per run, so its compose label is
// the project the up ran. A service with no such container, or with several
// (a scaled service: keploy stops one app), and a project keploy cannot read,
// are errors, which leave the set's end unread rather than read with the app
// running. With no service known (keploy did not set the compose file up), it
// is the name, as before.

// composeAppProject is the app's compose project: the compose label on
// keploy's agent's container (compose's own, in-process or not), kept once
// read; "" while it cannot be read. The inspect is asked again when it fails
// (composeProjectLabelAnswered), so it ends on the daemon's answer, its word
// that the agent's container is gone, ctx, or keploy's runnable-time bound
// on no answer: one failed read, taken for no project, lost the app's exit at
// the up's end, took a halted project down whole, keploy's agent with it,
// and failed the next set's start, the set lost as halted.
func (a *App) composeAppProject(ctx context.Context) string {
	if p := a.composeApp.project.Load(); p != nil {
		return *p
	}
	p := a.composeProjectLabelAnswered(ctx, a.keployContainer, "keploy cannot tell which container is the application's")
	if p != "" {
		a.composeApp.project.Store(&p)
	}
	return p
}

// composeAppContainer is the ID of the app's container: the one compose made
// for the app's service in the app's project (composeAppProject), one-offs
// (`compose run`) aside. With no service known, it is the name keploy was
// given, as before. The list is asked again when it fails (askDaemon), as
// composeAppProject's inspect is: no container of the service, or several,
// are the daemon's answer; a failed list is no answer.
func (a *App) composeAppContainer(ctx context.Context) (string, error) {
	if a.composeService == "" {
		return a.container, nil
	}
	project := a.composeAppProject(ctx)
	if project == "" {
		return "", fmt.Errorf("keploy could not read the compose project of keploy's agent's container %q, so it cannot tell which container is the application's", a.keployContainer)
	}
	list, err := askDaemon(ctx, a.logger, fmt.Sprintf("the containers of the application's service %q (with no answer, keploy cannot tell which container is the application's)", a.composeService),
		func(ctx context.Context) ([]container.Summary, error) {
			return a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
				filters.Arg("label", composeProjectLabelKey+"="+project),
				filters.Arg("label", composeServiceLabelKey+"="+a.composeService),
			)})
		})
	if err != nil {
		return "", fmt.Errorf("keploy could not list the containers of the application's service %q in compose project %q: %w", a.composeService, project, err)
	}
	var ids []string
	for _, c := range list {
		if !isComposeOneOffLabel(c.Labels) {
			ids = append(ids, c.ID)
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("compose project %q has no container of the application's service %q", project, a.composeService)
	}
	return "", fmt.Errorf("the application's service %q runs %d containers in compose project %q; keploy stops and starts one application", a.composeService, len(ids), project)
}

// isComposeOneOffLabel reports whether labels are those of a one-off
// container compose made (`compose run`): not the app keploy runs.
func isComposeOneOffLabel(labels map[string]string) bool {
	return strings.EqualFold(labels[composeOneOffLabelKey], "true")
}

// isComposeAppEvent reports whether m, an event the watch asked for, is of
// the app's container (composeAppContainer), told from the labels the event
// carries: of the app's service, in the app's project, not a one-off. With no
// service known, the watch asked for the container by its name. A project
// keploy cannot read takes no event for the app's: the stop at the set's end
// reads the app's container itself.
func (a *App) isComposeAppEvent(ctx context.Context, m events.Message) bool {
	if a.composeService == "" {
		return true
	}
	attrs := m.Actor.Attributes
	if attrs[composeServiceLabelKey] != a.composeService || isComposeOneOffLabel(attrs) {
		return false
	}
	project := a.composeAppProject(ctx)
	if project == "" {
		a.logger.Debug("keploy could not read the application's compose project; an exit of its service's container is not taken for the application's",
			zap.String("container", m.Actor.ID))
		return false
	}
	return attrs[composeProjectLabelKey] == project
}

// composeHalt is an exit of the app's container keploy did not ask for, or
// of keploy's agent's after the app started, or a failed setup of the agent
// compose's retry started again (agentSetup).
type composeHalt struct {
	exitCode int
	// agent is keploy's agent's container when the halt is its exit, not
	// the app's: exitCode is then the agent's.
	agent string
	// agentSetup is why keploy could not set up again the agent compose's
	// retry of the bring-up started again (composeAgentSetupFailed); agent
	// is then its container, and exitCode -1: no container exited.
	agentSetup error
	// oomKilled: the daemon says the app's container ran out of memory.
	oomKilled bool
	// startedAt is when the run of the app that exited began, on the
	// daemon's clock, in Unix nanoseconds; 0: not said.
	startedAt int64
	// service is the compose service whose container's exit ended the run
	// under the user's abort flags (composeAbortDied), with its exit code
	// (serviceCode) and those flags; "": the halt is the app's or the
	// agent's. exitCode is then the run's: --exit-code-from's service's
	// (codeFrom) when another's exit ended it (composeHaltCodeFrom).
	service, codeFrom, flags string
	serviceCode              int
	// beforeApp: service's exit came before the app's container started.
	beforeApp bool
	// bringUps: service is one the app depends on, and it crashed before
	// the app started on each of this many bring-ups (the first and keploy's
	// retries of it), so the run ends on its crash; 0: no retry was taken.
	bringUps int
	// stackNotStopped: service is one the app depends on, it crashed before
	// the app started, and keploy could not stop the stack to retry the
	// bring-up (composeKeepsAnUnstoppedDepCrash), so the run ends on its
	// crash.
	stackNotStopped bool
}

func (h composeHalt) Error() string {
	if h.agentSetup != nil {
		return fmt.Sprintf("keploy's agent container %q, which docker compose started again in keploy's retry of the bring-up (a dependency crashed before the application started), could not be set up again: %v. "+
			"The application waits on that agent, and it holds none of the run's mocks or settings, so keploy does not release the application to it, and the run ends. "+
			"Next step: read keploy's log above for why the setup failed (the agent's own last lines among it), fix that and run keploy again; "+
			"if the agent ran but refused its setup, report it to keploy with that log", h.agent, h.agentSetup)
	}
	if h.agent != "" {
		return fmt.Sprintf("keploy's agent container %q exited with code %d after the application started, and keploy had not stopped it: "+
			"the application runs in the agent's network and process namespaces, so the run ends. "+
			"Next step: find why the agent stopped (keploy's log above holds its last lines; 137 with no kill of yours is most often docker's memory limit), "+
			"fix that and run keploy again; if the agent crashed, report it to keploy with that log", h.agent, h.exitCode)
	}
	if h.service != "" {
		when := "after"
		if h.beforeApp {
			when = "before"
		}
		msg := fmt.Sprintf("the container of compose service %q exited with code %d %s the application started, and the command's %s ends the run on it", h.service, h.serviceCode, when, h.flags)
		if h.stackNotStopped {
			msg = fmt.Sprintf("the container of compose service %q, which the application depends on, crashed (exit code %d) before the application started, and keploy could not stop the stack to retry the bring-up (the docker daemon did not list the project's containers), so the command's %s ends the run on it. "+
				"Next step: check that the docker daemon answers (docker ps), read that service's log above, and run keploy again", h.service, h.serviceCode, h.flags)
		}
		if h.bringUps > 0 {
			msg = fmt.Sprintf("the container of compose service %q, which the application depends on, crashed (its last exit code %d) before the application started, on each of keploy's %d bring-ups of the stack, and the command's %s ends the run on it. "+
				"Next step: read that service's log above, and fix what makes it fail to start", h.service, h.serviceCode, h.bringUps, h.flags)
		}
		if h.codeFrom != "" {
			msg += fmt.Sprintf("; the run's exit code is that of service %q, %d", h.codeFrom, h.exitCode)
		}
		return msg
	}
	if h.oomKilled {
		return fmt.Sprintf("the application's container exited with code %d (out of memory: docker's memory limit for it was reached), and keploy had not stopped it", h.exitCode)
	}
	return fmt.Sprintf("the application's container exited with code %d, and keploy had not stopped it", h.exitCode)
}

// Is makes a halt models.ErrAppExitedOnItsOwn.
func (h composeHalt) Is(target error) bool { return target == models.ErrAppExitedOnItsOwn }

// appError is the run's end on the halt: the app's exit, as compose's abort
// on it ended the run (APP_HALTED), with its exit code. The agent's exit is
// never a clean stop of the app, whatever its code.
func (h composeHalt) appError(appLogs string) models.AppError {
	if h.exitCode != 0 || h.agent != "" {
		return models.AppError{AppErrorType: models.ErrUnExpected, Err: h, AppLogs: appLogs, ExitCode: h.exitCode}
	}
	return models.AppError{AppErrorType: models.ErrAppStopped, AppLogs: appLogs, ExitCode: 0}
}

// watchComposeApp watches the app's container for its exits until ctx ends,
// or until the returned stop is called. An exit keploy did not ask for
// (stopping unset), of a container that is not running again, is a halt: it
// is kept (composeApp.halted) and end is called, once, to end the run. So is
// an exit of keploy's agent's container after the app started
// (composeAgentDied), on the same stream, so the two are read in the order
// the daemon logged them. The
// stream is the daemon's own events, from the moment the watch begins on the
// daemon's clock (composeWatchSince), read before watchComposeApp returns and
// so before compose's up starts; a stream that breaks is asked for again
// from the last event it gave, so no exit is missed. It is of the containers
// of the app's service, as compose labels them (composeAppDieFilter): the
// app's container does not exist yet when the watch begins, and its name is
// compose's; the app's is told among them by their labels
// (isComposeAppEvent).
func (a *App) watchComposeApp(ctx context.Context, end func()) (stop func()) {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	since := a.composeWatchSince(wctx, "the exits of the application's container")
	go func() {
		defer close(done)
		for wctx.Err() == nil {
			msgs, errs := a.docker.Events(wctx, events.ListOptions{
				Since:   fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
				Filters: a.composeAppDieFilter(),
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
					a.logger.Debug("the docker events stream for the application's container broke; asking for it again from its last event",
						zap.String("container", a.container), zap.Error(err))
					break stream
				case m := <-msgs:
					if m.TimeNano > 0 {
						since = time.Unix(0, m.TimeNano)
					}
					if a.composeDied(wctx, m) {
						end()
						return
					}
				}
			}
			// A daemon that refuses the stream at once is asked again at a
			// walking pace, not in a spin.
			select {
			case <-wctx.Done():
				return
			case <-time.After(composeDaemonAskPace):
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// composeWatchSince is the moment the app's watch reads the daemon's events
// from, on the daemon's own clock. The daemon stamps each event by its own
// clock; it sends a stream every event it logs once the stream is
// subscribed, whatever its since, and replays to it those it logged before
// that are stamped at or after its since (of the last 256 it logged, of any
// container). The subscription is made after
// the daemon has answered the ask, so the up's first events can come before
// it, and a broken stream's are logged while it is asked for again: both
// reach the watch only by that replay. On the daemon's clock, read before
// the up starts, every event of this run is replayed and none of a run
// before it. From keploy's clock, a daemon behind it dropped them (an app
// that died at once was never seen, and compose's attached up, the agent
// still up, never returned), and a daemon ahead of it replayed the exits of
// the run before (their containers gone, each read as this run's halt).
//
// The up starts only once this read is done, so it never waits on a daemon
// that will not say its time: one that refuses the read, gives no time,
// fails it composeWatchSinceReads times, or cannot be reached
// (composeAbortLost) leaves keploy's clock, said at Warn: it is as right as
// the daemon's clock is close to keploy's.
func (a *App) composeWatchSince(ctx context.Context, what string) time.Time {
	var since time.Time
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
		// Bounded by a count, not a time: a daemon that keeps failing this
		// one request (a 5xx, a read past its cap) must not hold the up's
		// start.
		if reads >= composeWatchSinceReads {
			unsaid = err
			return nil
		}
		return err
	})
	if !since.IsZero() {
		return since
	}
	if unsaid == nil {
		unsaid = err
	}
	if ctx.Err() == nil {
		a.logger.Warn("keploy could not read the docker daemon's time; it reads the daemon's events for "+what+" from keploy's clock instead, which is as right as the daemon's clock is close to keploy's",
			zap.Int("reads", reads), zap.Error(unsaid))
	}
	return time.Now()
}

// composeWatchSinceReads is the count of reads of the daemon's time from
// which a read that fails makes the watch take keploy's clock; one that
// fails before it is read again. Every read counts, failed dials too, so a
// failed dial, another, then a 500 take keploy's clock at the third read.
// Two failures are apart from it: a refusal takes keploy's clock at any
// read, and a failed dial is read again at any read, until
// composeAbortUnreachableAsks of them in a row take keploy's clock.
const composeWatchSinceReads = 3

// composeDaemonAskPace is the pace at which keploy asks the daemon again for
// a stream or a wait that it failed at once: a walking pace, not a spin. It
// bounds nothing: what keploy waits for is the daemon's answer.
const composeDaemonAskPace = 200 * time.Millisecond

// composeAppDieFilter is what the watch asks the daemon for: the die and
// kill events of the containers of the app's service, or, with no service
// known, of the container keploy was given by name, as before. With keploy's
// agent's container known, it is the containers' events with no label: the
// daemon ANDs label filters, so one stream of the app's service and the
// agent's container cannot be asked for by label, and two streams would lose
// the order the daemon logged their events in. The watch tells them apart
// (isComposeAgentEvent, isComposeAppEvent).
func (a *App) composeAppDieFilter() filters.Args {
	args := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("event", string(events.ActionDie)),
		filters.Arg("event", string(events.ActionKill)),
	)
	switch {
	case a.composeService == "":
		args.Add("container", a.container)
		if a.keployContainer != "" {
			args.Add("container", a.keployContainer)
		}
	case a.keployContainer == "":
		args.Add("label", composeServiceLabelKey+"="+a.composeService)
	}
	return args
}

// composeDied is the watch's decision on m: an exit of keploy's agent's
// container (composeAgentDied) or of the app's (composeAppDied) that ends the
// run. A kill of the app's container is kept (appKilledAt) and ends nothing.
func (a *App) composeDied(ctx context.Context, m events.Message) bool {
	if a.isComposeAgentEvent(m) {
		return m.Action == events.ActionDie && a.composeAgentDied(ctx, m)
	}
	if m.Action == events.ActionKill {
		if a.isComposeAppEvent(ctx, m) {
			at := m.TimeNano
			if at <= 0 {
				at = time.Now().UnixNano()
			}
			a.composeApp.appKilledAt.Store(at)
		}
		return false
	}
	return a.composeAppDied(ctx, m) || a.composeAbortDied(ctx, m)
}

// isComposeAgentEvent reports whether m is of keploy's agent's container,
// by the name keploy gave it, one per run (a prior phase's agent in the same
// project has another).
func (a *App) isComposeAgentEvent(m events.Message) bool {
	return a.keployContainer != "" && (m.Actor.Attributes["name"] == a.keployContainer || m.Actor.ID == a.keployContainer)
}

// KEPLOY'S AGENT'S EXIT AFTER THE APP STARTED ENDS THE RUN, AND NAMES THE AGENT.
//
// The app runs in the agent's network and pid namespaces. With compose's
// abort flags gone, nothing ended the run when the agent's container exited
// with the app up: the app, killed with the namespace (137), was named as
// the halt, and an agent that died while keploy had stopped the app between
// sets was found only when the next set's start failed. So an exit of the
// agent's container that keploy did not ask for (agentStopping unset), once
// the app has started in this run, is a halt that names the agent, its exit
// code and the next step. Before the app has started, compose's up decides:
// it fails on the agent's health, and a retry may recreate the agent.
//
// The app's die comes before the agent's: the kernel ends the namespace's
// other processes before its first one. So an app killed with SIGKILL, not
// for its own memory, and with no kill of it in the daemon's log
// (composeHaltKilledWithItsAgent), is read as the agent's end once the
// agent's container is down (composeAgentNamesTheHalt).
//
// AN APP KILLED FROM OUTSIDE DOCKER ENDS THE RUN AT ONCE TOO.
//
// The agent of such an app stays up, and keploy waited for docker to say
// it was down for utils.StopStallBudget (60 s on Linux) of its runnable
// time before the app's own halt stood. The agent's health check now tells
// the two apart as soon as it is asked: the kernel kills the namespace's other
// processes only once every thread of its first process is exiting
// (zap_pid_ns_processes, from the exit of its last thread), and a thread
// that is exiting never runs the agent's code again. So an answer of the
// agent after the app's die proves the agent was not what killed the app,
// and the app's halt stands at once; an agent that is exiting answers
// nothing, and docker says its container is down within moments. The stall
// bound is left only as the guard on a daemon and an agent that both say
// nothing at all.

// composeAgentDied reports whether m, a die of keploy's agent's container,
// ends the run, keeping its halt if so: keploy had not begun to stop the
// agent, the container is not running again, the app has started in this
// run, and no halt is kept yet (one of the app's own stands).
func (a *App) composeAgentDied(ctx context.Context, m events.Message) bool {
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	info, read := a.readComposeContainer(ctx, a.keployContainer, "keploy decides the agent's exit on the event")
	if ctx.Err() != nil || a.composeApp.agentStopping.Load() || (read && info.State.Running) {
		return false
	}
	if !a.composeAppStarted(ctx) {
		a.logger.Debug("keploy's agent's container exited before the application started; docker compose up decides the run",
			zap.String("agent", a.keployContainer))
		return false
	}
	code := -1
	if read {
		code = info.State.ExitCode
	} else if c, err := strconv.Atoi(m.Actor.Attributes["exitCode"]); err == nil {
		code = c
	}
	if !a.composeApp.halted.CompareAndSwap(nil, &composeHalt{exitCode: code, agent: a.keployContainer}) {
		return false
	}
	a.logger.Info("keploy's agent's container exited after the application started, and keploy had not stopped it: ending the run",
		zap.String("agent", a.keployContainer), zap.Int("exitCode", code))
	return true
}

// composeAppStarted reports whether the app's container has started in this
// run (composeAppFirstStart).
func (a *App) composeAppStarted(ctx context.Context) bool {
	return a.composeAppFirstStart(ctx) != 0
}

// composeAppFirstStart is when the app's container first started in this
// run, on the daemon's clock, in Unix nanoseconds; 0 when it has not
// started. docker says when the container last started; compose down removes
// the project's containers at each run's end, so a start it says is this
// run's, and the first one keploy reads is kept (appStartedAt): keploy's own
// stop of the app at a set's end reads it before it starts the app again, so
// a later start keploy gave it never stands for the first.
func (a *App) composeAppFirstStart(ctx context.Context) int64 {
	if at := a.composeApp.appStartedAt.Load(); at != 0 {
		return at
	}
	id, err := a.composeAppContainer(ctx)
	if err != nil {
		return 0
	}
	info, read := a.readComposeContainer(ctx, id, "keploy takes the application for one that never started")
	if !read {
		return 0
	}
	if at := daemonNanos(info.State.StartedAt); at != 0 {
		a.composeApp.appStartedAt.CompareAndSwap(0, at)
	}
	return a.composeApp.appStartedAt.Load()
}

// composeExitBeforeApp reports whether an exit the daemon recorded at
// exitedAt (its FinishedAt, or its die event's time, on the daemon's clock;
// 0: not said) came before the app's container first started in this run:
// decided by the daemon's own instants, never by what runs when keploy reads,
// which a start compose's up gave since (it starts a waiting service as soon
// as the dependency's exit meets its condition) would answer wrongly. With no
// instant said, the app has not started only if it has not started yet.
func (a *App) composeExitBeforeApp(ctx context.Context, exitedAt int64) bool {
	started := a.composeAppFirstStart(ctx)
	return started == 0 || (exitedAt != 0 && exitedAt < started)
}

// daemonNanos is a time the daemon gives (RFC 3339), in Unix nanoseconds; 0
// when it gives none.
func daemonNanos(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// composeHaltKilledWithItsAgent reports whether h is an exit of the app that
// its pid namespace's end gives it: SIGKILL (137), not for its own memory,
// and with no kill of this run of the app in the daemon's log (appKilledAt,
// from h's start on; with no start known, any kill).
func (a *App) composeHaltKilledWithItsAgent(h *composeHalt) bool {
	if h == nil || h.agent != "" || h.service != "" || h.oomKilled || h.exitCode != 128+int(syscall.SIGKILL) {
		return false
	}
	killed := a.composeApp.appKilledAt.Load()
	return killed == 0 || killed < h.startedAt
}

// composeAgentNamesTheHalt makes a halt of an app killed with its pid
// namespace (composeHaltKilledWithItsAgent) the agent's, with the agent's
// exit code, once docker says the agent's container is down: keploy reads it
// again at a walking pace until docker says so, a kill of the app comes to
// light in the daemon's log, or ctx ends. An agent that answers its health
// check meanwhile (composeAgentAnswered) is alive: the app was killed from
// outside docker (a kill of its process on the host), and its halt stands,
// at once. utils.StopStallBudget of keploy's runnable time with neither the
// daemon saying the agent is down nor the agent answering leaves the app's
// halt standing too: a guard on a daemon and an agent that both hang.
func (a *App) composeAgentNamesTheHalt(ctx context.Context) {
	if a.keployContainer == "" || !a.composeHaltKilledWithItsAgent(a.composeApp.halted.Load()) {
		return
	}
	actx, stopAsking := context.WithCancel(ctx)
	defer stopAsking()
	answered := a.composeAgentAnswered(actx)
	meter := stall.NewMeter(nil, utils.StopStallBudget)
	defer meter.Stop()
	for a.composeHaltKilledWithItsAgent(a.composeApp.halted.Load()) {
		info, read, gone := a.inspectContainer(ctx, a.keployContainer,
			fmt.Sprintf("keploy's agent container %q (with no answer, the application's exit ends the run)", a.keployContainer))
		if gone || (read && !info.State.Running) {
			code := -1
			if read {
				code = info.State.ExitCode
			}
			a.composeApp.halted.Store(&composeHalt{exitCode: code, agent: a.keployContainer})
			a.logger.Info("the application was killed with keploy's agent's container, which exited, and keploy had not stopped it: ending the run",
				zap.String("agent", a.keployContainer), zap.Int("exitCode", code))
			return
		}
		if meter.Expired() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-answered:
			a.logger.Info("the application's container was killed while keploy's agent runs and answers: something outside docker killed it, and its exit ends the run",
				zap.String("container", a.container), zap.String("agent", a.keployContainer))
			return
		case <-time.After(composeDaemonAskPace):
		}
	}
}

// composeAgentAnswered is closed once keploy's agent answers its health
// check (composeAgentAnswers), asked at a walking pace until it does or ctx
// ends. It is asked apart from the daemon's reads, so a request that hangs
// (an address that no longer leads anywhere) holds none of them.
func (a *App) composeAgentAnswered(ctx context.Context) <-chan struct{} {
	answered := make(chan struct{})
	go func() {
		for ctx.Err() == nil {
			if a.composeAgentAnswers(ctx) {
				close(answered)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(composeDaemonAskPace):
			}
		}
	}()
	return answered
}

// composeAgentProbe asks keploy's agent for its health: with no proxy (the
// agent is this host's, or a container's of keploy's own pod) and no kept
// connection, so each answer is a fresh one of the agent's process.
var composeAgentProbe = &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}

// composeAgentAnswers reports whether keploy's agent answers its health
// check (AgentURI/health, which takes no token) with 200: its process runs.
// An agent keploy has no address for answers nothing. The request's error
// is not logged: it holds the URI, which may hold a password.
func (a *App) composeAgentAnswers(ctx context.Context) bool {
	uri := a.agentAddress()
	if uri == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(uri, "/")+"/health", nil)
	if err != nil {
		return false
	}
	res, err := composeAgentProbe.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// composeAppDied reports whether m, a die event of a container of the app's
// service, is an exit of the app's container keploy did not ask for, keeping
// it if so.
func (a *App) composeAppDied(ctx context.Context, m events.Message) bool {
	// Told before the stop lock, from the event's own labels: an exit that is
	// not the app's container's has nothing to decide.
	if !a.isComposeAppEvent(ctx, m) {
		return false
	}
	// Decided under stopMu, as keploy's stops and starts of the container
	// are made: a stop is either not begun (the exit is a halt) or through
	// (stopping says the exit was keploy's), and a start either not begun or
	// through (the container runs). Unserialised, one exit got two answers:
	// a halt to the watch and a clean stop to keploy's stop begun as the app
	// died, or a halt of an app keploy had just started again.
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	// An event of a run of the container that keploy has started again since
	// is not this run's end. The container read is the one that died, by
	// the ID the event gives.
	ref := a.container
	if a.composeService != "" && m.Actor.ID != "" {
		ref = m.Actor.ID
	}
	info, read := a.readComposeContainer(ctx, ref, "keploy decides the die on the event")
	// A watch that has ended decides nothing: its die is read by keploy's
	// next stop of the container, which finds it down.
	if ctx.Err() != nil {
		return false
	}
	if read && info.State.Running {
		return false
	}
	// A die sent before the container's current run began is an earlier
	// run's (composeDieOfAnEarlierRun): the current run's own die is still
	// to come, and decides.
	if read && composeDieOfAnEarlierRun(m, info.State) {
		return false
	}
	// Read after the inspect: a stop keploy began meanwhile is its own, and
	// so is a run keploy's stop of the stack took down, read after the run's
	// retry of the bring-up began (composeStoppedRun).
	if a.composeApp.stopping.Load() || (read && a.composeStoppedRun(info.ID, info.State)) {
		return false
	}
	// The halt's exit code is the run's that is down now, as the daemon
	// records it; the event's only when the container cannot be read.
	code := -1
	if read {
		code = info.State.ExitCode
	} else if c, err := strconv.Atoi(m.Actor.Attributes["exitCode"]); err == nil {
		code = c
	}
	h := &composeHalt{exitCode: code}
	if read {
		h.oomKilled, h.startedAt = info.State.OOMKilled, daemonNanos(info.State.StartedAt)
	}
	// A halt kept already (keploy's agent's) stands; its end is under way.
	if !a.composeApp.halted.CompareAndSwap(nil, h) {
		return false
	}
	a.logger.Info("the application's container exited, and keploy had not stopped it: ending the run",
		zap.String("container", a.container), zap.String("id", m.Actor.ID), zap.Int("exitCode", code))
	return true
}

// A DIE IS DECIDED ON THE DAEMON'S ANSWER, HOWEVER SLOW IT IS TO GIVE IT.
//
// The watch's read of the container a die names had a wall-clock budget of
// 2 s. On a loaded host the daemon's answer took
// longer, the read failed, and a die the watch could not place was taken for
// a halt whenever no stop of keploy's was in progress: the late die of
// keploy's own stop, read after keploy had started the container again for
// the next set (which ends that stop), ended the run, and the set was lost,
// as through the event timing composeDieOfAnEarlierRun closes. A read the
// daemon fails says nothing of the container, so the watch asks again; what
// bounds the wait for the daemon's answer is keploy's runnable time
// (utils.StopStallBudget of it, stall.Meter), the bound on no news at all of
// keploy's stop, and the watch's own end, never a number of seconds. The
// start of the next set's run reads the container the same way: with the
// 2 s budget, a start of a container whose stop the daemon had recorded,
// read too slowly, was told not anew, and the set was charged the earlier
// sets' capture losses, as by an app keploy's stop had not ended.

// readComposeContainer is the container ref as the daemon has it now; read
// reports whether the daemon said. It asks as askDaemon does (inspectContainer):
// until the daemon answers, says it has no such container (removed: it has no
// more to say of it), utils.StopStallBudget of keploy's runnable time passes
// with no answer, or ctx ends. unread says, in the log, what keploy does when
// the daemon never says.
func (a *App) readComposeContainer(ctx context.Context, ref, unread string) (info container.InspectResponse, read bool) {
	info, read, _ = a.inspectContainer(ctx, ref, fmt.Sprintf("the application's container %q (with no answer, %s)", ref, unread))
	return info, read
}

// A DIE OF AN EARLIER RUN IS NOT THE CURRENT RUN'S END.
//
// The die of keploy's own stop can reach the watch after keploy has started
// the container again (the daemon records the stop, which keploy's stop waits
// for, before it sends the die, and the stream is late to deliver it). When
// the new run has exited too by the time the watch reads that die, the
// inspect reads a container down with no stop of keploy's in progress, and
// the die was taken for the new run's halt, with the exit code of keploy's
// interrupt (0 or 130) for an app that had crashed: the report misled the
// user's debugging, and the new run's own die was never read. The daemon
// sends a container's die while it holds the container, before any start of
// it can begin the next run (moby's handleContainerExit logs it before it
// lets the container go), so a die sent before the current run began (its
// StartedAt) is an earlier run's, and is dropped; the current run's own die
// comes after it on the stream, and decides. And the halt's exit code is the
// one the inspect reads, of the run that is down now, not the event's.

// composeDieOfAnEarlierRun reports whether m, a die of the container state
// reads, was sent before the container's current run began: an earlier
// run's. A die or a run whose time the daemon does not give is not. A die
// the daemon gives only to the second (no TimeNano) is compared to the
// second: one sent in the second the run began may be the run's own.
func composeDieOfAnEarlierRun(m events.Message, state *container.State) bool {
	began, err := time.Parse(time.RFC3339Nano, state.StartedAt)
	if err != nil || began.IsZero() {
		return false
	}
	switch {
	case m.TimeNano != 0:
		return time.Unix(0, m.TimeNano).Before(began)
	case m.Time != 0:
		return m.Time < began.Unix()
	}
	return false
}

// composeHaltEnd is the run's end on a halt of the app: halt is closed, the
// app's exit is no longer taken for one (stopping), and the project is
// stopped, as compose's abort stopped it, or taken down (composeDown) when
// keploy cannot tell it. Both the watch and the run, on an up that returned on
// the app's own exit, may come to it at once: it runs once.
func (a *App) composeHaltEnd(ctx context.Context, halt chan struct{}, composeDown func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			// Before the halt is said (close): the run reads it then.
			a.composeAgentNamesTheHalt(ctx)
			close(halt)
			a.composeApp.stopping.Store(true)
			a.composeApp.agentStopping.Store(true)
			if !a.stopComposeProject(context.WithoutCancel(ctx)) {
				composeDown()
			}
			// Once the stack is down: --exit-code-from's service's code.
			a.composeHaltCodeFrom(context.WithoutCancel(ctx))
		})
	}
}

// composeTeardownWait is how composeAwaitTeardown ended.
type composeTeardownWait int

const (
	// composeNoWait: the up did not return on keploy's stop of the app.
	composeNoWait composeTeardownWait = iota
	// composeTeardownCame: keploy's teardown ended the run.
	composeTeardownCame
	// composeHaltCame: the app, started again by keploy since, exited on
	// its own.
	composeHaltCame
)

// composeAwaitTeardown holds the run when its `compose up` returned because
// keploy stopped the app itself (stopping, with no halt): until keploy's
// teardown ends the run (ctx), or the app keploy started again since halts
// (halt, closed by end, the watch's end). An up attached to the app alone
// (`docker compose up app`) returns as soon as the app has stopped, and the
// run, so ended, took the project down (its deferred ComposeDown) with
// keploy's agent in it, while the recording was still draining what the agent
// had captured, or before the replay had read the set's end from it.
// Compose's own up, its abort flags dropped, keeps the project up only while
// something attached to it runs; the project's end is keploy's: the watch's on a halt, the teardown's otherwise.
//
// Such an up also returns on the app's own exit, at once, which the watch may
// not have taken yet (its event, its inspect): the container says, as the
// watch would, and the run ends the project as the watch would (end, which
// runs once). Left to the watch, the run ended as a clean stop, and its down
// took keploy's agent down mid-drain, or the halt's exit code was lost.
func (a *App) composeAwaitTeardown(ctx context.Context, halt <-chan struct{}, end func()) composeTeardownWait {
	// Decided under stopMu, as the watch decides (composeAppDied); end runs
	// outside it: it stops the project, keploy's agent with it, which waits
	// for the drain that keploy's own stop, waiting on stopMu, comes before.
	a.composeApp.stopMu.Lock()
	halted := a.composeApp.halted.Load() != nil
	stopping := a.composeApp.stopping.Load()
	var h composeHalt
	exited := false
	abortExited := false
	if !halted && !stopping {
		if h, exited = a.composeAppExited(ctx); exited {
			a.composeApp.halted.CompareAndSwap(nil, &h)
		} else {
			// An up of the user's services alone returns once they
			// exit: their exit ends the run as the user's flags say.
			abortExited = a.composeAbortExited(ctx)
		}
	}
	a.composeApp.stopMu.Unlock()
	if abortExited {
		end()
		return composeHaltCame
	}
	if halted {
		// Kept by the watch, or by keploy's stop finding the app exited
		// (which does not end the run itself): the project ends with it,
		// once (end), not on the watch's event alone, which a daemon can
		// drop under load.
		end()
		return composeNoWait
	}
	if !stopping {
		if !exited {
			// Not an exit of the app (an up that failed leaves it only
			// created, or the container cannot be read): the up's own end
			// stands, as before.
			return composeNoWait
		}
		a.logger.Info("the application's container exited, and keploy had not stopped it: ending the run",
			zap.String("container", a.container), zap.Int("exitCode", h.exitCode))
		end()
		return composeHaltCame
	}
	a.logger.Debug("docker compose up returned on keploy's stop of the application; the project stays up for keploy's teardown",
		zap.String("container", a.container))
	select {
	case <-ctx.Done():
		return composeTeardownCame
	case <-halt:
		// The halt's end is through before the run reads it: its stop
		// of the stack, and the run's exit code it reads after.
		end()
		return composeHaltCame
	}
}

// composeAppExited is the app's container's exit, when it has exited: not
// one compose only created (an up that failed leaves it so), nor one that
// runs, nor one the daemon never describes. The read asks the daemon again
// when it fails (readComposeContainer): with a 2 s budget, the read of an app
// that had exited on its own, slow on a loaded daemon, was taken for no exit,
// and the run ended as on a clean stop, its down taking keploy's agent down
// mid-drain and the halt's exit code lost.
func (a *App) composeAppExited(ctx context.Context) (composeHalt, bool) {
	id, err := a.composeAppContainer(ctx)
	if err != nil {
		return composeHalt{}, false
	}
	info, read := a.readComposeContainer(ctx, id, "keploy takes the up's end for a clean one")
	if !read || info.State.Running {
		return composeHalt{}, false
	}
	if st := info.State.Status; st != "exited" && st != "dead" {
		return composeHalt{}, false
	}
	return composeHalt{exitCode: info.State.ExitCode, oomKilled: info.State.OOMKilled, startedAt: daemonNanos(info.State.StartedAt)}, true
}

// stopComposeProject stops every container of the app's compose project that
// still runs, keploy's agent among them, as compose's --abort-on-container-exit
// stopped them when the app exited: each is asked to stop (SIGTERM), with its
// own stop grace. The agent, so asked, waits for keploy to drain what it
// captured, and goes when that drain is through (its grace under keploy record
// is 24 h, a bound on an agent that never goes, not when one does), as it did
// under the abort. `compose up` then returns, its containers gone.
// It is false when it could not tell or list the project. The project is
// the app's (composeAppProject), not the label of the container a name
// reaches, which can be another stack's; with no service known, that label,
// as before.
func (a *App) stopComposeProject(ctx context.Context) bool {
	stopped, _ := a.stopComposeStack(ctx)
	return stopped
}

// stopComposeStack is stopComposeProject, and lateStopped is closed once the
// stops of the containers it took down as late starters have ended too
// (stopComposeLateStarters): after the run's `compose up` has returned, as
// compose's abort ended its stop of late starters before its up returned.
// It is closed at once when no late starter can be stopped.
func (a *App) stopComposeStack(ctx context.Context) (stopped bool, lateStopped <-chan struct{}) {
	none := make(chan struct{})
	close(none)
	project := a.composeAppProject(ctx)
	if project == "" && a.composeService == "" {
		project = a.composeProjectLabelAnswered(ctx, a.container, "keploy takes the project down instead")
	}
	if project == "" {
		a.logger.Warn("keploy could not tell the application's compose project; taking it down instead",
			zap.String("container", a.container))
		return false, none
	}
	// Armed before the listing, as compose's own abort arms its stop of
	// late starters: a container compose's up starts after the listing (a
	// service whose wait a stopped dependency's exit just met) is stopped
	// too, or the up, waiting on it, never returned and the run hung.
	lateStopped = a.stopComposeLateStarters(ctx, project)
	// The list asks the daemon again when it fails (askDaemon): with a 2 s
	// budget, a loaded daemon's slow list took the project down whole
	// instead, keploy's agent with it, mid-drain.
	running, err := askDaemon(ctx, a.logger, fmt.Sprintf("the running containers of compose project %q (with no answer, keploy takes the project down)", project),
		func(ctx context.Context) ([]container.Summary, error) {
			return a.docker.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project))})
		})
	if err != nil {
		a.logger.Warn("keploy could not list the application's compose project to stop it; taking it down instead", zap.String("project", project), zap.Error(err))
		return false, lateStopped
	}
	var wg sync.WaitGroup
	for _, c := range running {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			a.composeStopsARun(ctx, id)
			// The container's own stop grace, as compose's abort gave it:
			// keploy's agent, under keploy record, waits for keploy to drain
			// what it captured, and goes by that event; its grace
			// (stop_grace_period) only bounds an agent that never goes.
			if err := a.docker.ContainerStop(ctx, id, container.StopOptions{}); err != nil && !errdefs.IsNotFound(err) {
				a.logger.Debug("could not stop a container of the application's compose project", zap.String("container", id), zap.Error(err))
			}
		}(c.ID)
	}
	wg.Wait()
	return true, lateStopped
}

// A STOP OF THE STACK ENDS BEFORE THE RETRY'S BRING-UP, AND ITS EXITS STAY KEPLOY'S.
//
// keploy's stop of the stack for a dependency's crash stands for compose's
// abort, whose up returns only once every container it stopped is down. The
// retry's bring-up began as soon as keploy's up returned, while the stop
// still waited out a container's grace: `up --exit-code-from runner`, a
// runner that does not handle SIGTERM, was left running into the retried up,
// which reused it, and then the stop killed it (137), with keploy no longer
// holding the stack stopped, so its exit ended the run and the retried
// bring-up recorded one test of six. The retry now waits for the stop to end
// (composeAwaitsTheStackStop), and an exit of a run the stop took down is
// keploy's own whenever the watch reads it (composeStoppedRun): the watch's
// read of a die comes after the stop, and may come after the retry began.

// composeStopsARun keeps the run of container id that keploy's stop of the
// stack is about to take down, by its StartedAt on the daemon's clock, so
// that run's exit is read as keploy's own stop (composeStoppedRun); a
// container the daemon does not read is kept as no run.
func (a *App) composeStopsARun(ctx context.Context, id string) {
	if info, read := a.readComposeContainer(ctx, id, "keploy reads its exit as any other"); read {
		a.composeApp.stoppedRuns.Store(id, daemonNanos(info.State.StartedAt))
	}
}

// composeStoppedRun reports whether st, container id's state, is of a run
// keploy's stop of the stack took down (composeStopsARun): a start the
// retry's up gave the container since is another run.
func (a *App) composeStoppedRun(id string, st *container.State) bool {
	run, ok := a.composeApp.stoppedRuns.Load(id)
	return ok && run.(int64) == daemonNanos(st.StartedAt)
}

// composeAwaitsTheStackStop waits for keploy's stop of the stack for a
// crash (stackStop) to end, before the run's retry of the bring-up begins;
// false when ctx ends first.
func (a *App) composeAwaitsTheStackStop(ctx context.Context) bool {
	stopped := a.composeApp.stackStop.Load()
	if stopped == nil {
		return true
	}
	a.logger.Debug("the retry of the compose bring-up waits for keploy's stop of the stack to end")
	select {
	case <-*stopped:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopComposeLateStarters stops each container of project that starts from
// now until the run's `compose up` returns (upReturned), as compose's own
// abort stops the services that start after its stop's listing
// (stopLateStarters): compose's up, run without the abort flags, goes on
// starting services, and one whose depends_on wait a stopped container's
// exit met (service_completed_successfully) runs on, the up with it, and the
// run never ended. It reads the daemon's start events of the project from the
// daemon's clock read now, so a start between that read and the stream's
// subscription is replayed to it; a stream that breaks is asked for again
// from its last event. With no up running, nothing can start late. The
// returned channel is closed once the watch has ended (the up returned) and
// every stop it began has ended.
func (a *App) stopComposeLateStarters(ctx context.Context, project string) <-chan struct{} {
	ended := make(chan struct{})
	upReturned := a.composeUpReturned()
	select {
	case <-upReturned:
		close(ended)
		return ended
	default:
	}
	since := a.composeWatchSince(ctx, "the starts of the compose project's containers")
	go func() {
		defer close(ended)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var stops sync.WaitGroup
		defer stops.Wait()
		for ctx.Err() == nil {
			msgs, errs := a.docker.Events(ctx, events.ListOptions{
				Since: fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
				Filters: filters.NewArgs(
					filters.Arg("type", string(events.ContainerEventType)),
					filters.Arg("event", string(events.ActionStart)),
					filters.Arg("label", composeProjectLabelKey+"="+project),
				),
			})
		stream:
			for {
				select {
				case <-upReturned:
					return
				case err := <-errs:
					a.logger.Debug("the docker events stream for the starts of the compose project's containers broke; asking for it again from its last event",
						zap.String("project", project), zap.Error(err))
					break stream
				case m := <-msgs:
					select {
					case <-upReturned:
						// The up has returned: what starts now is no late
						// starter of its.
						return
					default:
					}
					if m.TimeNano > 0 {
						since = time.Unix(0, m.TimeNano)
					}
					a.logger.Info("a container of the application's compose project started after keploy began to stop the stack; keploy stops it too, as compose's own abort stops a late starter",
						zap.String("project", project), zap.String("service", m.Actor.Attributes[composeServiceLabelKey]), zap.String("container", m.Actor.ID))
					stops.Add(1)
					go func(id string) {
						defer stops.Done()
						a.composeStopsARun(ctx, id)
						if err := a.docker.ContainerStop(ctx, id, container.StopOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
							a.logger.Debug("could not stop a late-started container of the application's compose project", zap.String("container", id), zap.Error(err))
						}
					}(m.Actor.ID)
				}
			}
			select {
			case <-upReturned:
				return
			case <-time.After(composeDaemonAskPace):
			}
		}
	}()
	return ended
}

// composeUpRunning marks this bring-up's `compose up` running; the returned
// func marks it returned (upReturned).
func (a *App) composeUpRunning() (returned func()) {
	ch := make(chan struct{})
	a.composeApp.upReturned.Store(&ch)
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// composeUpReturned is closed once the run's `compose up` has returned, and
// is closed already when none runs.
func (a *App) composeUpReturned() <-chan struct{} {
	if ch := a.composeApp.upReturned.Load(); ch != nil {
		return *ch
	}
	none := make(chan struct{})
	close(none)
	return none
}

// composeAppHalt is how the app's container exited when keploy had not asked
// it to, nil when it has not.
func (a *App) composeAppHalt() *composeHalt {
	return a.composeApp.halted.Load()
}

// StopComposeApp stops the app's container of a compose stack, its stack
// running on (keploy's agent among it): an interrupt
// the daemon takes for a stop (composeInterrupt: the container's own stop
// signal, SIGINT when it declares none) to the container compose made for
// the app's service (composeAppContainer), then a wait for it to stop on the
// stop's events, as a docker run
// app's stop waits (stopDockerRunByEvents): the daemon's word, or its main
// process exiting (its pidfd), which a starved daemon is slow to report. It
// kills only when the container's processes made no progress on its way out
// for utils.StopStallBudget of the time both they and keploy could run
// (utils.ExitClock), or at the user's Ctrl+C, which it offers; never on a
// number of seconds. It says why it had to kill, "" when the app exited on
// the interrupt, or had exited already. It returns once the daemon has
// recorded the stop (awaitComposeStopRecorded), so the next start and stop
// of the container read that record.
// The drains wait for it (utils.BeginAppStop). The app's exit is not a halt
// (watchComposeApp) until StartComposeApp starts it again.
func (a *App) StopComposeApp(ctx context.Context) (killed string, err error) {
	killed, err = a.stopComposeApp(ctx)
	// A recording's stop says so in the recording (P101-b-k); a set's end
	// says it in the set's report.
	if killed != "" && a.StopsARecording() {
		a.noteRecordKill("keploy stopped the application at the recording's end and " + killed)
	}
	return killed, err
}

// stopComposeApp is StopComposeApp's stop. An app that had exited on its own
// (halted) before keploy asked it to stop is no stop: it answers the halt
// (models.ErrAppExitedOnItsOwn), which its watch may not have seen yet (the
// die event comes after the inspect), and keeps it. So is one the daemon is
// starting again (restarting: it exited on its own, and its restart policy
// starts it), which the daemon calls running, and which keploy's interrupt
// would only have stopped from starting: its own exit, after the set's last
// answer, was taken for keploy's clean stop.
func (a *App) stopComposeApp(ctx context.Context) (killed string, err error) {
	defer utils.BeginAppStop()()
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	// stopping is set only once the container is found running: an app found
	// exited on its own leaves it unset, so its watch, which reads it after
	// its own inspect, takes the same die for a halt and ends the run.
	wasStopping := a.composeApp.stopping.Load()
	if h := a.composeAppHalt(); h != nil {
		return "", *h
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id, err := a.composeAppContainer(wctx)
	if err != nil {
		return "", fmt.Errorf("keploy could not tell which container is the application's, to stop it: %w", err)
	}
	w := a.watchContainer(wctx, id)
	defer w.release()
	if w.restarting && !wasStopping {
		h := composeHalt{exitCode: w.exitCode}
		a.composeApp.halted.CompareAndSwap(nil, &h)
		return "", *a.composeAppHalt()
	}
	if !w.running {
		if w.seen && !wasStopping {
			h := composeHalt{exitCode: w.exitCode}
			a.composeApp.halted.CompareAndSwap(nil, &h)
			return "", *a.composeAppHalt()
		}
		// Gone, or stopped by keploy already: nothing to stop.
		return "", nil
	}
	// The app's first start, kept before keploy's start of it again
	// moves its StartedAt (composeAppFirstStart).
	a.composeAppFirstStart(wctx)
	a.composeApp.stopping.Store(true)
	// Asked for before the interrupt: an exit before it is still seen.
	daemonSaid, waitErr := a.docker.ContainerWait(wctx, id, container.WaitConditionNotRunning)
	if err := a.signalComposeApp(wctx, id, composeInterrupt(w.stopSignal)); err != nil {
		if errdefs.IsConflict(err) || errdefs.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("keploy could not interrupt the application's container %q: %w", a.container, err)
	}
	// stopped: the container has stopped, on the daemon's word or on its
	// main process's exit, which a starved daemon is slow to report.
	// recorded: the daemon has recorded the stop (its word), or has no more
	// to say of it (the container was removed).
	stopped, recorded := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(recorded)
		mainGone := w.mainGone
		reasked := false
		for {
			select {
			case <-daemonSaid:
			case err := <-waitErr:
				if !errdefs.IsNotFound(err) {
					if wctx.Err() != nil {
						return
					}
					// The wait failed (its connection broke, or the daemon
					// refused it), which says nothing of the stop: the main
					// process's exit is not the daemon's record of it, so
					// the daemon is asked again. A wait asked once the stop
					// is recorded answers at once; one that fails again is
					// asked again at a walking pace, not in a spin. The stop
					// holds stopMu all the while, and bounds the wait for
					// the answer (awaitComposeStopRecorded).
					a.logger.Debug("the daemon's wait for the app's container failed; keploy asks the daemon again", zap.Error(err))
					if reasked {
						select {
						case <-wctx.Done():
							return
						case <-time.After(composeDaemonAskPace):
						}
					}
					reasked = true
					daemonSaid, waitErr = a.docker.ContainerWait(wctx, id, container.WaitConditionNotRunning)
					continue
				}
				// Removed, once seen: stopped, and the daemon has no more
				// to say of it.
			case <-mainGone:
				close(stopped)
				mainGone = nil
				continue
			case <-wctx.Done():
				return
			}
			if mainGone != nil {
				close(stopped)
			}
			return
		}
	}()
	// One offer to end the stop at the user's Ctrl+C stands across its two
	// waits, for the container's stop and then for the daemon's record of it
	// (awaitComposeStopRecorded): a press the first wait's offer took, even
	// in the look that found the app's processes gone, ends the second.
	notice := utils.NewStopNowNotice(utils.StopOffer)
	defer notice.Withdraw()
	end, bound := utils.WaitExitOffered(a.logger, stopped, w.procs, a.teardown.Load(), notice, utils.OfferKillNow)
	switch end {
	case utils.StopExited:
		return "", a.awaitComposeStopRecorded(recorded, notice)
	case utils.StopPressed:
		killed = "keploy killed its container at the press of Ctrl+C, before it had stopped"
	default:
		if w.readable {
			killed = fmt.Sprintf("its container made no progress on its way out for %s after keploy interrupted it, so keploy killed it", bound)
		} else {
			killed = fmt.Sprintf("keploy could not read its container's processes, and the docker daemon did not say it had stopped, for %s after keploy interrupted it, so keploy killed it", utils.StopStallSaid())
		}
	}
	// The kill's waits make their own offer: a press the first offer took
	// was its answer (the kill).
	notice.Withdraw()
	a.logger.Warn("keploy is killing the app it stopped: "+killed, zap.String("container", a.container))
	if err := a.signalComposeApp(wctx, id, "SIGKILL"); err != nil {
		if errdefs.IsConflict(err) || errdefs.IsNotFound(err) {
			// It had stopped, or was gone, by the kill: no kill reached it.
			return "", nil
		}
		return killed, fmt.Errorf("keploy could not kill the application's container %q: %w", a.container, err)
	}
	// The kill ends it but in the kernel: wait for that at the same rule;
	// a container still there after it is an error the next set's start
	// would meet.
	killedNotice := utils.NewStopNowNotice(utils.StopOffer)
	defer killedNotice.Withdraw()
	if end, _ := utils.WaitExitOffered(a.logger, stopped, w.procs, a.teardown.Load(), killedNotice, utils.OfferKillNow); end != utils.StopExited {
		return killed, fmt.Errorf("the application's container %q had not stopped after keploy killed it", a.container)
	}
	return killed, a.awaitComposeStopRecorded(recorded, killedNotice)
}

// KEPLOY'S STOP OF A COMPOSE APP IS THROUGH ONCE THE DAEMON HAS RECORDED IT.
//
// The stop can see the app's container stop before the docker daemon has
// recorded it: its main process exits (its pidfd), or all its processes are
// gone, while the daemon is still handling the exit (it takes the task down
// and drains the container's output first). Until the daemon has, it calls
// the container running. keploy's start of it for the next set was then the
// daemon's no-op (a start of a running container), so that set ran with no
// app; and the stop's die, which the daemon sent once it had recorded the
// exit, reached the watch after that start had ended keploy's stop
// (stopping), and was taken for the app's halt: the run ended, and the set
// was lost. A second stop in that window (the teardown's) would find the
// container running too, and interrupt it again.
//
// So the stop returns only once the daemon has recorded it: its word, the
// wait asked for before keploy's interrupt, for the run keploy interrupted.
// Every step after the stop (the next set's start, another stop, the watch's
// reading of the stop's die) reads the daemon's own record of it, whenever
// the die reaches the watch. The wait is for an event, the daemon's: no news
// at all ends it only after utils.StopStallBudget of keploy's runnable time
// (utils.WaitExitOffered with no processes to read), or at the user's
// Ctrl+C, and the stop then says so.

// The wait for the record takes the offer of the wait before it (notice):
// one offer to end the stop at the user's Ctrl+C stands for the whole stop.
// With a notice of its own, a press the first wait's offer took in the look
// that found the app's processes gone, or any press in the StopOffer after
// they went, ended nothing: the stop waited on for the daemon until a second
// offer and press, or the stall bound. Once the processes are gone, the offer
// says what a press does then: keploy stops waiting, as nothing is left to
// kill.

// composeOfferStopWaiting is the stop's offer once the app's container has
// stopped and keploy waits for the daemon's record of it.
const composeOfferStopWaiting = "the application's container has stopped, and keploy waits for the docker daemon to record its stop; press Ctrl+C again to stop waiting"

// awaitComposeStopRecorded waits for recorded, closed once the daemon has
// recorded keploy's stop of the app's container (or has no more to say of
// it), with the stop's offer to end at the user's Ctrl+C (notice).
func (a *App) awaitComposeStopRecorded(recorded <-chan struct{}, notice *utils.StopNowNotice) error {
	select {
	case <-recorded:
		return nil
	default:
	}
	a.logger.Debug("the application's container has stopped; keploy waits for the docker daemon to record its stop before it starts or stops the container again",
		zap.String("container", a.container))
	switch end, _ := utils.WaitExitOffered(a.logger, recorded, nil, a.teardown.Load(), notice, composeOfferStopWaiting); end {
	case utils.StopExited:
		return nil
	case utils.StopPressed:
		return fmt.Errorf("the application's container %q stopped, and keploy stopped waiting for the docker daemon to record its stop at the press of Ctrl+C", a.container)
	}
	return fmt.Errorf("the application's container %q stopped, but the docker daemon had not recorded its stop %s after it did", a.container, utils.StopStallSaid())
}

// composeInterrupt is the signal keploy interrupts the app's container
// with: the stop signal it declares (its compose stop_signal, or its image's
// STOPSIGNAL), SIGINT when it declares none (P101-c-A2 SF-3(b)). The docker
// daemon cancels a container's restart policy only for a kill it takes for a
// stop: one with the container's own stop signal, any kill of a container
// that declares none, or SIGKILL (moby's killWithSignal, ExitOnNext); a
// ContainerStop with another signal is the same kill. keploy's SIGINT to an
// app that declares stop_signal: SIGTERM stopped it, its restart policy
// (always, or on-failure on a non-zero exit) started it again, and the set's
// end was read with it running, with nothing said; the next set's start was a
// no-op on a container the daemon had started before that set was staged, so
// that start's calls were no set's. The signal is the container's own word
// for it, as the daemon reads it ("SIGTERM", "TERM", "15"), sent as it is.
// keploy's start (StartComposeApp), a start through the API, arms the policy
// again.
func composeInterrupt(stopSignal string) string {
	if stopSignal == "" {
		return "SIGINT"
	}
	return stopSignal
}

// signalComposeApp sends signal to the app's container, id. The call is
// bounded only as a hang guard (utils.StopStallBudget): a daemon that never
// answers cannot hold the stop for good.
func (a *App) signalComposeApp(ctx context.Context, id, signal string) error {
	kctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	defer cancel()
	return a.docker.ContainerKill(kctx, id, signal)
}

// StartComposeApp starts the app's container of a compose stack again, after
// StopComposeApp: its exits are watched as a halt
// again from then on. start says what an inspect before it read of the
// container: models.ComposeAppStartedAnew where it was not running (the app
// is a new one, and what an earlier one's processes lost is not its);
// models.ComposeAppFoundRunning where it runs already (a stop that did not
// end it, or a start told failed that the daemon carried out: the start is
// the daemon's no-op); models.ComposeAppUnread where the daemon gave no
// answer of it for utils.StopStallBudget of keploy's runnable time, or the
// start failed (err). Neither of the last two is a new app: it may be the one
// that ran before. anew (never nil) is called where the inspect read the
// container not running, before the docker start, under the stop's lock: a
// new app is told to whoever must know before its first instruction runs
// (keploy's agent, whose clock a new process must find at its own set's
// first case, never at a later instant only the app before it read). It is
// called whatever the start then answers: a container read not running holds
// no process of the app before it, so whatever runs in it next is a new one.
func (a *App) StartComposeApp(ctx context.Context, anew func()) (start models.ComposeAppStart, err error) {
	// Under stopMu, as keploy's stops are made and the watch decides
	// (composeAppDied): a die the watch takes during the start is told
	// apart by the container running once it is through.
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	id, err := a.composeAppContainer(ctx)
	if err != nil {
		return models.ComposeAppUnread, fmt.Errorf("keploy could not tell which container is the application's, to start it again: %w", err)
	}
	// The stop's record, read as the watch reads a die's container
	// (readComposeContainer): a daemon slow to describe a container it has
	// stopped is asked again, never taken to say it runs on. A read the
	// daemon never answered says nothing of the container: it is not told
	// as one found running.
	info, read := a.readComposeContainer(ctx, id, "keploy starts it, and cannot tell whether the app it starts is a new one")
	start = models.ComposeAppUnread
	if read {
		start = models.ComposeAppFoundRunning
		if !info.State.Running {
			start = models.ComposeAppStartedAnew
			anew()
		}
	}
	// Bounded only as a hang guard (utils.StopStallBudget), as the stop's
	// calls are: a wedged daemon cannot hold the set for good.
	sctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	defer cancel()
	if err := a.docker.ContainerStart(sctx, id, container.StartOptions{}); err != nil {
		return models.ComposeAppUnread, fmt.Errorf("keploy could not start the application's container %q again: %w", a.container, err)
	}
	a.composeApp.stopping.Store(false)
	return start, nil
}
