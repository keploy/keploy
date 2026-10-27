package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/utils"
)

// keploy's stop of a compose app whose processes it reads waits for the
// container's stop on its events, as a docker run app's does: its main
// process exiting is the stop, though the daemon has not said so (a starved
// daemon is slow to); a shutdown that keeps making progress past the stall
// bound is waited for; one that makes none is killed once the stall bound of
// the time both it and keploy could run has passed, and the kill is said with
// the bound.
func TestTheComposeAppsStopWaitsForTheContainersStopEvents(t *testing.T) {
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		stallBudget(t, 3*time.Second)
		main := started(t, "sh", "-c", "sleep 600 & sleep 1")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		killed, err := composeApp(d).StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err != nil || killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want the main process's exit taken for the stop, no kill", killed, err, signals)
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

// stoppedUnrecorded is a compose app keploy stopped, its stop seeing the exit
// the daemon has yet to record: on the interrupt, its main process's exit;
// with kill set, the end of its processes at keploy's kill.
func stoppedUnrecorded(t *testing.T, kill bool) (*composeDocker, *App) {
	t.Helper()
	argv, stall, said := []string{"sh", "-c", "sleep 600 & sleep 1"}, 3*time.Second, ""
	if kill {
		argv, stall, said = []string{"sleep", "600"}, 300*time.Millisecond, "its container made no progress"
	}
	stallBudget(t, stall)
	main := started(t, argv...)
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	// The kill ends the app's processes; the daemon records it later.
	d.onKill = func(signal string) bool {
		if signal != "SIGKILL" {
			return false
		}
		_ = syscall.Kill(-main.Process.Pid, syscall.SIGKILL)
		return true
	}
	a := composeApp(d)
	killed, err := a.StopComposeApp(context.Background())
	if err != nil || (said == "") != (killed == "") || !strings.Contains(killed, said) {
		t.Fatalf("the stop said %q (err %v); want the app's exit taken for it, a kill said only where it killed", killed, err)
	}
	if running, _, _ := d.state(); !running {
		t.Fatal("the daemon recorded the exit before the start; the case needs it unrecorded")
	}
	return d, a
}

// keploy's start of a compose app again, after its stop saw the app exit
// before the daemon recorded the exit, waits for the daemon's word, then
// starts a new app: an exit on the interrupt (its main process's) and one
// on keploy's kill alike, whether the daemon records the exit once the
// start's wait is asked or between the start's inspect and its wait (where
// a wait for the next exit would wait for good). Started while the daemon
// still called the container running, the start was the daemon's no-op:
// the app was not started for the next set, and the exit the daemon
// recorded next, keploy's own stop's, was taken for a halt, which ended the
// run before that set had a report.
func TestTheComposeAppsStartWaitsForTheDaemonToRecordTheExitItsStopSaw(t *testing.T) {
	for _, stop := range []struct {
		name string
		kill bool
	}{
		{"its main process exits on the interrupt", false},
		{"keploy kills it", true},
	} {
		for _, at := range []string{"once the start's wait is asked", "between the start's inspect and its wait"} {
			t.Run(stop.name+", the daemon records it "+at, func(t *testing.T) {
				d, a := stoppedUnrecorded(t, stop.kill)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				record := func() {
					d.mu.Lock()
					d.stop()
					d.mu.Unlock()
				}
				waited := false
				d.mu.Lock()
				if at == "once the start's wait is asked" {
					d.onWait = func(_ container.WaitCondition, pending bool) {
						waited = true
						if pending {
							record()
						}
					}
				} else {
					d.afterInspect = record
					// Nothing records an exit after this one: a wait
					// left pending would wait for good.
					d.onWait = func(cond container.WaitCondition, pending bool) {
						waited = true
						if pending {
							cancel(fmt.Errorf("the start's wait (%s) waits for an exit after the one the daemon recorded already", cond))
						}
					}
				}
				d.mu.Unlock()
				anew, err := a.StartComposeApp(ctx)
				running, _, starts := d.state()
				if err != nil || !anew || !running || starts != 1 || !waited {
					t.Fatalf("the start said anew=%v (err %v; %v), the container running=%v after %d start(s), the daemon asked to wait %v; want a new app started once the daemon recorded the exit", anew, err, context.Cause(ctx), running, starts, waited)
				}
				if a.composeAppDied(context.Background(), die("1")) || a.composeAppHalt() != nil {
					t.Fatal("the daemon's record of the exit keploy's stop saw was taken for a halt of the app")
				}
				// The start is through: a later stop that does not end the
				// app (an interrupt the daemon refused) leaves it running,
				// which the next start must not wait on.
				if a.composeApp.exitSeen.Load() {
					t.Fatal("the start left the stop's exit marked seen")
				}
			})
		}
	}
}

// A stop whose kill did not end the app (its processes still there) fails,
// and replay starts the next set all the same: that start finds the app
// running, as it is, and starts nothing anew, with no wait for an exit that
// will not come.
func TestTheComposeAppsStartAfterAKillThatDidNotEndTheAppDoesNotWait(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	main := started(t, "sleep", "600")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	// The daemon takes the kill; the app's processes stay.
	d.onKill = func(signal string) bool { return signal == "SIGKILL" }
	a := composeApp(d)
	killed, err := a.StopComposeApp(context.Background())
	if err == nil || !strings.Contains(err.Error(), "had not stopped after keploy killed it") || killed == "" {
		t.Fatalf("the stop said %q (err %v); want the kill said, and the app still there an error", killed, err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	d.mu.Lock()
	d.onWait = func(cond container.WaitCondition, pending bool) {
		if pending {
			cancel(fmt.Errorf("the start waits (%s) for the exit of an app whose processes are all still there", cond))
		}
	}
	d.mu.Unlock()
	anew, err := a.StartComposeApp(ctx)
	running, _, starts := d.state()
	if err != nil || anew || !running || starts != 1 {
		t.Fatalf("the start said anew=%v (err %v; %v), the container running=%v after %d start(s); want the running app found, nothing new", anew, err, context.Cause(ctx), running, starts)
	}
}

// A daemon whose wait fails cannot say the container stopped: the start
// errors and starts nothing, for a start then is the daemon's no-op on a
// container it still calls running.
func TestTheComposeAppsStartErrsWhenTheDaemonsWaitFails(t *testing.T) {
	d, a := stoppedUnrecorded(t, false)
	failed := errors.New("the daemon's wait failed")
	d.mu.Lock()
	d.waitErr = failed
	d.mu.Unlock()
	anew, err := a.StartComposeApp(context.Background())
	_, _, starts := d.state()
	if !errors.Is(err, failed) || anew || starts != 0 {
		t.Fatalf("the start said anew=%v (err %v) after %d start(s); want the wait's error, no start", anew, err, starts)
	}
}
