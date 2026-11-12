package learn

import (
	"runtime"
	"testing"
)

// The rows of this file hold that every read of a set gives back what it
// reserved of the set's readInFlight (reserve), whatever it found. A read
// that kept some leaves the set's later reads waiting for room no read
// gives back, so a row after it that reads a set's recordings with
// several readers waits for good, and the package ends at its timeout
// with no row named. These rows run first in the package (go test runs a
// package's files in name order, each file's rows in order), so such a
// read fails here, by name, before any other row can wait on it.

// A call's warm that comes to a recording another reader holds (take: not
// the call's, here the set's staging reading it) gives back what it
// reserved for it before it goes on, as it does once it has read one of
// its own: a reader that kept it would leave its reservation (up to all of
// readInFlight) taken after the call, and once none is left every later
// read of the set (the next call's, the staging's) waits for room no read
// gives back, and the learn step hangs. This is the common way a call's
// warm meets the staging. Counted: the staging's one reader holds the
// first recording, the call reads every other one and waits for the held
// one (a wait the set counts), and all of readInFlight is free once both
// end.
func TestACallsWarmGivesBackWhatItReservedForARecordingAnotherReaderHolds(t *testing.T) {
	const n = 6
	row := newHeldRow(n, 4)
	raw := row.raw()
	warmReadersOf(t, 2)
	oneStageReader(t)
	_, reading, _, release := heldUntil(t, raw, raw[0].payload().text)
	tr := NewTracker()
	tr.NewSet()
	s := tr.derived
	staged := tr.WarmSet(srcOf(row.mocks))
	<-reading

	tr.Begin()
	dc := tr.Drifts()
	call := make([]Raw, n)
	for i, m := range row.mocks {
		call[i] = dc.LazyRawOf(m, true, nil)
		if w := readWeight(call[i], readCost); w <= 0 || w*n >= readInFlight {
			t.Fatalf("a recording's read reserves %d of the %d in flight: want some, %d of them under all", w, readInFlight, n)
		}
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		dc.warm(call)
	}()
	for {
		if _, _, waits := readsOf(s); waits > 0 {
			break
		}
		select {
		case <-ended:
			t.Fatal("the call's warm ended while the staging held a recording of its: want it to wait for that read")
		default:
		}
		runtime.Gosched()
	}
	release()
	<-ended
	tr.End()
	if use := <-staged; use.Staged != 1 || use.Err != nil {
		t.Fatalf("the set's staging reported %+v: want the held recording alone read by it", use)
	}
	if st, called, waits := readsOf(s); st != 1 || called != n-1 || waits != 1 || s.finished != n-1 {
		t.Fatalf("the staging read %d and the rest of %d, the call %d, waiting %d times: want 1, the rest of the call's %d, and one wait, for the held one", st, s.finished, called, waits, n-1)
	}
	if !s.inFlight.TryAcquire(readInFlight) {
		t.Fatal("the call's warm and the staging kept some of the set's reads in flight after they ended: want all given back")
	}
	s.inFlight.Release(readInFlight)
}

// A read the set has no room for (take: nil, its held tier full) gives back
// what it reserved of the set's readInFlight before it took the recording,
// in a call's warm and in the set's staging alike: a read that kept it
// would leave the set's later reads (its next call's, the staging's)
// waiting for room no read gives back. Counted: all of readInFlight is free
// once each ends, and each reserved some.
func TestAReadTheSetHasNoRoomForGivesBackWhatItReserved(t *testing.T) {
	const n = 6
	row := newHeldRow(n, 4)
	warmReadersOf(t, 2)
	was := stageReaders
	stageReaders = func() int { return 2 }
	t.Cleanup(func() { stageReaders = was })
	tr := NewTracker()
	tr.NewSet()
	keepsNoMoreHeld(tr)
	s := tr.derived
	free := func(who string) {
		t.Helper()
		if !s.inFlight.TryAcquire(readInFlight) {
			t.Fatalf("%s kept some of the set's reads in flight after it ended: want all given back", who)
		}
		s.inFlight.Release(readInFlight)
	}

	tr.Begin()
	dc := tr.Drifts()
	raw := make([]Raw, n)
	for i, m := range row.mocks {
		raw[i] = dc.LazyRawOf(m, true, nil)
		if w := readWeight(raw[i], readCost); w <= 0 || w >= readInFlight {
			t.Fatalf("a recording's read reserves %d of the %d in flight: want some, under all of them", w, readInFlight)
		}
	}
	dc.warm(raw)
	tr.End()
	free("a call's warm")

	if w := readWeight(dc.LazyRawOf(row.mocks[0], false, withAnswer(row.mocks[0])), stageCost); w <= 0 || w >= readInFlight {
		t.Fatalf("a staged recording's read reserves %d of the %d in flight: want some, under all of them", w, readInFlight)
	}
	if use := <-tr.WarmSet(srcOf(row.mocks)); use.Staged != 0 || !use.HeldFull {
		t.Fatalf("the set's staging reported %+v: want none read, its held tier full", use)
	}
	free("the set's staging")
}
