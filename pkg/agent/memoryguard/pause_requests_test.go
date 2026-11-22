package memoryguard

import (
	"testing"
)

// endPauseAtCleanup leaves the guard unpaused after the test, whatever it
// left on.
func endPauseAtCleanup(t *testing.T) {
	t.Helper()
	applyPausedState(false)
	t.Cleanup(func() { applyPausedState(false) })
}

// ingress counts n requests as the ingress does: each counted, then reading
// the pause. It returns how many read it on.
func ingress(n int) (paused int) {
	for i := 0; i < n; i++ {
		IngressRequest()
		if IsRecordingPaused() {
			paused++
		}
	}
	return paused
}

// A real pause (the guard's applyPausedState): the requests counted between its
// start and its end came during it. The guard re-applies a pause on every tick
// it lasts; that is no new pause.
func TestAPauseTallyCountsTheRequestsBetweenAPausesStartAndItsEnd(t *testing.T) {
	endPauseAtCleanup(t)
	tally := OpenPauseTally()
	defer tally.Close()

	ingress(2)
	applyPausedState(true)
	ingress(1)
	applyPausedState(true) // the next tick, still short of memory
	read := ingress(3)
	applyPausedState(false)
	ingress(5)

	if read != 3 {
		t.Fatalf("%d of the 3 requests after the re-applied pause read it", read)
	}
	if got, want := tally.Read(), (PauseRequests{Requests: 11, DuringPauses: 4, BeforeFirstPause: 2}); got != want {
		t.Fatalf("the tally counted %+v, want %+v", got, want)
	}

	applyPausedState(true)
	ingress(6)
	// The pause still on counts to now.
	if got, want := tally.Read(), (PauseRequests{Requests: 17, DuringPauses: 10, BeforeFirstPause: 2}); got != want {
		t.Fatalf("with a second pause on, the tally counted %+v, want %+v", got, want)
	}
	applyPausedState(false)
	ingress(1)
	if got, want := tally.Read(), (PauseRequests{Requests: 18, DuringPauses: 10, BeforeFirstPause: 2}); got != want {
		t.Fatalf("after the second pause, the tally counted %+v, want %+v", got, want)
	}
}

// The forced pause "from:to": the requests during it are from to to-1, the
// ones that read it, and those before it are the from-1 before.
func TestAPauseTallyCountsTheForcedPausesRequestsAsThoseThatReadIt(t *testing.T) {
	endPauseAtCleanup(t)
	t.Cleanup(SetForcedPauseForTest("4:9"))
	tally := OpenPauseTally()
	defer tally.Close()

	read := ingress(12)
	if read != 5 {
		t.Fatalf("%d requests read the forced pause 4:9, want 5", read)
	}
	if got, want := tally.Read(), (PauseRequests{Requests: 12, DuringPauses: 5, BeforeFirstPause: 3}); got != want {
		t.Fatalf("the tally counted %+v, want %+v", got, want)
	}
}

// A tally counts from its open: requests and pauses before it are not its
// own, and a pause on as it opens counts from then, with none before it.
func TestAPauseTallyCountsFromItsOpen(t *testing.T) {
	endPauseAtCleanup(t)
	early := OpenPauseTally()
	defer early.Close()

	ingress(3)
	applyPausedState(true)
	ingress(2)
	late := OpenPauseTally()
	defer late.Close()
	ingress(4)
	applyPausedState(false)
	ingress(1)

	if got, want := early.Read(), (PauseRequests{Requests: 10, DuringPauses: 6, BeforeFirstPause: 3}); got != want {
		t.Fatalf("the tally opened first counted %+v, want %+v", got, want)
	}
	if got, want := late.Read(), (PauseRequests{Requests: 5, DuringPauses: 4, BeforeFirstPause: 0}); got != want {
		t.Fatalf("the tally opened during the pause counted %+v, want %+v", got, want)
	}
}

// Closed, a tally keeps what it counted until then, a pause still on as it
// closed counted to its close, and counts nothing after.
func TestAClosedPauseTallyKeepsItsCount(t *testing.T) {
	endPauseAtCleanup(t)
	tally := OpenPauseTally()
	ingress(2)
	applyPausedState(true)
	ingress(3)
	tally.Close()
	ingress(4)
	applyPausedState(false)
	applyPausedState(true)
	ingress(5)
	tally.Close()

	if got, want := tally.Read(), (PauseRequests{Requests: 5, DuringPauses: 3, BeforeFirstPause: 2}); got != want {
		t.Fatalf("the closed tally reads %+v, want %+v", got, want)
	}
	pauseEdgeMu.Lock()
	_, open := tallies[tally]
	pauseEdgeMu.Unlock()
	if open {
		t.Fatal("a closed tally is still told of each pause")
	}

	// With no pause at all, every request came before the first.
	applyPausedState(false)
	none := OpenPauseTally()
	ingress(7)
	none.Close()
	if got, want := none.Read(), (PauseRequests{Requests: 7, BeforeFirstPause: 7}); got != want {
		t.Fatalf("with no pause, the tally reads %+v, want %+v", got, want)
	}
}
