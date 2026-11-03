package utils

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"golang.org/x/sync/errgroup"
)

// reporterLike is record's progress reporter in the frame drain's shape: its
// notice is made on the caller, its ticker on its own goroutine, every
// notice period; it ends at stop or at StopNow, its offer withdrawn before
// exited is closed. pressAtOffer presses as soon as its notice offers (a
// user who answers at once). offered counts the notices that offered;
// endedAt, read once exited is closed, is the time just before it was.
func reporterLike(pressAtOffer bool) (exited <-chan struct{}, offered *atomic.Int32, endedAt *time.Time, stop func()) {
	n := NewStopNowNotice(drainNotice)
	done := make(chan struct{})
	out := make(chan struct{})
	offered = &atomic.Int32{}
	endedAt = new(time.Time)
	go func() {
		defer close(out)
		defer func() { *endedAt = time.Now() }()
		defer n.Withdraw()
		tk := time.NewTicker(drainNotice)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-StopNow():
				return
			case <-tk.C:
			}
			if n.Next() {
				if offered.Add(1) == 1 && pressAtOffer {
					MarkInterruptedAgain()
				}
			}
		}
	}()
	return out, offered, endedAt, func() { close(done); <-out }
}

// Record's frame drain runs inside its progress reporter's notices: the
// reporter offers to end at the next Ctrl+C, and the drain's group ends at
// that press. The reporter's ticker starts on its own goroutine and the
// drain's on the caller, both every notice period, so either may notice
// first. Whichever does, the reporter's offer is the only one: the drain's
// is made for it (DrainErrGroupProgressCarried), and the drain says nothing
// until the reporter has ended. A press as soon as the offer stands ends the
// group, which the drain joins. When the drain's own notice came
// first, it offered as well, and under load about two runs in five showed
// two offers with different consequences; with the press at once, about one
// in fifteen gave up a group that ended at the press.
func TestARecordFrameDrainSaysOneOfferWhicheverNoticesFirst(t *testing.T) {
	for i := 0; i < 200; i++ {
		drainStopNowTest(t)
		exited, offered, endedAt, stop := reporterLike(true)
		var g errgroup.Group
		g.Go(func() error { <-StopNow(); return nil })
		res, logs := startCarriedDrain(&stall.ManualClock{}, &g, 30*time.Second, func() uint64 { return 0 }, exited)
		r := awaitEnd(t, res, "at its group's end, at the press")
		stop()
		for _, e := range logs.FilterMessageSnippet("made no progress").All() {
			if e.Time.Before(*endedAt) {
				t.Fatalf("run %d: the drain spoke while its reporter carried the offer: %v", i, logs.All())
			}
		}
		if offered.Load() == 0 {
			t.Fatalf("run %d: the drain ended %+v before the reporter's offer", i, r)
		}
		if r.err != nil || r.end != DrainJoined {
			t.Fatalf("run %d: the drain returned %+v: its group ended at the press, want it joined", i, r)
		}
	}
}

// While the wait that makes its offer runs, a drain says nothing and offers
// nothing, however many notices pass. Once that wait ends, the drain's own
// notices begin: it says it made no progress, and offers only a whole notice
// period after that end, so a press that answered the other wait, or one
// right after it, is not an answer to the drain. A press after its offer
// gives it up.
func TestADrainWhoseOfferAnotherWaitMakesBeginsItsNoticesAtThatWaitsEnd(t *testing.T) {
	drainStopNowTest(t)
	drainNotice = 50 * time.Millisecond
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	t.Cleanup(SetNoticeClockForTest(func() time.Time { return time.Unix(0, now.Load()) }))
	carrier := make(chan struct{})
	var g errgroup.Group
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
	var looks atomic.Uint64
	res, logs := startCarriedDrain(&stall.ManualClock{}, &g, 30*time.Second, func() uint64 { looks.Add(1); return 0 }, carrier)
	awaitLooks(t, &looks, 0, 10)
	if n := logs.FilterMessageSnippet("made no progress").Len(); n != 0 || offersStanding() != 0 {
		t.Fatalf("the drain spoke (%d lines), or offered (%d offers stand), while the other wait carried its offer: %v", n, offersStanding(), logs.All())
	}
	// Ten notice periods pass while the other wait runs; then it ends.
	now.Add(int64(10 * drainNotice))
	closedAt := time.Now()
	close(carrier)
	said := awaitLog(t, logs, "made no progress", res)
	if offersStanding() != 0 || strings.Contains(said.Message, drainOffer) {
		t.Fatalf("the drain offered at its first notice after the other wait ended (%q, %d offers stand): want its offer a whole notice period after that end", said.Message, offersStanding())
	}
	if d := said.Time.Sub(closedAt); d < drainNotice {
		t.Fatalf("the drain gave its first notice %v after the other wait ended, want a whole notice period (%v) on", d, drainNotice)
	}
	if MarkInterruptedAgain() {
		t.Fatal("a press before the drain's offer ended a stop")
	}
	notReturned(t, res, "at a press before its offer")
	now.Add(int64(drainNotice))
	awaitLog(t, logs, drainOffer, res)
	if !MarkInterruptedAgain() {
		t.Fatal("the press after the drain's offer ended no stop")
	}
	if r := awaitEnd(t, res, "at the press after its offer"); r.end != DrainStopped {
		t.Fatalf("the drain returned %+v at the press after its offer, want it given up there (DrainStopped)", r)
	}
}

// While another wait's offer stands, the drain says nothing and makes no
// offer of its own: one it made unsaid would stand alone once the other is
// withdrawn, and a press then gave the drain up before it told the user
// anything. So a press right after the other is withdrawn ends the drain only
// at its own offer, said first; otherwise the drain goes on to its group's
// end.
func TestADrainBesideAnotherWaitsOfferMakesNoOfferUnsaid(t *testing.T) {
	for i := 0; i < 50; i++ {
		drainStopNowTest(t)
		_, withdraw := OfferStopNow() // the other wait's offer
		var g errgroup.Group
		block := make(chan struct{})
		g.Go(func() error { <-block; return nil })
		var looks atomic.Uint64
		res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, func() uint64 { looks.Add(1); return 0 })
		awaitLooks(t, &looks, 0, 10)
		if n := logs.FilterMessageSnippet("made no progress").Len(); n != 0 || offersStanding() != 1 {
			t.Fatalf("run %d: beside another wait's offer the drain spoke (%d lines), or offered (%d offers stand, want the other's alone): %v", i, n, offersStanding(), logs.All())
		}
		withdraw()
		MarkInterruptedAgain()
		at := looks.Load()
		var r drainResult
		ended := false
		for !ended && looks.Load() < at+10 {
			select {
			case r = <-res:
				ended = true
			default:
				time.Sleep(time.Millisecond)
			}
		}
		if ended {
			if r.end != DrainStopped || logs.FilterMessageSnippet(drainOffer).Len() == 0 {
				t.Fatalf("run %d: the drain ended %+v at a press it had made no offer for (said: %v)", i, r, logs.All())
			}
			close(block)
			continue
		}
		close(block)
		if r = awaitEnd(t, res, "at its group's end"); r.err != nil || r.end != DrainJoined {
			t.Fatalf("run %d: the drain returned %+v, want its group joined", i, r)
		}
	}
}

// A stop that began on its own (the app exited, a record timer) is not
// interrupted when the wait that made its drain's offer ends. The user's
// interrupt that comes later is seen at the drain's next notice, and the
// drain offers a whole notice period after that: a copy of the stop's signal
// (sudo hands its own on) is never taken for the answer to an offer.
func TestADrainWhoseOfferAnotherWaitMadeOffersAPeriodAfterTheInterruptIsSeen(t *testing.T) {
	drainStopNowTestOnItsOwn(t)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	t.Cleanup(SetNoticeClockForTest(func() time.Time { return time.Unix(0, now.Load()) }))
	carrier := make(chan struct{})
	var g errgroup.Group
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
	var looks atomic.Uint64
	res, logs := startCarriedDrain(&stall.ManualClock{}, &g, 30*time.Second, func() uint64 { looks.Add(1); return 0 }, carrier)
	awaitLooks(t, &looks, 0, 2)
	close(carrier)
	awaitLog(t, logs, "made no progress", res) // its notices have begun
	now.Add(int64(10 * drainNotice))
	MarkInterrupted()
	at := looks.Load()
	awaitLooks(t, &looks, at, 10)
	if n := logs.FilterMessageSnippet(drainOffer).Len(); n != 0 || offersStanding() != 0 {
		t.Fatalf("the drain offered (%d lines, %d offers stand) at the notice that first saw the interrupt: want a whole notice period after", n, offersStanding())
	}
	now.Add(int64(drainNotice))
	awaitLog(t, logs, drainOffer, res)
}

// The drain's own notices begin at its carrier's end, wherever in the
// drain's notice period that end comes: its first notice is a whole period
// after it. Each run ends the carrier about half a period after one of the
// drain's looks, so a notice kept on the period the drain began with would
// come about half a period after that end, sooner than the scheduler's delay
// could hide. The drain is not interrupted here, so its notices only say it
// made no progress.
func TestACarriedDrainsFirstNoticeComesAWholePeriodAfterItsCarrierEnds(t *testing.T) {
	for i := 0; i < 5; i++ {
		drainStopNowTestOnItsOwn(t)
		drainNotice = 100 * time.Millisecond
		carrier := make(chan struct{})
		var g errgroup.Group
		block := make(chan struct{})
		g.Go(func() error { <-block; return nil })
		var looks atomic.Uint64
		res, logs := startCarriedDrain(&stall.ManualClock{}, &g, 30*time.Second, func() uint64 { looks.Add(1); return 0 }, carrier)
		awaitLooks(t, &looks, 0, 2) // its first look, then its first notice
		time.Sleep(drainNotice / 2)
		closedAt := time.Now()
		close(carrier)
		said := awaitLog(t, logs, "made no progress", res)
		if d := said.Time.Sub(closedAt); d < drainNotice {
			close(block)
			t.Fatalf("run %d: the drain gave its first notice %v after its carrier ended, want a whole notice period (%v) on", i, d, drainNotice)
		}
		close(block)
		if r := awaitEnd(t, res, "at its group's end"); r.err != nil || r.end != DrainJoined {
			t.Fatalf("run %d: the drain returned %+v, want its group joined", i, r)
		}
	}
}
