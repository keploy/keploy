package http

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// p is a teardown progress report from an agent that records its steps.
func p(seq, beat uint64, done bool) utils.TeardownProgress {
	return utils.TeardownProgress{Seq: seq, Beat: beat, Steps: true, Done: done}
}

// busy is p for an agent in the middle of work that takes no steps.
func busy(seq, beat uint64) utils.TeardownProgress {
	return utils.TeardownProgress{Seq: seq, Beat: beat, Steps: true, Busy: true}
}

// unstepped is p for an agent that does not say it records its steps.
func unstepped(seq, beat uint64) utils.TeardownProgress {
	return utils.TeardownProgress{Seq: seq, Beat: beat}
}

// The judge's verdicts, report by report: the founder's rule.
func TestStopJudge(t *testing.T) {
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	type step struct {
		report utils.TeardownProgress
		want   stopVerdict
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"a teardown that takes a step every check is never killed", []step{
			{p(1, 0, false), keepWaiting}, {p(2, 1, false), keepWaiting}, {p(3, 2, false), keepWaiting},
			{p(4, 3, false), keepWaiting}, {p(5, 4, false), keepWaiting}, {p(6, 5, false), keepWaiting},
		}},
		{"no step across stallChecks checks: killed at the last of them, not before", []step{
			{p(4, 0, false), keepWaiting}, {p(4, 1, false), keepWaiting}, {p(4, 2, false), keepWaiting},
			{p(4, 3, false), killStalled},
		}},
		{"a step resets the count", []step{
			{p(4, 0, false), keepWaiting}, {p(4, 1, false), keepWaiting}, {p(4, 2, false), keepWaiting},
			{p(5, 3, false), keepWaiting}, {p(5, 4, false), keepWaiting}, {p(5, 5, false), keepWaiting},
			{p(5, 6, false), killStalled},
		}},
		{"reads between beats are no checks", []step{
			{p(4, 0, false), keepWaiting}, {p(4, 0, false), keepWaiting}, {p(4, 1, false), keepWaiting},
			{p(4, 1, false), keepWaiting}, {p(4, 1, false), keepWaiting}, {p(4, 2, false), keepWaiting},
			{p(4, 2, false), keepWaiting},
		}},
		{"beats missed between reads count, one check each", []step{
			{p(4, 0, false), keepWaiting}, {p(4, 3, false), killStalled},
		}},
		{"detach done: killed a check later, if still there", []step{
			{p(4, 0, false), keepWaiting}, {p(5, 1, true), keepWaiting}, {p(5, 1, true), keepWaiting},
			{p(5, 2, true), killDetached},
		}},
		{"detach done at the first report: killed a check later", []step{
			{p(9, 7, true), keepWaiting}, {p(9, 7, true), keepWaiting}, {p(9, 8, true), killDetached},
		}},
		{"work in progress takes no step, and is never a stall", []step{
			{busy(4, 0), keepWaiting}, {busy(4, 1), keepWaiting}, {busy(4, 5), keepWaiting},
			{busy(4, 40), keepWaiting}, {p(5, 41, false), keepWaiting}, {p(5, 42, false), keepWaiting},
			{p(5, 43, false), keepWaiting}, {p(5, 44, false), killStalled},
		}},
		{"an agent that does not record its steps is never killed for want of them", []step{
			{unstepped(4, 0), keepWaiting}, {unstepped(4, 10), keepWaiting}, {unstepped(4, 1000), keepWaiting},
		}},
		{"but it is when it has detached everything and not exited", []step{
			{unstepped(4, 0), keepWaiting}, {utils.TeardownProgress{Seq: 4, Beat: 1, Done: true}, keepWaiting},
			{utils.TeardownProgress{Seq: 4, Beat: 2, Done: true}, killDetached},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var j stopJudge
			for i, s := range tc.steps {
				if got := j.observe(s.report); got != s.want {
					t.Fatalf("report %d %+v: verdict %d, want %d", i, s.report, got, s.want)
				}
			}
		})
	}
}

// fakeAgent is a stopping agent for stopWatch: its reports, its exit, and
// whether it was killed.
type fakeAgent struct {
	mu     sync.Mutex
	report utils.TeardownProgress
	has    bool
	gone   chan struct{}
	exit   sync.Once
	killed atomic.Int32
	reads  chan utils.TeardownProgress // each read's result, as far as the buffer holds
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{gone: make(chan struct{}), reads: make(chan utils.TeardownProgress, 4096)}
}

func (f *fakeAgent) set(r utils.TeardownProgress) {
	f.mu.Lock()
	f.report, f.has = r, true
	f.mu.Unlock()
}

func (f *fakeAgent) read() (utils.TeardownProgress, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.reads <- f.report:
	default:
	}
	return f.report, f.has
}

func (f *fakeAgent) exited() { f.exit.Do(func() { close(f.gone) }) }

func (f *fakeAgent) kill() error {
	f.killed.Add(1)
	f.exited() // a killed agent exits
	return nil
}

// watching is a stopWatch run on a fakeAgent.
type watching struct {
	finished chan struct{}
	killed   atomic.Bool
	judged   chan utils.TeardownProgress
}

// startWatch runs a stopWatch on f.
func startWatch(f *fakeAgent, reported chan<- struct{}) *watching {
	w := &watching{finished: make(chan struct{}), judged: make(chan utils.TeardownProgress, 4096)}
	sw := stopWatch{logger: zap.NewNop(), gone: f.gone, read: f.read, kill: f.kill, poll: time.Millisecond, reported: reported,
		judged: func(r utils.TeardownProgress) {
			select {
			case w.judged <- r:
			default:
			}
		}}
	go func() {
		w.killed.Store(sw.run())
		close(w.finished)
	}()
	return w
}

// awaitObserved returns once the watch has judged want -- its verdict on it
// already acted on, a kill included -- or once the watch has returned.
func awaitObserved(t *testing.T, f *fakeAgent, w *watching, want utils.TeardownProgress) {
	t.Helper()
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		select {
		case r := <-w.judged:
			if r == want {
				// The verdict is acted on after the judge reports: wait for
				// the next judgement, or the end, so a kill it decided on
				// has happened.
				select {
				case <-w.judged:
				case <-w.finished:
				case <-hung:
					t.Fatalf("the watch stopped judging after %+v", want)
				}
				return
			}
		case <-w.finished:
			return
		case <-hung:
			t.Fatalf("the watch never judged %+v", want)
		}
	}
}

func awaitWatch(t *testing.T, w *watching) bool {
	t.Helper()
	select {
	case <-w.finished:
		return w.killed.Load()
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the watch did not return")
		return false
	}
}

// A teardown that keeps taking steps is never killed, however many checks it
// takes, and the watch returns when the agent exits.
func TestStopWatchWaitsForAProgressingTeardown(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	f := newFakeAgent()
	reported := make(chan struct{})
	w := startWatch(f, reported)
	for beat := uint64(0); beat < 20; beat++ {
		f.set(p(beat+1, beat, false))
		awaitObserved(t, f, w, p(beat+1, beat, false))
	}
	select {
	case <-reported:
	default:
		t.Fatal("the watch did not say the agent had reported")
	}
	f.exited()
	if awaitWatch(t, w) || f.killed.Load() != 0 {
		t.Fatal("a teardown that kept making progress was killed")
	}
}

// A teardown that takes no step across stallChecks checks is killed, and not
// a check before.
func TestStopWatchKillsAStalledTeardown(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	f := newFakeAgent()
	w := startWatch(f, nil)
	for beat := uint64(0); beat < stallChecks; beat++ {
		f.set(p(7, beat, false))
		awaitObserved(t, f, w, p(7, beat, false))
		if f.killed.Load() != 0 {
			t.Fatalf("killed after %d checks without progress, want %d", beat, stallChecks)
		}
	}
	f.set(p(7, stallChecks, false))
	if !awaitWatch(t, w) || f.killed.Load() != 1 {
		t.Fatalf("a teardown stalled for %d checks was not killed once (kills: %d)", stallChecks, f.killed.Load())
	}
}

// An agent that has detached everything and is still there a check later is
// killed; not in the check that reported it.
func TestStopWatchKillsAnAgentDetachedButNotExiting(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	f := newFakeAgent()
	w := startWatch(f, nil)
	f.set(p(5, 4, true))
	awaitObserved(t, f, w, p(5, 4, true))
	if f.killed.Load() != 0 {
		t.Fatal("killed in the check that reported the detach done: it was given no check to exit")
	}
	f.set(p(5, 5, true))
	if !awaitWatch(t, w) || f.killed.Load() != 1 {
		t.Fatal("an agent detached but not exiting was not killed")
	}
}

// Ctrl+C pressed again kills the agent at once, progressing or not.
func TestStopWatchKillsAtASecondCtrlC(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	f := newFakeAgent()
	f.set(p(1, 0, false))
	w := startWatch(f, nil)
	awaitObserved(t, f, w, p(1, 0, false))
	f.set(p(2, 1, false)) // still progressing
	utils.MarkInterruptedAgain()
	if !awaitWatch(t, w) || f.killed.Load() != 1 {
		t.Fatal("Ctrl+C pressed again did not kill the agent")
	}
}

// An agent that reports nothing -- one that predates the report -- is never
// killed for want of progress: the watch waits for its exit.
func TestStopWatchNeverKillsAnAgentThatReportsNothing(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 1
	f := newFakeAgent()
	w := startWatch(f, nil)
	for i := 0; i < 50; i++ {
		select {
		case <-f.reads:
		case <-w.finished:
			t.Fatal("the watch returned while the agent was still there")
		case <-time.After(time.Minute):
			t.Fatal("the watch stopped reading")
		}
	}
	f.exited()
	if awaitWatch(t, w) || f.killed.Load() != 0 {
		t.Fatal("an agent that reported nothing was killed")
	}
}
