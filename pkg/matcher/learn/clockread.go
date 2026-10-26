package learn

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A value the learn step learns as a read of the application's clock (a
// plain read, Learned.Read, or one plus a constant offset, KindClockOffset)
// is still compared at a replay: a caller writes it to models.MockSpec.Clocks
// (Learned.Clock), and its replay checks the value its call carries there is
// such a read in the call's own span on the application's clock
// (ClockSpanFrom, CheckClockRead), refusing it otherwise with a reason.
// A replay that cannot tell the application's clock at the call refuses it
// too: a value checked against no clock would pass whatever it is.

// Clock is what a replay checks of ln's leaf, as models.MockSpec.Clocks
// holds it: the offset of a KindClockOffset, the plain read of a noise leaf
// that is a read of the clock (Read). ok is false for any other leaf.
func (ln Learned) Clock() (models.ClockOffset, bool) {
	switch {
	case ln.Kind == KindClockOffset && ln.Offset != nil:
		return *ln.Offset, true
	case ln.Kind == KindNoise && ln.Read != nil:
		return *ln.Read, true
	}
	return models.ClockOffset{}, false
}

// readClockText reads s as one instant whole, by the clock layouts the word
// rule reads (clockLayouts): its instant and the resolution its text writes.
// ok only where one layout's pattern matches all of s and reads it, and the
// layout names a day (a time of day does not: no window holds it).
func readClockText(s string) (time.Time, time.Duration, bool) {
	if s == "" {
		return time.Time{}, 0, false
	}
	for _, lay := range clockLayouts {
		if lay.wraps {
			continue
		}
		loc := lay.re.FindStringIndex(s)
		if loc == nil || loc[0] != 0 || loc[1] != len(s) {
			continue
		}
		t, res, ok := lay.read(s)
		if !ok || !inClockRange(t) {
			return time.Time{}, 0, false
		}
		return t, res, true
	}
	return time.Time{}, 0, false
}

// textClockRead is Learned.Read of a text or number leaf recorded as rec
// and seen in record's full round as live, whose every changed word the
// learn step read as a read of the clock: a plain read at the coarsest of
// the two values' resolutions and the granularity they share, where each is
// one instant whole (readClockText); nil where either is not.
func textClockRead(rec, live string) *models.ClockOffset {
	rt, rres, ok := readClockText(rec)
	if !ok {
		return nil
	}
	lt, lres, ok := readClockText(live)
	if !ok {
		return nil
	}
	return &models.ClockOffset{Res: max(rres, lres, sharedGrain(rt, lt))}
}

// clockReadOf reads l, a leaf of a replay's call, as the instant a clock
// read wrote there: an instant its protocol types that names its moment, or
// a text or a number that is one instant whole (readClockText), with the
// resolution its form writes. ok is false for any other value.
func clockReadOf(l Leaf) (time.Time, time.Duration, bool) {
	if l.Type == Instant {
		ti, ok := readTyped(l)
		if !ok || ti.names != namesMoment {
			return time.Time{}, 0, false
		}
		return ti.at, ti.res, true
	}
	return readClockText(l.Value)
}

// ClockReadHolds reports whether live, the leaf a replay's call carries at a
// place the learn step learned a read of the application's clock at (o, as
// models.MockSpec.Clocks holds it), is such a read: an instant made in s,
// the call's span on the application's clock (ClockSpanFrom), plus o's
// offset, read at the resolution its own form writes
// (models.ClockOffset.Holds; of a plain read, a read in s itself). False for
// a live value that reads as no instant (another type, a text that is not
// one instant whole), and for every value where s is no span (its From is
// zero): it fails closed.
func ClockReadHolds(o models.ClockOffset, live Leaf, s Span) bool {
	at, res, ok := clockReadOf(live)
	if !ok {
		return false
	}
	return o.Holds(at, res, s.From, s.To)
}

// CheckClockRead is a replay's check of live, the leaf its call carries at
// path, a place the learn step learned as o (models.MockSpec.Clocks), in s,
// the call's span on the application's clock, which known says the replay
// could tell (ClockSpanFrom): ok where live is a read of the clock made in s
// plus o's offset (ClockReadHolds). Otherwise why it refuses the call, which
// names path and o and says what failed: keploy could not tell the
// application's clock at the call; the value is no instant keploy reads; it
// is cut coarser than the learn step read it (o.Res a cut coarser than the
// value's encoding, models.ClockOffset.CutCoarserThan), so it would pass or
// fail by where its read fell in the cut; or it is no such read: it changed.
// It never passes a value it could not check.
func CheckClockRead(path string, o models.ClockOffset, live Leaf, s Span, known bool) (bool, string) {
	read := "a read of the application's clock"
	if !o.Plain() {
		read += " plus that offset"
	}
	learned := path + " was learned as " + o.String() + ", " + read
	if !known {
		return false, learned + ", which a replay checks against its call's window on the application's clock, but keploy cannot tell the application's clock at this call (no time hooks report it, or they could not tell it here), so it refuses the value rather than pass it unchecked"
	}
	at, res, ok := clockReadOf(live)
	if !ok {
		return false, learned + ", and the value this call carries is no instant keploy reads: it changed"
	}
	if o.CutCoarserThan(res) {
		return false, learned + ", at a resolution of " + models.ShortDuration(o.Res) + " (both of the learn step's values were whole multiples of it), coarser than the " + models.ShortDuration(res) + " this value's encoding writes: the application cuts its value, and a check at its encoding's resolution would pass or fail by where its read fell in the cut, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	if o.Holds(at, res, s.From, s.To) {
		return true, ""
	}
	return false, learned + ", and the value this call carries is not " + read + " made in this call's window: it changed"
}

// ClockSpanFrom is the span on the application's clock in which the call
// that reached keploy at the real instant arrival made its clock reads in
// this run (Tracker.LiveSpan), by the tracker on ctx (WithTracker). It reads
// the tracker whether or not the set's learn step has ended (TrackerFrom
// gives none then): a replay checks every learned clock read, in every cycle.
// ok is false where ctx holds no tracker, or the tracker cannot tell the
// span.
func ClockSpanFrom(ctx context.Context, arrival time.Time) (Span, bool) {
	if ctx == nil {
		return Span{}, false
	}
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	return t.LiveSpan(arrival)
}
