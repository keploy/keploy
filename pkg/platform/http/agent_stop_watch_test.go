package http

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	now    atomic.Bool                 // the last kill was to be at once (Ctrl+C), not deferrable
	reads  chan utils.TeardownProgress // each read's result, as far as the buffer holds
	// busy: a kill that is not at once does not end it (it is in the
	// middle of teardown work, and dies once that is over).
	busy bool
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{gone: make(chan struct{}), reads: make(chan utils.TeardownProgress, 4096)}
}

func (f *fakeAgent) set(r utils.TeardownProgress) {
	f.mu.Lock()
	f.report, f.has = r, true
	f.mu.Unlock()
}

func (f *fakeAgent) read(context.Context) (utils.TeardownProgress, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.reads <- f.report:
	default:
	}
	return f.report, f.has
}

func (f *fakeAgent) exited() { f.exit.Do(func() { close(f.gone) }) }

func (f *fakeAgent) kill(now bool) error {
	f.now.Store(now)
	f.killed.Add(1)
	if now || !f.busy {
		f.exited() // a killed agent exits
	}
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
	awaitWatch(t, w)
	if f.killed.Load() != 1 {
		t.Fatalf("a teardown stalled for %d checks was not killed once (kills: %d)", stallChecks, f.killed.Load())
	}
	if f.now.Load() {
		t.Fatal("a stalled agent was killed at once: it must be told to die once no teardown work is in progress")
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
	awaitWatch(t, w)
	if f.killed.Load() != 1 {
		t.Fatal("an agent detached but not exiting was not killed")
	}
	if f.now.Load() {
		t.Fatal("a detached agent was killed at once: it must be told to die once no teardown work is in progress")
	}
}

// A press while keploy waits for the agent, once the stop has said a press
// stops it, escalates one step: the first has the agent die once no teardown
// work is in progress -- one that is in the middle of some finishes it first
// -- and a press after the stop has offered again kills it at once,
// progressing or not.
func TestStopWatchKillsAtASecondCtrlC(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	theStopWasASignal(t)
	f := newFakeAgent()
	f.busy = true // removing what it attached to the kernel
	f.set(p(1, 0, false))
	w := startWatch(f, nil)
	awaitObserved(t, f, w, p(1, 0, false))
	f.set(p(2, 1, false)) // still progressing
	ctrlCWhileWaited(t)
	within(t, "the first press's kill", func() {
		for f.killed.Load() < 1 {
			time.Sleep(time.Millisecond)
		}
	})
	if f.now.Load() {
		t.Fatal("the first press killed the agent at once, in the middle of its teardown work")
	}
	ctrlCAgainWhileWaited(t)
	if !awaitWatch(t, w) || f.killed.Load() != 2 || !f.now.Load() {
		t.Fatalf("the press after the second offer did not kill the agent at once (kills %d, now %v)", f.killed.Load(), f.now.Load())
	}
}

// The first press, with the agent between teardown steps, has it die at once
// of the escalation: the watch sees it exit, and reports no kill at once.
func TestStopWatchFirstCtrlCLetsAnIdleAgentDie(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	theStopWasASignal(t)
	f := newFakeAgent()
	f.set(p(1, 0, false))
	w := startWatch(f, nil)
	awaitObserved(t, f, w, p(1, 0, false))
	ctrlCWhileWaited(t)
	if awaitWatch(t, w) || f.killed.Load() != 1 || f.now.Load() {
		t.Fatalf("want the escalation only, and the watch to see the agent exit (kills %d, now %v)", f.killed.Load(), f.now.Load())
	}
}

// A press that lands as the agent exits on its own (both ready when the
// watch's loop looks) kills nothing: the watch reports the exit, never a kill
// at once, which would have what waits on its exit let go and drop it.
func TestStopWatchAPressAsTheAgentExitsIsNoKill(t *testing.T) {
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	for i := 0; i < 40; i++ { // the loop's select takes either: 40 runs catch a wrong pick all but surely
		utils.ResetInterruptedAgain()
		utils.MarkInterrupted()
		core, logs := observer.New(zapcore.InfoLevel)
		f := newFakeAgent()
		f.set(p(1, 0, true)) // between steps, past the judge's first look
		var block atomic.Bool
		inJudged := make(chan struct{}, 1)
		release := make(chan struct{})
		sw := stopWatch{logger: zap.New(core), gone: f.gone, read: f.read, kill: f.kill, poll: time.Millisecond,
			judged: func(utils.TeardownProgress) {
				if block.CompareAndSwap(true, false) {
					inJudged <- struct{}{}
					<-release // the watch's loop is busy a moment
				}
			}}
		done := make(chan bool, 1)
		go func() { done <- sw.run() }()
		within(t, "the offer", func() {
			for logs.FilterMessageSnippet("Press Ctrl+C again to stop the keploy agent").Len() == 0 {
				time.Sleep(time.Millisecond)
			}
		})
		block.Store(true)
		within(t, "the watch's loop held", func() { <-inJudged })
		f.exited()                   // the agent exits on its own
		utils.MarkInterruptedAgain() // as the user presses, as told
		close(release)
		var killedAtOnce bool
		within(t, "the watch's return", func() { killedAtOnce = <-done })
		if killedAtOnce {
			t.Fatalf("run %d: a kill at once was reported for an agent that exited on its own (kills %d)", i, f.killed.Load())
		}
	}
}

// A stop that began on its own (a record timer, the app's exit), on which a
// pattern kill lands while keploy waits for the agent: `sudo pkill -INT -f
// keploy` reaches keploy twice, once from sudo, which hands its own copy on.
// The copy is no Ctrl+C pressed while keploy waits: the agent, mid-teardown,
// is not killed, and is only once the user presses after the stop has said a
// press stops it (a whole stopWatchOffer after the signal was seen; the stop's
// clock is the test's here, so nothing hangs on scheduling), and then as the
// first press does: it dies once no teardown work is in progress.
func TestStopWatchDoesNotTakeACopyOfTheStopForCtrlC(t *testing.T) {
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	defer utils.SetNoticeClockForTest(func() time.Time { return time.Unix(0, clock.Load()) })()
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	core, logs := observer.New(zapcore.InfoLevel)
	f := newFakeAgent()
	f.set(p(1, 0, false))
	w := &watching{finished: make(chan struct{})}
	sw := stopWatch{logger: zap.New(core), gone: f.gone, read: f.read, kill: f.kill, poll: time.Millisecond}
	go func() {
		w.killed.Store(sw.run())
		close(w.finished)
	}()
	t.Cleanup(f.exited)
	for len(f.reads) == 0 { // the watch is under way
		time.Sleep(time.Millisecond)
	}
	utils.MarkInterrupted()      // the pattern kill's first copy
	utils.MarkInterruptedAgain() // sudo's copy of it
	// The watch sees the signal on its next tick, at the clock's now.
	for len(f.reads) < 3 {
		time.Sleep(time.Millisecond)
	}
	advance(stopWatchOffer)
	const offered = "Press Ctrl+C again to stop the keploy agent"
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for logs.FilterMessageSnippet(offered).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch never said Ctrl+C stops the agent now")
		}
		time.Sleep(time.Millisecond)
		advance(time.Millisecond)
	}
	if f.killed.Load() != 0 {
		t.Fatalf("a copy of the stop killed the agent mid-teardown (kills %d, now %v)", f.killed.Load(), f.now.Load())
	}
	utils.MarkInterruptedAgain() // the user, as told
	if awaitWatch(t, w) || f.killed.Load() != 1 || f.now.Load() {
		t.Fatalf("the user's Ctrl+C after the offer did not have the agent die once idle (kills %d, now %v)", f.killed.Load(), f.now.Load())
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

// A stop holds keploy's drains (utils.BeginAgentStop) only from the agent's
// first report that it records its steps, and until the watch returns: such
// a stop ends on its own. One of an agent that does not record its steps may
// never end, and must not keep keploy from exiting.
func TestOnlyAStopOfAnAgentThatRecordsItsStepsHoldsTheDrains(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	for _, steps := range []bool{false, true} {
		f := newFakeAgent()
		w := startWatch(f, nil)
		r := utils.TeardownProgress{Seq: 1, Beat: 0, Steps: steps}
		f.set(r)
		awaitObserved(t, f, w, r)
		if got := utils.AgentStopInProgress(); got != steps {
			t.Fatalf("steps=%v: the stop holds the drains = %v", steps, got)
		}
		f.exited()
		awaitWatch(t, w)
		if utils.AgentStopInProgress() {
			t.Fatalf("steps=%v: the stop still holds the drains after it ended", steps)
		}
	}
}

// A kill the judge decided on lets the agent finish the teardown work it is
// in the middle of; the watch goes on waiting for its exit, and Ctrl+C
// pressed meanwhile kills it at once.
func TestStopWatchWaitsAfterAVerdictAndKillsAtCtrlC(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	theStopWasASignal(t)
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 2
	f := newFakeAgent()
	f.busy = true
	w := startWatch(f, nil)
	for beat := uint64(0); beat <= stallChecks; beat++ {
		f.set(p(3, beat, false))
		awaitObserved(t, f, w, p(3, beat, false))
	}
	if f.killed.Load() != 1 || f.now.Load() {
		t.Fatalf("a stalled agent was not sent the kill that waits for its work (kills %d, now %v)", f.killed.Load(), f.now.Load())
	}
	select { // only that it has NOT returned yet
	case <-w.finished:
		t.Fatal("the watch stopped waiting for an agent still in the middle of its work")
	case <-time.After(100 * time.Millisecond):
	}
	ctrlCWhileWaited(t)
	if !awaitWatch(t, w) || f.killed.Load() != 2 || !f.now.Load() {
		t.Fatalf("Ctrl+C after the verdict did not kill the agent at once (kills %d, now %v)", f.killed.Load(), f.now.Load())
	}
}

// A read that does not return (an engine's `cp` that hangs) never holds up
// the watch's return at the agent's exit: its context is cancelled first.
func TestStopWatchDoesNotWaitOnASlowRead(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	gone := make(chan struct{})
	reading := make(chan struct{}, 1)
	sw := stopWatch{logger: zap.NewNop(), gone: gone, poll: time.Millisecond,
		read: func(ctx context.Context) (utils.TeardownProgress, bool) {
			select {
			case reading <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return utils.TeardownProgress{}, false
		},
		kill: func(bool) error { return nil }}
	returned := make(chan struct{})
	go func() { sw.run(); close(returned) }()
	select {
	case <-reading:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the watch never read")
	}
	close(gone)
	select {
	case <-returned:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the watch waited on a read that does not return")
	}
}

// A stop holds the drains only while the agent beats: a frozen one (stopped,
// or frozen by its cgroup), whose stop would not end, lets them keep their
// bound; its beats coming back hold them again.
func TestAFrozenAgentsStopLetsTheDrainsGo(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(d time.Duration) { holdStale = d }(holdStale)
	holdStale = 50 * time.Millisecond
	awaitHeld := func(want bool) {
		t.Helper()
		hung := time.After(time.Minute) // only turns a hang into a failure
		for utils.AgentStopInProgress() != want {
			select {
			case <-hung:
				t.Fatalf("the stop holds the drains = %v, want %v", !want, want)
			case <-time.After(time.Millisecond):
			}
		}
	}
	f := newFakeAgent()
	w := startWatch(f, nil)
	f.set(p(1, 0, false)) // then frozen: the same report, read after read
	awaitHeld(true)
	awaitHeld(false)
	f.set(p(2, 1, false)) // it beats again
	awaitHeld(true)
	f.exited()
	awaitWatch(t, w)
	if utils.AgentStopInProgress() {
		t.Fatal("the stop still holds the drains after it ended")
	}
}
