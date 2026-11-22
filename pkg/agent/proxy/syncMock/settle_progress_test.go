package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// progressingWatermark settles nothing, and counts the progress of what holds
// a test case's verdict (HeldProgressor) as the test moves it.
type progressingWatermark struct {
	settled  atomic.Bool
	progress atomic.Uint64
}

func (p *progressingWatermark) Settled(_, _ string, _, _ time.Time) bool { return p.settled.Load() }
func (p *progressingWatermark) HeldProgress(_, _ string, _, _ time.Time) uint64 {
	return p.progress.Load()
}

// A test case whose verdict waits on a parser working through its backlog is
// held for as long as that parser moves, however long that takes in all: the
// hold's bound is on progress, in the time the process could run. Before, it
// was 30 s of wall time since the test case came, and a busy or starved
// capture had its test cases saved unsettled (a parser that then failed on
// the traffic they overlap left them without their mocks). Once what holds it
// stops moving for the bound, the test case is let go, marked unsettled.
func TestTestCaseHold_HoldsWhileWhatHoldsItProgresses(t *testing.T) {
	w := &progressingWatermark{}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("behind", time.Unix(1000, 0)), "", "a")
	for i := 0; i < 10; i++ { // ten minutes of run time, the parser moving
		clk.Advance(50 * time.Second)
		w.progress.Add(1)
		if got := h.Release(); len(got) != 0 {
			t.Fatalf("released %v while what held it progressed (%d min)", names(got), (i+1)*50/60)
		}
	}
	clk.Advance(59 * time.Second)
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v inside the bound since its last progress", names(got))
	}
	clk.Advance(time.Second)
	got := h.Release()
	if len(got) != 1 || !got[0].Unsettled {
		t.Fatalf("released %+v once what held it stopped for the bound, want it marked unsettled", got)
	}
}

// Wall time the process could not run does not count: a starved capture's
// test cases are not let go for it.
func TestTestCaseHold_AStarvedCaptureKeepsItsTestCases(t *testing.T) {
	const bound = 50 * time.Millisecond
	w := &fakeWatermark{at: map[string]time.Time{}} // counts no progress
	h := NewTestCaseHold(w, bound, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("starved", time.Unix(1000, 0)), "", "a")
	for end := time.Now().Add(4 * bound); time.Now().Before(end); { // four bounds of wall time, no CPU
		if got := h.Release(); len(got) != 0 {
			t.Fatalf("released %v on wall time the process could not run", names(got))
		}
		time.Sleep(time.Millisecond)
	}
	clk.Advance(bound)
	if got := h.Release(); len(got) != 1 || !got[0].Unsettled {
		t.Fatalf("released %+v after the bound of run time, want it marked unsettled", got)
	}
}

// The hold holds the process clock only while it holds a test case: an idle
// stream costs no sampling.
func TestTestCaseHold_HoldsTheClockOnlyWhileHolding(t *testing.T) {
	w := &fakeWatermark{at: map[string]time.Time{"a": time.Unix(2000, 0)}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	held := 0
	h.clk = &stall.ManualClock{}
	h.hold = func() func() {
		held++
		return func() { held-- }
	}
	if held != 0 {
		t.Fatal("an empty hold holds the clock")
	}
	h.Add(heldTC("one", time.Unix(1000, 0)), "", "a")
	h.Add(heldTC("two", time.Unix(1000, 0)), "", "a")
	if held != 1 {
		t.Fatalf("holding test cases, the clock is held %d times, want once", held)
	}
	if got := h.Release(); len(got) != 2 {
		t.Fatalf("released %v, want both settled", names(got))
	}
	if held != 0 {
		t.Fatal("an emptied hold still holds the clock")
	}
	h.Add(heldTC("three", time.Unix(3000, 0)), "", "a")
	h.Drain()
	if held != 0 {
		t.Fatal("a drained hold still holds the clock")
	}
}

// The manager's hand-on counts as progress for a test case waiting on a mock
// of its window: delivered or dropped.
func TestHandOnProgressMovesWithEachSendDecided(t *testing.T) {
	m := New(nil)
	ch := make(chan *models.Mock, 1)
	m.SetOutputChannel(ch)
	before := m.HandOnProgress()
	if !sendOwnedAndWait(t, m, &models.Mock{Name: "a"}, "") {
		t.Fatal("setup: not delivered")
	}
	if m.HandOnProgress() != before+1 {
		t.Fatalf("hand-on progress %d after a delivery, want %d", m.HandOnProgress(), before+1)
	}
	stalledOutClock(m)
	if sendOwnedAndWait(t, m, &models.Mock{Name: "b"}, "") {
		t.Fatal("setup: a mock for a full channel no one reads was delivered")
	}
	if m.HandOnProgress() != before+2 {
		t.Fatalf("hand-on progress %d after a drop, want %d", m.HandOnProgress(), before+2)
	}
}

// podProgressWatermark settles a test case once its pod's position has passed
// its end (fakeWatermark), and counts the progress of what holds it as the
// test moves it.
type podProgressWatermark struct {
	*fakeWatermark
	progress uint64
}

func (p *podProgressWatermark) HeldProgress(_, _ string, _, _ time.Time) uint64 { return p.progress }

// A test case held behind an earlier one of its pod (every test case of a
// stream is of one pod in proxy mode) is held for as long as what holds it
// progresses, too: when the earlier one goes, it is not let go unsettled at
// once for the time it waited behind it.
func TestTestCaseHold_OneBehindAnEarlierOneIsHeldWhileWhatHoldsItProgresses(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &podProgressWatermark{fakeWatermark: &fakeWatermark{at: map[string]time.Time{"": t0}}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("first", t0.Add(time.Second)), "", "")
	h.Add(heldTC("second", t0.Add(2*time.Second)), "", "")
	for i := 0; i < 10; i++ { // the parser moving through the backlog
		clk.Advance(50 * time.Second)
		w.progress++
		if got := h.Release(); len(got) != 0 {
			t.Fatalf("released %v while what held them progressed", names(got))
		}
	}
	clk.Advance(10 * time.Second)
	w.progress++
	w.at[""] = t0.Add(time.Second) // past first, not yet past second
	if got := h.Release(); len(got) != 1 || got[0].TC.Name != "first" || got[0].Unsettled {
		t.Fatalf("released %+v, want only first, settled: second's parser is still moving", got)
	}
	clk.Advance(49 * time.Second)
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v inside the bound since what held it last moved", names(got))
	}
	clk.Advance(time.Second)
	if got := h.Release(); len(got) != 1 || got[0].TC.Name != "second" || !got[0].Unsettled {
		t.Fatalf("released %+v once what held it stopped for the bound, want second, unsettled", got)
	}
}

// Test cases of a pod whose holder stopped are let go together at the bound:
// the one behind does not wait a bound of its own after the first goes.
func TestTestCaseHold_APodsWedgedTestCasesGoTogether(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &podProgressWatermark{fakeWatermark: &fakeWatermark{at: map[string]time.Time{"": t0}}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("first", t0.Add(time.Second)), "", "")
	h.Add(heldTC("second", t0.Add(2*time.Second)), "", "")
	clk.Advance(30 * time.Second)
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v inside the bound", names(got))
	}
	clk.Advance(30 * time.Second)
	if got := names(h.Release()); len(got) != 2 {
		t.Fatalf("released %v at the bound, want both", got)
	}
}

// Several watermarks: a test case is held on the progress of those that hold
// its verdict. One that has settled it does not keep it held by moving, and
// one that holds it but cannot count its progress is not covered by
// another's: it goes once the bound has passed since it came.
func TestTestCaseHold_SeveralWatermarksCountOnlyWhatHoldsTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder Watermark
	}{
		{"a holder whose count stopped", &progressingWatermark{}},
		{"a holder that cannot count", &fakeWatermark{at: map[string]time.Time{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy := &progressingWatermark{} // settled it, and its capture is busy
			busy.settled.Store(true)
			h := NewTestCaseHold(allWatermarks{tc.holder, busy}, time.Minute, 1<<20)
			clk := &stall.ManualClock{}
			h.clk, h.hold = clk, nil
			h.Add(heldTC("wedged", time.Unix(1000, 0)), "", "a")
			for i := 0; i < 5; i++ {
				clk.Advance(10 * time.Second)
				busy.progress.Add(1)
				if got := h.Release(); len(got) != 0 {
					t.Fatalf("released %v inside the bound", names(got))
				}
			}
			clk.Advance(10 * time.Second)
			busy.progress.Add(1)
			if got := h.Release(); len(got) != 1 || !got[0].Unsettled {
				t.Fatalf("released %+v, want it let go unsettled at the bound: what moved does not hold it", got)
			}
		})
	}
}

// A capture's Progressor moves with every parser it runs: it does not stand
// for the progress of what holds one test case, and a busy capture does not
// keep a test case whose own blocker has stopped.
func TestTestCaseHold_ABusyCapturesProgressDoesNotHoldATestCase(t *testing.T) {
	w := &backlogWatermark{} // settles nothing; its capture moves
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("wedged", time.Unix(1000, 0)), "", "a")
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Second)
		w.progress.Add(1)
		if got := h.Release(); len(got) != 0 {
			t.Fatalf("released %v inside the bound", names(got))
		}
	}
	clk.Advance(10 * time.Second)
	w.progress.Add(1)
	if got := h.Release(); len(got) != 1 || !got[0].Unsettled {
		t.Fatalf("released %+v, want it let go unsettled at the bound since it came", got)
	}
}

// The hold's bound outlasts the mock stream's, and that bound's look: a test
// case held for a mock of its window that the stream gives up is checked once
// the mock's drop is recorded (and left out), not saved before it, without it.
// Both stalls start at the last hand-on.
func TestTestCaseHoldMaxOutlastsTheMockStreamsStall(t *testing.T) {
	if lim := outChanStall + stall.CheckEvery(outChanStall) + TestCaseHoldTick; TestCaseHoldMax <= lim {
		t.Fatalf("TestCaseHoldMax %v does not outlast the mock stream's stall bound and its look (%v)", TestCaseHoldMax, lim)
	}
}
