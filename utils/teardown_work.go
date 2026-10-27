package utils

import (
	"sync"
	"sync/atomic"
	"time"
)

// TEARDOWN WORK: A STAGE THE AGENT MUST NOT BE KILLED IN THE MIDDLE OF --
// WHILE IT MAKES PROGRESS.
//
// Removing what the agent attached to the kernel (a uprobe, a tracefs probe,
// an INT3 a tracer planted) and a CA tool rewriting a trust store are cut
// short by a kill: the kernel abandons a uprobe removal whose thread has a
// fatal signal pending ("leaking uprobe"), and a trust store is left half
// rewritten. Each can also take long without a step of its own -- one close(2)
// that walks every process mapping the probed library -- so the CLI that
// stops the agent does not count a check made while one makes progress as a
// check without progress (TeardownProgress.Busy), and the agent's own end on
// the CLI's kill waits until none does (DieOnEscalation).
//
// "While it makes progress", not "while it is in progress": a removal can
// wedge, the kernel never finishing it (a lock that is never released), and a
// stage in progress forever kept the agent from ever being killed. What says
// a stage makes progress is the kernel's own account of the thread doing it
// and of the processes it runs (workProbe): scheduled since the last check,
// or runnable now. Each check (one every TeardownBeat) that finds it so
// records a step of the teardown; one that does not finds it quiet. A stage
// is wedged once the teardown as a whole has taken no step across
// wedgedAfter of its checks -- the CLI's own rule for the whole teardown
// (TeardownStallChecks), so an agent the CLI judges to have stopped making
// progress also finds its work wedged when the CLI's kill reaches it. A stage
// whose progress cannot be seen (no probe: not on Linux) counts as making
// progress throughout, as every stage did before.
//
// The rule is a trade, made on purpose: a removal held in one uninterruptible
// wait for the whole count of checks -- a lock the kernel does release, but
// only after that long, on a host that loaded -- is taken for wedged, and a
// kill then cuts it short, leaking what it was removing. Waiting on instead
// would wait for ever on one that is wedged, which is what kept an agent from
// ever being killed. A stage that is scheduled even once across the count is
// never taken for wedged.

// TeardownStallChecks is the count of consecutive checks of a stopping
// agent's teardown, one every TeardownBeat, that take no step after which the
// teardown is taken for one that has stopped making progress: by the CLI
// that stops the agent, which then kills it, and by the agent itself, which
// then no longer waits for a stage of teardown work wedged in the middle.
const TeardownStallChecks = 30

// wedgedAfter is TeardownStallChecks, which tests shorten.
var wedgedAfter uint64 = TeardownStallChecks

// workProbe watches the thread doing a stage of teardown work.
type workProbe interface {
	// moved reports whether the stage has made progress since the last call:
	// its thread, or a process it runs, scheduled since then, or runnable
	// now. The first call, and one that cannot tell, report true: progress
	// that cannot be seen is never taken for none.
	moved() bool
	// release lets the thread go; the work's own goroutine calls it as the
	// work ends.
	release()
}

// describedProbe is a workProbe that can also say how the stage's thread
// stands: the kernel's state of it, and the kernel function it sleeps in (its
// wait point, empty when it runs or cannot be read). What an agent's start
// that stalls reports of the stages it stalled in (BootProgress.Stages).
type describedProbe interface {
	describe() (state, wait string)
}

// newWorkProbe returns the probe for a stage of teardown work begun on the
// calling goroutine, or nil where its progress cannot be seen. A variable so
// that tests that must count every step see none from the checks.
var newWorkProbe = platformWorkProbe

// teardownWorkItem is one stage of teardown work in progress.
type teardownWorkItem struct {
	probe workProbe     // nil: it counts as making progress throughout
	ended chan struct{} // closed as it ends: its checks stop
	// Guarded by teardownWorkMu.
	progressing bool   // it made progress at its last check, or has had none
	quiet       uint64 // its checks since the teardown last took a step
	lastSeq     uint64 // the teardown's step count at its last check
}

var (
	// teardownWorkMu guards the work in progress and orders the start of
	// teardown work against the escalation's check that none is making
	// progress (DieOnEscalation).
	teardownWorkMu sync.Mutex
	teardownWorks  = map[*teardownWorkItem]struct{}{}
	// teardownWork counts the stages in progress (TeardownBusy).
	teardownWork atomic.Int64
	// progressingWork counts the stages in progress that made progress at
	// their last check, or have had none: what the reporter writes as Busy.
	progressingWork atomic.Int64
	// workChanged is closed, and replaced, whenever a stage begins, ends or
	// is checked: what the escalation waits on.
	workChanged = make(chan struct{})
)

// workChangedLocked wakes what waits on workChanged. Under teardownWorkMu.
func workChangedLocked() {
	close(workChanged)
	workChanged = make(chan struct{})
}

// BeginTeardownWork records the start of a stage of the teardown the agent
// must not be killed in the middle of while it makes progress, and which can
// take long without a step: removing what it attached to the kernel, a CA
// tool rewriting a trust store. While one makes progress the CLI counts no
// check as one without progress (TeardownProgress.Busy); every check that
// finds it making progress is a step, and so are its start and its end -- the
// returned func, which ends it once however often it is called.
//
// The stage is watched through the thread it runs on and the processes that
// thread runs: the calling goroutine stays on its thread until it ends the
// stage, and must be the goroutine that calls end (called from another, end
// still ends the stage, but the calling goroutine stays locked to its thread
// until it exits). So a stage's progress is only what its own goroutine
// does: one that waits on another goroutine -- for a removal, or a uprobe's
// registration, in flight there -- sees no progress while it waits, and the
// work done there must be a stage of its own, begun on that goroutine.
func BeginTeardownWork() (end func()) { return beginTeardownWork(nil) }

// beginTeardownWork is BeginTeardownWork, whose end runs between (tests; nil
// otherwise) after its step and before the work is over.
func beginTeardownWork(between func()) (end func()) {
	w := &teardownWorkItem{probe: newWorkProbe(), ended: make(chan struct{}), progressing: true}
	teardownWorkMu.Lock()
	teardownWorks[w] = struct{}{}
	teardownWork.Add(1)
	progressingWork.Add(1)
	workChangedLocked()
	teardownWorkMu.Unlock()
	TeardownStep()
	if w.probe != nil {
		go checkTeardownWork(w, teardownBeat)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// The step before the work is over: a report, which loads Busy
			// before Seq (snapshotTeardown), that reads the work as over
			// reads this step too.
			TeardownStep()
			if between != nil {
				between()
			}
			close(w.ended)
			teardownWorkMu.Lock()
			delete(teardownWorks, w)
			teardownWork.Add(-1)
			if w.progressing {
				progressingWork.Add(-1)
			}
			workChangedLocked()
			teardownWorkMu.Unlock()
			if w.probe != nil {
				w.probe.release()
			}
		})
	}
}

// checkTeardownWork checks w every beat until it ends: a check that finds it
// making progress records a step of the teardown, and one at which the
// teardown has taken no step since the last counts as quiet.
func checkTeardownWork(w *teardownWorkItem, every time.Duration) {
	// A timer set again once each check is done, not a ticker: a check that
	// came late, or took long, is followed by a whole beat, not by one at
	// once that could only find no progress.
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		select {
		case <-w.ended:
			return
		case <-t.C:
		}
		moved := w.probe.moved()
		if moved {
			TeardownStep()
		}
		seq := teardownSeq.Load()
		teardownWorkMu.Lock()
		if _, in := teardownWorks[w]; !in { // ended meanwhile
			teardownWorkMu.Unlock()
			return
		}
		if moved != w.progressing {
			w.progressing = moved
			if moved {
				progressingWork.Add(1)
			} else {
				progressingWork.Add(-1)
			}
		}
		if seq != w.lastSeq {
			w.lastSeq, w.quiet = seq, 0
		} else {
			w.quiet++
		}
		workChangedLocked()
		teardownWorkMu.Unlock()
		t.Reset(every)
	}
}

// teardownWorkHaltedLocked reports whether no stage of teardown work in
// progress may still be making progress: none is in progress, or every one is
// wedged -- seen, and quiet across wedgedAfter checks without a step of the
// teardown. Under teardownWorkMu.
func teardownWorkHaltedLocked() bool {
	for w := range teardownWorks {
		if w.probe == nil || w.quiet < wedgedAfter {
			return false
		}
	}
	return true
}

// TeardownProgressSeen reports whether this platform sees the progress of a
// stage of teardown work (workProbe). Where it does not, a stage counts as
// making progress throughout: work that only may be teardown work -- an
// agent's start, which a stop waits for -- is not worth marking as such
// there, as a wedged one would then never be killed for want of progress.
func TeardownProgressSeen() bool { return platformProgressSeen }

// TeardownBusy reports whether a stage of the teardown begun with
// BeginTeardownWork is in progress, making progress or not.
func TeardownBusy() bool { return teardownWork.Load() > 0 }
