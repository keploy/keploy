package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
// code (APP_HALTED). keploy stops the app (StopComposeApp: an interrupt, then a
// wait for its exit on events, a kill only when it made no progress on its way
// out, and the kill is said) and starts it again (StartComposeApp), while the
// agent and the app's dependencies run on.
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
	// project is the app's compose project, kept once read
	// (composeAppProject); nil while it has not been. Each run reads it
	// afresh.
	project atomic.Pointer[string]
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
// read; "" while it cannot be read. The inspect is bounded only as a hang
// guard (utils.StopStallBudget), as the stop's other calls to the daemon are.
func (a *App) composeAppProject(ctx context.Context) string {
	if p := a.composeApp.project.Load(); p != nil {
		return *p
	}
	ictx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	p := a.composeProjectLabelOf(ictx, a.keployContainer)
	cancel()
	if p != "" {
		a.composeApp.project.Store(&p)
	}
	return p
}

// composeAppContainer is the ID of the app's container: the one compose made
// for the app's service in the app's project (composeAppProject), one-offs
// (`compose run`) aside. With no service known, it is the name keploy was
// given, as before. The list is bounded only as a hang guard, as
// composeAppProject's inspect is.
func (a *App) composeAppContainer(ctx context.Context) (string, error) {
	if a.composeService == "" {
		return a.container, nil
	}
	project := a.composeAppProject(ctx)
	if project == "" {
		return "", fmt.Errorf("keploy could not read the compose project of keploy's agent's container %q, so it cannot tell which container is the application's", a.keployContainer)
	}
	lctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	list, err := a.docker.ContainerList(lctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", composeProjectLabelKey+"="+project),
		filters.Arg("label", composeServiceLabelKey+"="+a.composeService),
	)})
	cancel()
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
// exit is missed. It is of the containers of the app's service, as compose
// labels them (composeAppDieFilter): the app's container does not exist yet
// when the watch begins, and its name is compose's; the app's is told among
// them by their labels (isComposeAppEvent).
func (a *App) watchComposeApp(ctx context.Context, end func()) (stop func()) {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		since := time.Now()
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

// composeAppDieFilter is what the watch asks the daemon for: the die events
// of the containers of the app's service, or, with no service known, of the
// container keploy was given by name, as before.
func (a *App) composeAppDieFilter() filters.Args {
	args := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("event", string(events.ActionDie)),
	)
	if a.composeService != "" {
		args.Add("label", composeServiceLabelKey+"="+a.composeService)
	} else {
		args.Add("container", a.container)
	}
	return args
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
	ictx, icancel := context.WithTimeout(ctx, dockerInspectBudget)
	info, err := a.docker.ContainerInspect(ictx, ref)
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
		zap.String("container", a.container), zap.String("id", m.Actor.ID), zap.Int("exitCode", code))
	return true
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
			close(halt)
			a.composeApp.stopping.Store(true)
			if !a.stopComposeProject(context.WithoutCancel(ctx)) {
				composeDown()
			}
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
// something attached to it runs; under the switch the project's end is
// keploy's: the watch's on a halt, the teardown's otherwise.
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
	if !halted && !stopping {
		if h, exited = a.composeAppExited(ctx); exited {
			a.composeApp.halted.CompareAndSwap(nil, &h)
		}
	}
	a.composeApp.stopMu.Unlock()
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
		return composeHaltCame
	}
}

// composeAppExited is the app's container's exit, when it has exited: not
// one compose only created (an up that failed leaves it so), nor one that
// runs, nor one that cannot be read.
func (a *App) composeAppExited(ctx context.Context) (composeHalt, bool) {
	id, err := a.composeAppContainer(ctx)
	if err != nil {
		return composeHalt{}, false
	}
	ictx, cancel := context.WithTimeout(ctx, dockerInspectBudget)
	defer cancel()
	info, err := a.docker.ContainerInspect(ictx, id)
	if err != nil || info.ContainerJSONBase == nil || info.State == nil || info.State.Running {
		return composeHalt{}, false
	}
	if st := info.State.Status; st != "exited" && st != "dead" {
		return composeHalt{}, false
	}
	return composeHalt{exitCode: info.State.ExitCode}, true
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
	project := a.composeAppProject(ctx)
	if project == "" && a.composeService == "" {
		project = a.composeProjectLabel(ctx)
	}
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
// proven switch, its stack running on (keploy's agent among it): an interrupt
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
// the interrupt, or had exited already.
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
	a.composeApp.stopping.Store(true)
	// Asked for before the interrupt: an exit before it is still seen.
	daemonSaid, waitErr := a.docker.ContainerWait(wctx, id, container.WaitConditionNotRunning)
	if err := a.signalComposeApp(wctx, id, composeInterrupt(w.stopSignal)); err != nil {
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
	switch utils.WaitExitByEvents(a.logger, stopped, w.procs, a.teardown.Load()) {
	case utils.StopExited:
		return "", nil
	case utils.StopPressed:
		killed = "keploy killed its container at the press of Ctrl+C, before it had stopped"
	default:
		if w.readable {
			killed = fmt.Sprintf("its container made no progress on its way out for %s after keploy interrupted it, so keploy killed it", utils.StopStallRanSaid())
		} else {
			killed = fmt.Sprintf("keploy could not read its container's processes, and the docker daemon did not say it had stopped, for %s after keploy interrupted it, so keploy killed it", utils.StopStallSaid())
		}
	}
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
	if utils.WaitExitByEvents(a.logger, stopped, w.procs, a.teardown.Load()) != utils.StopExited {
		return killed, fmt.Errorf("the application's container %q had not stopped after keploy killed it", a.container)
	}
	return killed, nil
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

// StartComposeApp starts the app's container of a compose stack again, under
// the proven switch, after StopComposeApp: its exits are watched as a halt
// again from then on.
func (a *App) StartComposeApp(ctx context.Context) error {
	// Under stopMu, as keploy's stops are made and the watch decides
	// (composeAppDied): a die the watch takes during the start is told
	// apart by the container running once it is through.
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	id, err := a.composeAppContainer(ctx)
	if err != nil {
		return fmt.Errorf("keploy could not tell which container is the application's, to start it again: %w", err)
	}
	// Bounded only as a hang guard (utils.StopStallBudget), as the stop's
	// calls are: a wedged daemon cannot hold the set for good.
	sctx, cancel := context.WithTimeout(ctx, utils.StopStallBudget)
	defer cancel()
	if err := a.docker.ContainerStart(sctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("keploy could not start the application's container %q again: %w", a.container, err)
	}
	a.composeApp.stopping.Store(false)
	return nil
}
