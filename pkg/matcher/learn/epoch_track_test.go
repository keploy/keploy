package learn

import (
	"strconv"
	"testing"
	"time"
)

// pollAt is when a timer's call i reached keploy at record: a 2 s ticker
// (2.37 s apart once the work between ticks is counted) whose reads of the
// clock fall at a fraction of a second that moves across the second.
func pollAt(base time.Time, i int) time.Time {
	return base.Add(time.Duration(i)*2*time.Second + time.Duration(i*370)*time.Millisecond)
}

// pollReads is n recordings of a leaf the timer writes as its clock's second
// moved by off (value = now.Unix() + off), each read made just before its
// call reached keploy and after the previous call (the timer's last) did.
func pollReads(base time.Time, n int, off time.Duration) []EpochRead {
	reads := make([]EpochRead, n)
	for i := range reads {
		at := pollAt(base, i)
		read := at.Add(-time.Millisecond)
		reads[i] = EpochRead{
			Value: strconv.FormatInt(read.Truncate(time.Second).Add(off).Unix(), 10),
			Span:  Span{From: at.Add(-1950 * time.Millisecond), To: at},
		}
	}
	return reads
}

var epochBase = time.Date(2026, 10, 29, 10, 0, 0, 400*int(time.Millisecond), time.UTC)

// A timer's poll (api-server's ordinal recompute: findAndModify {dirtyAt <=
// now-2, firstDirtyAt <= now-30, claimedAt <= now-60} $set claimedAt=now, in
// untyped epoch seconds) writes four leaves that each track its calls by one
// offset: each is learned as a read of the clock, with that offset, and a
// replay's call made on another day holds when its value tracks the run's
// clock by the same offset, and not when it is an hour off either way.
func TestEpochTrack_APollsEpochLeavesAreReadsOfTheClock(t *testing.T) {
	for _, off := range []time.Duration{0, -2 * time.Second, -30 * time.Second, -60 * time.Second} {
		reads := pollReads(epochBase, 6, off)
		e, ok := EpochTrack(reads)
		if !ok {
			t.Fatalf("offset %v: the poll's leaf is not learned as a read of the clock", off)
		}
		if e.Res != time.Second || e.Lo > off || e.Hi < off || e.Hi-e.Lo > 2*time.Second {
			t.Fatalf("offset %v: learned %+v, want resolution 1s and a bound no wider than the spans holding %v", off, e, off)
		}
		// One recording whose call came just after another (its span a
		// tenth of a second) pins the offset for all of them, however late
		// the first recording's call reached keploy after its read.
		reads[2].Span.From = reads[2].Span.To.Add(-100 * time.Millisecond)
		reads[0].Span.To = reads[0].Span.To.Add(3 * time.Second)
		if e2, ok := EpochTrack(reads); !ok || e2.Lo != off || e2.Hi != off {
			t.Fatalf("offset %v: with a short span learned %+v %v, want exactly %v", off, e2, ok, off)
		}
		live := time.Date(2026, 10, 30, 8, 0, 7, 700*int(time.Millisecond), time.UTC)
		span := Span{From: live.Add(-1500 * time.Millisecond), To: live.Add(time.Millisecond)}
		at := func(d time.Duration) string {
			return strconv.FormatInt(live.Truncate(time.Second).Add(off+d).Unix(), 10)
		}
		if !e.Holds(at(0), span) {
			t.Fatalf("offset %v: a replay's call that tracks the run clock by the offset does not hold", off)
		}
		if e.Holds(at(time.Hour), span) || e.Holds(at(-time.Hour), span) {
			t.Fatalf("offset %v: a value an hour off the run clock holds", off)
		}
	}
}

// The same timer writing milliseconds is learned at a millisecond.
func TestEpochTrack_MillisecondsAreLearnedAtTheirResolution(t *testing.T) {
	reads := make([]EpochRead, 4)
	for i := range reads {
		at := pollAt(epochBase, i)
		reads[i] = EpochRead{Value: strconv.FormatInt(at.Add(-time.Minute).UnixMilli(), 10), Span: Span{From: at.Add(-time.Second), To: at}}
	}
	e, ok := EpochTrack(reads)
	if !ok || e.Res != time.Millisecond || e.Lo > -time.Minute || e.Hi < -time.Minute {
		t.Fatalf("got %+v %v, want a millisecond read of the clock less a minute", e, ok)
	}
}

// A number that only looks like an epoch and never moves (a phone number) is
// no read of the clock, even where every call's span reaches back far enough
// to hold one moment for all of them (a timer's first calls after an idle
// stretch): only values that moved count.
func TestEpochTrack_AConstantIsNoReadOfTheClock(t *testing.T) {
	reads := make([]EpochRead, 5)
	for i := range reads {
		at := pollAt(epochBase, i)
		reads[i] = EpochRead{Value: strconv.FormatInt(epochBase.Unix(), 10), Span: Span{From: epochBase.Add(-time.Hour), To: at}}
	}
	if e, ok := EpochTrack(reads); ok {
		t.Fatalf("a constant is learned as a read of the clock: %+v", e)
	}
	for i := range reads {
		reads[i].Value = "2025551234"
	}
	if e, ok := EpochTrack(reads); ok {
		t.Fatalf("a phone number is learned as a read of the clock: %+v", e)
	}
}

// Values that move but keep no one offset from their calls (a counter, a
// random id in the clock's range) are no read of the clock.
func TestEpochTrack_ValuesThatDoNotTrackTheirCallsAreNoReadOfTheClock(t *testing.T) {
	reads := pollReads(epochBase, 3, 0)
	for i, d := range []int64{0, 500, -700} {
		v, _ := strconv.ParseInt(reads[i].Value, 10, 64)
		reads[i].Value = strconv.FormatInt(v+d, 10)
	}
	if e, ok := EpochTrack(reads); ok {
		t.Fatalf("values off their calls by 0, 500 and -700 s are learned as a read of the clock: %+v", e)
	}
}

// Two recordings are not enough; three are.
func TestEpochTrack_NeedsThreeRecordingsThatMoved(t *testing.T) {
	if e, ok := EpochTrack(pollReads(epochBase, MinEpochReads-1, -30*time.Second)); ok {
		t.Fatalf("%d recordings are learned as a read of the clock: %+v", MinEpochReads-1, e)
	}
	if _, ok := EpochTrack(pollReads(epochBase, MinEpochReads, -30*time.Second)); !ok {
		t.Fatalf("%d recordings that track their calls are not learned", MinEpochReads)
	}
}

// A recording no call before bounds (the set's first call: its span has no
// start) is still checked at its span's end, since the application read its
// clock for it no later than it sent it. It is learned with, never from: it
// narrows only the offset's low end, it is none of the MinEpochReads values,
// and a value the offset would place after its own call is no read of the
// clock.
func TestEpochTrack_ARecordingWithNoStartIsLearnedWithNotFrom(t *testing.T) {
	const off = -2 * time.Second
	reads := pollReads(epochBase, 5, off)
	want, ok := EpochTrack(reads[1:])
	if !ok {
		t.Fatal("the bounded recordings are not learned")
	}
	reads[0].Span.From = time.Time{}
	e, ok := EpochTrack(reads)
	if !ok || e.Hi != want.Hi || e.Lo < want.Lo || e.Lo > off || e.Res != want.Res {
		t.Fatalf("with the set's first call the leaf is %+v (%v); want the others' %+v, its low end no lower and %v held", e, ok, want, off)
	}
	few := pollReads(epochBase, MinEpochReads, off)
	few[0].Span.From = time.Time{}
	if e, ok := EpochTrack(few); ok {
		t.Fatalf("%d bounded recordings and one of no start are learned: %+v", MinEpochReads-1, e)
	}
	after := pollReads(epochBase, 5, off)
	after[0].Span.From = time.Time{}
	after[0].Value = strconv.FormatInt(pollAt(epochBase, 0).Add(5*time.Second).Truncate(time.Second).Add(off).Unix(), 10)
	if e, ok := EpochTrack(after); ok {
		t.Fatalf("a first call whose read the offset places after the call itself is learned: %+v", e)
	}
}

// Every recording must write the leaf at one resolution, as one layout.
func TestEpochTrack_MixedResolutionsAreNoOneRead(t *testing.T) {
	reads := pollReads(epochBase, 4, 0)
	at := pollAt(epochBase, 3)
	reads[3].Value = strconv.FormatInt(at.Add(-time.Millisecond).UnixMilli(), 10)
	if e, ok := EpochTrack(reads); ok {
		t.Fatalf("seconds and milliseconds are learned as one read: %+v", e)
	}
}

// A leaf that is not a bare epoch number whole is not read.
func TestEpochTrack_TextAroundTheNumberIsNoEpoch(t *testing.T) {
	reads := pollReads(epochBase, 4, 0)
	for i := range reads {
		reads[i].Value = "id-" + reads[i].Value
	}
	if e, ok := EpochTrack(reads); ok {
		t.Fatalf("an id holding an epoch is learned: %+v", e)
	}
}

// A live call is checked only on a span it has, at the learned resolution.
func TestEpochOffset_HoldsOnlyOnAKnownSpanAtItsResolution(t *testing.T) {
	e, ok := EpochTrack(pollReads(epochBase, 5, 0))
	if !ok {
		t.Fatal("the poll's stamp is not learned")
	}
	live := time.Date(2026, 10, 30, 8, 0, 7, 700*int(time.Millisecond), time.UTC)
	v := strconv.FormatInt(live.Unix(), 10)
	if e.Holds(v, Span{To: live.Add(time.Millisecond)}) {
		t.Fatal("a live call with no span before it holds")
	}
	if !e.Holds(v, Span{From: live.Add(-time.Second), To: live.Add(time.Millisecond)}) {
		t.Fatal("the live stamp does not hold on its span")
	}
	if e.Holds(strconv.FormatInt(live.UnixMilli(), 10), Span{From: live.Add(-time.Second), To: live.Add(time.Millisecond)}) {
		t.Fatal("a live value in milliseconds holds against a read learned in seconds")
	}
	if e.Holds("id-"+v, Span{From: live.Add(-time.Second), To: live.Add(time.Millisecond)}) {
		t.Fatal("a live value that is no bare epoch holds")
	}
}
