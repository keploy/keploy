package app

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

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

// keploy's start of a compose app again, after its stop saw the app exit
// before the daemon recorded the exit, waits for the daemon's word, then
// starts a new app: an exit on the interrupt (its main process's) and one
// on keploy's kill alike. Started while the daemon still called the
// container running, the start was the daemon's no-op: the app was not
// started for the next set, and the exit the daemon recorded next, keploy's
// own stop's, was taken for a halt, which ended the run before that set had
// a report.
func TestTheComposeAppsStartWaitsForTheDaemonToRecordTheExitItsStopSaw(t *testing.T) {
	for _, tc := range []struct {
		name, kill string
		argv       []string
		stall      time.Duration
	}{
		{"its main process exits on the interrupt", "", []string{"sh", "-c", "sleep 600 & sleep 1"}, 3 * time.Second},
		{"keploy kills it", "its container made no progress", []string{"sleep", "600"}, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stallBudget(t, tc.stall)
			main := started(t, tc.argv...)
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
			if err != nil || (tc.kill == "") != (killed == "") || !strings.Contains(killed, tc.kill) {
				t.Fatalf("the stop said %q (err %v); want the app's exit taken for it, a kill said only where it killed", killed, err)
			}
			if running, _, _ := d.state(); !running {
				t.Fatal("the daemon recorded the exit before the start; the case needs it unrecorded")
			}
			for drained := false; !drained; {
				select {
				case <-d.inspected:
				default:
					drained = true
				}
			}
			// The daemon records the exit once the start has read the
			// container.
			recorded := make(chan struct{})
			go func() {
				defer close(recorded)
				<-d.inspected
				d.mu.Lock()
				d.stop()
				d.mu.Unlock()
			}()
			anew, err := a.StartComposeApp(context.Background())
			<-recorded
			running, _, starts := d.state()
			if err != nil || !anew || !running || starts != 1 {
				t.Fatalf("the start said anew=%v (err %v), the container running=%v after %d start(s); want a new app started once the daemon recorded the exit", anew, err, running, starts)
			}
			if a.composeAppDied(context.Background(), die("1")) || a.composeAppHalt() != nil {
				t.Fatal("the daemon's record of the exit keploy's stop saw was taken for a halt of the app")
			}
			// The start is through: a later stop that does not end the app
			// (an interrupt the daemon refused) leaves it running, which
			// the next start must not wait on.
			if a.composeApp.exitSeen.Load() {
				t.Fatal("the start left the stop's exit marked seen")
			}
		})
	}
}
