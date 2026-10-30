package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
		a, logs := loggedComposeApp(d)
		done := stopInTheBackground(a)
		// The daemon records the stop once keploy's stop waits for it.
		awaitTheRecordOrTheEnd(t, logs, done, "once the app's main process had exited")
		d.mu.Lock()
		d.stop()
		d.mu.Unlock()
		r := <-done
		_, signals, _ := d.state()
		if r.err != nil || r.killed != "" || strings.Join(signals, ",") != "SIGINT" {
			t.Fatalf("said %q (err %v) after %v; want the main process's exit taken for the stop, no kill", r.killed, r.err, signals)
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
	d := interruptedApp(t)
	a, logs := loggedComposeApp(d)
	stream, ended, stopWatch := watched(t, a, d)
	done := stopInTheBackground(a)
	eventually(t, "keploy's stop waits for the daemon to record it, or returns", func() bool {
		return logs.FilterMessageSnippet(awaitsRecord).Len() > 0 || len(done) > 0
	})
	early := len(done) > 0
	record := func() {
		d.mu.Lock()
		d.stop()
		d.mu.Unlock()
	}
	if !early {
		// The daemon records the stop while keploy's stop waits for it.
		record()
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
	anew, err := a.StartComposeApp(context.Background())
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

// watched starts a's watch of its container until the test ends, or until
// stop is called: the stream its events go out on, and the channel each end
// of the run is said on. Once stop has returned, the watch has decided every
// event it read.
func watched(t *testing.T, a *App, d *composeDocker) (stream chan events.Message, ended chan struct{}, stop func()) {
	t.Helper()
	ended = make(chan struct{}, 1)
	stop = a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
	t.Cleanup(stop)
	stream = <-d.streams
	<-d.errStreams
	return stream, ended, stop
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
// start, stops the watch once it has read the die (stopWatch), and fails t
// unless the start started the app anew and the die was no halt.
func theNextSetStartsAnew(t *testing.T, a *App, d *composeDocker, stream chan events.Message, ended chan struct{}, stopWatch func(), code string) {
	t.Helper()
	anew, err := a.StartComposeApp(context.Background())
	for len(d.inspected) > 0 {
		<-d.inspected
	}
	stream <- die(code)
	<-d.inspected // the watch has read the die
	stopWatch()   // and has decided it
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
// next set's start starts the app anew, and the kill's die is no halt. A
// daemon that never records it leaves the stop in error, with the kill said,
// once utils.StopStallBudget of keploy's runnable time has passed: the next
// set would start a container the daemon still calls running.
func TestAComposeStopsKillIsThroughOnceTheDaemonHasRecordedIt(t *testing.T) {
	// killedApp is an app that ignores keploy's interrupt (it is not sent
	// on); keploy's kill ends its process, and the daemon records the stop
	// only when the test says.
	killedApp := func(t *testing.T) *composeDocker {
		main := started(t, "sleep", "600")
		d := newComposeDocker(true, false)
		d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
		d.holdsKill = true
		d.onKill = func(signal string) {
			if signal == "SIGKILL" {
				_ = syscall.Kill(main.Process.Pid, syscall.SIGKILL)
			}
		}
		return d
	}
	t.Run("the daemon records the stop", func(t *testing.T) {
		stallBudget(t, time.Second)
		d := killedApp(t)
		a, logs := loggedComposeApp(d)
		stream, ended, stopWatch := watched(t, a, d)
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
		theNextSetStartsAnew(t, a, d, stream, ended, stopWatch, "137")
	})
	t.Run("the daemon never records it", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		d := killedApp(t)
		killed, err := composeApp(d).StopComposeApp(context.Background())
		_, signals, _ := d.state()
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not recorded its stop") || killed == "" || strings.Join(signals, ",") != "SIGINT,SIGKILL" {
			t.Fatalf("said %q (err %v) after %v; want the stall's kill said, and the stop in error: the daemon had not recorded it", killed, err, signals)
		}
	})
}

// interruptedApp is a compose app whose container's process (a real one, its
// cgroup named the container's) keploy's interrupt ends; the daemon records
// the container's stop only when the test says.
func interruptedApp(t *testing.T) *composeDocker {
	t.Helper()
	main := started(t, "sleep", "600")
	d := newComposeDocker(true, false)
	d.pid, d.id = main.Process.Pid, cgroupOf(t, main.Process.Pid)
	d.onKill = func(signal string) {
		if signal == "SIGINT" {
			_ = syscall.Kill(main.Process.Pid, syscall.SIGINT)
		}
	}
	return d
}

// waitsAsked is how many waits the daemon d was asked for.
func waitsAsked(d *composeDocker) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waits
}

// The docker daemon's wait for keploy's stop of a compose app can fail (its
// connection breaks): before the container's main process exits, after it,
// or once the daemon has recorded the stop. That says nothing of the stop:
// the main process's exit is not the daemon's record of it (the race keploy's
// stop waits out), so keploy asks the daemon again, for the container not
// running, which the daemon answers at once for a stop it has recorded; the
// stop is through on its answer, with no kill and no error. Asked again for
// the container's next exit, a recorded stop is never answered.
func TestAComposeStopAsksTheDaemonAgainWhenItsWaitFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		// when the wait fails: before the main process exits, after it,
		// or once the daemon has recorded the stop.
		after, recorded bool
	}{
		{"before the main process exits", false, false},
		{"after the main process exits", true, false},
		{"once the daemon has recorded the stop", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stallBudget(t, time.Minute)
			d := interruptedApp(t)
			fails := make(chan error, 1)
			d.waitFails = fails
			broke := errors.New("unexpected EOF")
			if !tc.after {
				fails <- broke
			}
			a, logs := loggedComposeApp(d)
			stream, ended, stopWatch := watched(t, a, d)
			done := stopInTheBackground(a)
			awaitTheRecordOrTheEnd(t, logs, done, "once the app's main process had exited")
			record := func() {
				d.mu.Lock()
				d.stop()
				d.mu.Unlock()
			}
			if tc.recorded {
				record()
			}
			if tc.after {
				fails <- broke
			}
			if !tc.recorded {
				eventually(t, "keploy asks the daemon again, or its stop returns", func() bool {
					return waitsAsked(d) >= 2 || len(done) > 0
				})
				if len(done) > 0 {
					r := <-done
					t.Fatalf("keploy's stop returned (said %q, err %v) once the daemon's wait had failed, before the daemon had recorded the stop: the main process's exit was taken for the daemon's record", r.killed, r.err)
				}
				record()
			}
			r := <-done
			_, signals, _ := d.state()
			if r.err != nil || r.killed != "" || strings.Join(signals, ",") != "SIGINT" || waitsAsked(d) != 2 {
				t.Fatalf("said %q (err %v) after %v, %d wait(s); want the stop through on the daemon's answer to the one wait asked again, no kill", r.killed, r.err, signals, waitsAsked(d))
			}
			theNextSetStartsAnew(t, a, d, stream, ended, stopWatch, "130")
		})
	}
}

// A daemon that fails every wait for keploy's stop of a compose app (it
// refuses them) is asked again at a walking pace (composeDaemonAskPace), not
// in a spin that would ask it thousands of times while it is loaded, and the
// stop ends in error once utils.StopStallBudget of keploy's runnable time
// has passed (here shortened) with no word of the daemon's: it had not
// recorded the stop. It does not kill: the app has stopped.
func TestAComposeStopAsksADaemonThatFailsItsWaitsAtAWalkingPace(t *testing.T) {
	stallBudget(t, time.Second)
	d := interruptedApp(t)
	d.waitErr = errors.New("Error response from daemon: the daemon is shutting down")
	began := time.Now()
	killed, err := composeApp(d).StopComposeApp(context.Background())
	asked := waitsAsked(d)
	took := time.Since(began)
	_, signals, _ := d.state()
	if err == nil || !strings.Contains(err.Error(), "the docker daemon had not recorded its stop") || killed != "" || strings.Join(signals, ",") != "SIGINT" {
		t.Fatalf("said %q (err %v) after %v; want the stop in error, the daemon had not recorded it, no kill", killed, err, signals)
	}
	// The first wait, the one asked again at once, and one for each pace
	// the stop took.
	if most := int(took/composeDaemonAskPace) + 2; asked < 2 || asked > most {
		t.Fatalf("keploy asked the daemon for %d waits in %v; want it asked again, at most %d times (one each %v)", asked, took, most, composeDaemonAskPace)
	}
}

// A container the docker daemon no longer has (removed once its main process
// has exited) ends keploy's stop of it at the daemon's word: it has no more to
// say of the container. The stop is through with that one wait, no error and
// no kill; asked again, the daemon says the same until the stall bound, and
// the set ended in error ("the docker daemon had not recorded its stop").
func TestAComposeStopOfAContainerTheDaemonNoLongerHasIsThroughAtOnce(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	d := interruptedApp(t)
	fails := make(chan error, 1)
	d.waitFails = fails
	a, logs := loggedComposeApp(d)
	done := stopInTheBackground(a)
	awaitTheRecordOrTheEnd(t, logs, done, "once the app's main process had exited")
	// Removed: this wait, and every later one, answer that the daemon has
	// no such container.
	d.mu.Lock()
	d.removed = true
	d.mu.Unlock()
	fails <- errdefs.NotFound(errors.New("No such container: app-1"))
	r := <-done
	_, signals, _ := d.state()
	if r.err != nil || r.killed != "" || strings.Join(signals, ",") != "SIGINT" || waitsAsked(d) != 1 {
		t.Fatalf("said %q (err %v) after %v, %d wait(s); want the gone container the stop's end, at its one wait, no kill", r.killed, r.err, signals, waitsAsked(d))
	}
}

// A docker daemon that never records keploy's stop of a compose app, whose
// main process has exited, leaves the stop in error once
// utils.StopStallBudget of keploy's runnable time has passed: the next set
// would start a container the daemon still calls running. It does not kill:
// the app has stopped.
func TestAComposeStopTheDaemonNeverRecordsIsAnError(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	d := interruptedApp(t)
	killed, err := composeApp(d).StopComposeApp(context.Background())
	_, signals, _ := d.state()
	if err == nil || !strings.Contains(err.Error(), "the docker daemon had not recorded its stop") || killed != "" || strings.Join(signals, ",") != "SIGINT" {
		t.Fatalf("said %q (err %v) after %v; want the stop in error, the daemon had not recorded it, no kill", killed, err, signals)
	}
}

// A Ctrl+C at keploy's offer to end its stop of a compose app ends the stop
// while keploy waits for the docker daemon to record it, the app's processes
// gone. The stop waits twice, for the container's stop and then for the
// daemon's record of it, and one offer stands across both waits. With an
// offer each, a press the first wait's offer took in the look that found the
// processes gone, or a press after they went and before the second wait's
// own offer (StopOffer after it began: 5 s), ended nothing: the stop waited
// on for the daemon. Once the processes are gone, the offer says a press
// stops the wait, as nothing is left to kill. The stall bound is an hour, so
// a press that ends nothing fails the row at its 20 s bound.
func TestACtrlCAtTheComposeStopsOfferEndsItsWaitForTheDaemonsRecord(t *testing.T) {
	const pressed = "keploy stopped waiting for the docker daemon to record its stop at the press of Ctrl+C"
	t.Run("a press taken at the offer in the look that finds the app gone", func(t *testing.T) {
		var taken atomic.Bool
		var once sync.Once
		r, offers := stopWithAPress(t, nil, func(msg string, pid int) {
			if strings.Contains(msg, "press Ctrl+C again") {
				once.Do(func() {
					// The user's press, taken by the standing offer; the
					// app's own shutdown ends at the same moment.
					taken.Store(utils.MarkInterruptedAgain())
					killAndAwait(pid)
				})
			}
		})
		if !taken.Load() || r.killed != "" || r.err == nil || !strings.Contains(r.err.Error(), pressed) {
			t.Fatalf("said %q, err %v, the press taken %v; want the stop ended at the press, with no kill", r.killed, r.err, taken.Load())
		}
		if offers[0] != utils.OfferKillNow {
			t.Fatalf("offers %q; want the first to offer the kill", offers)
		}
		for _, o := range offers[1:] {
			if o != composeOfferStopWaiting {
				t.Fatalf("offers %q; want every offer once the app's processes are gone to say a press stops the wait", offers)
			}
		}
	})
	t.Run("a press after the app has gone, before the wait for the record offers", func(t *testing.T) {
		var once sync.Once
		var press atomic.Bool
		r, _ := stopWithAPress(t, nil, func(msg string, pid int) {
			switch {
			case strings.Contains(msg, "press Ctrl+C again"):
				once.Do(func() {
					// The first wait's offer stands; the app's shutdown
					// ends now, with no press yet. A wait that made its own
					// offer would make it an hour from its start.
					utils.StopOffer = time.Hour
					killAndAwait(pid)
				})
			case strings.Contains(msg, awaitsRecord):
				// The user presses Ctrl+C as keploy waits for the daemon.
				press.Store(true)
				utils.MarkInterruptedAgain()
			}
		})
		if !press.Load() || r.killed != "" || r.err == nil || !strings.Contains(r.err.Error(), pressed) {
			t.Fatalf("said %q, err %v, pressed %v; want the stop ended at the press, with no kill", r.killed, r.err, press.Load())
		}
	})
	t.Run("the offer once the app has exited says a press stops the wait", func(t *testing.T) {
		var once sync.Once
		r, offers := stopWithAPress(t, func(signal string, pid int) {
			if signal == "SIGINT" {
				// The app exits at keploy's interrupt; the daemon has not
				// recorded it.
				killAndAwait(pid)
			}
		}, func(msg string, _ int) {
			if msg == composeOfferStopWaiting {
				once.Do(func() { utils.MarkInterruptedAgain() })
			}
		})
		if r.killed != "" || r.err == nil || !strings.Contains(r.err.Error(), pressed) {
			t.Fatalf("said %q, err %v (offers %q); want the stop ended at the press of the offer to stop waiting, with no kill", r.killed, r.err, offers)
		}
	})
}

// stopWithAPress runs keploy's stop of a compose app whose main process (a
// real one) ignores the interrupt, which is not sent on to it, and whose
// daemon records the stop only at a SIGKILL (or the test's end). The run is
// interrupted, StopOffer is 0 and the stall bound an hour: only a press ends
// a stop that waits. onKill (nil: none) sees each signal keploy sends the
// container, as the daemon takes it; hook sees each message keploy logs, on
// the stop's own goroutine. It returns the stop's end and the offers it made
// to end at a press, and fails t when the stop does not end in 20 s.
func stopWithAPress(t *testing.T, onKill func(signal string, pid int), hook func(msg string, pid int)) (composeStopEnd, []string) {
	t.Helper()
	stallBudget(t, time.Hour)
	offer := utils.StopOffer
	utils.StopOffer = 0
	t.Cleanup(func() { utils.StopOffer = offer; utils.ClearInterrupted(); utils.ResetInterruptedAgain() })
	utils.MarkInterrupted()
	main := started(t, "sleep", "600")
	pid := main.Process.Pid
	d := newComposeDocker(true, false)
	d.pid, d.id = pid, cgroupOf(t, pid)
	if onKill != nil {
		d.onKill = func(signal string) { onKill(signal, pid) }
	}
	// Runs before started's cleanup: the daemon records the stop, so a stop
	// still waiting ends.
	t.Cleanup(func() { d.mu.Lock(); d.stop(); d.mu.Unlock() })
	var mu sync.Mutex
	var offers []string
	core, _ := observer.New(zap.DebugLevel)
	a := composeApp(d)
	a.logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "press Ctrl+C again") {
			mu.Lock()
			offers = append(offers, e.Message)
			mu.Unlock()
		}
		hook(e.Message, pid)
		return nil
	}))
	var r composeStopEnd
	select {
	case r = <-stopInTheBackground(a):
	case <-time.After(20 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the user's Ctrl+C did not end keploy's stop in 20 s: it waits on for the daemon's record (offers made: %q)", offers)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(offers) == 0 {
		t.Fatalf("the stop ended (said %q, err %v) with no offer made", r.killed, r.err)
	}
	return r, append([]string(nil), offers...)
}

// killAndAwait kills process pid and waits, for up to 5 s, until it is gone
// (reaped, or a zombie).
func killAndAwait(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 5000; i++ {
		if _, ok := utils.ProcOf(pid); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
