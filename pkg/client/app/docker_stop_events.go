package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stopDockerRunByEvents is the stop of a `docker run` app, under the proven
// switch, at a test set's or a recording's end, the SIGINT sent to its
// docker client. It waits for the app's container to stop: the daemon's wait
// saying so (an event, not a poll), the container's main process exiting
// (its pidfd), or, where nothing better can be asked (a container never
// seen, or one whose wait the daemon failed), its docker client exiting,
// which an attached client does once its container stops. While it waits the container's processes make
// progress on their way out (utils.ExitMark); once they are all gone the
// container has stopped. It kills only when they made none for
// utils.StopStallBudget of keploy's runnable time, or at the user's Ctrl+C,
// which it offers: docker's own stop and removal on a starved host take as
// long as they take, and are no kill (P101-b-k). killed says why it had to
// kill, "" when it did not. The drains wait for it (utils.BeginAppStop).
//
// What it cannot read it does not take for a stall of the app's. A container
// whose processes keploy cannot see (the daemon in another PID namespace:
// inContainer), or that the daemon did not describe, is waited for on the
// daemon's word and its client's exit alone, and a kill once neither came
// for the stall bound is said as such. A container keploy never saw
// (keploy did not name it, or it is gone already) is waited for through its
// docker client's exit.
func (a *App) stopDockerRunByEvents(cmd *exec.Cmd) (killed string, err error) {
	end := utils.BeginAppStop()
	defer end()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The docker client, named as it runs now: its exit is an event, and
	// only it is killed at the end, not a process its pid names later.
	client, _ := utils.ProcOf(cmd.Process.Pid)
	clientGone, releaseClient := utils.ExitOf(client)
	defer releaseClient()

	w := a.watchContainer(ctx, a.container)
	defer w.release()
	if w.seen && !w.running {
		return "", nil
	}
	seen, readable, procs, mainGone := w.seen, w.readable, w.procs, w.mainGone

	stopped := make(chan struct{})
	var daemonSaid <-chan container.WaitResponse
	var waitErr <-chan error
	if seen {
		daemonSaid, waitErr = a.docker.ContainerWait(ctx, a.container, container.WaitConditionNotRunning)
	}
	go func() {
		// The client's exit is the container's stop only where nothing
		// better can be asked: a container never seen, or one whose wait
		// the daemon failed. A client that went with its container still
		// running (a shell around it that died on the interrupt, a client
		// that does not proxy signals) leaves the container's own events,
		// and the stall rule, to decide.
		clientExit, clientExited, waitFailed := clientGone, false, !seen
		for {
			select {
			case <-daemonSaid:
			case err := <-waitErr:
				if !errdefs.IsNotFound(err) {
					// No news of the container (removed, once seen, is
					// stopped): the other events stand.
					a.logger.Debug("the daemon's wait for the app's container failed; waiting for its other events", zap.Error(err))
					daemonSaid, waitErr, waitFailed = nil, nil, true
					if !clientExited {
						continue
					}
				}
			case <-mainGone:
			case <-clientExit:
				clientExit, clientExited = nil, true
				if !waitFailed {
					a.logger.Debug("the app's docker client exited with its container still running; waiting for the container", zap.String("container", a.container))
					continue
				}
			case <-ctx.Done():
				return
			}
			close(stopped)
			return
		}
	}()

	var why string
	switch utils.WaitExitByEvents(a.logger, stopped, procs, a.teardown.Load()) {
	case utils.StopExited:
		return "", nil
	case utils.StopPressed:
		why = "keploy killed its container at the press of Ctrl+C, before it had stopped"
		if !seen {
			why = "keploy killed its docker client at the press of Ctrl+C, before it had exited: whether the app finished stopping is not known"
		}
	default:
		switch {
		case readable:
			why = fmt.Sprintf("its container made no progress on its way out for %s of keploy's runnable time after keploy interrupted it, so keploy killed it", utils.StopStallBudget)
		case seen:
			why = fmt.Sprintf("keploy could not read its container's processes, and neither the docker daemon nor its docker client said it had stopped, for %s of keploy's runnable time after keploy interrupted it, so keploy killed it", utils.StopStallBudget)
		default:
			why = fmt.Sprintf("keploy could not find its container, and its docker client had not exited %s of keploy's runnable time after keploy interrupted it, so keploy killed the client: whether the app finished stopping is not known", utils.StopStallBudget)
		}
	}
	a.logger.Warn("keploy is killing the app it stopped: "+why, zap.String("container", a.container))
	if seen {
		kctx, kcancel := context.WithTimeout(ctx, utils.StopStallBudget)
		kerr := a.docker.ContainerKill(kctx, a.container, "SIGKILL")
		kcancel()
		if kerr != nil && (errdefs.IsNotFound(kerr) || errdefs.IsConflict(kerr)) {
			// It had stopped, or was gone, by the kill: no kill reached it.
			why = ""
		}
	}
	// The client's group, while the client is the process keploy started.
	select {
	case <-clientGone:
		return why, nil
	default:
	}
	return why, utils.SendSignal(a.logger, -client.PID, syscall.SIGKILL)
}

// containerWatch is what keploy reads of a container it stops: whether the
// daemon has it (seen: it described it, or did not answer the inspect) and
// runs it (running; one the daemon did not describe is taken for running),
// whether its processes can be read (readable: inContainer), those processes
// as they run now, its main process's exit (mainGone; never, where it cannot
// be read), and what releases that watch.
type containerWatch struct {
	seen, running, readable bool
	// restarting: the daemon is starting it again after an exit, by its
	// restart policy, and calls it running meanwhile (running is set too).
	restarting bool
	// exitCode is a stopped or restarting container's last exit's, as the
	// daemon gave it.
	exitCode int
	// stopSignal is the stop signal the container declares (its compose
	// stop_signal, or its image's STOPSIGNAL), "" when it declares none.
	stopSignal string
	procs      []utils.Reached
	mainGone   <-chan struct{}
	release    func()
}

// watchContainer reads container id for its stop. The daemon's word is
// bounded only as a hang guard: one that does not answer is waited for on
// the stop's other events.
func (a *App) watchContainer(ctx context.Context, id string) containerWatch {
	w := containerWatch{mainGone: make(<-chan struct{}), release: func() {}}
	ictx, icancel := context.WithTimeout(ctx, utils.StopStallBudget)
	info, ierr := a.docker.ContainerInspect(ictx, id)
	icancel()
	switch {
	case ierr == nil && info.ContainerJSONBase != nil && info.State != nil:
		w.seen, w.running = true, info.State.Running
		if info.Config != nil {
			w.stopSignal = info.Config.StopSignal
		}
		if !w.running || info.State.Restarting {
			// A restarting container has no process to read (its pid is 0).
			w.restarting = info.State.Restarting
			w.exitCode = info.State.ExitCode
			return w
		}
		if main, ok := utils.ProcOf(info.State.Pid); ok && inContainer(info.State.Pid, info.ID) {
			w.readable = true
			w.procs = utils.TreeOf(info.State.Pid)
			w.mainGone, w.release = utils.ExitOf(main)
		}
	case errdefs.IsNotFound(ierr):
		// Never seen, or removed already (--rm) once it stopped.
	default:
		w.seen, w.running = true, true
		a.logger.Debug("the docker daemon did not describe the app's container; waiting for its word instead", zap.String("container", id), zap.Error(ierr))
	}
	return w
}

// inContainer reports whether pid, as keploy's /proc shows it, is a process
// of container id: its cgroup names the container. The daemon's pid of a
// container is in the daemon's PID namespace, which is keploy's only when the
// daemon is local and rootful; another (a remote DOCKER_HOST, a rootless
// daemon) names some other process here, or none, whose progress is no news
// of the container's.
func inContainer(pid int, id string) bool {
	if pid <= 0 || id == "" {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return err == nil && strings.Contains(string(b), id)
}

// stopsByEvents reports whether a stop that says its kills (a test set's or a
// recording's end) waits on the app's exit event: under the proven switch,
// on Linux, where the processes' progress can be read.
func stopsByEvents(says bool) bool {
	return says && provenswitch.On() && runtime.GOOS == "linux"
}
