package utils

import (
	"errors"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
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
// Bounding the drain guarantees the process exits promptly after cancellation.
// On timeout it logs (so the offending goroutine is discoverable) and returns
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
// So `!timedOut` is the "every goroutine in the group has returned" signal: a
// non-nil err still means the group finished (one goroutine returned an error),
// whereas timedOut means it did not join. A caller that then touches an artifact
// a wedged group goroutine could also be writing — e.g. a post-record pass that
// rewrites the mock file — must gate on `!timedOut`, not on `err == nil`.
func DrainErrGroupStatus(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) (err error, timedOut bool) {
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case e := <-done:
		return e, false
	case <-time.After(timeout):
		logger.Error("teardown drain timed out after cancellation; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
			zap.String("group", name),
			zap.Duration("timeout", timeout))
		return nil, true
	}
}

// DrainErrGroupProgress is DrainErrGroupStatus bounded by progress instead of by
// a fixed time: it waits for as long as progress() keeps changing, and gives up
// only once it has not changed for stall. A group that is still draining (a
// recording's sinks writing what the agent held at the stop) is never cut off
// for taking long; one that is wedged costs stall. Same contract as
// DrainErrGroupStatus otherwise: timedOut means a goroutine may still be running.
func DrainErrGroupProgress(logger *zap.Logger, name string, g *errgroup.Group, stall time.Duration, progress func() uint64) (err error, timedOut bool) {
	done := make(chan struct{})
	var werr error
	go func() { werr = g.Wait(); close(done) }()
	if WaitWhileProgressing(done, stall, progress) {
		return werr, false
	}
	logger.Error("teardown drain made no progress; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
		zap.String("group", name),
		zap.Duration("stalledFor", stall))
	return nil, true
}

// WaitWhileProgressing waits for done for as long as progress() keeps changing,
// and reports false once it has not changed for stall (true: done closed).
func WaitWhileProgressing(done <-chan struct{}, stall time.Duration, progress func() uint64) bool {
	t := time.NewTicker(min(max(stall/10, time.Millisecond), time.Second))
	defer t.Stop()
	last, lastMoved := progress(), time.Now()
	for {
		select {
		case <-done:
			return true
		case now := <-t.C:
			if p := progress(); p != last {
				last, lastMoved = p, now
			} else if now.Sub(lastMoved) >= stall {
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
//   - clear: the agent will hand over nothing more from before the stop.
//     The wait ends.
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
