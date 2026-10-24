package app

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// laggingDocker is a composeDocker whose app container's main process is a
// real one (pid), which keploy's interrupt ends at once, while the daemon goes
// on calling the container running until it takes the exit
// (composeDocker.stop, which the test calls): the kernel's word before the
// daemon's, as a starved daemon gives it. The interrupt reaches the process
// as SIGTERM, which no test process has ignored on entry (a background job's
// SIGINT is). waitFails: that many of the daemon's waits for the container
// fail at once (-1: every one). killLags: a SIGKILL ends the main process
// the same way, the daemon calling the container running on; else the daemon
// takes a SIGKILL's exit at once.
type laggingDocker struct {
	*composeDocker
	waitFails int
	killLags  bool
}

func (d *laggingDocker) ContainerKill(ctx context.Context, id, signal string) error {
	if signal != "SIGINT" && !(signal == "SIGKILL" && d.killLags) {
		return d.composeDocker.ContainerKill(ctx, id, signal)
	}
	d.mu.Lock()
	d.signals = append(d.signals, signal)
	running := d.running
	d.mu.Unlock()
	if !running {
		return errdefs.Conflict(errors.New("container is not running"))
	}
	sig := syscall.SIGTERM
	if signal == "SIGKILL" {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(d.pid, sig)
}

func (d *laggingDocker) ContainerWait(ctx context.Context, id string, c container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	fail := d.waitFails != 0
	if d.waitFails > 0 {
		d.waitFails--
	}
	d.mu.Unlock()
	if !fail {
		return d.composeDocker.ContainerWait(ctx, id, c)
	}
	errs := make(chan error, 1)
	errs <- errors.New("the daemon's stream broke")
	return make(chan container.WaitResponse), errs
}

// laggingApp is an App whose compose app's main process is main, under a
// laggingDocker.
func laggingApp(t *testing.T, main int) (*App, *laggingDocker) {
	t.Helper()
	d := &laggingDocker{composeDocker: newComposeDocker(true, false)}
	d.pid, d.id = main, cgroupOf(t, main)
	a := composeApp(d.composeDocker)
	a.docker = d
	return a, d
}

// startedAfter starts `sh -c "<setup>; exec sleep 600"` and returns once
// setup has run (the shell says so on a pipe): a signal sent after it meets
// what setup set up. Its process group is killed at the test's end.
func startedAfter(t *testing.T, setup string) *exec.Cmd {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", setup+"; echo ready; exec sleep 600")
	cmd.Stdout = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done; r.Close() })
	if line, err := bufio.NewReader(r).ReadString('\n'); line != "ready\n" {
		t.Fatalf("the container's process said %q (err %v); want ready", line, err)
	}
	return cmd
}

// exitedOf is a channel closed once process pid exits, its watch released
// at the test's end.
func exitedOf(t *testing.T, pid int) <-chan struct{} {
	t.Helper()
	p, ok := utils.ProcOf(pid)
	if !ok {
		t.Fatal("the container's main process is not running")
	}
	gone, release := utils.ExitOf(p)
	t.Cleanup(release)
	return gone
}

// pressAtOffer has a's stop answered by a press of Ctrl+C at its first offer
// to end at one ("press Ctrl+C again"), as the user's, once ready fires (nil:
// at once), in the offer's own look: the press only a standing offer takes
// (utils.MarkInterruptedAgain). It sets the offer at the run's interrupt
// (utils.StopOffer 0, the run interrupted) for the test. taken says whether
// the press was taken; read it once the stop is over.
func pressAtOffer(t *testing.T, a *App, ready <-chan struct{}) (taken *atomic.Bool) {
	t.Helper()
	taken, _ = pressAtNthOffer(t, a, 1, ready)
	return taken
}

// pressAtNthOffer is pressAtOffer at each of the stop's first nth offers,
// the nth once ready fires: each of a stop's waits offers once (a kill's
// wait after the stop's own is the second). taken says whether the nth press
// was taken. logs is what a's stop said, at Info and above.
func pressAtNthOffer(t *testing.T, a *App, nth int32, ready <-chan struct{}) (taken *atomic.Bool, logs *observer.ObservedLogs) {
	t.Helper()
	offer := utils.StopOffer
	utils.StopOffer = 0
	t.Cleanup(func() { utils.StopOffer = offer; utils.ClearInterrupted(); utils.ResetInterruptedAgain() })
	utils.MarkInterrupted()
	taken = &atomic.Bool{}
	var offers atomic.Int32
	core, logs := observer.New(zapcore.InfoLevel)
	a.logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if !strings.Contains(e.Message, "press Ctrl+C again") {
			return nil
		}
		switch n := offers.Add(1); {
		case n < nth:
			utils.MarkInterruptedAgain()
		case n == nth:
			if ready != nil {
				<-ready
			}
			taken.Store(utils.MarkInterruptedAgain())
		}
		return nil
	}))
	return taken, logs
}

// keploy's stop of a compose app whose processes it reads waits for the
// container's stop on its events, as a docker run app's does. Its main
// process exiting is no kill, though the daemon has not said so (a starved
// daemon is slow to), and no stop either: the stop is through once the daemon
// says so, and the next set's start finds the container stopped. Taken for
// the stop, the main process's exit let the next set's start be the daemon's
// no-op on a container it still called running, and the exit the daemon took
// after it was a halt of the app keploy had just started: that set ended
// APP_HALTED with no report. A daemon that never says so is an error once the
// bound on no news has passed, or at Ctrl+C, and no kill, as nothing of the
// container's runs. A wait that fails is asked again; only where that fails
// too does the main process's exit stand for the stop. A shutdown that keeps
// making progress past the stall bound is waited for; one that makes none is
// killed once the stall bound of the time both it and keploy could run has
// passed, and the kill is said with the bound.
func TestTheComposeAppsStopWaitsForTheContainersStopEvents(t *testing.T) {
	t.Run("its main process exits, and the daemon never says so", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		a, d := laggingApp(t, main.Process.Pid)
		killed, err := a.StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not said its container") || killed != "" || strings.Join(signals, ",") != "SIGINT" || !running {
			t.Fatalf("said %q (err %v) after %v, the daemon calling it running %v; want the daemon's silence an error, no kill", killed, err, signals, running)
		}
	})
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		main := started(t, "sleep", "600")
		a, d := laggingApp(t, main.Process.Pid)
		gone := exitedOf(t, main.Process.Pid)
		over := make(chan struct{})
		defer close(over)
		go func() {
			select {
			case <-gone:
				d.mu.Lock()
				d.stop()
				d.mu.Unlock()
			case <-over:
			}
		}()
		killed, err := a.StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err != nil || killed != "" || strings.Join(signals, ",") != "SIGINT" || running {
			t.Fatalf("said %q (err %v) after %v, the daemon calling it running %v; want the stop through on the daemon's word, no kill", killed, err, signals, running)
		}
		anew, err := a.StartComposeApp(context.Background())
		if err != nil || !anew {
			t.Fatalf("the next start: anew %v (err %v); want a container the daemon calls stopped, started anew", anew, err)
		}
	})
	// The stop offers to end at Ctrl+C at its first look (the run
	// interrupted, the offer at once), and the press comes in that look,
	// once the main process has exited: the wait for the daemon ends on
	// it. The stall bound is 10s, so a press the stop loses fails the row
	// (the daemon's silence an error after the bound), not holds it.
	t.Run("its main process exits, and Ctrl+C is pressed at the stop's offer before the daemon says so", func(t *testing.T) {
		stallBudget(t, 10*time.Second)
		main := started(t, "sleep", "600")
		a, d := laggingApp(t, main.Process.Pid)
		taken := pressAtOffer(t, a, exitedOf(t, main.Process.Pid))
		killed, err := a.StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "at the press of Ctrl+C") || killed != "" || strings.Join(signals, ",") != "SIGINT" || !running || !taken.Load() {
			t.Fatalf("said %q (err %v) after %v, the daemon calling it running %v, the press taken %v; want the press to end the wait for the daemon, no kill", killed, err, signals, running, taken.Load())
		}
	})
	t.Run("Ctrl+C is pressed at the stop's offer while its main process runs on", func(t *testing.T) {
		stallBudget(t, 10*time.Second)
		// An app that ignores keploy's interrupt.
		main := startedAfter(t, `trap "" TERM`)
		a, d := laggingApp(t, main.Process.Pid)
		taken := pressAtOffer(t, a, nil)
		killed, err := a.StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err != nil || killed != "keploy killed its container at the press of Ctrl+C, before it had stopped" || strings.Join(signals, ",") != "SIGINT,SIGKILL" || !taken.Load() {
			t.Fatalf("said %q (err %v) after %v, the press taken %v; want the press's kill said", killed, err, signals, taken.Load())
		}
	})
	t.Run("its main process exits, and every wait of the daemon's fails", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		a, d := laggingApp(t, main.Process.Pid)
		d.waitFails = -1
		killed, err := a.StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err != nil || killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want the main process's exit taken for the stop, no kill", killed, err, signals)
		}
	})
	t.Run("its main process exits, the daemon's wait fails once, and the daemon never says so", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		a, d := laggingApp(t, main.Process.Pid)
		d.waitFails = 1
		killed, err := a.StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not said its container") || killed != "" || strings.Join(signals, ",") != "SIGINT" || !running {
			t.Fatalf("said %q (err %v) after %v, the daemon calling it running %v; want the wait asked again, and the daemon's silence an error", killed, err, signals, running)
		}
	})
	t.Run("no progress, and the daemon never says its kill ended it", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		// An app that ignores keploy's interrupt (an ignored signal stays
		// ignored across exec).
		main := startedAfter(t, `trap "" TERM`)
		a, d := laggingApp(t, main.Process.Pid)
		d.killLags = true
		killed, err := a.StopComposeApp(context.Background())
		running, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not said its container") || !strings.Contains(killed, "made no progress") || strings.Join(signals, ",") != "SIGINT,SIGKILL" || !running {
			t.Fatalf("said %q (err %v) after %v, the daemon calling it running %v; want the stall kill said, and the daemon's silence after it an error", killed, err, signals, running)
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
