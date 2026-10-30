package app

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy's stop of a compose app whose processes it reads waits for the
// container's stop on its events, as a docker run app's does: its main
// process exiting says no kill is due, though the daemon has not said it
// stopped yet (a starved daemon is slow to), and the stop is through once the
// daemon says so; a shutdown that keeps making progress past the stall
// bound is waited for; one that makes none is killed once the stall bound of
// the time both it and keploy could run has passed, and the kill is said with
// the bound.
func TestTheComposeAppsStopWaitsForTheContainersStopEvents(t *testing.T) {
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		// The stall bound is shorter than the daemon's lag: the main
		// process's exit, not the daemon's word, keeps the kill away.
		stallBudget(t, 3*time.Second)
		main := started(t, "sh", "-c", "sleep 600 & sleep 1")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		// The daemon says the container stopped 2 s after its main process
		// exited.
		go func() {
			for {
				if _, alive := utils.ProcOf(main.Process.Pid); !alive {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(2 * time.Second)
			d.mu.Lock()
			d.stop()
			d.mu.Unlock()
		}()
		killed, err := composeApp(d).StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err != nil || killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want the main process's exit to keep the kill away", killed, err, signals)
		}
		if running {
			t.Fatal("the stop was through with the daemon calling the container running")
		}
	})
	t.Run("the daemon never says so", func(t *testing.T) {
		stallBudget(t, 2*time.Second)
		main := started(t, "sh", "-c", "sleep 1")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		killed, err := composeApp(d).StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not said it had stopped") || killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want no kill, and the daemon's silence an error", killed, err, signals)
		}
	})
	t.Run("a shutdown that keeps making progress past the stall bound", func(t *testing.T) {
		stallBudget(t, 2*time.Second)
		main := progressingProcess(t)
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		go func() {
			time.Sleep(5 * time.Second)
			d.mu.Lock()
			d.stop()
			d.mu.Unlock()
		}()
		a := composeApp(d)
		// The run's teardown: the container's steps out move it.
		scope := &utils.TeardownScope{}
		a.teardown.Store(scope)
		killed, err := a.StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err != nil || killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want a container making progress waited for", killed, err, signals)
		}
		if scope.Moves() == 0 {
			t.Fatal("the container's steps out moved the run's teardown not once")
		}
	})
	t.Run("no progress", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		killed, err := composeApp(d).StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err != nil || !strings.Contains(killed, "its container made no progress on its way out for 300ms of its and keploy's runnable time") || strings.Join(signals, ",") != "SIGINT,SIGKILL" {
			t.Fatalf("said %q (err %v) after %v; want the stall kill said", killed, err, signals)
		}
	})
}

// keploy's stop of a compose app is through only once the docker daemon has
// its container stopped, not when its main process has exited: what comes
// next is the daemon's. A starved daemon handles the exit late; until then it
// calls the container running, and the next set's start of it is its no-op
// (304). Taken for through at the process's exit, the stop let that start
// run: the app was not started again, its run's watch was told the stop was
// over, and the die the daemon then gave, of keploy's own stop, was taken for
// a halt that ended the run (compose-proven-setend scenario 1, 2026-10-30:
// "the application's container exited, and keploy had not stopped it" as the
// second set staged, exit code 0, compose showing one exit of the app). Here
// the daemon catches up when a start reaches it (answered first, as the
// running container's), or, with none, 2 s after the process exited.
func TestAComposeStopIsThroughOnlyOnceTheDaemonHasTheAppStopped(t *testing.T) {
	stallBudget(t, time.Minute)
	main := started(t, "sh", "-c", "sleep 1")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	a := composeApp(d)
	ended := make(chan struct{}, 1)
	stopWatch := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
	defer stopWatch()
	stream := <-d.streams

	type stopped struct {
		killed string
		err    error
	}
	stopDone := make(chan stopped, 1)
	go func() {
		killed, err := a.StopComposeApp(context.Background())
		stopDone <- stopped{killed, err}
	}()
	eventually(t, "the app's main process exited", func() bool {
		_, alive := utils.ProcOf(main.Process.Pid)
		return !alive
	})
	type startedApp struct {
		anew bool
		err  error
	}
	startDone := make(chan startedApp, 1)
	go func() {
		anew, err := a.StartComposeApp(context.Background())
		startDone <- startedApp{anew, err}
	}()
	var start *startedApp
	select {
	case s := <-startDone:
		start = &s
	case <-time.After(2 * time.Second):
	}
	// The daemon catches up: the container stopped, its die event out.
	d.mu.Lock()
	d.stop()
	d.mu.Unlock()
	stream <- die("0")
	stop := <-stopDone
	if start == nil {
		s := <-startDone
		start = &s
	}
	if _, signals, _ := d.state(); stop.err != nil || stop.killed != "" || strings.Join(signals, ",") != "SIGINT" {
		t.Errorf("the stop said %q (err %v) after %v; want no kill: the app exited on the interrupt", stop.killed, stop.err, signals)
	}
	if running, _, starts := d.state(); start.err != nil || !start.anew || !running || starts != 1 {
		t.Errorf("the start after keploy's stop: anew %v (err %v), container running %v after %d starts; want the app started anew, running", start.anew, start.err, running, starts)
	}
	// Three inspects: the stop's, the start's, and the watch's of the stop's
	// die; the watch has read it once they have answered.
	for i := 0; i < 3; i++ {
		<-d.inspected
	}
	select {
	case <-ended:
		t.Fatalf("keploy's own stop's die was taken for a halt: %+v", a.composeAppHalt())
	default:
	}
	// The app now exits on its own: that is the run's halt, with its code.
	d.mu.Lock()
	d.stop()
	d.mu.Unlock()
	stream <- die("3")
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("the app's own exit did not end the run")
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
		t.Fatalf("the halt kept is %+v; want the app's own exit, code 3, not keploy's stop's die (code 0)", h)
	}
}

// lagKillDocker is composeDocker whose SIGKILL ends the app's processes, as
// the kernel does, while the daemon says the container stopped only when
// sayStopped is called: a starved daemon that handles the kill's exit late.
type lagKillDocker struct{ *composeDocker }

func (d lagKillDocker) ContainerKill(ctx context.Context, id, signal string) error {
	if signal != "SIGKILL" {
		return d.composeDocker.ContainerKill(ctx, id, signal)
	}
	d.mu.Lock()
	d.signals = append(d.signals, signal)
	pid := d.pid
	d.mu.Unlock()
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// keploy's kill of a compose app that made no progress on its way out is
// through, as its interrupt is, only once the docker daemon has the container
// stopped: the kill's exit is the kernel's first, the daemon's after, and the
// next set's start of a container the daemon still calls running is its no-op
// (304). A daemon that never says so is an error, with the kill said; one
// that does ends the stop with the container stopped. Before, the kill was
// through at its processes' exit.
func TestAComposeKillIsThroughOnlyOnceTheDaemonHasTheAppStopped(t *testing.T) {
	for _, says := range []bool{false, true} {
		t.Run(map[bool]string{false: "the daemon never says so", true: "the daemon says so once the processes are gone"}[says], func(t *testing.T) {
			stallBudget(t, 300*time.Millisecond)
			main := started(t, "sleep", "600")
			d := newComposeDocker(true, false)
			d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
			if says {
				go func() {
					for {
						if _, alive := utils.ProcOf(main.Process.Pid); !alive {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					d.mu.Lock()
					d.stop()
					d.mu.Unlock()
				}()
			}
			a := &App{logger: zap.NewNop(), docker: lagKillDocker{d}, container: "app-1", kind: "docker-compose"}
			killed, err := a.StopComposeApp(context.Background())
			running, signals, _ := d.state()
			if !strings.Contains(killed, "made no progress on its way out") || strings.Join(signals, ",") != "SIGINT,SIGKILL" {
				t.Fatalf("said %q (err %v) after %v; want the stall kill said", killed, err, signals)
			}
			if _, alive := utils.ProcOf(main.Process.Pid); alive {
				t.Fatal("the app's main process outlived keploy's kill")
			}
			switch {
			case !says && (err == nil || !strings.Contains(err.Error(), "the docker daemon had not said it had stopped")):
				t.Fatalf("the stop ended with err %v, the daemon calling the container running %v; want the daemon's silence after the kill an error", err, running)
			case says && (err != nil || running):
				t.Fatalf("the stop ended with err %v, the daemon calling the container running %v; want it through once the daemon had it stopped", err, running)
			}
		})
	}
}
