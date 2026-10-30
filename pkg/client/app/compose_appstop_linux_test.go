package app

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// keploy's stop of a compose app whose processes it reads waits for the
// container's stop on its events, as a docker run app's does: its main
// process exiting is the stop, no kill, though the daemon has not said so (a
// starved daemon is slow to), and the stop returns once the daemon has
// recorded it; a shutdown that keeps making progress past the stall
// bound is waited for; one that makes none is killed once the stall bound of
// the time both it and keploy could run has passed, and the kill is said with
// the bound.
func TestTheComposeAppsStopWaitsForTheContainersStopEvents(t *testing.T) {
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		stallBudget(t, 3*time.Second)
		main := started(t, "sh", "-c", "sleep 600 & sleep 1")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		core, logs := observer.New(zap.DebugLevel)
		a := composeApp(d)
		a.logger = zap.New(core)
		type stopEnd struct {
			killed string
			err    error
		}
		done := make(chan stopEnd, 1)
		go func() {
			killed, err := a.StopComposeApp(context.Background())
			done <- stopEnd{killed, err}
		}()
		// The daemon records the stop once keploy's stop waits for it.
		eventually(t, "keploy's stop waits for the daemon to record it, or returns", func() bool {
			return logs.FilterMessageSnippet("keploy waits for the docker daemon to record its stop").Len() > 0 || len(done) > 0
		})
		d.mu.Lock()
		d.stop()
		d.mu.Unlock()
		r := <-done
		killed, err := r.killed, r.err
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

// keploy's stop of a compose app sees its container stop (its main process
// exits) before the docker daemon has recorded the stop, which a loaded
// daemon takes a while to do; until it has, it calls the container running.
// The stop returned on the exit, and the next set's start was the daemon's
// no-op on a container it still called running, so that set ran with no
// app; the stop's die, sent once the daemon had recorded it, reached the
// watch after that start had ended keploy's stop, and was taken for the
// app's halt: the run ended, and the set was lost. The stop is through only
// once the daemon has recorded it: the start then starts the app anew, and
// the stop's die is no halt, though it reaches the watch after the start.
func TestAComposeStopTheDaemonHasNotRecordedIsNoHaltOfTheNextSet(t *testing.T) {
	stallBudget(t, time.Minute)
	main := started(t, "sleep", "600")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	// keploy's interrupt ends the container's process; the daemon records
	// the container's stop only when the test says.
	d.onKill = func(signal string) {
		if signal == "SIGINT" {
			_ = syscall.Kill(main.Process.Pid, syscall.SIGINT)
		}
	}
	core, logs := observer.New(zap.DebugLevel)
	a := composeApp(d)
	a.logger = zap.New(core)
	ctx := context.Background()
	ended := make(chan struct{}, 1)
	stopWatch := a.watchComposeApp(ctx, func() { ended <- struct{}{} })
	defer stopWatch()
	stream := <-d.streams
	<-d.errStreams

	stopDone := make(chan error, 1)
	go func() {
		_, err := a.StopComposeApp(ctx)
		stopDone <- err
	}()
	const awaits = "keploy waits for the docker daemon to record its stop"
	eventually(t, "keploy's stop waits for the daemon to record it, or returns", func() bool {
		return logs.FilterMessageSnippet(awaits).Len() > 0 || len(stopDone) > 0
	})
	early := len(stopDone) > 0
	record := func() {
		d.mu.Lock()
		d.stop()
		d.mu.Unlock()
	}
	if !early {
		// The daemon records the stop while keploy's stop waits for it.
		record()
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	anew, err := a.StartComposeApp(ctx)
	if early {
		// The daemon records the stop after the next set's start.
		record()
	}
	// The stop's die reaches the watch after the start; the watch's
	// inspect says it has read it.
	for len(d.inspected) > 0 {
		<-d.inspected
	}
	stream <- die("0")
	<-d.inspected
	stopWatch()
	running, _, starts := d.state()
	if early || err != nil || !anew || !running || starts != 1 {
		t.Errorf("keploy's stop returned before the daemon had recorded it (%v); the next start: anew %v (err %v), the container running %v after %d start(s); want the stop to wait for the daemon's record, and a start that starts the app anew",
			early, anew, err, running, starts)
	}
	select {
	case <-ended:
		t.Errorf("keploy's own stop was taken for the app's halt (%+v): its die reached the watch after the next set's start", a.composeAppHalt())
	default:
	}
}

// awaitsRecord is what keploy's stop logs as it waits for the daemon to
// record a stop it has seen.
const awaitsRecord = "keploy waits for the docker daemon to record its stop"

// composeStopEnd is how keploy's stop of a compose app ended.
type composeStopEnd struct {
	killed string
	err    error
}

// loggedComposeApp is composeApp(d) with its log kept.
func loggedComposeApp(d *composeDocker) (*App, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	a := composeApp(d)
	a.logger = zap.New(core)
	return a, logs
}

// watched starts a's watch of its container until the test ends: the stream
// its events go out on, and the channel each end of the run is said on.
func watched(t *testing.T, a *App, d *composeDocker) (chan events.Message, chan struct{}) {
	t.Helper()
	ended := make(chan struct{}, 1)
	t.Cleanup(a.watchComposeApp(context.Background(), func() { ended <- struct{}{} }))
	stream := <-d.streams
	<-d.errStreams
	return stream, ended
}

// stopInTheBackground runs keploy's stop of a; its end comes on the channel.
func stopInTheBackground(a *App) chan composeStopEnd {
	done := make(chan composeStopEnd, 1)
	go func() {
		killed, err := a.StopComposeApp(context.Background())
		done <- composeStopEnd{killed, err}
	}()
	return done
}

// awaitTheRecordOrTheEnd waits until keploy's stop waits for the daemon to
// record it (logs), or has ended (done), and fails t if it ended: it
// returned before the daemon had recorded the stop.
func awaitTheRecordOrTheEnd(t *testing.T, logs *observer.ObservedLogs, done chan composeStopEnd, what string) {
	t.Helper()
	eventually(t, "keploy's stop waits for the daemon to record it, or returns", func() bool {
		return logs.FilterMessageSnippet(awaitsRecord).Len() > 0 || len(done) > 0
	})
	if len(done) > 0 {
		r := <-done
		t.Fatalf("keploy's stop returned (said %q, err %v) %s, before the docker daemon had recorded the stop", r.killed, r.err, what)
	}
}

// theNextSetStartsAnew starts the app again after keploy's stop, which the
// daemon has recorded, sends the stop's die (code) to the watch after the
// start, and fails t unless the start started the app anew and the die was
// no halt.
func theNextSetStartsAnew(t *testing.T, a *App, d *composeDocker, stream chan events.Message, ended chan struct{}, code string) {
	t.Helper()
	anew, err := a.StartComposeApp(context.Background())
	for len(d.inspected) > 0 {
		<-d.inspected
	}
	stream <- die(code)
	<-d.inspected // the watch has read the die
	running, _, starts := d.state()
	if err != nil || !anew || !running || starts != 1 {
		t.Errorf("the next start: anew %v (err %v), the container running %v after %d start(s); want the app started anew", anew, err, running, starts)
	}
	select {
	case <-ended:
		t.Errorf("keploy's own stop was taken for the app's halt (%+v): its die reached the watch after the next set's start", a.composeAppHalt())
	default:
	}
}

// keploy's kill of a compose app that made no progress on its way out ends
// its container's process before the docker daemon has recorded the stop, as
// its interrupt's exit can: the kill is the stall's end that load brings on.
// The stop is through only once the daemon has recorded it there too, so the
// next set's start starts the app anew, and the kill's die is no halt.
func TestAComposeStopsKillIsThroughOnceTheDaemonHasRecordedIt(t *testing.T) {
	stallBudget(t, time.Second)
	main := started(t, "sleep", "600")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	// The app ignores keploy's interrupt (it is not sent on); keploy's kill
	// ends its process, and the daemon records the stop only when the test
	// says.
	d.holdsKill = true
	d.onKill = func(signal string) {
		if signal == "SIGKILL" {
			_ = syscall.Kill(main.Process.Pid, syscall.SIGKILL)
		}
	}
	a, logs := loggedComposeApp(d)
	stream, ended := watched(t, a, d)
	done := stopInTheBackground(a)
	awaitTheRecordOrTheEnd(t, logs, done, "once its kill had ended the app's process")
	d.mu.Lock()
	d.stop()
	d.mu.Unlock()
	r := <-done
	_, signals, _ := d.state()
	if r.err != nil || r.killed == "" || strings.Join(signals, ",") != "SIGINT,SIGKILL" {
		t.Fatalf("said %q (err %v) after %v; want the stall's kill said, and the stop through on the daemon's record", r.killed, r.err, signals)
	}
	theNextSetStartsAnew(t, a, d, stream, ended, "137")
}

// The docker daemon's wait for keploy's stop of a compose app can fail (its
// connection breaks), before or after the container's main process exits.
// That says nothing of the stop: the main process's exit is not the daemon's
// record of it (the race keploy's stop waits out), so keploy asks the daemon
// again, and the stop is through on its answer, with no kill and no error.
func TestAComposeStopAsksTheDaemonAgainWhenItsWaitFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after bool // the wait fails after the main process exits
	}{
		{"before the main process exits", false},
		{"after the main process exits", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stallBudget(t, time.Minute)
			main := started(t, "sleep", "600")
			d := newComposeDocker(true, false)
			d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
			d.onKill = func(signal string) {
				if signal == "SIGINT" {
					_ = syscall.Kill(main.Process.Pid, syscall.SIGINT)
				}
			}
			fails := make(chan error, 1)
			d.waitFails = fails
			broke := errors.New("unexpected EOF")
			if !tc.after {
				fails <- broke
			}
			a, logs := loggedComposeApp(d)
			stream, ended := watched(t, a, d)
			done := stopInTheBackground(a)
			awaitTheRecordOrTheEnd(t, logs, done, "once the app's main process had exited")
			if tc.after {
				fails <- broke
			}
			eventually(t, "keploy asks the daemon again, or its stop returns", func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.waits >= 2 || len(done) > 0
			})
			if len(done) > 0 {
				r := <-done
				t.Fatalf("keploy's stop returned (said %q, err %v) once the daemon's wait had failed, before the daemon had recorded the stop: the main process's exit was taken for the daemon's record", r.killed, r.err)
			}
			d.mu.Lock()
			d.stop()
			d.mu.Unlock()
			r := <-done
			_, signals, _ := d.state()
			if r.err != nil || r.killed != "" || strings.Join(signals, ",") != "SIGINT" {
				t.Fatalf("said %q (err %v) after %v; want the stop through on the daemon's answer to the wait asked again, no kill", r.killed, r.err, signals)
			}
			theNextSetStartsAnew(t, a, d, stream, ended, "130")
		})
	}
}

// A docker daemon that never records keploy's stop of a compose app, whose
// main process has exited, leaves the stop in error once
// utils.StopStallBudget of keploy's runnable time has passed: the next set
// would start a container the daemon still calls running. It does not kill:
// the app has stopped.
func TestAComposeStopTheDaemonNeverRecordsIsAnError(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	main := started(t, "sleep", "600")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	d.onKill = func(signal string) {
		if signal == "SIGINT" {
			_ = syscall.Kill(main.Process.Pid, syscall.SIGINT)
		}
	}
	killed, err := composeApp(d).StopComposeApp(context.Background())
	_, signals, _ := d.state()
	if err == nil || !strings.Contains(err.Error(), "the docker daemon had not recorded its stop") || killed != "" || strings.Join(signals, ",") != "SIGINT" {
		t.Fatalf("said %q (err %v) after %v; want the stop in error, the daemon had not recorded it, no kill", killed, err, signals)
	}
}
