package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// composeDocker is a docker daemon holding one app container: a kill with
// SIGINT stops it when exitsOnInt, a SIGKILL does unless holdsKill; a start
// runs it; events go out on the stream each Events call opens.
type composeDocker struct {
	docker.Client
	mu         sync.Mutex
	running    bool
	exitsOnInt bool
	signals    []string
	starts     int
	exitedCh   chan struct{}
	streams    chan chan events.Message
	errStreams chan chan error
	// inspected gets one value for each inspect, once it has answered.
	inspected chan struct{}
	// project, the app's compose project; others, its other running
	// containers; stopped, those a ContainerStop stopped, with the grace.
	project string
	others  []string
	stopped map[string]int
	// pid and id are the app container's main process and id, as the
	// daemon gives them (0 and "": its processes cannot be read).
	pid int
	id  string
	// exitCode is the stopped container's.
	exitCode int
	// intDelay, set: a SIGINT stops the container this long after it,
	// not at once (an app whose shutdown takes a while).
	intDelay time.Duration
	// onKill, set, is called with each signal a kill sends (the Linux
	// tests send it on to the container's real process).
	onKill func(signal string)
	// holdsKill: a SIGKILL does not stop the container; the daemon records
	// the stop only when the test says (stop), as a loaded one is late to.
	holdsKill bool
	// waitFails, set, is the next wait's end: it fails with the error the
	// test sends on it, and never answers otherwise (a wait whose
	// connection broke); waitErr, set, fails every wait at once (a daemon
	// that refuses them); removed: the container is gone, and every wait
	// answers that the daemon has no such container. waits counts the
	// waits asked for.
	waitFails chan error
	waitErr   error
	removed   bool
	waits     int
	// inspectFails, set, is asked at each inspect: an error it returns is
	// the inspect's (a loaded daemon's read that gave up, or broke off).
	inspectFails func(ctx context.Context) error
	// startedAt is when the container's current run began (a start that
	// started it), as its inspect gives it; zero: not said.
	startedAt time.Time
}

func (d *composeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !opts.Filters.ExactMatch("label", "com.docker.compose.project="+d.project) {
		return nil, nil
	}
	var out []container.Summary
	for _, id := range d.others {
		if _, done := d.stopped[id]; !done {
			out = append(out, container.Summary{ID: id})
		}
	}
	return out, nil
}

func (d *composeDocker) ContainerStop(_ context.Context, id string, opts container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		d.stopped = map[string]int{}
	}
	d.stopped[id] = -1
	if opts.Timeout != nil {
		d.stopped[id] = *opts.Timeout
	}
	return nil
}

func newComposeDocker(running, exitsOnInt bool) *composeDocker {
	return &composeDocker{running: running, exitsOnInt: exitsOnInt, exitedCh: make(chan struct{}),
		streams: make(chan chan events.Message, 8), errStreams: make(chan chan error, 8), inspected: make(chan struct{}, 8)}
}

func (d *composeDocker) stop() {
	if d.running {
		d.running = false
		close(d.exitedCh)
	}
}

func (d *composeDocker) ContainerKill(_ context.Context, _ string, signal string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signals = append(d.signals, signal)
	if !d.running {
		return errdefs.Conflict(errors.New("container is not running"))
	}
	if d.onKill != nil {
		d.onKill(signal)
	}
	switch {
	case signal == "SIGINT" && d.intDelay > 0:
		go func() {
			time.Sleep(d.intDelay)
			d.mu.Lock()
			d.stop()
			d.mu.Unlock()
		}()
	case (signal == "SIGKILL" && !d.holdsKill) || (signal == "SIGINT" && d.exitsOnInt):
		d.stop()
	}
	return nil
}

// ContainerWait answers as the daemon does for the condition: not-running at
// once on a stopped container, next-exit only at the exit of a run to come
// (none here: a stop holds keploy's stop lock, which its starts take).
func (d *composeDocker) ContainerWait(ctx context.Context, _ string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	exited := d.exitedCh
	running := d.running
	fails := d.waitFails
	d.waitFails = nil
	refused, removed := d.waitErr, d.removed
	d.waits++
	d.mu.Unlock()
	out, errs := make(chan container.WaitResponse, 1), make(chan error, 1)
	switch {
	case removed:
		errs <- errdefs.NotFound(errors.New("No such container: app-1"))
		return out, errs
	case refused != nil:
		errs <- refused
		return out, errs
	}
	if fails != nil {
		go func() {
			select {
			case err := <-fails:
				errs <- err
			case <-ctx.Done():
				errs <- ctx.Err()
			}
		}()
		return out, errs
	}
	if !running {
		if cond != container.WaitConditionNextExit {
			out <- container.WaitResponse{}
			return out, errs
		}
		exited = nil
	}
	go func() {
		select {
		case <-exited:
			out <- container.WaitResponse{}
		case <-ctx.Done():
			errs <- ctx.Err()
		}
	}()
	return out, errs
}

func (d *composeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	if !d.running {
		d.running = true
		d.exitedCh = make(chan struct{})
		d.startedAt = time.Now()
	}
	return nil
}

func (d *composeDocker) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	defer func() { d.inspected <- struct{}{} }()
	d.mu.Lock()
	fails := d.inspectFails
	d.mu.Unlock()
	if fails != nil {
		if err := fails(ctx); err != nil {
			return container.InspectResponse{}, err
		}
	}
	d.mu.Lock()
	running, project, pid, id, code := d.running, d.project, d.pid, d.id, d.exitCode
	var startedAt string
	if !d.startedAt.IsZero() {
		startedAt = d.startedAt.UTC().Format(time.RFC3339Nano)
	}
	d.mu.Unlock()
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, State: &container.State{Running: running, Pid: pid, ExitCode: code, StartedAt: startedAt}},
		Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": project}},
	}, nil
}

func (d *composeDocker) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs, errs := make(chan events.Message, 4), make(chan error, 1)
	d.streams <- msgs
	d.errStreams <- errs
	return msgs, errs
}

func (d *composeDocker) state() (running bool, signals []string, starts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running, append([]string(nil), d.signals...), d.starts
}

func composeApp(d *composeDocker) *App {
	return &App{logger: zap.NewNop(), docker: d, container: "app-1", kind: "docker-compose"}
}

// stallBudget sets utils.StopStallBudget to d for the test.
func stallBudget(t *testing.T, d time.Duration) {
	t.Helper()
	budget := utils.StopStallBudget
	utils.StopStallBudget = d
	t.Cleanup(func() { utils.StopStallBudget = budget })
}

// keploy's stop of a compose app is SIGINT, then a wait for the container's
// stop on its events, then SIGKILL once nothing came for the stall bound
// (here shortened), which it says; a container that is not running is
// nothing to stop. Its exit is no halt until keploy starts it again. Here the
// daemon gives no process of the container to read (see the Linux tests for
// one whose processes keploy reads).
func TestTheComposeAppsStopInterruptsWaitsThenKills(t *testing.T) {
	stallBudget(t, 300*time.Millisecond)
	for _, tc := range []struct {
		name                string
		running, exitsOnInt bool
		signals             string
		killed              bool
	}{
		{"it exits on the interrupt", true, true, "SIGINT", false},
		{"it does not", true, false, "SIGINT,SIGKILL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newComposeDocker(tc.running, tc.exitsOnInt)
			a := composeApp(d)
			killed, err := a.StopComposeApp(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			running, signals, _ := d.state()
			if running || strings.Join(signals, ",") != tc.signals || (killed != "") != tc.killed {
				t.Fatalf("running %v after signals %v, said %q; want stopped after %s, a kill said %v", running, signals, killed, tc.signals, tc.killed)
			}
			if tc.killed && !strings.Contains(killed, "keploy could not read its container's processes, and the docker daemon did not say it had stopped, for 300ms") {
				t.Fatalf("the kill is said as %q, want the bound named", killed)
			}
			if !a.composeApp.stopping.Load() {
				t.Fatal("after keploy's stop the app's exit would be taken for a halt")
			}
			if _, err := a.StartComposeApp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if running, _, starts := d.state(); !running || starts != 1 || a.composeApp.stopping.Load() {
				t.Fatalf("after the start: running %v, %d starts, stopping %v; want running again, watched", running, starts, a.composeApp.stopping.Load())
			}
		})
	}
}

// A compose app that is not running when keploy comes to stop it, which keploy
// had not asked to stop, exited on its own: a halt, which the stop answers
// (models.ErrAppExitedOnItsOwn) with the container's exit code, and keeps,
// though its watch has not seen the die event yet. One keploy had stopped
// already is nothing to stop.
func TestTheComposeAppsStopAnswersAHaltItFinds(t *testing.T) {
	d := newComposeDocker(false, false)
	d.exitCode = 3
	a := composeApp(d)
	killed, err := a.StopComposeApp(context.Background())
	_, signals, _ := d.state()
	if killed != "" || !errors.Is(err, models.ErrAppExitedOnItsOwn) || len(signals) != 0 {
		t.Fatalf("said %q (err %v) after %v; want the halt answered, no signal", killed, err, signals)
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
		t.Fatalf("the halt kept: %v, want exit code 3", h)
	}
	if a.composeApp.stopping.Load() {
		t.Fatal("a halt found at the stop left the app marked as stopping: its watch would take the die for keploy's stop, and not end the run")
	}
	if _, err := a.StopComposeApp(context.Background()); !errors.Is(err, models.ErrAppExitedOnItsOwn) {
		t.Fatalf("a later stop: err %v, want the halt again", err)
	}

	stopped := newComposeDocker(true, true)
	b := composeApp(stopped)
	if _, err := b.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if killed, err := b.StopComposeApp(context.Background()); killed != "" || err != nil || b.composeAppHalt() != nil {
		t.Fatalf("one keploy had stopped: said %q (err %v), halt %v; want nothing to stop", killed, err, b.composeAppHalt())
	}
}

// keploy's stops of a compose app come one at a time: the stop at a set's end
// and the teardown's can come at once (an interrupt as a set ends), and the
// second finds the container stopped by the first, rather than interrupt it
// again and cut its shutdown short.
func TestTheComposeAppsStopsComeOneAtATime(t *testing.T) {
	d := newComposeDocker(true, false)
	d.intDelay = 300 * time.Millisecond
	a := composeApp(d)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = a.StopComposeApp(context.Background())
		}(i)
	}
	wg.Wait()
	_, signals, _ := d.state()
	if errs[0] != nil || errs[1] != nil || strings.Join(signals, ",") != "SIGINT" {
		t.Fatalf("two stops at once: errors %v, signals %v; want one SIGINT, no error", errs, signals)
	}
}

// die is a die event of the app's container with exit code code.
func die(code string) events.Message {
	return events.Message{Action: events.ActionDie, Actor: events.Actor{Attributes: map[string]string{"exitCode": code}}, TimeNano: time.Now().UnixNano()}
}

// An exit of the app's container keploy did not ask for ends the run, once,
// with the app's exit code; one keploy asked for, or a stale one of a
// container running again, does not. A stream that breaks is asked for
// again, and an exit on the new one is still seen.
func TestTheComposeAppsWatchEndsTheRunOnAnExitKeployDidNotAskFor(t *testing.T) {
	d := newComposeDocker(true, true)
	a := composeApp(d)
	var ends atomic.Int32
	ended := make(chan struct{}, 1)
	stop := a.watchComposeApp(context.Background(), func() { ends.Add(1); ended <- struct{}{} })
	defer stop()
	stream := <-d.streams
	errs := <-d.errStreams

	// keploy's own stop, the container down: no halt.
	if _, err := a.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream <- die("0")
	<-d.inspected // the watch has read the event, the container down
	// A stale event of a container keploy has started again: no halt.
	if _, err := a.StartComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream <- die("137")
	<-d.inspected
	// The stream breaks; the watch asks for it again.
	errs <- errors.New("unexpected EOF")
	stream = <-d.streams
	<-d.errStreams
	select {
	case <-ended:
		t.Fatal("the run was ended for an exit keploy asked for, or for a container running again")
	default:
	}

	d.mu.Lock()
	d.exitCode = 3
	d.stop()
	d.mu.Unlock()
	stream <- die("3")
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("an exit keploy did not ask for did not end the run")
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 || ends.Load() != 1 {
		t.Fatalf("halt %+v after %d ends; want exit code 3, once", h, ends.Load())
	}
}

// The die of keploy's own stop can reach the watch after keploy has started
// the container again, and after that new run has exited on its own: it was
// taken for the new run's halt, with keploy's interrupt's exit code (130) for
// an app that had crashed (3), and the new run's own die was never read. A
// die sent before the container's current run began is an earlier run's: it
// is dropped, and the current run's own die ends the run, with its exit code.
// One the watch cannot place (the daemon gives it no time) still ends it
// with the exit code of the run that is down now, as the daemon records it.
func TestALateDieOfKeploysStopIsNotTheHaltOfTheRunAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// placed: the late die has the time the daemon sent it.
		placed bool
	}{
		{"a die sent before the run began is dropped", true},
		{"a die the watch cannot place says the run's own exit code", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d := newComposeDocker(true, true)
			a := composeApp(d)
			ended := make(chan struct{}, 2)
			stop := a.watchComposeApp(ctx, func() { ended <- struct{}{} })
			defer stop()
			stream := <-d.streams
			<-d.errStreams

			// keploy's stop, which the daemon has recorded; its die has
			// not reached the watch yet.
			if _, err := a.StopComposeApp(ctx); err != nil {
				t.Fatal(err)
			}
			if anew, err := startAnew(a.StartComposeApp(ctx)); err != nil || !anew {
				t.Fatalf("the next start: anew %v, err %v; want the app started anew", anew, err)
			}
			// The die was sent before the start began the new run.
			d.mu.Lock()
			late := die("130")
			late.TimeNano = d.startedAt.Add(-time.Millisecond).UnixNano()
			if !tc.placed {
				late.TimeNano = 0
			}
			// The new run exits on its own, with 3.
			d.exitCode = 3
			d.stop()
			d.mu.Unlock()
			for len(d.inspected) > 0 {
				<-d.inspected
			}
			stream <- late
			<-d.inspected // the watch has read the late die
			if tc.placed {
				// The watch reads the new run's own die only once it has
				// decided the late one: an end before that read is the
				// late die's.
				stream <- die("3")
				select {
				case <-d.inspected:
				case <-ended:
					t.Fatalf("the late die of keploy's own stop ended the run (halt %+v); want it dropped, as an earlier run's", a.composeAppHalt())
				}
			}
			select {
			case <-ended:
			case <-time.After(time.Minute):
				t.Fatal("the new run's own exit did not end the run")
			}
			if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
				t.Fatalf("halt %+v; want the new run's own exit code, 3, not keploy's interrupt's (130)", h)
			}
		})
	}
}

// A die is of an earlier run of its container only when the daemon sent it
// before the container's current run began. One the daemon dates only to the
// second is compared to the second: read as the start of that second, the
// current run's own die, sent in the second the run began, was dropped. One
// with no date, or of a run with none, is not placed.
func TestADieIsOfAnEarlierRunOnlyWhenSentBeforeTheRunBegan(t *testing.T) {
	began := time.Date(2026, 10, 31, 10, 0, 0, 300_000_000, time.UTC)
	state := &container.State{StartedAt: began.Format(time.RFC3339Nano)}
	for _, tc := range []struct {
		name    string
		m       events.Message
		state   *container.State
		earlier bool
	}{
		{"sent before the run began, to the nanosecond", events.Message{TimeNano: began.Add(-time.Millisecond).UnixNano()}, state, true},
		{"sent after the run began, to the nanosecond", events.Message{TimeNano: began.Add(time.Millisecond).UnixNano()}, state, false},
		{"sent in the second before the run's, to the second", events.Message{Time: began.Unix() - 1}, state, true},
		{"sent in the run's own second, to the second", events.Message{Time: began.Unix()}, state, false},
		{"not dated", events.Message{}, state, false},
		{"of a run with no start said", events.Message{TimeNano: began.Add(-time.Millisecond).UnixNano()}, &container.State{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeDieOfAnEarlierRun(tc.m, tc.state); got != tc.earlier {
				t.Fatalf("an earlier run's: %v; want %v", got, tc.earlier)
			}
		})
	}
}

// The watch's read of the container a die names had a wall-clock budget of
// 2 s: a loaded daemon slower than that failed it, and the die it could not
// place was taken for a halt when no stop of keploy's was in progress, as for
// the late die of keploy's own stop, read after keploy had started the
// container again: the run ended, and the next set was lost. A read the
// daemon fails is asked again: the watch decides the die on the daemon's
// answer. One the daemon never answers is given up once utils.StopStallBudget
// of keploy's runnable time has passed (here shortened), and the die is
// decided on the event: a real halt still ends the run.
func TestTheComposeAppsWatchAsksAgainWhenTheDaemonFailsToDescribeADiesContainer(t *testing.T) {
	t.Run("a slow read that fails", func(t *testing.T) {
		stallBudget(t, time.Minute)
		ctx := context.Background()
		d := newComposeDocker(true, true)
		a := composeApp(d)
		ended := make(chan struct{}, 2)
		stop := a.watchComposeApp(ctx, func() { ended <- struct{}{} })
		defer stop()
		stream := <-d.streams
		<-d.errStreams
		// keploy's stop, which the daemon has recorded, and its start of
		// the next set's run; the stop's die reaches the watch after both.
		if _, err := a.StopComposeApp(ctx); err != nil {
			t.Fatal(err)
		}
		if anew, err := startAnew(a.StartComposeApp(ctx)); err != nil || !anew {
			t.Fatalf("the next start: anew %v, err %v; want the app started anew", anew, err)
		}
		for len(d.inspected) > 0 {
			<-d.inspected
		}
		// The daemon is slow to answer the watch's first read, which then
		// fails, as a read that gave up does; it fails the next four too,
		// and answers the sixth.
		const failed = 5
		var reads atomic.Int32
		d.mu.Lock()
		d.inspectFails = func(ctx context.Context) error {
			read := reads.Add(1)
			if read > failed {
				return nil
			}
			if read == 1 {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
				}
			}
			return context.DeadlineExceeded
		}
		d.mu.Unlock()
		stream <- die("0")
		// The reads that fail, then the one the daemon answers.
		for i := 0; i <= failed; i++ {
			select {
			case <-d.inspected:
			case <-ended:
				t.Fatalf("the die of keploy's own stop ended the run (halt %+v) after the daemon had failed %d read(s) of its container; want the daemon asked again until it answers", a.composeAppHalt(), reads.Load())
			}
		}
		// Deciding the late die, the watch reads the next one: the run's
		// own exit, which ends the run with its code.
		d.mu.Lock()
		d.exitCode = 3
		d.stop()
		d.mu.Unlock()
		stream <- die("3")
		select {
		case <-ended:
		case <-time.After(time.Minute):
			t.Fatal("the run's own exit did not end the run")
		}
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
			t.Fatalf("halt %+v; want the run's own exit code, 3", h)
		}
	})
	t.Run("a daemon that fails every read", func(t *testing.T) {
		// Asked again at a walking pace (composeDaemonAskPace), not in a
		// spin, until the stall bound; the die is then decided on the
		// event.
		stallBudget(t, time.Second)
		d := newComposeDocker(false, true)
		var reads atomic.Int32
		d.inspectFails = func(context.Context) error {
			reads.Add(1)
			return errors.New("Error response from daemon: the daemon is shutting down")
		}
		a := composeApp(d)
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		defer stop()
		stream := <-d.streams
		<-d.errStreams
		drained := make(chan struct{})
		defer close(drained)
		go func() {
			for {
				select {
				case <-d.inspected:
				case <-drained:
					return
				}
			}
		}()
		began := time.Now()
		stream <- die("3")
		select {
		case <-ended:
		case <-time.After(time.Minute):
			t.Fatal("an exit keploy did not ask for, whose container the daemon never described, did not end the run")
		}
		asked, took := int(reads.Load()), time.Since(began)
		if most := int(took/composeDaemonAskPace) + 2; asked < 2 || asked > most {
			t.Fatalf("the watch read the container %d times in %v; want it asked again, at most %d times (one each %v)", asked, took, most, composeDaemonAskPace)
		}
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
			t.Fatalf("halt %+v; want the event's exit code, 3", h)
		}
	})
	t.Run("a container the daemon no longer has", func(t *testing.T) {
		// Removed: the daemon has no more to say of it, and the die is
		// decided on the event at once, with that one read.
		stallBudget(t, time.Minute)
		d := newComposeDocker(false, true)
		var reads atomic.Int32
		d.inspectFails = func(context.Context) error {
			reads.Add(1)
			return errdefs.NotFound(errors.New("No such container: app-1"))
		}
		a := composeApp(d)
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		defer stop()
		stream := <-d.streams
		<-d.errStreams
		stream <- die("3")
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Fatalf("the die of a container the daemon no longer has was not decided at once (%d reads)", reads.Load())
		}
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 || reads.Load() != 1 {
			t.Fatalf("halt %+v after %d read(s); want the event's exit code, 3, at the one read", h, reads.Load())
		}
	})
	t.Run("a watch that ends while it waits for the read", func(t *testing.T) {
		// It decides nothing: keploy's next stop of the container reads
		// the die. Its end ends the read: the stall bound is an hour, so a
		// read that went on past the watch's end would hold the watch's
		// stop (and stopMu) until that bound.
		stallBudget(t, time.Hour)
		d := newComposeDocker(false, true)
		reading := make(chan struct{}, 1)
		d.inspectFails = func(ctx context.Context) error {
			reading <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		a := composeApp(d)
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		stream := <-d.streams
		<-d.errStreams
		stream <- die("3")
		<-reading
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(time.Minute):
			t.Fatal("the watch's stop did not end its read of the die's container: the read waits for the stall bound")
		}
		select {
		case <-ended:
			t.Fatalf("a watch that had ended took the die it was reading for a halt (%+v)", a.composeAppHalt())
		default:
		}
		if h := a.composeAppHalt(); h != nil {
			t.Fatalf("a watch that had ended kept the halt %+v", h)
		}
	})
	t.Run("a read the daemon never answers", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		d := newComposeDocker(false, true)
		d.inspectFails = func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		a := composeApp(d)
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		defer stop()
		stream := <-d.streams
		<-d.errStreams
		stream <- die("3")
		select {
		case <-ended:
		case <-time.After(time.Minute):
			t.Fatal("an exit keploy did not ask for, whose container the daemon never described, did not end the run")
		}
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
			t.Fatalf("halt %+v; want the event's exit code, 3", h)
		}
	})
}

// A halt ends the run as compose's abort did: APP_HALTED, with the app's exit
// code, and no error for a clean exit.
func TestAComposeHaltIsTheAppsExit(t *testing.T) {
	if e := (composeHalt{exitCode: 3}).appError("logs"); e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 || e.Err == nil || e.AppLogs != "logs" {
		t.Fatalf("exit 3: %+v", e)
	}
	if e := (composeHalt{exitCode: 0}).appError(""); e.AppErrorType != models.ErrAppStopped || e.ExitCode != 0 || e.Err != nil {
		t.Fatalf("exit 0: %+v", e)
	}
}

// Under the proven switch compose runs without the flags that end the stack
// on a container's exit, the user's own too, the rest of the command as
// written; with the switch off they are added as before.
func TestUnderTheSwitchComposeRunsWithoutItsAbortFlags(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for in, want := range map[string]string{
		"docker compose up": "docker compose up",
		"docker compose up --abort-on-container-exit --exit-code-from app":       "docker compose up",
		"docker compose -f a.yml up --exit-code-from=app --build":                "docker compose -f a.yml up --build",
		"docker compose up --abort-on-container-failure  --build":                "docker compose up  --build",
		"docker compose -p abort-on-container-exit up --abort-on-container-exit": "docker compose -p abort-on-container-exit up",
	} {
		if got := ensureComposeExitOnAppFailure(in, "app", false); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	t.Setenv(provenswitch.Env, "")
	if got := ensureComposeExitOnAppFailure("docker compose up", "app", false); got != "docker compose up --abort-on-container-exit --exit-code-from app" {
		t.Errorf("switch off: %q", got)
	}
}

// An exit keploy did not ask for stops the rest of the app's compose project
// as compose's abort did: each container still running, keploy's agent among
// them, is asked to stop with its own grace, so the agent goes once keploy has
// drained what it captured, not on a clock of keploy's, and compose up
// returns.
func TestAHaltStopsTheProjectAsComposesAbortDid(t *testing.T) {
	d := newComposeDocker(false, true)
	d.project, d.others = "proj", []string{"keploy-agent", "dep"}
	a := composeApp(d)
	if !a.stopComposeProject(context.Background()) {
		t.Fatal("the project was not stopped")
	}
	if len(d.stopped) != 2 || d.stopped["keploy-agent"] != -1 || d.stopped["dep"] != -1 {
		t.Fatalf("stopped %v; want the agent and the dependency, each with its own stop grace (-1: none set by keploy)", d.stopped)
	}
	d.project = ""
	if a.stopComposeProject(context.Background()) {
		t.Fatal("a project keploy could not tell was taken for stopped")
	}
}

// keploy record's stop of a compose app, under the switch, says a kill in the
// recording, as for any app it records (P101-b-k); a replay's does not.
func TestAComposeRecordingsStopSaysItsKill(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stallBudget(t, 300*time.Millisecond)
	for _, tc := range []struct {
		mode models.Mode
		want string
	}{
		{models.MODE_RECORD, "keploy stopped the application at the recording's end and keploy could not read its container's processes"},
		{models.MODE_TEST, ""},
	} {
		d := newComposeDocker(true, false)
		a := composeApp(d)
		a.opts.Mode = tc.mode
		if _, err := a.StopComposeApp(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := a.takeSetEndKill(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Fatalf("%s: the run says %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// An up that returned because keploy stopped the app itself holds the run
// until keploy's teardown ends it, or the app keploy started again since
// halts: an up attached to the app alone returns as soon as the app has
// stopped, and the run, so ended, took the project down, keploy's agent with
// it, while the recording was draining it. An up that did not return on
// keploy's stop does not wait.
func TestComposeAwaitTeardownHoldsTheRunAfterKeploysStop(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan struct{})
	close(closed)
	exited := func(status string, code int) map[string]container.InspectResponse {
		return map[string]container.InspectResponse{"app": {ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: status, ExitCode: code}}}}
	}
	for _, tc := range []struct {
		name     string
		stopping bool
		halted   bool
		app      map[string]container.InspectResponse
		ctx      context.Context
		halt     <-chan struct{}
		want     composeTeardownWait
		ended    int
	}{
		{"keploy did not stop the app, which runs on", false, false, nil, cancelled, nil, composeNoWait, 0},
		{"keploy did not stop the app, which compose only created", false, false, exited("created", 0), cancelled, nil, composeNoWait, 0},
		{"the app exited on its own, before the watch took it", false, false, exited("exited", 3), context.Background(), nil, composeHaltCame, 1},
		{"the app halted", true, true, nil, cancelled, nil, composeNoWait, 1},
		{"keploy's teardown ends the run", true, false, nil, cancelled, nil, composeTeardownCame, 0},
		{"the app keploy started again halts", true, false, nil, context.Background(), closed, composeHaltCame, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{logger: zap.NewNop(), container: "app", docker: &inspectRecorder{inspects: tc.app}}
			a.composeApp.stopping.Store(tc.stopping)
			if tc.halted {
				a.composeApp.halted.Store(&composeHalt{exitCode: 1})
			}
			ended := 0
			if got := a.composeAwaitTeardown(tc.ctx, tc.halt, func() { ended++ }); got != tc.want || ended != tc.ended {
				t.Fatalf("composeAwaitTeardown = %d, ending the run %d times; want %d, %d", got, ended, tc.want, tc.ended)
			}
			if tc.app != nil && tc.ended > 0 {
				if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
					t.Fatalf("the halt kept is %+v; want the app's exit, code 3", h)
				}
			}
		})
	}
}

// The run of `docker compose up app` (attached to the app alone) under the
// switch: the up returns once keploy has stopped the app itself, and the run
// holds the project up, keploy's agent in it, until keploy's teardown ends
// the run (its context, which descends from keploy's own: a signal to keploy
// ends it too), or until the app keploy started again since halts. Only then
// is the project taken down. Before, the run ended with the up, and its
// deferred down took the agent down mid-drain.
func TestRunHoldsTheProjectAfterKeploysStopOfAnAppAttachedAlone(t *testing.T) {
	for _, end := range []string{"keploy's teardown", "a halt of the app keploy started again", "the app's own exit, before the watch takes it"} {
		t.Run(end, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			argvLog := stubDockerCLI(t, "", 0)
			upEnds := filepath.Join(t.TempDir(), "app-stopped")
			t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", upEnds)
			core, logs := observer.New(zap.DebugLevel)
			rec := &inspectRecorder{events: make(chan events.Message, 1)}
			a := &App{
				logger:          zap.New(core),
				docker:          rec,
				kind:            utils.DockerCompose,
				cmd:             "docker compose -f /tmp/keploy-compose.yaml up app",
				composeFile:     "/tmp/keploy-compose.yaml",
				keployContainer: "keploy-v3",
				container:       "app",
				composeServices: []string{"app", "keploy-agent"},
			}
			// The project an earlier run read is not this run's: each run
			// reads its own (composeAppProject).
			stale := "an-earlier-runs-project"
			a.composeApp.project.Store(&stale)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ran := make(chan models.AppError, 1)
			go func() { ran <- a.run(ctx) }()

			for !strings.Contains(recordedLog(t, argvLog), "ARG up") {
				time.Sleep(10 * time.Millisecond)
			}
			if p := a.composeApp.project.Load(); p != nil {
				// Errorf: the run goes on to its end below.
				t.Errorf("the run kept the compose project %q an earlier run read", *p)
			}
			if end == "the app's own exit, before the watch takes it" {
				// The app exits on its own (no die event reaches the
				// watch), and the up attached to it returns: the container
				// says it exited, with its code.
				rec.mu.Lock()
				rec.inspects = map[string]container.InspectResponse{"app": {
					ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited", ExitCode: 3}},
					Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": "p1"}},
				}}
				rec.mu.Unlock()
				if err := os.WriteFile(upEnds, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				appErr := <-ran
				if appErr.ExitCode != 3 {
					t.Fatalf("the run ended %+v; want ended by the app's own exit, its exit code 3", appErr)
				}
				if !rec.listedProject("p1") {
					t.Fatal("the run did not stop the app's project, as the watch would on the halt")
				}
				if !strings.Contains(recordedLog(t, argvLog), "ARG down") {
					t.Fatalf("the project was not taken down once the run ended:\n%s", recordedLog(t, argvLog))
				}
				return
			}
			// keploy stops the app (StopComposeApp marks it stopping before
			// its SIGINT), and the up attached to it returns.
			a.composeApp.stopping.Store(true)
			if err := os.WriteFile(upEnds, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for logs.FilterMessageSnippet("the project stays up for keploy's teardown").Len() == 0 {
				select {
				case appErr := <-ran:
					t.Fatalf("the run ended with the up, before keploy's teardown (%+v):\n%s", appErr, recordedLog(t, argvLog))
				default:
				}
				time.Sleep(10 * time.Millisecond)
			}
			if strings.Contains(recordedLog(t, argvLog), "ARG down") {
				t.Fatalf("the project was taken down while the run held it for keploy's teardown:\n%s", recordedLog(t, argvLog))
			}

			var appErr models.AppError
			if end == "keploy's teardown" {
				cancel()
				appErr = <-ran
				if appErr.AppErrorType != models.ErrCtxCanceled {
					t.Fatalf("the run ended %+v; want ended by keploy's teardown", appErr)
				}
			} else {
				// keploy started the app again (StartComposeApp), and it
				// exits on its own: the watch ends the run on the halt.
				a.composeApp.stopping.Store(false)
				rec.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie,
					Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "3"}}}
				appErr = <-ran
				if appErr.ExitCode != 3 {
					t.Fatalf("the run ended %+v; want ended by the app's halt, its exit code 3", appErr)
				}
			}
			if !strings.Contains(recordedLog(t, argvLog), "ARG down") {
				t.Fatalf("the project was not taken down once the run ended:\n%s", recordedLog(t, argvLog))
			}
		})
	}
}

// The run's end on a halt runs once, though the watch and the run, on an up
// that returned on the app's own exit, both come to it.
func TestComposeHaltEndRunsOnce(t *testing.T) {
	rec := &inspectRecorder{inspects: map[string]container.InspectResponse{"app": {
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited", ExitCode: 3}},
		Config:            &container.Config{Labels: map[string]string{"com.docker.compose.project": "p1"}},
	}}}
	a := &App{logger: zap.NewNop(), container: "app", docker: rec}
	halt := make(chan struct{})
	downs := 0
	end := a.composeHaltEnd(context.Background(), halt, func() { downs++ })
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); end() }()
	}
	wg.Wait()
	select {
	case <-halt:
	default:
		t.Fatal("the halt was not said")
	}
	if !a.composeApp.stopping.Load() {
		t.Fatal("the app's exit is still taken for a halt after the run's end")
	}
	rec.mu.Lock()
	lists := len(rec.listedLabels)
	rec.mu.Unlock()
	if lists != 1 || downs != 0 {
		t.Fatalf("the project was stopped %d times (and taken down %d); want once, stopped", lists, downs)
	}
}

// fakeAppDocker answers the inspect of the app's container with running,
// and records its start (StartComposeApp), which makes it run. With locked
// set, it says each call it gets while the stop lock is free.
type fakeAppDocker struct {
	docker.Client
	mu      sync.Mutex
	running bool
	started int
	locked  func(call string)
}

// underStopLock is a fakeAppDocker's locked: it fails t when a docker call
// runs while a's stop lock is free, made outside it.
func underStopLock(t *testing.T, a *App) func(string) {
	return func(call string) {
		if a.composeApp.stopMu.TryLock() {
			a.composeApp.stopMu.Unlock()
			t.Errorf("%s ran outside the stop lock", call)
		}
	}
}

func (d *fakeAppDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	if d.locked != nil {
		d.locked("the inspect")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := &container.State{Status: "exited", Running: d.running}
	if d.running {
		st.Status = "running"
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: st}}, nil
}

func (d *fakeAppDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	if d.locked != nil {
		d.locked("the start")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running = true
	d.started++
	return nil
}

func (d *fakeAppDocker) startedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

// The watch decides an exit under the stop lock, as keploy's stops and starts
// are made: a die that comes as keploy's stop is under way (the lock held,
// stopping not yet set) is keploy's once the stop is through; one that
// comes as keploy starts the app again finds it running once the start is
// through. Unserialised, the first was a halt beside a clean stop, and the
// second a halt of an app keploy had just started.
func TestComposeAppDiedIsDecidedUnderTheStopLock(t *testing.T) {
	die := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "137"}}}
	for _, tc := range []struct {
		name  string
		while func(a *App, d *fakeAppDocker)
	}{
		{"keploy's stop under way", func(a *App, _ *fakeAppDocker) { a.composeApp.stopping.Store(true) }},
		{"keploy's start under way", func(a *App, d *fakeAppDocker) {
			d.mu.Lock()
			d.running = true
			d.mu.Unlock()
			a.composeApp.stopping.Store(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeAppDocker{}
			a := &App{logger: zap.NewNop(), docker: d, container: "app"}
			a.composeApp.stopMu.Lock()
			decided := make(chan bool, 1)
			go func() { decided <- a.composeAppDied(context.Background(), die) }()
			tc.while(a, d)
			a.composeApp.stopMu.Unlock()
			if <-decided || a.composeAppHalt() != nil {
				t.Fatal("an exit that came as keploy's stop or start was under way was taken for a halt")
			}
		})
	}
}

// keploy's start of the app, the watch's decision of an exit, and the run's
// reading of how its up ended are each made under the stop lock: the docker
// calls they make run while it is held.
func TestTheComposeAppsTransitionsAreMadeUnderTheStopLock(t *testing.T) {
	die := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "1"}}}
	t.Run("keploy's start", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		a.composeApp.stopping.Store(true)
		if _, err := a.StartComposeApp(context.Background()); err != nil || d.startedCount() != 1 || a.composeApp.stopping.Load() {
			t.Fatalf("the start: %v, started %d, stopping %v", err, d.startedCount(), a.composeApp.stopping.Load())
		}
	})
	t.Run("the watch's decision", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		if !a.composeAppDied(context.Background(), die) {
			t.Fatal("an exit keploy did not ask for was not a halt")
		}
	})
	t.Run("the run's reading", func(t *testing.T) {
		d := &fakeAppDocker{}
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		d.locked = underStopLock(t, a)
		if got := a.composeAwaitTeardown(context.Background(), nil, func() {}); got != composeHaltCame {
			t.Fatalf("an up that returned on the app's own exit ended %d; want a halt (%d)", got, composeHaltCame)
		}
	})
}

// The run decides how its up ended under the stop lock too: an up that
// returned as keploy's stop of the app was under way (the app already gone,
// stopping not yet set) is keploy's stop once the stop is through, held for
// keploy's teardown, not a halt.
func TestComposeAwaitTeardownIsDecidedUnderTheStopLock(t *testing.T) {
	d := &fakeAppDocker{}
	a := &App{logger: zap.NewNop(), docker: d, container: "app"}
	ctx, cancel := context.WithCancel(context.Background())
	a.composeApp.stopMu.Lock()
	ended := make(chan composeTeardownWait, 1)
	go func() { ended <- a.composeAwaitTeardown(ctx, nil, func() {}) }()
	a.composeApp.stopping.Store(true)
	a.composeApp.stopMu.Unlock()
	cancel()
	if got := <-ended; got != composeTeardownCame {
		t.Fatalf("an up that returned on keploy's stop under way ended %d; want held for keploy's teardown (%d)", got, composeTeardownCame)
	}
}

// THE DOCKER DAEMON, AS KEPLOY'S STOP OF A COMPOSE APP MEETS IT.
//
// daemonModel holds containers as a docker daemon does, and answers by its
// rules (moby v28): a reference is a full ID, else a name, else a unique ID
// prefix (daemon.GetContainer); a list matches its label filters exactly
// (MatchKVList); an events filter matches its labels exactly and its
// container by a prefix of the ID or the name (FuzzyMatch). A kill with the
// container's own stop signal, any kill of one that declares none, and
// SIGKILL cancel its restart policy (killWithSignal, ExitOnNext); any kill
// marks it stopped by hand (unless-stopped). An exit the policy restarts
// leaves it restarting, which the daemon calls running, and its die is sent
// while it is; the restart follows (at once here, unless held). A start
// through the API arms the policy again and zeroes its restart count.
type daemonModel struct {
	docker.Client
	mu      sync.Mutex
	cs      []*modelContainer
	signals []string // "<container's name>:<signal>", each kill as it came
	subs    []modelSub
	// starts counts the API's starts, by container name; inspects, the
	// inspects, by the reference asked for.
	starts, inspects map[string]int
	// listErr, set, fails every list.
	listErr error
}

type modelContainer struct {
	id, name   string
	labels     map[string]string
	stopSignal string // declared stop signal ("" none)
	restart    string // restart policy: "", "always", "on-failure", "unless-stopped"
	// exitsOn: the code it exits with on each signal it acts on; SIGKILL
	// always ends it, 137.
	exitsOn         map[string]int
	running         bool
	restarting      bool
	holdRestart     bool // a restart waits for finishRestart
	canceled        bool // ExitOnNext
	manuallyStopped bool
	exitCode        int
	restarts        int
	stopWaiters     []chan struct{}
}

type modelSub struct {
	ctx  context.Context
	f    events.ListOptions
	msgs chan events.Message
}

func modelSignal(s string) int {
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return map[string]int{"HUP": 1, "INT": 2, "QUIT": 3, "KILL": 9, "USR1": 10, "USR2": 12, "TERM": 15}[s]
}

func (d *daemonModel) add(c *modelContainer) *modelContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.labels == nil {
		c.labels = map[string]string{}
	}
	d.cs = append(d.cs, c)
	return c
}

// lookup is daemon.GetContainer: a full ID, a name, a unique ID prefix.
func (d *daemonModel) lookup(ref string) (*modelContainer, error) {
	if ref == "" {
		return nil, errdefs.InvalidParameter(errors.New("no container named"))
	}
	for _, c := range d.cs {
		if c.id == ref {
			return c, nil
		}
	}
	for _, c := range d.cs {
		if c.name == ref {
			return c, nil
		}
	}
	var found *modelContainer
	for _, c := range d.cs {
		if strings.HasPrefix(c.id, ref) {
			if found != nil {
				return nil, errors.New("multiple IDs found with provided prefix")
			}
			found = c
		}
	}
	if found == nil {
		return nil, errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	return found, nil
}

func (c *modelContainer) attributes(extra map[string]string) map[string]string {
	a := map[string]string{"name": c.name, "image": "img"}
	for k, v := range c.labels {
		a[k] = v
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// emit sends a container event of c to every stream whose filters take it.
func (d *daemonModel) emit(c *modelContainer, action events.Action, extra map[string]string) {
	m := events.Message{Type: events.ContainerEventType, Action: action, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: c.id, Attributes: c.attributes(extra)}}
	for _, s := range d.subs {
		f := s.f.Filters
		if f.Contains("type") && !f.ExactMatch("type", string(m.Type)) ||
			f.Contains("event") && !f.ExactMatch("event", string(m.Action)) ||
			f.Contains("label") && !f.MatchKVList("label", m.Actor.Attributes) ||
			f.Contains("container") && !f.FuzzyMatch("container", m.Actor.ID) && !f.FuzzyMatch("container", m.Actor.Attributes["name"]) {
			continue
		}
		select {
		case s.msgs <- m:
		case <-s.ctx.Done():
		}
	}
}

// exit ends c's process with code: monitor.go's handleContainerExit.
func (d *daemonModel) exit(c *modelContainer, code int) {
	c.exitCode = code
	restart := false
	if !c.canceled {
		switch c.restart {
		case "always":
			restart = true
		case "unless-stopped":
			restart = !c.manuallyStopped
		case "on-failure":
			restart = code != 0
		}
	}
	c.running, c.restarting = restart, restart
	for _, w := range c.stopWaiters {
		close(w)
	}
	c.stopWaiters = nil
	d.emit(c, events.ActionDie, map[string]string{"exitCode": strconv.Itoa(code)})
	if restart && !c.holdRestart {
		d.restarted(c)
	}
}

func (d *daemonModel) restarted(c *modelContainer) {
	c.restarting, c.running = false, true
	c.restarts++
	d.emit(c, events.ActionStart, nil)
}

// finishRestart ends a held restart of the container named name: it runs
// again, unless a kill cancelled the restart meanwhile.
func (d *daemonModel) finishRestart(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, _ := d.lookup(name)
	if c.restarting {
		d.restarted(c)
	}
}

// exitOnItsOwn ends the container named name's process with code, as an app
// that exits on its own does.
func (d *daemonModel) exitOnItsOwn(name string, code int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, _ := d.lookup(name)
	d.exit(c, code)
}

func (d *daemonModel) ContainerKill(_ context.Context, ref, signal string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.lookup(ref)
	if err != nil {
		return err
	}
	d.signals = append(d.signals, c.name+":"+signal)
	if !c.running {
		return errdefs.Conflict(fmt.Errorf("container %s is not running", c.id))
	}
	sig := modelSignal(signal)
	if c.stopSignal == "" || sig == 9 || modelSignal(c.stopSignal) == sig {
		c.canceled = true
	}
	c.manuallyStopped = true
	if c.restarting {
		// No process to signal: the monitor is told not to restart it.
		if c.canceled {
			c.restarting, c.running = false, false
			for _, w := range c.stopWaiters {
				close(w)
			}
			c.stopWaiters = nil
		}
		return nil
	}
	if sig == 9 {
		d.exit(c, 137)
		return nil
	}
	for s, code := range c.exitsOn {
		if modelSignal(s) == sig {
			d.exit(c, code)
			break
		}
	}
	return nil
}

func (d *daemonModel) ContainerWait(ctx context.Context, ref string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, errs := make(chan container.WaitResponse, 1), make(chan error, 1)
	c, err := d.lookup(ref)
	if err != nil {
		errs <- err
		return out, errs
	}
	if !c.running {
		out <- container.WaitResponse{StatusCode: int64(c.exitCode)}
		return out, errs
	}
	w := make(chan struct{})
	c.stopWaiters = append(c.stopWaiters, w)
	go func() {
		select {
		case <-w:
			out <- container.WaitResponse{}
		case <-ctx.Done():
			errs <- ctx.Err()
		}
	}()
	return out, errs
}

func (d *daemonModel) ContainerStart(_ context.Context, ref string, _ container.StartOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.lookup(ref)
	if err != nil {
		return err
	}
	if d.starts == nil {
		d.starts = map[string]int{}
	}
	d.starts[c.name]++
	if c.running {
		return nil // 304: already started
	}
	c.running, c.restarting, c.canceled, c.manuallyStopped, c.restarts = true, false, false, false, 0
	d.emit(c, events.ActionStart, nil)
	return nil
}

func (d *daemonModel) ContainerStop(_ context.Context, ref string, _ container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.lookup(ref)
	if err != nil {
		return err
	}
	d.signals = append(d.signals, c.name+":stop")
	if c.running {
		c.canceled, c.manuallyStopped = true, true
		d.exit(c, 0)
	}
	return nil
}

func (d *daemonModel) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inspects == nil {
		d.inspects = map[string]int{}
	}
	d.inspects[ref]++
	c, err := d.lookup(ref)
	if err != nil {
		return container.InspectResponse{}, err
	}
	status := "exited"
	switch {
	case c.restarting:
		status = "restarting"
	case c.running:
		status = "running"
	}
	labels := map[string]string{}
	for k, v := range c.labels {
		labels[k] = v
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: c.id, Name: "/" + c.name, RestartCount: c.restarts,
			State: &container.State{Status: status, Running: c.running, Restarting: c.restarting, ExitCode: c.exitCode}},
		Config: &container.Config{Labels: labels, StopSignal: c.stopSignal},
	}, nil
}

func (d *daemonModel) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	var out []container.Summary
	for _, c := range d.cs {
		if !o.All && !c.running {
			continue
		}
		if o.Filters.Contains("label") && !o.Filters.MatchKVList("label", c.labels) {
			continue
		}
		labels := map[string]string{}
		for k, v := range c.labels {
			labels[k] = v
		}
		out = append(out, container.Summary{ID: c.id, Names: []string{"/" + c.name}, Labels: labels})
	}
	return out, nil
}

func (d *daemonModel) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	msgs, errs := make(chan events.Message, 16), make(chan error, 1)
	d.subs = append(d.subs, modelSub{ctx: ctx, f: o, msgs: msgs})
	return msgs, errs
}

// state is the container named name's: running (the daemon's word: a
// restarting one runs), restarts, and the kills it got.
func (d *daemonModel) state(name string) (running bool, restarts int, kills []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, _ := d.lookup(name)
	for _, s := range d.signals {
		if n, sig, _ := strings.Cut(s, ":"); n == name {
			kills = append(kills, sig)
		}
	}
	return c.running, c.restarts, kills
}

func (d *daemonModel) inspectsOf(ref string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inspects[ref]
}

func (d *daemonModel) startsOf(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts[name]
}

// eventually fails t unless cond holds within a minute: a test's bound on a
// wait the code under test may never end.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within a minute: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// inspectFailsDocker is a daemon whose inspect of one container (failID)
// fails, as a wedged or starved daemon's does, while every other call works.
type inspectFailsDocker struct {
	*daemonModel
	failID string
}

func (d *inspectFailsDocker) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	if ref == d.failID {
		return container.InspectResponse{}, context.DeadlineExceeded
	}
	return d.daemonModel.ContainerInspect(ctx, ref)
}

// A start whose inspect of the app's container, before it, the daemon never
// answers cannot tell whether it starts one that was not running: it never
// says anew, though the start itself works, as the container runs on (a stop
// that did not end it). Said anew, the replay would count the app's losses
// from the start of an app that ran before it. Nor does it say the container
// was found running: the daemon never said so, and the replay's reason for
// the losses it charges the set would name a running container no one saw.
func TestAStartWhoseInspectFailsIsNoStartAnew(t *testing.T) {
	stallBudget(t, 2*time.Second)
	d, app := appStack()
	a := stackApp(d, "app")
	a.docker = &inspectFailsDocker{daemonModel: d, failID: app.id}
	start, err := a.StartComposeApp(context.Background())
	if err != nil || start != models.ComposeAppUnread {
		t.Fatalf("the daemon never described the container, and the start said %d (err %v); want unread (%d): neither anew nor found running", start, err, models.ComposeAppUnread)
	}
}

// startAnew is whether a start of keploy's said the app it started is a new
// one, with the start's error.
func startAnew(start models.ComposeAppStart, err error) (bool, error) {
	return start.Anew(), err
}

// A start reads the stop's record as the daemon has it, however slow the
// daemon is to give it: a stopped container the daemon takes longer than a
// few seconds to describe (a loaded host) is started anew. With a 2 s
// wall-clock budget on that read, the start was told not anew, and the
// replay charged the set the earlier sets' capture losses, as of an app
// keploy's stop had not ended.
func TestAStartReadsTheStopsRecordHoweverSlowTheDaemonIs(t *testing.T) {
	stallBudget(t, time.Hour)
	start := func(t *testing.T, inspect func(ctx context.Context, read int32) error) {
		t.Helper()
		d := newComposeDocker(false, true)
		a := composeApp(d)
		var reads atomic.Int32
		d.inspectFails = func(ctx context.Context) error { return inspect(ctx, reads.Add(1)) }
		type result struct {
			anew bool
			err  error
		}
		done := make(chan result, 1)
		go func() {
			anew, err := startAnew(a.StartComposeApp(context.Background()))
			done <- result{anew, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(time.Minute):
			t.Fatalf("the start did not end after %d read(s) of the container", reads.Load())
		}
		running, _, starts := d.state()
		if r.err != nil || !r.anew || !running || starts != 1 {
			t.Fatalf("anew %v (err %v), running %v after %d start(s), %d read(s); want anew, running after 1 start", r.anew, r.err, running, starts, reads.Load())
		}
	}
	t.Run("a daemon that takes 2.5 s to describe the container", func(t *testing.T) {
		start(t, func(ctx context.Context, _ int32) error {
			select {
			case <-time.After(2500 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	t.Run("reads the daemon fails are asked again", func(t *testing.T) {
		// Five in a row: a read given up after one or two failed asks
		// passes a row that fails only the first.
		var asked atomic.Int32
		start(t, func(_ context.Context, read int32) error {
			asked.Store(read)
			if read <= 5 {
				return errors.New("the daemon's connection broke")
			}
			return nil
		})
		if n := asked.Load(); n != 6 {
			t.Fatalf("the start read the container %d times; want the five failed reads asked again, and the sixth answered", n)
		}
	})
	t.Run("a start whose ctx ends while the daemon holds its read", func(t *testing.T) {
		// It returns at its ctx's end, and not anew: the read never said
		// the container had stopped. The stall bound is an hour, so a read
		// that went on past the ctx would hold the start, and stopMu (the
		// teardown's stop), until that bound.
		d := newComposeDocker(false, true)
		a := composeApp(d)
		reading := make(chan struct{}, 1)
		d.inspectFails = func(ctx context.Context) error {
			select {
			case reading <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			anew bool
			err  error
		}
		done := make(chan result, 1)
		go func() {
			anew, err := startAnew(a.StartComposeApp(ctx))
			done <- result{anew, err}
		}()
		<-reading
		cancel()
		select {
		case r := <-done:
			if r.anew {
				t.Fatalf("a start whose read ended at its ctx's end said anew (err %v); want not anew", r.err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a start whose ctx had ended did not return: its read of the container waits on for the stall bound")
		}
	})
}

// appStack is a daemon holding compose project "proj": keploy's agent
// (keploy-v3-agent, its service keploy-agent) and the app's service "app",
// its container as compose names it (proj-app-1, no container_name), which
// exits 0 on SIGINT and on SIGTERM. app is the app's container.
func appStack() (d *daemonModel, app *modelContainer) {
	d = &daemonModel{}
	d.add(&modelContainer{id: "a9e47c0de5", name: "keploy-v3-agent", running: true,
		labels: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "keploy-agent"}})
	app = d.add(&modelContainer{id: "f00dfeed01", name: "proj-app-1", running: true,
		exitsOn: map[string]int{"SIGINT": 0, "SIGTERM": 0},
		labels:  map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", composeOneOffLabelKey: "False"}})
	return d, app
}

// stackApp is keploy's App of appStack's app, given by name (its
// --container-name), its service found in the compose file keploy set up.
func stackApp(d *daemonModel, name string) *App {
	return &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, container: name,
		composeService: "app", keployContainer: "keploy-v3-agent"}
}

// keploy's stop and start of a compose app are of the container compose made
// for the app's service in the project of keploy's agent, whatever the name
// keploy was given reaches (P101-b-n): the service's key, which names no
// container (compose names it proj-app-1); its container_name; or a name that
// reaches another stack's container, running or exited, which keploy leaves
// alone. A one-off (`compose run`) of the service is not the app. Read by the
// name, the stop found no container, took that for an app it had stopped, and
// the set's end was read with the app running; or it interrupted the other
// container, or took its exit for the app's halt.
func TestTheComposeAppsStopIsOfTheContainerComposeMadeForItsService(t *testing.T) {
	// The fake app stops on its signal at once: a stop that waits is a wrong one.
	stallBudget(t, 2*time.Second)
	for _, tc := range []struct {
		name  string
		given string
		setup func(d *daemonModel, app *modelContainer)
	}{
		{"given the service's key", "app", nil},
		{"given its container_name", "shop-app", func(_ *daemonModel, app *modelContainer) { app.name = "shop-app" }},
		{"given a name another stack's running container has", "app", func(d *daemonModel, _ *modelContainer) {
			d.add(&modelContainer{id: "0b0e00a1", name: "app", running: true, exitsOn: map[string]int{"SIGINT": 0},
				labels: map[string]string{composeProjectLabelKey: "other", composeServiceLabelKey: "app"}})
		}},
		{"given a name an exited container has", "app", func(d *daemonModel, _ *modelContainer) {
			d.add(&modelContainer{id: "0b0e00a1", name: "app", exitCode: 1,
				labels: map[string]string{composeProjectLabelKey: "other", composeServiceLabelKey: "app"}})
		}},
		{"given a prefix of the app's ID", "f00d", nil},
		{"a one-off of the service runs beside it", "app", func(d *daemonModel, _ *modelContainer) {
			d.add(&modelContainer{id: "0e0ff0001", name: "proj-app-run-1a2b", running: true, exitsOn: map[string]int{"SIGINT": 0},
				labels: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", composeOneOffLabelKey: "True"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, app := appStack()
			if tc.setup != nil {
				tc.setup(d, app)
			}
			a := stackApp(d, tc.given)
			killed, err := a.StopComposeApp(context.Background())
			if err != nil || killed != "" {
				t.Fatalf("the stop: said %q, err %v; want the app stopped on its interrupt", killed, err)
			}
			running, _, kills := d.state(app.name)
			if running || strings.Join(kills, ",") != "SIGINT" {
				t.Fatalf("the app's container: running %v after kills %v; want stopped by keploy's SIGINT: its set's end would be read with it running", running, kills)
			}
			d.mu.Lock()
			all := append([]string(nil), d.signals...)
			d.mu.Unlock()
			if len(all) != 1 {
				t.Fatalf("keploy signalled %v; want the app's container alone", all)
			}
			if a.composeAppHalt() != nil {
				t.Fatalf("keploy took another container's exit for the app's halt: %+v", a.composeAppHalt())
			}
			if _, err := a.StartComposeApp(context.Background()); err != nil {
				t.Fatalf("the start: %v", err)
			}
			if running, _, _ := d.state(app.name); !running || d.startsOf(app.name) != 1 {
				t.Fatalf("after keploy's start the app's container runs %v (%d starts); want started once", running, d.startsOf(app.name))
			}
		})
	}
}

// What keploy cannot tell for the app's container it does not stop, start or
// take for an app that stopped: a scaled service (keploy stops one app), a
// service with no container, a daemon that cannot list them, and an agent
// whose project keploy cannot read are errors, each naming why, and nothing
// is signalled. The set's end is then not read, rather than read with the
// app running.
func TestTheComposeAppsStopRefusesAContainerItCannotTell(t *testing.T) {
	// The fake app stops on its signal at once: a stop that waits is a wrong one.
	stallBudget(t, 2*time.Second)
	for _, tc := range []struct {
		name  string
		setup func(d *daemonModel, app *modelContainer)
		says  string
	}{
		{"a scaled service", func(d *daemonModel, _ *modelContainer) {
			d.add(&modelContainer{id: "f00dfeed02", name: "proj-app-2", running: true, exitsOn: map[string]int{"SIGINT": 0},
				labels: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app"}})
		}, `"app" runs 2 containers in compose project "proj"`},
		{"no container of the service", func(_ *daemonModel, app *modelContainer) {
			app.labels[composeServiceLabelKey] = "web"
		}, `compose project "proj" has no container of the application's service "app"`},
		{"a daemon that cannot list them", func(d *daemonModel, _ *modelContainer) {
			d.listErr = errors.New("daemon busy")
		}, "daemon busy"},
		{"an agent whose project keploy cannot read", func(d *daemonModel, _ *modelContainer) {
			d.cs[0].name = "gone"
		}, `the compose project of keploy's agent's container "keploy-v3-agent"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, app := appStack()
			tc.setup(d, app)
			a := stackApp(d, "app")
			killed, err := a.StopComposeApp(context.Background())
			if err == nil || errors.Is(err, models.ErrAppExitedOnItsOwn) || !strings.Contains(err.Error(), tc.says) || killed != "" {
				t.Fatalf("the stop: said %q, err %v; want an error that says %q", killed, err, tc.says)
			}
			d.mu.Lock()
			signals := append([]string(nil), d.signals...)
			d.mu.Unlock()
			if len(signals) != 0 {
				t.Fatalf("keploy signalled %v, a container it could not tell for the app's", signals)
			}
			if _, err := a.StartComposeApp(context.Background()); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the start: err %v; want an error that says %q", err, tc.says)
			}
		})
	}
}

// The watch ends the run on an exit of the app's container, found as compose
// made it for the app's service in its project, whatever the name keploy was
// given (the daemon's own filter rules apply); a die of another stack's
// container of a service of the same name, or of a one-off of the app's
// service, is not the app's, nor is one keploy asked for.
func TestTheComposeAppsWatchIsOfTheContainerComposeMadeForItsService(t *testing.T) {
	// The fake app stops on its signal at once: a stop that waits is a wrong one.
	stallBudget(t, 2*time.Second)
	d, app := appStack()
	other := d.add(&modelContainer{id: "0b0e00a1", name: "app", running: true, exitsOn: map[string]int{"SIGINT": 0},
		labels: map[string]string{composeProjectLabelKey: "other", composeServiceLabelKey: "app"}})
	oneOff := d.add(&modelContainer{id: "0e0ff0001", name: "proj-app-run-1a2b", running: true,
		labels: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "app", composeOneOffLabelKey: "True"}})
	a := stackApp(d, "app")
	ended := make(chan struct{}, 1)
	stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
	defer stop()
	eventually(t, "the watch asked the daemon for events", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.subs) > 0
	})
	// keploy's own stop of the app: no halt. The stop reads the app's
	// container once; the watch reads it again for the stop's die, by the
	// ID the event gives, before keploy starts it again.
	if _, err := a.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the watch read the app's container for keploy's stop's die", func() bool { return d.inspectsOf(app.id) >= 2 })
	if _, err := a.StartComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.exitOnItsOwn(other.name, 1)
	d.exitOnItsOwn(oneOff.name, 2)
	d.exitOnItsOwn(app.name, 3)
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("the app's container exited on its own and the run was not ended")
	}
	if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
		t.Fatalf("the halt kept is %+v; want the app's exit, code 3 (not another stack's 1, nor a one-off's 2)", h)
	}
}

// A project keploy cannot read takes no exit for the app's: the watch does not
// end the run on a die of a container of the app's service it cannot place
// (another stack's, for all keploy can tell), and the stop at the set's end
// reads the app's container itself.
func TestTheComposeAppsWatchTakesNoExitWhoseProjectItCannotTell(t *testing.T) {
	d, app := appStack()
	d.cs[0].name = "gone" // keploy's agent cannot be read
	core, logs := observer.New(zap.DebugLevel)
	a := stackApp(d, "app")
	a.logger = zap.New(core)
	ended := make(chan struct{}, 1)
	stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
	defer stop()
	eventually(t, "the watch asked the daemon for events", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.subs) > 0
	})
	d.exitOnItsOwn(app.name, 3)
	eventually(t, "the watch read the die", func() bool {
		return logs.FilterMessageSnippet("an exit of its service's container is not taken for the application's").Len() > 0
	})
	select {
	case <-ended:
		t.Fatal("the run was ended on an exit keploy could not place in the app's project")
	default:
	}
	if h := a.composeAppHalt(); h != nil {
		t.Fatalf("a halt was kept: %+v", h)
	}
}

// The app's project, once read, is kept for the run: keploy's stop and start
// of the app still find it when its agent can no longer be read.
func TestTheAppsComposeProjectIsKeptOnceRead(t *testing.T) {
	stallBudget(t, 2*time.Second)
	d, app := appStack()
	a := stackApp(d, "app")
	if _, err := a.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.cs[0].name = "gone"
	d.mu.Unlock()
	if _, err := a.StartComposeApp(context.Background()); err != nil {
		t.Fatalf("the start, the agent no longer readable: %v", err)
	}
	if running, _, _ := d.state(app.name); !running {
		t.Fatal("the app was not started again")
	}
}

// The run's reading of how its up ended, and a halt's stop of the project,
// are of the app's container as compose made it, and of keploy's agent's
// project: not the container a name reaches, which can be another stack's
// (whose project the halt would have stopped).
func TestTheRunsReadingAndAHaltsStopAreOfTheAppsService(t *testing.T) {
	d, app := appStack()
	d.add(&modelContainer{id: "0b0e00a1", name: "app", running: true,
		labels: map[string]string{composeProjectLabelKey: "other", composeServiceLabelKey: "app"}})
	a := stackApp(d, "app")
	if _, exited := a.composeAppExited(context.Background()); exited {
		t.Fatal("an app that runs was read as exited")
	}
	d.exitOnItsOwn(app.name, 3)
	if h, exited := a.composeAppExited(context.Background()); !exited || h.exitCode != 3 {
		t.Fatalf("the app's container exited 3: read %+v, %v", h, exited)
	}
	if !a.stopComposeProject(context.Background()) {
		t.Fatal("the app's project was not stopped")
	}
	if running, _, kills := d.state("app"); !running || len(kills) != 0 {
		t.Fatalf("another stack's container was stopped (%v) on the app's halt", kills)
	}
	if running, _, kills := d.state("keploy-v3-agent"); running || strings.Join(kills, ",") != "stop" {
		t.Fatalf("keploy's agent: running %v after %v; want stopped, as compose's abort stopped it", running, kills)
	}
}

// keploy stops a compose app with a kill the docker daemon takes for a stop
// (P101-c-A2 SF-3(b)): its own stop signal, or SIGINT when it declares none.
// The daemon cancels a container's restart policy only for such a kill (and
// SIGKILL); keploy's SIGINT to an app that declares stop_signal: SIGTERM
// stopped it, the daemon started it again (restart: always, or on-failure on
// a non-zero exit), and the set's end was read with it running, with nothing
// said, its next set's start a no-op on a container the daemon had started
// before the set was staged. keploy's start (the API's) arms the policy
// again: an exit of its own after it is restarted as before.
func TestTheComposeAppsStopIsOneTheDaemonTakesForAStop(t *testing.T) {
	// The fake app stops on its signal at once: a stop that waits is a wrong one.
	stallBudget(t, 2*time.Second)
	for _, tc := range []struct {
		restart, stopSignal, sent string
		exitCode                  int
	}{
		{"always", "", "SIGINT", 0},
		{"always", "SIGTERM", "SIGTERM", 0},
		{"always", "TERM", "TERM", 0},
		{"always", "15", "15", 0},
		{"always", "SIGINT", "SIGINT", 0},
		{"always", "SIGQUIT", "SIGQUIT", 0},
		{"on-failure", "SIGTERM", "SIGTERM", 130},
		{"unless-stopped", "SIGTERM", "SIGTERM", 0},
		{"", "SIGTERM", "SIGTERM", 0},
	} {
		t.Run(tc.restart+"/"+tc.stopSignal, func(t *testing.T) {
			d, app := appStack()
			app.restart, app.stopSignal = tc.restart, tc.stopSignal
			app.exitsOn = map[string]int{"SIGINT": tc.exitCode, "SIGTERM": tc.exitCode, "SIGQUIT": tc.exitCode}
			a := stackApp(d, app.name)
			if killed, err := a.StopComposeApp(context.Background()); err != nil || killed != "" {
				t.Fatalf("the stop: said %q, err %v", killed, err)
			}
			running, restarts, kills := d.state(app.name)
			if running || restarts != 0 || strings.Join(kills, ",") != tc.sent {
				t.Fatalf("after keploy's stop the app's container runs %v (%d restarts by the daemon) after kills %v; want stopped by %s, not started again", running, restarts, kills, tc.sent)
			}
			if _, err := a.StartComposeApp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.restart == "" {
				return
			}
			d.exitOnItsOwn(app.name, 1)
			if running, restarts, _ := d.state(app.name); !running || restarts != 1 {
				t.Fatalf("after keploy's start an exit of its own: running %v, %d restarts; want its restart policy armed again", running, restarts)
			}
		})
	}
}

// A compose app the docker daemon is starting again when keploy comes to stop
// it exited on its own (the daemon calls it running): a halt, with that exit's
// code, and nothing signalled. keploy's kill would cancel the restart, and
// the app's own exit, after the set's last answer, was taken for keploy's
// clean stop: the set passed.
func TestAComposeAppTheDaemonIsRestartingIsAHalt(t *testing.T) {
	for _, stopSignal := range []string{"", "SIGTERM"} {
		t.Run("stop signal "+stopSignal, func(t *testing.T) {
			stallBudget(t, 300*time.Millisecond)
			d, app := appStack()
			app.restart, app.stopSignal, app.holdRestart = "always", stopSignal, true
			d.exitOnItsOwn(app.name, 3)
			a := stackApp(d, app.name)
			killed, err := a.StopComposeApp(context.Background())
			if !errors.Is(err, models.ErrAppExitedOnItsOwn) || killed != "" {
				t.Fatalf("the stop: said %q, err %v; want the app's own exit answered as a halt", killed, err)
			}
			if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
				t.Fatalf("the halt kept is %+v; want exit code 3", h)
			}
			if _, _, kills := d.state(app.name); len(kills) != 0 {
				t.Fatalf("keploy signalled %v the app the daemon was starting again", kills)
			}
		})
	}
	// One keploy had stopped already, which the daemon is starting again
	// (its stop was not one the daemon took for a stop), is no halt: keploy
	// stops it again, which cancels the restart.
	t.Run("keploy had stopped it", func(t *testing.T) {
		d, app := appStack()
		app.restart, app.holdRestart = "always", true
		d.exitOnItsOwn(app.name, 0)
		a := stackApp(d, app.name)
		a.composeApp.stopping.Store(true)
		killed, err := a.StopComposeApp(context.Background())
		if err != nil || killed != "" || a.composeAppHalt() != nil {
			t.Fatalf("the stop: said %q, err %v, halt %+v; want it stopped again, no halt", killed, err, a.composeAppHalt())
		}
		if running, _, kills := d.state(app.name); running || strings.Join(kills, ",") != "SIGINT" {
			t.Fatalf("the app: running %v after %v; want its restart cancelled by keploy's interrupt", running, kills)
		}
	})
}

// keploy's start of the app's container says whether it started one that was
// not running: after a stop that ended it, yes; on a container that runs on
// (a stop that did not end it, a start told failed that the daemon carried
// out), no, found running, the start being the daemon's no-op; and no with an
// error.
func TestAComposeStartSaysWhetherItStartedTheAppAnew(t *testing.T) {
	stallBudget(t, 2*time.Second)
	d, app := appStack()
	a := stackApp(d, "app")
	if start, err := a.StartComposeApp(context.Background()); err != nil || start != models.ComposeAppFoundRunning {
		t.Fatalf("a start of the running app: %d (%v), want found running (%d): it runs on", start, err, models.ComposeAppFoundRunning)
	}
	if _, err := a.StopComposeApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if anew, err := startAnew(a.StartComposeApp(context.Background())); err != nil || !anew {
		t.Fatalf("a start after a stop that ended it: anew %v (%v), want true", anew, err)
	}
	if start, err := a.StartComposeApp(context.Background()); err != nil || start != models.ComposeAppFoundRunning {
		t.Fatalf("a second start: %d (%v), want found running (%d): the first one's app runs on", start, err, models.ComposeAppFoundRunning)
	}
	d.mu.Lock()
	kept := d.cs[:0]
	for _, c := range d.cs {
		if c != app {
			kept = append(kept, c)
		}
	}
	d.cs = kept
	d.mu.Unlock()
	if anew, err := startAnew(a.StartComposeApp(context.Background())); err == nil || anew {
		t.Fatalf("a start that failed: anew %v (%v), want false with an error", anew, err)
	}
}

// A loaded daemon fails the reads of the run's end before it answers them:
// the app that exited on its own (code 3) is still the run's halt, and the
// halt stops the project as compose's abort did, keploy's agent with it. The
// reads had 2 s budgets, and a failed one was taken for an answer: the app
// for one that had not exited (the run ended as on a clean stop, its down
// taking the agent down mid-drain, the halt's exit code lost), the project
// for one keploy could not list (taken down whole instead).
func TestTheRunsEndReadsTheDaemonsAnswerHoweverSlowItIs(t *testing.T) {
	t.Run("the app's exit", func(t *testing.T) {
		inner := &inspectRecorder{inspects: map[string]container.InspectResponse{"app": {ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited", ExitCode: 3}}}}}
		d := &slowDaemon{Client: inner, failInspects: 5}
		a := &App{logger: zap.NewNop(), container: "app", docker: d}
		ended := 0
		if got := a.composeAwaitTeardown(context.Background(), nil, func() { ended++ }); got != composeHaltCame || ended != 1 {
			t.Fatalf("composeAwaitTeardown = %d, ending the run %d times; want the halt (%d), once", got, ended, composeHaltCame)
		}
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 {
			t.Fatalf("the halt kept is %+v; want the app's exit, code 3", h)
		}
	})
	t.Run("the project's containers", func(t *testing.T) {
		inner := newComposeDocker(false, true)
		inner.project, inner.others = "proj", []string{"keploy-agent", "dep"}
		d := &slowDaemon{Client: inner, failInspects: 5, failLists: 5}
		a := composeApp(inner)
		a.docker = d
		if !a.stopComposeProject(context.Background()) {
			t.Fatal("the project was taken down whole, not stopped")
		}
		if len(inner.stopped) != 2 {
			t.Fatalf("stopped %v; want the agent and the dependency", inner.stopped)
		}
	})
}

// The reads that find the app's container compose made for its service, the
// project's name on keploy's agent and the list of the service's containers,
// are asked again when the daemon fails them, at each place keploy decides on
// them. One failed read was taken for no project, or no container: the up's
// end took the app that exited with 3 for one that had not (the run ended as
// on a clean stop, the halt's exit code lost); a halt took the project down
// whole, keploy's agent with it, mid-drain; and the next set's start failed,
// which replay marks AppHalted, the whole set lost. Each subtest fails that
// many reads of each kind (the project's name, the service's list, and the
// reads after them); failed 0 is the control.
func TestTheAppsServiceIsReadOnTheDaemonsAnswerThoughItFailsReads(t *testing.T) {
	stallBudget(t, 10*time.Second)
	stack := func(failLists, failInspects int) (*App, *modelContainer, *daemonModel) {
		d, app := appStack()
		a := stackApp(d, "app")
		a.docker = &slowDaemon{Client: d, failLists: failLists, failInspects: failInspects}
		return a, app, d
	}
	for _, failed := range []int{0, 1, 5} {
		t.Run(fmt.Sprintf("the up's end, %d failed reads of each", failed), func(t *testing.T) {
			a, app, d := stack(failed, failed)
			d.mu.Lock()
			app.running, app.exitCode = false, 3
			d.mu.Unlock()
			if h, exited := a.composeAppExited(context.Background()); !exited || h.exitCode != 3 {
				t.Fatalf("composeAppExited = %+v, %v; want the app's exit, code 3", h, exited)
			}
		})
		t.Run(fmt.Sprintf("a halt's stop, %d failed reads of each", failed), func(t *testing.T) {
			a, _, _ := stack(failed, failed)
			if !a.stopComposeProject(context.Background()) {
				t.Fatal("the project was taken down whole, keploy's agent with it; want it stopped")
			}
		})
		t.Run(fmt.Sprintf("the next set's start, %d failed reads of each", failed), func(t *testing.T) {
			a, app, d := stack(failed, failed)
			d.mu.Lock()
			app.running = false
			d.mu.Unlock()
			if start, err := a.StartComposeApp(context.Background()); err != nil || start != models.ComposeAppStartedAnew {
				t.Fatalf("StartComposeApp = %d, %v; want started anew (an error is replay's AppHalted: the set lost)", start, err)
			}
		})
	}
}
