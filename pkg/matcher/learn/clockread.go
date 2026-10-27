package learn

import (
	"context"
	"strings"
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
// holds it: the offset of a KindClockOffset, the plain read of a
// KindClockRead (Read). ok is false for any other leaf: a KindNoise never
// carries a read, so a caller that writes its noise leaves to the noise a
// replay skips never stops comparing a read of the clock.
func (ln Learned) Clock() (models.ClockOffset, bool) {
	switch {
	case ln.Kind == KindClockOffset && ln.Offset != nil:
		return *ln.Offset, true
	case ln.Kind == KindClockRead && ln.Read != nil:
		return *ln.Read, true
	}
	return models.ClockOffset{}, false
}

// clockReadUncheckedWhy is a KindClockRead's Why (read after "<field>
// changed, and "): what a caller whose replay checks no read of the clock
// refuses it with, where keploy learned it as noise before it learned a read
// (the lead moved it, or a window held it). One keploy refused before it
// learned it as a read keeps that refusal's reason instead (classifyInstant).
const clockReadUncheckedWhy = "keploy learns it as a read of the application's clock, which a replay must check is a read made in its call's own span, and the replay of this call's protocol checks no such read: learned as noise it would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// noAppClockWhy is why an instant whole in both runs that neither the lead
// nor a window shows a read of the application's clock is refused, in a
// call with no window, where keploy cannot tell which clock the application
// reads (DriftCache.appClock): it would be a plain read of the clock
// (noWindowRead, classifyRead), but no replay can check one there
// (Tracker.LiveSpan), so it is refused at learn, by one rule wherever the
// recording fell in its second.
const noAppClockWhy = "nothing shows it is a read of the application's clock: keploy cannot tell which clock the application reads in this run (the agent's time hooks report none), so its call has no window on that clock, and no replay can check a read of it there. Learned as noise it would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// outsideWindowWhy is why a value that is one instant whole in both runs is
// refused where its call had a window on the application's clock that does
// not hold it in both runs, and the lead did not show it a read of the clock
// (read after "<field> changed, and "). A call with no window learns it as a
// plain read of the clock instead (Learned.Read), which a replay checks.
const outsideWindowWhy = "it is one instant whole that its call's window on the application's clock does not hold in both runs, a clock read before its call's window or no plain read of the clock, and it did not move by the lead that record's full round runs ahead of the recording: nothing shows it is a read of the application's clock made for its call, which a replay would check in the call's window, and noise there would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// windowHolds reports whether w, the window of the call that carries v, holds a
// read of the application's clock in both runs at one of the instants v
// changed (Window.holdsRead), in UTC or in the application's zone.
func (dc *DriftCache) windowHolds(v *valueDrift, w *Window) bool {
	for _, p := range v.instants {
		if w.holdsRead(p.rec, p.live, dc.zone) {
			return true
		}
	}
	return false
}

// readClockText reads s as one instant whole, by the clock layouts the word
// rule reads (clockLayouts): its instant and the resolution its text writes.
// ok only where one layout's pattern matches all of s and reads it, and the
// layout names a day (a time of day does not: no window holds it).
func readClockText(s string) (time.Time, time.Duration, bool) {
	t, res, _, ok := readClockTextWall(s)
	return t, res, ok
}

// readClockTextWall is readClockText, and whether s is a wall clock of no
// zone (namesZone: a date, or a date and time, its text names no zone of),
// whose instant it reads in UTC.
func readClockTextWall(s string) (time.Time, time.Duration, bool, bool) {
	if s == "" {
		return time.Time{}, 0, false, false
	}
	for k, lay := range clockLayouts {
		if lay.wraps {
			continue
		}
		loc := lay.re.FindStringIndex(s)
		if loc == nil || loc[0] != 0 || loc[1] != len(s) {
			continue
		}
		t, res, ok := lay.read(s)
		if !ok || !inClockRange(t) {
			return time.Time{}, 0, false, false
		}
		return t, res, !namesZone(k, strings.ToUpper(s)), true
	}
	return time.Time{}, 0, false, false
}

// wallZone is the zone a replay reads a wall clock of no zone in
// (models.ClockOffset.Zone) that the learn step learns as a plain read of
// the application's clock, rt and lt its readings in UTC as recorded and as
// record's full round saw it, each at the resolution its encoding writes
// (rres, lres), in the call recorded at call whose window is w (nil: none):
// models.ZoneUTC where the learn step reads such a clock in UTC alone (the
// cache's zone is nil). Otherwise the zone the recording tells at the
// value's resolution: of a call with a window, the zone in which w holds
// the reading in both runs; with none, the zone whose reading, a unit of
// the value long, holds the call (a date: the day of the call in that
// zone). Where both do (the call falls in one day of a date in UTC and in
// the application's zone), or the two readings are one instant (the
// application's zone is UTC there), the recording cannot tell the two
// apart: models.ZoneEither, which a replay passes read in either, so its
// verdict on the application's own value is one at every hour of the day,
// whichever of the two zones the application writes. Where neither does (a
// read no window holds that the lead showed, or one made before its call),
// the zone of its recorded reading nearest the call (nearestZone). The
// replay checks the value in that zone alone (clockReadIn), so a value read
// in another fails there, named.
func (dc *DriftCache) wallZone(rt, lt time.Time, rres, lres time.Duration, w *Window, call time.Time) string {
	if dc.zone == nil {
		return models.ZoneUTC
	}
	rz, lz := wallIn(rt.UnixNano(), dc.zone), wallIn(lt.UnixNano(), dc.zone)
	if rz.Equal(rt) && lz.Equal(lt) {
		return models.ZoneEither
	}
	in := func(r, l time.Time) bool {
		if w != nil {
			return w.Rec.holds(r, rres) && w.F.holds(l, lres)
		}
		return !call.IsZero() && !r.After(call) && call.Before(r.Add(rres))
	}
	switch utc, app := in(rt, lt), in(rz, lz); {
	case utc && app:
		return models.ZoneEither
	case utc:
		return models.ZoneUTC
	case app:
		return models.ZoneApp
	}
	return nearestZone(rt, call, dc.zone)
}

// nearestZone is the zone, UTC (models.ZoneUTC) or zone (models.ZoneApp), in
// which rt, a wall clock of no zone as recorded read in UTC, is the reading
// nearest the call recorded at call: of two readings no later than call the
// later, and of one, that one; of none, the earlier. ZoneUTC where the two
// readings are one instant (zone is UTC there) or call is not known (zero),
// as the learn step read such a value before it carried a zone.
func nearestZone(rt, call time.Time, zone *time.Location) string {
	if call.IsZero() {
		return models.ZoneUTC
	}
	z := wallIn(rt.UnixNano(), zone)
	uOK, zOK := !rt.After(call), !z.After(call)
	switch {
	case uOK && zOK:
		if z.After(rt) {
			return models.ZoneApp
		}
		return models.ZoneUTC
	case uOK:
		return models.ZoneUTC
	case zOK:
		return models.ZoneApp
	}
	if z.Before(rt) {
		return models.ZoneApp
	}
	return models.ZoneUTC
}

// textReadZone is the Zone of textClockRead's read of a text recorded as rec
// and seen in record's full round as live, in the call recorded at call
// whose window is w (nil: none): of a wall clock of no zone in both
// (readClockTextWall), the zone wallZone reads it in; "" where either names
// its zone, read as it names it.
func (dc *DriftCache) textReadZone(rec, live string, w *Window, call time.Time) string {
	rt, rres, rwall, okR := readClockTextWall(rec)
	lt, lres, lwall, okL := readClockTextWall(live)
	if !okR || !okL || !rwall || !lwall {
		return ""
	}
	return dc.wallZone(rt, lt, rres, lres, w, call)
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
// read wrote there: an instant its protocol types that names its moment or
// is a wall clock of no zone (WallClockLeaf), or a text or a number that is
// one instant whole (readClockText), with the resolution its form writes,
// and whether it is a wall clock of no zone (wall), whose instant it reads in
// UTC. typed says its protocol typed it. ok is false for any other value: a
// time of day too, which names no day.
func clockReadOf(l Leaf) (at time.Time, res time.Duration, wall, typed, ok bool) {
	if l.Type == Instant {
		ti, ok := readTyped(l)
		if !ok || ti.names == namesTimeOfDay {
			return time.Time{}, 0, false, true, false
		}
		return ti.at, ti.res, ti.names == namesWall, true, true
	}
	at, res, wall, ok = readClockTextWall(l.Value)
	return at, res, wall, false, ok
}

// clockReadIn reads live, the leaf a replay's call carries at a place the
// learn step learned as o, as the instants of the clock read o's Zone says
// it is (models.ClockOffset.Zone), in zone, the application's
// (Span.Zone): a wall clock of no zone in UTC (models.ZoneUTC), as the
// wall clock of zone (models.ZoneApp, wallIn), or both, each a reading the
// replay passes where it holds (models.ZoneEither); of a Zone of "", as the
// value names its moment, or a text of no zone in UTC, as a replay read it
// before Zone was learned (a typed wall clock is none then). Otherwise why
// it reads none, after "<path> was learned as <o>, <read>": the value is no
// instant keploy reads, it names its zone where the learn step read a wall
// clock of no zone, or o reads in the application's zone (alone or as one
// of the two) and the replay cannot tell it (zone nil).
func clockReadIn(o models.ClockOffset, live Leaf, zone *time.Location) ([]time.Time, time.Duration, string) {
	at, res, wall, typed, ok := clockReadOf(live)
	if !ok || o.Zone == "" && wall && typed {
		return nil, 0, ", and the value this call carries is no instant keploy reads: it changed"
	}
	switch o.Zone {
	case models.ZoneUTC, models.ZoneApp, models.ZoneEither:
		if !wall {
			return nil, 0, ", and the value this call carries names its zone, where the learn step read a wall clock of no zone: it changed"
		}
		if o.Zone == models.ZoneUTC {
			break
		}
		if zone == nil {
			return nil, 0, ", which a replay reads in the application's zone, but keploy cannot tell the application's zone at this call, so it refuses the value rather than read it in another zone"
		}
		if o.Zone == models.ZoneEither {
			return []time.Time{at, wallIn(at.UnixNano(), zone)}, res, ""
		}
		at = wallIn(at.UnixNano(), zone)
	}
	return []time.Time{at}, res, ""
}

// holdsAny reports whether o holds one of ats, the readings clockReadIn
// gives a replay's value, at resolution res in the span from from to to
// (models.ClockOffset.Holds).
func holdsAny(o models.ClockOffset, ats []time.Time, res time.Duration, from, to time.Time) bool {
	for _, at := range ats {
		if o.Holds(at, res, from, to) {
			return true
		}
	}
	return false
}

// ClockReadHolds reports whether live, the leaf a replay's call carries at a
// place the learn step learned a read of the application's clock at (o, as
// models.MockSpec.Clocks holds it), is such a read: an instant made in s,
// the call's span on the application's clock (ClockSpanFrom), plus o's
// offset, read at the resolution its own form writes and in the zone o's
// Zone says, or in either of the two it names (clockReadIn;
// models.ClockOffset.Holds; of a plain read, a read in s itself). False for
// a live value that reads as no instant (another type, a text that is not
// one instant whole), one clockReadIn reads in no zone, and for every value
// where s is no span (its From is zero): it fails closed.
func ClockReadHolds(o models.ClockOffset, live Leaf, s Span) bool {
	ats, res, why := clockReadIn(o, live, s.Zone)
	if why != "" {
		return false
	}
	return holdsAny(o, ats, res, s.From, s.To)
}

// CheckClockRead is a replay's check of live, the leaf its call carries at
// path, a place the learn step learned as o (models.MockSpec.Clocks), in s,
// the call's span on the application's clock, which known says the replay
// could tell (ClockSpanFrom): ok where live is a read of the clock made in s
// plus o's offset (ClockReadHolds). Otherwise why it refuses the call, which
// names path and o and says what failed: keploy could not tell the
// application's clock at the call; the value is no instant keploy reads in
// o's zone (clockReadIn: one that names its zone where the learn step read a
// wall clock of no zone, or one of the application's zone, which s does not
// carry); it is cut coarser than the learn step read it (o.Res a cut coarser than the
// value's encoding, models.ClockOffset.CutCoarserThan), so it would pass or
// fail by where its read fell in the cut; or it is no such read: it changed.
// An o two learnings of path met as Empty (models.ClockOffset.Meet: they
// share no offset, or read it in different zones) is refused whatever the
// call carries, saying so, and so is one of a Zone this keploy does not
// read (models.ClockOffset.ZoneKnown). It never
// passes a value it could not check.
func CheckClockRead(path string, o models.ClockOffset, live Leaf, s Span, known bool) (bool, string) {
	if o.Empty() {
		return false, path + " was learned at more than one call as " + o.String() + ": no one read of the application's clock explains them all, so keploy refuses the value whatever it is. Record the test again; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	read := "a read of the application's clock"
	if !o.Plain() {
		read += " plus that offset"
	}
	learned := path + " was learned as " + o.String() + ", " + read
	if !o.ZoneKnown() {
		return false, learned + ": keploy refuses the value whatever it is rather than read it in another zone. Record the test again with this keploy"
	}
	if !known {
		return false, learned + ", which a replay checks against its call's window on the application's clock, but keploy cannot tell the application's clock at this call (no time hooks report it, or they could not tell it here), so it refuses the value rather than pass it unchecked"
	}
	ats, res, why := clockReadIn(o, live, s.Zone)
	if why != "" {
		return false, learned + why
	}
	if o.CutCoarserThan(res) {
		return false, learned + ", at a resolution of " + models.ShortDuration(o.Res) + " (both of the learn step's values were whole multiples of it), coarser than the " + models.ShortDuration(res) + " this value's encoding writes: the application cuts its value, and a check at its encoding's resolution would pass or fail by where its read fell in the cut, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	if holdsAny(o, ats, res, s.From, s.To) {
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
