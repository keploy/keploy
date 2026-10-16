package utils

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// DrainErrGroup waits for g.Wait() to return, but gives up after `timeout`.
//
// Why this exists: keploy's record/replay teardown runs in a deferred function
// that is also the path SIGINT/SIGTERM takes (the signal cancels the root
// context, which triggers the defer). The teardown calls g.Wait() to drain the
// run/setup goroutines after cancel(). If any one of those goroutines does not
// observe context cancellation — e.g. an agent-in-docker bring-up or proxy
// setup that wedges while the host is under heavy CPU/IO contention — an
// unbounded g.Wait() blocks the teardown forever. Because that same teardown is
// what SIGINT triggers, the process then ignores SIGINT entirely and only dies
// when an outer `timeout`/CI sends SIGKILL minutes later (observed: a CI lane
// hung ~50 min despite a 15-min `timeout -s INT`).
//
// Bounding the drain guarantees the process exits promptly after cancellation
// -- except while it stops a keploy agent that will end on its own (an exit,
// or a kill once its teardown stops making progress: BeginAgentStop). On
// timeout it logs (so the offending goroutine is discoverable) and returns
// nil; the leaked goroutine is reaped when the process exits. In the normal
// case the goroutines drain in well under the timeout, so this is a no-op.
func DrainErrGroup(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) error {
	err, _ := DrainErrGroupStatus(logger, name, g, timeout)
	return err
}

// DrainErrGroupStatus is DrainErrGroup with the timeout made observable to the
// caller. It reports timedOut=true iff g.Wait() did NOT return within `timeout`
// — i.e. a goroutine is ignoring context cancellation and MAY STILL BE RUNNING
// (and writing) after this returns. On that path err is nil, preserving the
// "a timeout must never fail teardown" contract described above.
//
// The bound does not cut off a stop of a keploy agent still in progress
// (BeginAgentStop): it ends on its own, and the rest of the group gets the
// bound again once it has.
//
// So `!timedOut` is the "every goroutine in the group has returned" signal: a
// non-nil err still means the group finished (one goroutine returned an error),
// whereas timedOut means it did not join. A caller that then touches an artifact
// a wedged group goroutine could also be writing — e.g. a post-record pass that
// rewrites the mock file — must gate on `!timedOut`, not on `err == nil`.
func DrainErrGroupStatus(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) (err error, timedOut bool) {
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	for {
		select {
		case e := <-done:
			return e, false
		case <-time.After(timeout):
		}
		// An agent stop still in progress is waited for; once it ends, the
		// rest of the group gets the bound again.
		idle, inProgress := agentStopsIdle()
		if !inProgress {
			break
		}
		logger.Info(stopWaitedFor(), zap.String("group", name))
		select {
		case e := <-done:
			return e, false
		case <-idle:
		}
	}
	logger.Error("teardown drain timed out after cancellation; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
		zap.String("group", name),
		zap.Duration("timeout", timeout))
	return nil, true
}

// DrainErrGroupProgress is DrainErrGroupStatus bounded by progress instead of by
// a fixed time: it waits for as long as progress() keeps changing, and gives up
// only once it has not changed for bound, in the time this process could run
// (utils/stall), not wall time: a CLI starved of CPU (a loaded host, a frozen
// laptop) has not seen the group stall, only not run. A group that is still
// draining (a recording's sinks writing what the agent held at the stop) is
// never cut off for taking long; one that is wedged costs bound. Same contract
// as DrainErrGroupStatus otherwise: timedOut means a goroutine may still be
// running.
func DrainErrGroupProgress(logger *zap.Logger, name string, g *errgroup.Group, bound time.Duration, progress func() uint64) (err error, timedOut bool) {
	return drainErrGroupProgress(nil, logger, name, g, bound, progress)
}

// drainErrGroupProgress is DrainErrGroupProgress on clk (nil: the process's
// own runnable clock).
func drainErrGroupProgress(clk stall.Clock, logger *zap.Logger, name string, g *errgroup.Group, bound time.Duration, progress func() uint64) (err error, timedOut bool) {
	done := make(chan struct{})
	var werr error
	go func() { werr = g.Wait(); close(done) }()
	for !waitWhileProgressing(clk, done, bound, progress) {
		// An agent stop still in progress is waited for; once it ends, the
		// rest of the group gets the bound again.
		idle, inProgress := agentStopsIdle()
		if !inProgress {
			logger.Error("teardown drain made no progress; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
				zap.String("group", name),
				zap.Duration("stalledFor", bound))
			return nil, true
		}
		logger.Info(stopWaitedFor(), zap.String("group", name))
		select {
		case <-done:
		case <-idle:
		}
	}
	return werr, false
}

// WaitWhileProgressing waits for done for as long as progress() keeps changing,
// and reports false once it has not changed for bound, in the time this
// process could run (utils/stall) (true: done closed).
func WaitWhileProgressing(done <-chan struct{}, bound time.Duration, progress func() uint64) bool {
	return waitWhileProgressing(nil, done, bound, progress)
}

// waitWhileProgressing is WaitWhileProgressing on clk (nil: the process's own
// runnable clock). It looks at progress every tenth of the bound, at most a
// second (stall.CheckEvery) of wall time, before it looks at the stall: a change
// starts the stall again.
func waitWhileProgressing(clk stall.Clock, done <-chan struct{}, bound time.Duration, progress func() uint64) bool {
	m := stall.NewMeter(clk, bound)
	defer m.Stop()
	last := progress()
	for {
		select {
		case <-done:
			return true
		case <-m.Wait():
			if p := progress(); p != last {
				last = p
				m.Reset()
			} else if m.Expired() {
				return false
			}
		}
	}
}

// BacklogLooks is a stop's looks at what the agent still holds from before
// it (models.AgentBacklog), each taken once the stop's stream has gone quiet,
// and the rule that says whether it keeps waiting (Look).
type BacklogLooks struct {
	// Stall is how many looks in a row without progress end the wait.
	Stall int

	stalled  int
	frames   uint64
	progress uint64
	answered bool
	// last is the agent's answer at the last look, and looked says there was
	// one; gone says that look got none because the agent could not be
	// reached at all. prior is the answer at the latest look before it the
	// agent was not gone for, and hadPrior says there was one.
	last     models.BacklogVerdict
	looked   bool
	gone     bool
	prior    models.BacklogVerdict
	hadPrior bool
}

// Look takes one look: the agent's answer, the error asking for it gave (nil
// when it answered), and how many frames the stop has handed on so far (on
// any of its streams). It reports whether the stop keeps waiting, and gaveUp
// when it does not because the backlog made no progress.
//
// An error that says the agent could not be reached at all (agentGone:
// refused, reset, the connection closed mid-answer) is the agent going away:
// such a look says nothing of what it held (Held). One that did not come in
// time (a timeout) is an agent that may be starved, and may still hold
// something.
//
//   - clear: the agent holds nothing more from before the stop. The wait
//     ends. What it already handed on and this client has not received yet
//     is not counted (models.BacklogClear: clear is not delivered).
//   - unsupported: the agent can never tell. The wait ends; the stop goes by
//     its streams' quiet alone.
//   - pending, or cannot tell (no answer came): the stop keeps waiting. An
//     answer that did not come is never read as "nothing pending".
//
// A wait is bounded by progress, not time: a look that finds a frame handed
// on since the last one, or the agent's capture further along
// (AgentBacklog.Progress, from a pending answer), resets the count; Stall
// looks in a row with neither end the wait, gaveUp.
func (l *BacklogLooks) Look(b models.AgentBacklog, err error, frames uint64) (wait, gaveUp bool) {
	if l.looked && !l.gone {
		l.prior, l.hadPrior = l.last, true
	}
	l.last, l.looked, l.gone = b.Verdict, true, agentGone(err)
	switch b.Verdict {
	case models.BacklogClear, models.BacklogUnsupported:
		return false, false
	}
	moved := frames != l.frames
	l.frames = frames
	if b.Verdict == models.BacklogPending {
		if !l.answered || b.Progress != l.progress {
			moved = true
		}
		l.progress, l.answered = b.Progress, true
	}
	if moved {
		l.stalled = 0
	} else {
		l.stalled++
	}
	if l.stalled >= l.Stall {
		return false, true
	}
	return true, false
}

// Stalled is how many looks in a row saw no progress.
func (l *BacklogLooks) Stalled() int { return l.stalled }

// agentGone reports whether asking the agent failed because it could not be
// reached at all: its port refused or reset the connection, or closed it
// before the answer was through. By the error, not its text, so it holds on
// every platform (neterr).
func agentGone(err error) bool {
	return err != nil && (neterr.IsConnRefused(err) || neterr.IsConnReset(err) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
}

// LastGone reports whether the agent could not be reached at all at the last
// look (see Look).
func (l *BacklogLooks) LastGone() bool { return l.gone }

// Said is what the agent said at the latest look it was not gone for, and
// whether there was one: what Held goes by.
func (l *BacklogLooks) Said() (models.BacklogVerdict, bool) {
	if !l.looked {
		return 0, false
	}
	if l.gone {
		return l.prior, l.hadPrior
	}
	return l.last, true
}

// Held reports whether, at the latest look the agent was not already gone
// for, it said it still held something from before the stop, or did not
// answer: if its stream ends now, what it held may be lost. A look the agent
// was gone for says nothing of what it held.
func (l *BacklogLooks) Held() bool {
	v, ok := l.Said()
	return ok && (v == models.BacklogPending || v == models.BacklogCannotTell)
}

// agentStops counts this process's stops of a keploy agent still in progress
// (BeginAgentStop); idle is closed whenever there are none.
var agentStops = struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}{idle: closedChannel()}

func closedChannel() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// BeginAgentStop records that this process is stopping a keploy agent, until
// the returned end is called (once; later calls do nothing). A drain
// (DrainErrGroupStatus, DrainErrGroupProgress) does not give up while one is
// in progress: the stop ends on its own -- at the agent's exit, at a kill once
// its teardown has stopped making progress, or at a kill at once, at the
// press of Ctrl+C that the stop offered for it after an earlier press
// (pkg/platform/http, stopWatch) -- and a drain that gave up first let keploy
// exit with the agent still running, its hooks attached, and nothing left to
// kill it if its teardown had wedged.
func BeginAgentStop() (end func()) {
	agentStops.mu.Lock()
	if agentStops.n == 0 {
		agentStops.idle = make(chan struct{})
	}
	agentStops.n++
	agentStops.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			agentStops.mu.Lock()
			if agentStops.n--; agentStops.n == 0 {
				close(agentStops.idle)
			}
			agentStops.mu.Unlock()
		})
	}
}

// daemonStops counts the stops of daemons the application left running
// still in progress (BeginDaemonStop), which hold the drains as an agent's
// stop does.
var daemonStops atomic.Int64

// BeginDaemonStop is BeginAgentStop for keploy's stop of the daemons an
// application left running (pkg/platform/http, stopAdopted): it ends on its
// own, at their exit or at a kill once they make no progress on their way
// out, or at the press of Ctrl+C it offers; a drain that gave up first let the
// next test set's application start while they still ran.
func BeginDaemonStop() (end func()) {
	// Counted before the hold, and uncounted after it ends: agentStops.n
	// never falls below daemonStops (agentOnlyStopsIdle).
	daemonStops.Add(1)
	endHold := BeginAgentStop()
	var once sync.Once
	return func() {
		once.Do(func() {
			endHold()
			daemonStops.Add(-1)
		})
	}
}

// stopWaitedFor is what a drain says it waits for while a stop holds it.
func stopWaitedFor() string {
	if daemonStops.Load() > 0 {
		return "waiting for keploy to finish stopping the daemons the application left running before giving up on the rest of the teardown"
	}
	return "waiting for the keploy agent to finish stopping before giving up on the rest of the teardown"
}

// AgentStopInProgress reports whether a stop of a keploy agent is holding
// the drains (BeginAgentStop); a stop of daemons (BeginDaemonStop) alone is
// none.
func AgentStopInProgress() bool {
	_, inProgress := agentOnlyStopsIdle()
	return inProgress
}

// StopHoldsDrains reports whether a stop that ends on its own, of a keploy
// agent or of the daemons an application left running, is holding the
// drains (BeginAgentStop, BeginDaemonStop).
func StopHoldsDrains() bool {
	_, inProgress := agentStopsIdle()
	return inProgress
}

// agentOnlyStopsIdle is agentStopsIdle for the stops of a keploy agent
// alone: whether one is in progress, not a stop of daemons only. Its channel
// is closed once no stop at all holds the drains.
func agentOnlyStopsIdle() (idle <-chan struct{}, inProgress bool) {
	agentStops.mu.Lock()
	defer agentStops.mu.Unlock()
	return agentStops.idle, int64(agentStops.n) > daemonStops.Load()
}

// agentStopsIdle returns a channel closed once no agent stop is in progress
// (BeginAgentStop), and whether one is now.
func agentStopsIdle() (idle <-chan struct{}, inProgress bool) {
	agentStops.mu.Lock()
	defer agentStops.mu.Unlock()
	return agentStops.idle, agentStops.n > 0
}

// AwaitAgentExit waits for an agent this process stopped to exit (gone), for
// a step of keploy's teardown that needs it gone (the user's container
// restored onto the ports it held, a new agent started in its place). It
// gives up -- false -- after bound, as a drain does, unless an agent stop that
// will end on its own is holding the drains then (BeginAgentStop), in which
// case it waits for that to end and gives the exit the bound again; and it
// gives up once interrupted is closed: its caller says when the exit is no
// longer worth waiting for (pkg/platform/http: the restore of a
// --from-container app, once the agent's stop killed it at once at the user's
// Ctrl+C, agentRun.letGo; a retried start, at its context's end or the user's
// interrupt of its wait).
func AwaitAgentExit(gone <-chan struct{}, bound time.Duration, interrupted <-chan struct{}) bool {
	for {
		timer := time.NewTimer(bound)
		select {
		case <-gone:
			timer.Stop()
			return true
		case <-interrupted:
			timer.Stop()
			return false
		case <-timer.C:
		}
		// A stop of daemons is not the agent's: its exit does not wait on it.
		idle, inProgress := agentOnlyStopsIdle()
		if !inProgress {
			return false
		}
		select {
		case <-gone:
			return true
		case <-interrupted:
			return false
		case <-idle:
		}
	}
}
