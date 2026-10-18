package app

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// UNDER THE PROVEN SWITCH KEPLOY STOPS A DOCKER COMPOSE APP ITSELF (P101-c).
//
// keploy runs its agent as a service of the app's compose stack, the app in
// the agent's network and pid namespaces. With --abort-on-container-exit the
// app's exit stopped the whole stack, keploy's agent with it, so a stop of the
// app at a test set's end left that set's end nothing to read, and the stop
// was left out: compose sets were read with the app still running, and a call
// it made after the read was judged by no one.
//
// Under the switch compose runs without the abort flags (composeAbortFlags),
// and keploy watches the app's container itself (watchComposeApp): an exit
// keploy did not ask for ends the run, as the abort did, with the app's exit
// code (APP_HALTED). keploy stops the app (StopComposeApp: SIGINT, then a wait
// for its exit on events, a kill only when it made no progress on its way
// out, and the kill is said) and starts it again (StartComposeApp), while the
// agent and the app's dependencies run on.

// composeAppState is what keploy knows of a compose app's container.
type composeAppState struct {
	// stopMu makes keploy's stops of the container one at a time: the stop
	// at a set's end and the teardown's can come at once (an interrupt as a
	// set ends), and a second SIGINT would cut the app's shutdown short. The
	// second finds the container stopped by the first.
	stopMu sync.Mutex
	// stopping: keploy has asked the app to stop, so its exit is no halt.
	stopping atomic.Bool
	// halted is how the app's container exited when keploy had not asked
	// it to (watchComposeApp); nil while it has not.
	halted atomic.Pointer[composeHalt]
}

// composeHalt is an exit of the app's container keploy did not ask for.
type composeHalt struct {
	exitCode int
}

func (h composeHalt) Error() string {
	return fmt.Sprintf("the application's container exited with code %d, and keploy had not stopped it", h.exitCode)
}

// Is makes a halt models.ErrAppExitedOnItsOwn.
func (h composeHalt) Is(target error) bool { return target == models.ErrAppExitedOnItsOwn }

// appError is the run's end on the halt: the app's exit, as compose's abort
// on it ended the run (APP_HALTED), with its exit code.
func (h composeHalt) appError(appLogs string) models.AppError {
	if h.exitCode != 0 {
		return models.AppError{AppErrorType: models.ErrUnExpected, Err: h, AppLogs: appLogs, ExitCode: h.exitCode}
	}
	return models.AppError{AppErrorType: models.ErrAppStopped, AppLogs: appLogs, ExitCode: 0}
}

// composeStopsTheApp reports whether keploy stops and watches a compose app
// itself: under the proven switch.
func composeStopsTheApp() bool {
	return provenswitch.On()
}

// watchComposeApp watches the app's container for its exits until ctx ends,
// or until the returned stop is called. An exit keploy did not ask for
// (stopping unset), of a container that is not running again, is a halt: it
// is kept (composeApp.halted) and end is called, once, to end the run. The
// stream is the daemon's own events, from the moment the watch begins; a
// stream that breaks is asked for again from the last event it gave, so no
// exit is missed.
func (a *App) watchComposeApp(ctx context.Context, end func()) (stop func()) {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		since := time.Now()
		for wctx.Err() == nil {
			msgs, errs := a.docker.Events(wctx, events.ListOptions{
				Since: fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
				Filters: filters.NewArgs(
					filters.Arg("type", string(events.ContainerEventType)),
					filters.Arg("container", a.container),
					filters.Arg("event", string(events.ActionDie)),
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
					a.logger.Debug("the docker events stream for the application's container broke; asking for it again from its last event",
						zap.String("container", a.container), zap.Error(err))
					break stream
				case m := <-msgs:
					if m.TimeNano > 0 {
						since = time.Unix(0, m.TimeNano)
					}
					if a.composeAppDied(wctx, m) {
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
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// composeAppDied reports whether m, a die event of the app's container, is
// an exit keploy did not ask for, keeping it if so.
func (a *App) composeAppDied(ctx context.Context, m events.Message) bool {
	// An event of a run of the container that keploy has started again since
	// is not this run's end.
	ictx, icancel := context.WithTimeout(ctx, dockerInspectBudget)
	info, err := a.docker.ContainerInspect(ictx, a.container)
	icancel()
	if err == nil && info.ContainerJSONBase != nil && info.State != nil && info.State.Running {
		return false
	}
	// Read after the inspect: a stop keploy began meanwhile is its own.
	if a.composeApp.stopping.Load() {
		return false
	}
	code, err := strconv.Atoi(m.Actor.Attributes["exitCode"])
	if err != nil {
		code = -1
	}
	a.composeApp.halted.Store(&composeHalt{exitCode: code})
	a.logger.Info("the application's container exited, and keploy had not stopped it: ending the run",
		zap.String("container", a.container), zap.Int("exitCode", code))
	return true
}

// stopComposeProject stops every container of the app's compose project that
// still runs, keploy's agent among them, as compose's --abort-on-container-exit
// stopped them when the app exited: each is asked to stop (SIGTERM), with its
// own stop grace. The agent, so asked, waits for keploy to drain what it
// captured, and goes when that drain is through (its grace under keploy record
// is 24 h, a bound on an agent that never goes, not when one does), as it did
// under the abort. `compose up` then returns, its containers gone.
// It is false when it could not tell or list the project.
func (a *App) stopComposeProject(ctx context.Context) bool {
	project := a.composeProjectLabel(ctx)
	if project == "" {
		a.logger.Warn("keploy could not tell the application's compose project; taking it down instead",
			zap.String("container", a.container))
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, dockerInspectBudget)
	running, err := a.docker.ContainerList(lctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project))})
	cancel()
	if err != nil {
		a.logger.Warn("keploy could not list the application's compose project to stop it; taking it down instead", zap.String("project", project), zap.Error(err))
		return false
	}
	var wg sync.WaitGroup
	for _, c := range running {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
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
	return true
}

// composeAppHalt is how the app's container exited when keploy had not asked
// it to, nil when it has not.
func (a *App) composeAppHalt() *composeHalt {
	return a.composeApp.halted.Load()
}

// StopComposeApp stops the app's container of a compose stack, under the
// proven switch, its stack running on (keploy's agent among it): SIGINT to the
// container, then a wait for it to stop on the stop's events, as a docker run
// app's stop waits (stopDockerRunByEvents): the daemon's word, or its main
// process exiting (its pidfd), which a starved daemon is slow to report. It
// kills only when the container's processes made no progress on its way out
// for utils.StopStallBudget of keploy's runnable time, or at the user's
// Ctrl+C, which it offers; never on a number of seconds. It says why it had
// to kill, "" when the app exited on the interrupt, or had exited already.
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
// die event comes after the inspect), and keeps it.
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
	w := a.watchContainer(wctx, a.container)
	defer w.release()
	if !w.running {
		if w.seen && !wasStopping {
			h := composeHalt{exitCode: w.exitCode}
			a.composeApp.halted.CompareAndSwap(nil, &h)
			return "", *a.composeAppHalt()
		}
		// Gone, or stopped by keploy already: nothing to stop.
		return "", nil
	}
	a.composeApp.stopping.Store(true)
	// Asked for before the interrupt: an exit before it is still seen.
	daemonSaid, waitErr := a.docker.ContainerWait(wctx, a.container, container.WaitConditionNotRunning)
	if err := a.signalComposeApp(wctx, "SIGINT"); err != nil {
		if errdefs.IsConflict(err) || errdefs.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("keploy could not interrupt the application's container %q: %w", a.container, err)
	}
	stopped := make(chan struct{})
	go func() {
		for {
			select {
			case <-daemonSaid:
			case err := <-waitErr:
				if !errdefs.IsNotFound(err) {
					// No news of the container (removed, once seen, is
					// stopped): its main process's exit stands.
					a.logger.Debug("the daemon's wait for the app's container failed; waiting for its main process", zap.Error(err))
					daemonSaid, waitErr = nil, nil
					continue
				}
			case <-w.mainGone:
			case <-wctx.Done():
				return
			}
			close(stopped)
			return
		}
	}()
	switch utils.WaitExitByEvents(a.logger, stopped, w.procs) {
	case utils.StopExited:
		return "", nil
	case utils.StopPressed:
		killed = "keploy killed its container at the press of Ctrl+C, before it had stopped"
	default:
		if w.readable {
			killed = fmt.Sprintf("its container made no progress on its way out for %s after keploy interrupted it, so keploy killed it", utils.StopStallSaid())
		} else {
			killed = fmt.Sprintf("keploy could not read its container's processes, and the docker daemon did not say it had stopped, for %s after keploy interrupted it, so keploy killed it", utils.StopStallSaid())
		}
	}
	a.logger.Warn("keploy is killing the app it stopped: "+killed, zap.String("container", a.container))
	if err := a.signalComposeApp(wctx, "SIGKILL"); err != nil {
		if errdefs.IsConflict(err) || errdefs.IsNotFound(err) {
			// It had stopped, or was gone, by the kill: no kill reached it.
			return "", nil
		}
		return killed, fmt.Errorf("keploy could not kill the application's container %q: %w", a.container, err)
	}
	// The kill ends it but in the kernel: wait for that at the same rule;
	// a container still there after it is an error the next set's start
	// would meet.
	if utils.WaitExitByEvents(a.logger, stopped, w.procs) != utils.StopExited {
		return killed, fmt.Errorf("the application's container %q had not stopped after keploy killed it", a.container)
	}
	return killed, nil
}

// signalComposeApp sends signal to the app's container. The call is bounded
// only as a hang guard (utils.StopStallBudget): a daemon that never answers
// cannot hold the stop for good.
func (a *App) signalComposeApp(ctx context.Context, signal string) error {
	kctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	defer cancel()
	return a.docker.ContainerKill(kctx, a.container, signal)
}

// StartComposeApp starts the app's container of a compose stack again, under
// the proven switch, after StopComposeApp: its exits are watched as a halt
// again from then on.
func (a *App) StartComposeApp(ctx context.Context) error {
	// Bounded only as a hang guard (utils.StopStallBudget), as the stop's
	// calls are: a wedged daemon cannot hold the set for good.
	sctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	defer cancel()
	if err := a.docker.ContainerStart(sctx, a.container, container.StartOptions{}); err != nil {
		return fmt.Errorf("keploy could not start the application's container %q again: %w", a.container, err)
	}
	a.composeApp.stopping.Store(false)
	return nil
}
