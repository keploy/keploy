package learn

import (
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// AN INSTANT ITS PROTOCOL TYPES.
//
// A field its protocol's decoder types as one instant (a BSON UTC datetime,
// type 0x09, or timestamp, 0x11; a typed timestamp column) is one value on
// the wire: the words of its date and time exist only in keploy's text of
// it. The decoder hands it to the learn step as an Instant leaf
// (InstantLeaf), and ClassifyIn compares it as one instant
// (classifyInstant), never word by word: a minute of it is no word an
// unrelated recording can hold (a Mongo reply's requestId of 40, a version
// in an SDK's User-Agent, SQS's API version 2012-11-05). Only a decoder's
// type makes a leaf an instant. A string that reads as a date stays a
// String, read word by word: no text's shape decides anything. Where the
// word-by-word reading meets an instant leaf (an earlier exchange's, the
// call's own answer's), it reads its moment, never its words (appearedIn,
// unaccounted, Echoes, echoedPart).
//
// Read whole, a drifted instant gets a value's checks, each of the instant:
//
//   - a variable where a leaf of an exchange F served, or a part of the
//     test case's own request keploy test sent with a template value,
//     drifted with it: one that was, as recorded, the instant it was
//     recorded with (in any layout and form), or that held the moment it was
//     recorded with and the moment F saw, both ways (rule 1), whatever its
//     provenance;
//   - the echo, before any provenance excuses it (instants.go): where an
//     earlier place holds the moment F saw (an instant of it in any layout,
//     cut to the instant's own precision, or its time of day), by the rule
//     an earlier recording holds the moment it was recorded with, or alone,
//     it may be a pick from what the application was given, by its clock; it
//     is a variable of the place that holds the moment it was recorded with,
//     or, held nowhere, refused. An instant its protocol types is never read
//     as a value built on a coarser instant (a date with a time of its own):
//     matched at its own precision only;
//   - a variable where a leaf of an exchange F served leaf by leaf, as
//     recorded, or a payload served or recorded earlier holds the moment it
//     was recorded with, exactly, in any layout, an answer's own clock stamp
//     and a protocol-phase answer's own clock reads excepted (rules 2 and 3):
//     an instant equal to an earlier one is followed, whatever its
//     provenance (instantLeafFrom, instantRawFrom);
//   - refused where it is built on a moved place by an offset (offset.go):
//     where F gave the application, at a place of what it was given, another
//     instant than the recording did, moved as the value moved, at the
//     value's own resolution (a message's sent time and five minutes, F
//     delivering another recording of the receive);
//   - noise only where its provenance shows a read of the application's
//     clock: it lies within its call's window in both runs, or it moved by
//     FullRoundLead as a read made no later than its call moves
//     (readNoLaterThan: a minute, an hour or a day the lead takes a move of a
//     unit or two for a read, so the call bounds it). An echo of it in the
//     call's own recorded answer is then answered there (KindEcho);
//   - refused otherwise (KindUnaccounted), saying why: a false red that
//     fails closed, never noise. Read whole, an instant derived from what the
//     application was given shares no word with its source: the offset rule
//     above, not a word, is what finds it.

// InstantLeaf is the leaf of a field its protocol's decoder types as one
// instant: at in UTC, truncated to res (the decoder's resolution: a BSON
// datetime's millisecond, a BSON timestamp's second, a date column's day),
// written in the form of the coarsest of instantForms that res is no finer
// than: RFC 3339 with the fraction digits res needs (none, 3, 6 or 9), its
// time to the minute, or its date (ISO 8601). Its type is Instant whatever
// its year: of a year before 0 or after 9999 the form writes a text of
// another length (a sign, a fifth digit), which InstantOf does not read, and
// the learn step refuses it. A date names no zone (a date column's day is
// the application's): its form is a wall clock's (WallClockLeaf).
func InstantLeaf(path, noise string, at time.Time, res time.Duration) Leaf {
	f := formOf(instantForms, res)
	t := at.UTC()
	if res > 0 {
		t = t.Truncate(res)
	}
	return Leaf{Path: path, Noise: noise, Type: Instant, Value: t.Format(f.layout)}
}

// WallClockLeaf is the leaf of a field its protocol's decoder types as a wall
// clock that names no zone (a Postgres timestamp, or date): wall's date and
// time of day as wall writes them in its own location, truncated to res,
// written in the form of the coarsest of wallForms that res is no finer
// than: InstantLeaf's with no zone. It is an Instant leaf, compared whole,
// whose moment is its reading in UTC (InstantOf); where the learn step looks
// for a read of the application's clock it reads it in UTC and in the zone
// the application writes such a clock in (Tracker.SetAppZone), as it reads a
// text's wall clock of no zone (readNoLaterThan): an application east of UTC
// binds its own clock so.
func WallClockLeaf(path, noise string, wall time.Time, res time.Duration) Leaf {
	f := formOf(wallForms, res)
	t := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), time.UTC)
	if res > 0 {
		t = t.Truncate(res)
	}
	return Leaf{Path: path, Noise: noise, Type: Instant, Value: t.Format(f.layout)}
}

// TimeOfDayLeaf is the leaf of a field its protocol's decoder types as a
// time of day (a Postgres time, or timetz moved to UTC by its zone): tod
// into its day, from 0 to 24 hours (Postgres reads 24:00:00), truncated to
// res, written as ISO 8601 writes a time of day, after its designator T:
// "T15:04", "T15:04:05" or with the fraction digits res needs (3, 6 or 9),
// its hour 24 only at the day's end. The T keeps it apart from a wire text
// a parser keeps as an Instant leaf it reads no instant in (a Postgres
// timetz of no zone, "15:04:05"), which InstantOf does not read. It is an Instant leaf,
// compared whole; a time of day names no day, so no call's window holds it
// and no call bounds it: the learn step reads it as a read of the clock only
// where it moved by FullRoundLead, read modulo a day, as it reads a text's
// time of day (moved), and learns it so (todRead), which a replay checks
// modulo a day in its call's span (clockReadIn). Outside 0 to 24 hours it is written as it is, a text
// InstantOf does not read, and the learn step refuses it where it changed.
func TimeOfDayLeaf(path, noise string, tod, res time.Duration) Leaf {
	if res > 0 {
		tod = tod.Truncate(res)
	}
	return Leaf{Path: path, Noise: noise, Type: Instant, Value: timeOfDayText(tod, res)}
}

// timeOfDayText writes tod, a time into its day, in the form of the
// coarsest of timeOfDayForms that res is no finer than.
func timeOfDayText(tod, res time.Duration) string {
	if tod < 0 || tod > 24*time.Hour {
		return tod.String()
	}
	f := formOf(timeOfDayForms, res)
	h, m := tod/time.Hour, tod%time.Hour/time.Minute
	sec, ns := tod%time.Minute/time.Second, int64(tod%time.Second)
	out := fmt.Sprintf("T%02d:%02d", h, m)
	if f.res >= time.Minute {
		return out
	}
	out += fmt.Sprintf(":%02d", sec)
	if digits := len(f.layout) - len("T15:04:05."); digits > 0 {
		out += "." + fmt.Sprintf("%09d", ns)[:digits]
	}
	return out
}

// instantNames is what an Instant leaf's form names: a moment (InstantLeaf),
// a wall clock of no zone (WallClockLeaf, and a date), or a time of day
// (TimeOfDayLeaf), which names no day.
type instantNames byte

const (
	namesMoment instantNames = iota
	namesWall
	namesTimeOfDay
)

// typedInstant is an Instant leaf as InstantOf reads it: its instant, the
// resolution its form writes, and what that form names.
type typedInstant struct {
	at    time.Time
	res   time.Duration
	names instantNames
}

// InstantOf reads l, a leaf InstantLeaf, WallClockLeaf or TimeOfDayLeaf
// wrote: its instant (a wall clock's reading in UTC; a time of day's on day
// one of year 0), and the resolution its form writes (instantForms). ok is
// false for a leaf of any other type, a String that reads as a date among
// them (only a decoder types a leaf an instant), and for a value no form
// writes.
func InstantOf(l Leaf) (time.Time, time.Duration, bool) {
	ti, ok := readTyped(l)
	return ti.at, ti.res, ok
}

// readTyped is InstantOf, with what the leaf's form names.
func readTyped(l Leaf) (typedInstant, bool) {
	if l.Type != Instant {
		return typedInstant{}, false
	}
	// A time of day's form begins with its designator, T, as no date does.
	if strings.HasPrefix(l.Value, "T") {
		for _, f := range timeOfDayForms {
			if len(f.layout) != len(l.Value) {
				continue
			}
			tod, ok := readTimeOfDayText(l.Value)
			if !ok {
				return typedInstant{}, false
			}
			return typedInstant{at: timeOfDayDay.Add(tod), res: f.res, names: namesTimeOfDay}, true
		}
		return typedInstant{}, false
	}
	// Each form of a date writes its own length (a year of four digits), and
	// Go's parser would take a fraction its layout does not write: the
	// length picks the form.
	for _, forms := range [][]instantForm{instantForms, wallForms} {
		for _, f := range forms {
			if len(f.layout) != len(l.Value) {
				continue
			}
			t, err := time.ParseInLocation(f.layout, l.Value, time.UTC)
			if err != nil {
				return typedInstant{}, false
			}
			return typedInstant{at: t, res: f.res, names: f.names}, true
		}
	}
	return typedInstant{}, false
}

// timeOfDayDay is the day a time of day's instant is on (InstantOf): year
// 0, which no clock read of a recording falls in (inClockRange).
var timeOfDayDay = time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC)

// readTimeOfDayText reads s, a time of day in a form of timeOfDayForms: its
// hour 0 to 24, 24 only with every other digit 0.
func readTimeOfDayText(s string) (time.Duration, bool) {
	num := func(i, n int) (int64, bool) {
		var v int64
		for _, c := range s[i : i+n] {
			if c < '0' || c > '9' {
				return 0, false
			}
			v = v*10 + int64(c-'0')
		}
		return v, true
	}
	h, okH := num(1, 2)
	m, okM := num(4, 2)
	if !okH || !okM || s[3] != ':' || m > 59 {
		return 0, false
	}
	tod := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	if len(s) > len("T15:04") {
		sec, ok := num(7, 2)
		if !ok || s[6] != ':' || sec > 59 {
			return 0, false
		}
		tod += time.Duration(sec) * time.Second
	}
	if len(s) > len("T15:04:05") {
		digits := len(s) - len("T15:04:05.")
		frac, ok := num(10, digits)
		if !ok || s[9] != '.' {
			return 0, false
		}
		for i := digits; i < 9; i++ {
			frac *= 10
		}
		tod += time.Duration(frac)
	}
	if tod > 24*time.Hour {
		return 0, false
	}
	return tod, true
}

// InstantClockRead reports whether rec and live, an Instant leaf as
// recorded and as record's full round saw it, are a read of each run's
// clock by their provenance (instantClock), the lead's or w's, a wall clock
// of no zone in UTC alone. false for a leaf InstantOf does not read. An
// adapter that reads a test's DriftCache asks its InstantClockRead, which
// keploy mock's client tests read by w alone and which reads a wall clock of
// no zone in the application's zone too.
func InstantClockRead(rec, live Leaf, w *Window) bool {
	return instantClockRead(rec, live, w, true, nil)
}

// instantClockRead is InstantClockRead, by the lead too where lead is set,
// of a call whose recorded instant is not known, a wall clock of no zone in
// UTC and in zone (nil: UTC alone).
func instantClockRead(rec, live Leaf, w *Window, lead bool, zone *time.Location) bool {
	r, ok := readTyped(rec)
	if !ok {
		return false
	}
	l, ok := readTyped(live)
	if !ok {
		return false
	}
	return instantClock(r, l, w, lead, time.Time{}, zone)
}

// instantClock reports whether r and l, an instant as recorded and as F saw
// it, each at its own resolution, are a read of each run's clock, as clockAt
// reads an instant of a text: l is r moved by FullRoundLead (leadMoved) as a
// read made no later than its call, recorded at call, moves
// (readNoLaterThan; zero: not known), or each lies within its run's span of
// its call's window w (nil: none); without lead (keploy mock's client tests:
// DriftCache.ClientTests), within the window only. An instant before 2000 or
// after 2100 is no read of a recording's clock. One that names its moment
// (InstantLeaf) is read in UTC alone; a wall clock of no zone
// (WallClockLeaf) in UTC and in zone, the application's (nil: UTC alone),
// both runs in the same one, as clockAt and Window.holdsRead read a text's,
// and no other zone (readNoLaterThan says why). A time of day
// (TimeOfDayLeaf) names no day: no window holds it and no call bounds it, so
// it is a read only where it moved by the lead, read modulo a day, as moved
// reads a text's. r and l of different forms are none.
func instantClock(r, l typedInstant, w *Window, lead bool, call time.Time, zone *time.Location) bool {
	if r.names != l.names {
		return false
	}
	if r.names == namesTimeOfDay {
		return lead && leadMoved(r.at, l.at, r.res, l.res, true)
	}
	if !inClockRange(r.at) || !inClockRange(l.at) {
		return false
	}
	wall := r.names == namesWall
	in := func(rt, lt time.Time) bool { return w != nil && w.Rec.holds(rt, r.res) && w.F.holds(lt, l.res) }
	if in(r.at, l.at) || wall && zone != nil && in(wallIn(r.at.UnixNano(), zone), wallIn(l.at.UnixNano(), zone)) {
		return true
	}
	return lead && leadMoved(r.at, l.at, r.res, l.res, false) && readNoLaterThan(r.at, l.at, r.res, call, !wall, zone)
}

// afterCall reports whether r, an instant as recorded, is later than call
// in every reading instantClock gives it: a wall clock of no zone in UTC and
// in zone too (nil: UTC alone).
func (r typedInstant) afterCall(call time.Time, zone *time.Location) bool {
	if call.IsZero() || !r.at.After(call) {
		return false
	}
	return r.names != namesWall || zone == nil || wallIn(r.at.UnixNano(), zone).After(call)
}

// instantForm is one form an Instant leaf is written in: its layout, the
// resolution it writes, and what it names.
type instantForm struct {
	layout string
	res    time.Duration
	names  instantNames
}

// instantForms are the forms InstantLeaf writes an instant in, coarsest
// first, each with the resolution it writes: its date, its time to the
// minute, and RFC 3339 to the second, the millisecond, the microsecond and
// the nanosecond, in UTC. A date names no zone, as a wall clock's does.
var instantForms = []instantForm{
	{"2006-01-02", 24 * time.Hour, namesWall},
	{"2006-01-02T15:04Z", time.Minute, namesMoment},
	{"2006-01-02T15:04:05Z", time.Second, namesMoment},
	{"2006-01-02T15:04:05.000Z", time.Millisecond, namesMoment},
	{"2006-01-02T15:04:05.000000Z", time.Microsecond, namesMoment},
	{"2006-01-02T15:04:05.000000000Z", time.Nanosecond, namesMoment},
}

// wallForms are the forms WallClockLeaf writes a wall clock of no zone in:
// instantForms' with no zone, each a length none of those has but the date.
var wallForms = []instantForm{
	{"2006-01-02", 24 * time.Hour, namesWall},
	{"2006-01-02T15:04", time.Minute, namesWall},
	{"2006-01-02T15:04:05", time.Second, namesWall},
	{"2006-01-02T15:04:05.000", time.Millisecond, namesWall},
	{"2006-01-02T15:04:05.000000", time.Microsecond, namesWall},
	{"2006-01-02T15:04:05.000000000", time.Nanosecond, namesWall},
}

// timeOfDayForms are the forms TimeOfDayLeaf writes a time of day in
// (timeOfDayText), after ISO 8601's designator T, which no form of a date
// begins with.
var timeOfDayForms = []instantForm{
	{"T15:04", time.Minute, namesTimeOfDay},
	{"T15:04:05", time.Second, namesTimeOfDay},
	{"T15:04:05.000", time.Millisecond, namesTimeOfDay},
	{"T15:04:05.000000", time.Microsecond, namesTimeOfDay},
	{"T15:04:05.000000000", time.Nanosecond, namesTimeOfDay},
}

// formOf is the coarsest of forms that writes an instant of resolution res
// (the finest for none).
func formOf(forms []instantForm, res time.Duration) instantForm {
	for _, f := range forms {
		if res >= f.res {
			return f
		}
	}
	return forms[len(forms)-1]
}

// instantMoment is t, of resolution res, as the places' moments are compared
// (heldMoments): the instant, in UTC (an instant its protocol types names
// its moment, as an epoch does), the time of day it writes there, and its
// resolution, the precision a place's instant is cut to where it is matched
// against it (covers). It is typed: a place holds it only as an instant cut
// to its own precision, never as a coarser instant it is finer than (the
// founder's rule for an instant its protocol types: equal to an earlier
// instant at its own precision).
func instantMoment(t time.Time, res time.Duration) moment {
	t = t.UTC()
	return moment{key: instantKey{at: t.UnixNano()}, wall: clockOfDay(t), res: res, zoned: true, typed: true}
}

// moment is ti's moment at resolution res, typed (instantMoment). One that
// names its moment (InstantLeaf) is zoned, read in UTC alone. A wall clock
// of no zone (WallClockLeaf, and a date) is its reading in UTC and names no
// zone, as a text's wall clock of no zone is (instantKeyOf): the echo reads
// it in each zone a place's instant is in (echoedPairs, cuts), as the clock
// rule reads it in the application's (instantClock), so a pick from a
// served timetable the application writes in its own zone is followed or
// refused, never a read of the clock. A time of day's is its time into its
// day, modulo a day (24:00 is the next day's 00:00), which names no day and
// no zone, as a text's time of day is, not an instant on year 0, which no
// int64 of nanoseconds since 1970 holds.
func (ti typedInstant) moment(res time.Duration) moment {
	switch ti.names {
	case namesTimeOfDay:
		tod := int64(ti.at.Sub(timeOfDayDay)) % nsPerDay
		return moment{key: instantKey{at: tod, day: true}, wall: tod, res: res, typed: true}
	case namesWall:
		m := instantMoment(ti.at, res)
		m.zoned = false
		return m
	}
	return instantMoment(ti.at, res)
}

// The reasons a typed instant is refused for (Learned.Why), read after
// "<field> changed, and ".
const (
	// instantAfterCall is why an instant its protocol types that moved by
	// the lead is no read of the clock where its recorded moment is later
	// than its call (readNoLaterThan): a time to live, a due date.
	instantAfterCall = "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, and it moved by the lead that record's full round runs ahead of the recording, but the moment it was recorded with is later than its call, and a read of the clock a call carries is made no later than the call: it is built on the clock or another instant by an offset (a time to live, a due date), and its call's windows show no one offset from the clock in both runs. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
	// instantPastCall is why one that moved by about the lead is none where
	// the moment record's full round saw is later than a read no later than
	// its call moved by the lead (readNoLaterThan).
	instantPastCall = "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, and it moved by about the lead that record's full round runs ahead of the recording, but the moment record's full round saw is later than a read of the clock made no later than its call moves by the lead, so keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
	instantNotClock = "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, and it did not move by the lead that record's full round runs ahead of the recording, nor does a window of its call show a read. Read whole, an instant derived from a value the application was given shares no word with that value, so keploy learns an instant as noise only where it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
)

// timeOfDayClientTestWhy is why a time of day (TimeOfDayLeaf) that changed
// is refused in keploy mock's client tests: no window holds a time of day,
// and the lead, the only rule that reads one, is not theirs.
const timeOfDayClientTestWhy = "nothing shows it is a read of the application's clock: its protocol types it as a time of day, which keploy compares whole, and keploy mock's test harness gives the application its inputs, which keploy does not see, so keploy learns a changed instant as noise only where it lies within its call's window on the application's clock, and a time of day names no day, so no window holds it. If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it; if the application reads it from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// what is what an Instant leaf of form n names, as a reason writes it.
func (n instantNames) what() string {
	switch n {
	case namesWall:
		return "a wall clock that names no zone"
	case namesTimeOfDay:
		return "a time of day"
	}
	return "an instant that names its moment"
}

// instantFormChangedWhy is why an instant its protocol types is refused
// where its recorded form names r and the form record's full round saw names
// l, another: a read of the application's clock both runs make is written
// in one form, so the change is no read of it, whatever its instants.
func instantFormChangedWhy(r, l instantNames) string {
	return "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, but it was recorded as " + r.what() + " and record's full round saw " + l.what() + ", and a read of the clock both runs make is written in one form, so keploy cannot read the change as one. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// instantUnread is why a drifted leaf its protocol types as one instant
// (r as recorded, l as F saw it) that InstantOf cannot read on a side is
// refused: what keploy found there, and how to correct it.
func instantUnread(r, l Leaf) string {
	var found string
	switch {
	case r.Type != Instant:
		found = "its protocol types it as one instant as record's full round saw it, but not as it was recorded"
	case l.Type != Instant:
		found = "its protocol types it as one instant as it was recorded, but not as record's full round saw it"
	default:
		found = "its protocol types it as one instant, and keploy cannot read that instant as it was recorded or as record's full round saw it (a year before 0 or after 9999, or a text no decoder wrote as one)"
	}
	return found + ", so keploy cannot compare it whole or tell a read of the clock from a value that flowed. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// classifyInstant is ClassifyIn's reading of a drifted leaf its protocol
// types as one instant (r as recorded, l as F saw it: Instant leaves), in its
// call's window w, as the header of this file says: the instant whole, never
// its words. unread says why the test case's own request could not be read
// (requestUnread; "" when it was, or no test case's request applies): then
// the echo could not read it, and the instant is never noise or an echo.
func (dc *DriftCache) classifyInstant(r, l Leaf, at time.Time, served []Earlier, raw []Raw, resp []Leaf, w *Window, unread string, places func() *offsetPlaces, skip skipRule) Learned {
	ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
	rt, okR := readTyped(r)
	lt, okL := readTyped(l)
	if !okR || !okL {
		ln.Kind, ln.Why = KindUnaccounted, instantUnread(r, l)
		return ln
	}
	rec, live := rt.moment(rt.res), lt.moment(lt.res)
	// skip: read without the rules it names (skipRule), every other rule as
	// it stands. skipSource, as an instant no earlier place is the source of
	// (no variable's search of the places that held the moment it was
	// recorded with: classifyRead's copy of a read its own window holds);
	// the echo of a pick still reads it: a moment F saw that an earlier
	// place holds, with the recorded one held nowhere, is no copy the
	// recording shows. skipOffset, as one no moved place builds by an offset
	// (classifyRead's offset of a given instant its own window holds).
	if from, ok := dc.instantServedFrom(r, rec, live, served); ok && skip&skipSource == 0 {
		ln.Kind, ln.From = KindVariable, from
		return ln
	}
	// The echo, before any provenance excuses the instant.
	if echoed := echoedPairs([]driftedInstant{{rec: rec, live: live}}, dc.earlierMoments(at, served, raw), dc.recordedMoments(at, served, raw)); len(echoed) > 0 {
		if from, ok := dc.instantEchoFrom(r, echoed, at, served, raw); ok {
			ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
			return ln
		}
		where := dc.heldAt(echoed, at, served, raw)
		if where == "" {
			where = "earlier in the test"
		}
		ln.Kind, ln.Why = KindUnaccounted, pickUnaccounted(where)
		return ln
	}
	if from, ok := dc.instantLeafFrom(r, rec, served); ok && skip&skipSource == 0 {
		ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
		return ln
	}
	if from, ok := dc.instantRawFrom(r, rec, live, at, raw); ok && skip&skipSource == 0 {
		ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
		return ln
	}
	// The test case's own request, as recorded, holding the moment it was
	// recorded with, before any provenance excuses it: a pick from the
	// request by the clock whose pick in F the request does not list.
	if where, asRecorded := dc.requestHolds([]moment{rec}, []moment{live}, served); where != "" {
		ln.Kind, ln.Why = KindUnaccounted, requestHoldsWhy(where, asRecorded)
		return ln
	}
	// Built on a moved place by an offset (offset.go), before any provenance
	// excuses it.
	if m, ok := places().offsetFrom([]driftedInstant{{rec: rec, live: live}}); ok && skip&skipOffset == 0 {
		ln.Kind, ln.Why = KindUnaccounted, offsetWhy(m)
		return ln
	}
	// A copy of what F gave the application in another test or in a
	// recording made after the call, or a pick from it (the echo of those
	// places), before any provenance excuses it.
	if why := dc.echoedElsewhere([]driftedInstant{{rec: rec, live: live}}, at, served, raw, places); why != "" {
		ln.Kind, ln.Why = KindUnaccounted, why
		return ln
	}
	// noWindow, of an instant neither the lead nor a window shows a read
	// that is learned as a plain read all the same (noWindowRead), is why
	// keploy refused it before it learned it so: a caller whose replay
	// checks no read of the clock refuses it with that reason still
	// (KindClockRead). "" for any other.
	noWindow := ""
	// A read of the clock plus a constant offset (a time to live, a due
	// date; a cutoff below zero, createdAt >= now - 2m), in its call's
	// window in both runs: compared, never noise, and tried first, before
	// any plain read of the clock. A value its window holds shows no offset
	// (clockOffsetOf), nor does one of a call with no window. In keploy
	// test, the lead shows a read made no later than its call in a cutoff
	// made minutes before its window as it shows one made in it: such a
	// value is the offset its window shows where the offset rule learns
	// one, and only otherwise the plain read the lead shows, as before. In
	// keploy mock's client tests too: a value the harness gives the
	// application is compared all the same (the replay checks the offset,
	// never lets it pass), and one built on a read made before its call's
	// window shows no offset coarser than a second (clockOffset refuses
	// it), as a read the application made earlier stands from the window by
	// how long ago it was made.
	off, cutWhy, offset := offsetReading(fieldName(ln.Recorded), rt, lt, w, dc.zone)
	var tried Learned
	if offset {
		if tried = dc.clockOffset(ln, off, max(rt.res, lt.res), w, unread, rec, resp); tried.Kind == KindClockOffset {
			return tried
		}
	}
	if !instantClock(rt, lt, w, !dc.clientTests, at, dc.zone) {
		// Where the offset rule refuses an offset neither the lead nor a
		// window shows a plain read beside, the client test's own refusal
		// stands (clientTestWhy), which says how to correct a value the
		// harness supplies; so does an offset below zero's everywhere: a
		// value before its call's window is refused with the reason it was
		// before keploy learned one (a read made earlier stands from the
		// window by how long ago it was made). keploy test refuses an offset
		// above zero naming it.
		if offset && !dc.clientTests && off.Min > 0 {
			return tried
		}
		// An offset the values' cut leaves ambiguous is refused naming both
		// cuts (offsetReading), in a client test with its own correction
		// added (clientCutAmbiguousWhy).
		if cutWhy != "" {
			ln.Kind, ln.Why = KindUnaccounted, cutWhy
			if dc.clientTests {
				ln.Why = clientCutAmbiguousWhy(cutWhy)
			}
			return ln
		}
		why := instantNotClock
		switch {
		case rt.names != lt.names:
			why = instantFormChangedWhy(rt.names, lt.names)
		case dc.clientTests && rt.names == namesTimeOfDay:
			why = timeOfDayClientTestWhy
		case dc.clientTests:
			why = clientTestWhy("", w != nil)
		case inClockRange(rt.at) && inClockRange(lt.at) && leadMoved(rt.at, lt.at, rt.res, lt.res, false):
			// Moved by the lead, as a read made after its call would
			// (readNoLaterThan): the reason says so, never that it did
			// not move by it.
			why = instantPastCall
			if rt.afterCall(at, dc.zone) {
				why = instantAfterCall
			}
		}
		// With no window, one that names its moment is a plain read of
		// the clock all the same, checked at every replay (noWindowRead).
		if !dc.noWindowRead(rt, lt, w, at) {
			ln.Kind, ln.Why = KindUnaccounted, why
			return ln
		}
		// Where keploy cannot tell which clock the application reads, no
		// replay can check such a read: refused here, at learn, whatever the
		// lead did not show.
		if !dc.appClock {
			ln.Kind, ln.Why = KindUnaccounted, noAppClockWhy
			return ln
		}
		noWindow = why
	}
	// Noise, or an echo, only where the echo could read the test case's own
	// request, where the moment F saw may stand.
	if unread != "" {
		ln.Kind, ln.Why = KindUnaccounted, unreadRequestWhy(unread)
		return ln
	}
	if echo := dc.instantEchoes(rec, resp); len(echo) > 0 {
		// An echo answers with this run's value and checks none, so one
		// that neither the lead nor a window shows a read is refused.
		if noWindow != "" {
			ln.Kind, ln.Why = KindUnaccounted, noWindow
			return ln
		}
		ln.Kind, ln.Echo = KindEcho, echo
	}
	// A read of the clock, checked at every replay (KindClockRead, never
	// noise): one that names its moment as it names it, a wall clock of no
	// zone in the zone the learn step read it in (wallZone), at the cut its
	// two values show (readRes), which a replay checks it at
	// (CheckClockRead).
	if ln.Kind == KindNoise && rt.names == lt.names && rt.names != namesTimeOfDay {
		res := zonedRes(max(rt.res, lt.res), rt.at, lt.at, rt.names == namesWall, dc.zone)
		ln.Kind, ln.Why = KindClockRead, clockReadUncheckedWhy
		if noWindow != "" {
			ln.Why = noWindow
		}
		ln.Read = &models.ClockOffset{Res: res, Layout: models.LayoutTyped}
		if rt.names == namesWall {
			ln.Read.Zone = dc.wallZone(rt.at, lt.at, rt.res, lt.res, w, at)
		}
	}
	// A time of day the lead moved (instantClock) names no day, so no
	// window holds it: a read of the clock all the same, which every replay
	// checks modulo a day in its call's span, in the zone the learn step
	// read it in (todRead, clockReadIn), never noise that lets any value
	// pass; at the cut its two values show (todRead), which a replay checks
	// it at.
	if ln.Kind == KindNoise && rt.names == namesTimeOfDay && lt.names == namesTimeOfDay {
		day := time.Duration(nsPerDay)
		rtod, ltod := rt.at.Sub(timeOfDayDay)%day, lt.at.Sub(timeOfDayDay)%day
		read := dc.todRead(rtod, ltod, rt.res, lt.res, models.LayoutTypedTimeOfDay, models.LayoutTypedTimeOfDay, w, at)
		ln.Kind, ln.Why, ln.Read = KindClockRead, clockReadUncheckedWhy, read
	}
	return ln
}

// noWindowRead reports whether r and l, an instant its protocol types as
// recorded and as F saw it, that neither the lead nor a window shows a read
// of the application's clock (instantClock), is still learned as a plain
// read of it (Learned.Read), as a text one instant whole is (classifyRead):
// in a call with no window w, an instant of one form in both runs (a test's
// own call, read by the lead alone, but every other call with none too: a
// start-up or pool call whose window keploy could not tell, a background
// pairing; each is checked at replay where its caller checks reads, and
// refused at learn where keploy cannot tell the application's clock), in the
// years a recording's clock reads (inClockRange), and recorded no later than
// its call at (zero: not known), as a read the call carries is made. Every
// replay, record's confirming round first, checks it in its call's own span
// on the application's clock (CheckClockRead), so a value that is no read of
// the clock fails there, naming the field, never noise that lets any value
// pass. Whether the lead moved it depends on how late record's full round
// read its clock, and the verdict must not. Not in keploy mock's client
// tests, whose harness gives the application inputs keploy does not see
// (clientTestWhy), nor for a time of day, which names no day. A wall clock
// of no zone in both runs is one too, read in the zone its recording tells
// (wallZone), which a replay checks it in alone, or in UTC and the
// application's zone both where the recording cannot tell the two apart
// (models.ZoneEither). An echo of it in its call's own answer stays refused
// (classifyInstant): an echo is answered with this run's value, never
// checked as a read.
func (dc *DriftCache) noWindowRead(r, l typedInstant, w *Window, at time.Time) bool {
	return w == nil && !dc.clientTests && r.names == l.names && r.names != namesTimeOfDay &&
		inClockRange(r.at) && inClockRange(l.at) && !r.afterCall(at, dc.zone)
}

// clockOffsetOf is offsetReading's offset of r and l in w, their cut read
// in zone too (the application's, nil where keploy cannot read it), where it
// learns one; false where it learns none or refuses the cut it reads.
func clockOffsetOf(r, l typedInstant, w *Window, zone *time.Location) (models.ClockOffset, bool) {
	o, why, ok := offsetReading("", r, l, w, zone)
	return o, ok && why == ""
}

// offsetReading is the offset (models.ClockOffset) r and l, an instant as
// recorded and as F saw it, stand at from a read of each run's clock in its
// call's window w, a clock read plus a constant offset, then cut, being a
// clock read (2026-10-28): the offsets d that put some read t in w.Rec,
// plus d, at r cut to its cut (Res), and some read in w.F, plus d, at l cut
// to it (offsetAt), both. The cut is the one both values show, as a plain
// read's (zonedRes: the coarsest step of models.CutOf above the coarser of
// their encodings both are whole multiples of, read in UTC, in the offsets
// they write and in zone, the application's, nil where keploy cannot read
// it: an hour cut in UTC+05:30 is one there, never the minute it shows in
// UTC). Where their gap is a whole multiple of a coarser
// step (models.CutOf), an application that cuts its value to that step and
// adds a time to live that is no whole number of it (a minute's start plus
// 30m30s) writes such values too, neither a whole step, so:
//   - where the values' own cut puts one offset in both runs, both readings
//     explain them, and keploy cannot tell which cut a replay must check:
//     refused, why naming both (cutAmbiguousWhy). Two values of a writer of
//     seconds a whole minute apart (F's read 0.55 s to 2.55 s earlier than
//     the lead, against the recording's), or of milliseconds a whole
//     second apart, show it; a fast call's never do (FullRoundLead keeps
//     its gap off every step);
//   - otherwise the offset at the gap's step, if one holds both.
//
// A cut coarser than a minute (an hour, a day), which a zone's offset
// moves, is learned as any other, checked at the cut (founder decision,
// 2026-10-27: a cut is checked, never refused) and read in the
// application's zone (models.ClockOffset.AppZoneMatters: the learn step
// names the zone it read, and a replay reads the hour or the day the value
// is cut to in the application's zone, learn's CheckClockRead). The
// offset's bounds read no zone (offsetBounds: a value stands for the cut
// it starts, whatever zone that cut is of), and clockOffset refuses one
// whose range and three cuts are no narrower than its unit as it does at
// any cut (an hour's cut on an offset of a day or more is decided so), but
// a day's cut, and an hour's that reads in hours or minutes (clock + 2h, a
// cut hour plus 90 minutes), which no unit holds three of
// (models.ClockOffset.CoarseCut), only where its range is no narrower than
// the cut (models.ClockOffset.Narrow): a replay checks it at the cut.
// ok only where one offset
// does both: a constant offset from the clock, which a value built on a
// served instant or a constant shows in neither run's window. The offset is
// signed (models.ClockOffset's signed): a value later than every read in
// its window in both runs (each least offset above zero: an expiry) shows
// one above zero, and one whose cut ends before every read in its window in
// both runs (each bound, which it excludes, at or below zero: a cutoff the
// application queries from, createdAt >= now - 2m) one below; a value
// inside a run's window (a plain read's offsets lie around zero), or after
// it in one run and before it in the other, shows none. Only an instant that names its moment (InstantLeaf) shows one: a
// wall clock of no zone (a date too) stands from the clock by its zone's
// offset as much as by one the application adds, which keploy cannot tell
// apart, and a time of day names no day. One of two forms, outside the
// years a recording's clock reads (inClockRange), one that did not move,
// or of a call with no window shows none, and no why. path names the place
// in why.
func offsetReading(path string, r, l typedInstant, w *Window, zone *time.Location) (models.ClockOffset, string, bool) {
	if w == nil || r.names != namesMoment || l.names != namesMoment || !inClockRange(r.at) || !inClockRange(l.at) || r.at.Equal(l.at) {
		return models.ClockOffset{}, "", false
	}
	enc := max(r.res, l.res)
	cut := zonedRes(enc, r.at, l.at, false, zone)
	o, ok := offsetAt(r, l, w, cut)
	if step := models.CutOf(int64(l.at.Sub(r.at)), enc, 24*time.Hour); step > cut {
		if ok {
			return models.ClockOffset{}, cutAmbiguousWhy(path, o, l.at.Sub(r.at), step), false
		}
		cut = step
		o, ok = offsetAt(r, l, w, cut)
	}
	if !ok {
		return models.ClockOffset{}, "", false
	}
	return o, "", true
}

// offsetAt is the offset r and l stand at from a read of each run's clock
// in w at the cut res (offsetBounds): from the greater of the two least up
// to the lesser of the two bounds; ok only where it holds one, both values
// on one side of their calls, the same in both runs (later: an expiry;
// earlier: a cutoff).
func offsetAt(r, l typedInstant, w *Window, res time.Duration) (models.ClockOffset, bool) {
	rlo, rhi, ok := offsetBounds(r.at, res, w.Rec)
	if !ok {
		return models.ClockOffset{}, false
	}
	llo, lhi, ok := offsetBounds(l.at, res, w.F)
	if !ok {
		return models.ClockOffset{}, false
	}
	// On one side of its window, the same in both runs: later than every
	// read in it (an expiry), or earlier (a cutoff). A read made before its
	// window (before the previous call) stands from the window by how long
	// ago it was made, which the runs' timing sets, so it shows one offset
	// in both runs only where the two waited as long as each other to a
	// few milliseconds, and the checks of clockOffset (a unit coarser than
	// a second, one multiple of it in range) refuse it in all but a whole
	// number of minutes; even then every replay compares that offset, so a
	// read whose wait changed fails there, never passes unchecked.
	later, earlier := rlo > 0 && llo > 0, rhi <= 0 && lhi <= 0
	if !later && !earlier {
		return models.ClockOffset{}, false
	}
	o := models.ClockOffset{Min: max(rlo, llo), Max: min(rhi, lhi), Res: res, Layout: models.LayoutTyped}
	return o, o.Min < o.Max
}

// cutAmbiguousWhy is why an instant at the clock plus the offset off, read
// at the cut its two values show, is refused where they stand gap apart, a
// whole number of a coarser step (offsetReading): an application that cuts
// to the step and adds a time to live of no whole number of it writes them
// too, and a replay checked at the wrong cut would pass a value a whole
// step off or fail by where its read fell.
func cutAmbiguousWhy(path string, off models.ClockOffset, gap, step time.Duration) string {
	return clockOffsetSeen(path, off) + " at a cut of " + models.ShortDuration(off.Res) + ", but its two values stand " + models.ShortDuration(gap) + " apart, a whole number of " + models.ShortDuration(step) + ": an application that cuts its value to " + models.ShortDuration(step) + " and adds a time to live that is no whole number of it writes such values too, and keploy cannot tell at which cut a replay must check it, so it refuses it. Record the test again (record's full round then reads its clock at another moment); if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// sharedGrain is the granularity a and b share: the greatest duration both
// are whole multiples of, counted from the Unix epoch (the greatest common
// divisor of the two). An application that cuts its value coarser than its
// encoding writes (Go's Truncate(time.Second), stored as a millisecond
// $date) shows that cut in both runs' values. The learn step reads it only
// through the steps of models.CutOf (readRes, zonedRes), so two values that
// share a granularity between steps (both whole multiples of 3 ms, or of
// 492 ms, by chance) learn their encoding's resolution, never that
// granularity. Equal values share the value itself. Values before the epoch
// count from it the same.
func sharedGrain(a, b time.Time) time.Duration {
	x, y := a.UnixNano(), b.UnixNano()
	if x < 0 {
		x = -x
	}
	if y < 0 {
		y = -y
	}
	for y != 0 {
		x, y = y, x%y
	}
	return time.Duration(x)
}

// readRes is the resolution a plain read of the clock (Learned.Read: a
// typed instant, a text one, a time of day) is learned at, from enc, the
// resolution its two values' encodings write (a BSON datetime's
// millisecond, a layout's second; of a text, the finer of its two values'
// forms, textReadRes), and grain, the granularity the two values share
// (sharedGrain, todGrain): enc itself, whatever the two share by chance
// between steps (two millisecond values both whole multiples of 492 ms, or
// of 3 ms), so the same recording learns the same resolution whichever
// value record's full round read, and a replay's window is never wider for
// it. Where both are whole multiples of a step above enc (a unit, or a
// decimal step between units: models.CutOf), the coarsest such step: the
// cut the application made, or one both values happened to fall on, which
// a replay reads by its own value (CheckClockRead): a value that shows the
// cut is checked at it, so it may stand up to one cut stale. Two values of
// a writer that does not cut fall on such a step by chance only where F's
// read moved against the recording's by more than FullRoundLead keeps a
// fast call's within; where it did, over a spread a whole number of steps
// wide (or many steps), each value falls on it in one recording in step/r
// (r: the resolution the values carry, the clock's or the writer's, the
// coarser) and both in the square of that, and that recording's replays
// are checked at the step, never refused for it. Above a day no step is
// above enc and no cut is told: enc.
func readRes(enc, grain time.Duration) time.Duration {
	if grain <= enc {
		return enc
	}
	if u := models.CutOf(int64(grain), enc, 24*time.Hour); u > 0 {
		return u
	}
	return enc
}

// zonedRes is readRes of two values of a plain read of the clock, a and b,
// written at enc, read as the cut both show in each zone an application
// cuts its clock in: the coarsest of the cuts read in UTC, in the offset
// each value writes (a.Location(), b.Location(): "+05:30" of a text that
// names it), and in zone, the application's (nil: not known). An hour or a
// day the application cuts in a zone whose offset is no whole number of it
// (an hour in UTC+05:30, a day in UTC+02:00) is a finer cut in UTC (a
// minute, an hour), which a replay would check at that finer cut, and so
// fail by when in the hour or the day it ran; read in that zone, it is
// learned as the hour or the day it is. A minute or any finer step is one
// in every zone (zones' offsets are whole minutes). A wall clock of no zone
// (wall) writes its own zone's clock, so its cut is read on what it writes
// alone (a and b are those read in UTC). A replay reads its value's cut in
// the same zones (CheckClockRead).
func zonedRes(enc time.Duration, a, b time.Time, wall bool, zone *time.Location) time.Duration {
	res := readRes(enc, sharedGrain(a, b))
	// in is the cut both show read in x and y (models.CutIn: the coarsest
	// step each shows; steps nest, so both show the finer of the two).
	in := func(a, b time.Time, x, y *time.Location) {
		if x == nil || y == nil {
			return
		}
		res = max(res, enc, min(models.CutIn(a, x, enc, 24*time.Hour), models.CutIn(b, y, enc, 24*time.Hour)))
	}
	if wall {
		// A wall clock writes its own zone's clock: its digits, read where
		// the application's zone skipped the start of their hour or day.
		if zone != nil {
			in(wallIn(a.UnixNano(), zone), wallIn(b.UnixNano(), zone), zone, zone)
		}
		return res
	}
	in(a, b, time.UTC, time.UTC)
	in(a, b, a.Location(), b.Location())
	in(a, b, zone, zone)
	return res
}

// offsetBounds are the offsets d, from lo up to, not including, hi, that put
// a read t of the clock made in s, plus d, at v cut to resolution res
// (truncated: v stands for [v, v+res)): from v-s.To up to v+res-s.From. ok
// only for a span with both bounds, in order, and a resolution.
func offsetBounds(v time.Time, res time.Duration, s Span) (lo, hi time.Duration, ok bool) {
	if s.From.IsZero() || s.To.IsZero() || s.To.Before(s.From) || res <= 0 {
		return 0, 0, false
	}
	return v.Sub(s.To), v.Add(res).Sub(s.From), true
}

// clockOffset is classifyInstant's reading of an instant (ln) that is a
// read of the clock plus the offset off in its call's window w: learned as
// one (KindClockOffset) only where off reads in a unit coarser than a
// second, and off and three of its resolution (off.Res, the cut its values
// show: a replay checks a value within one cut of its span on either side)
// are narrower than that unit (off.Within: off holds one multiple of it,
// and a replay can tell a time to live changed by one), or, cut to a day or
// to an hour that no unit holds three of (off.CoarseCut: clock + 2h at the
// hour), off alone is narrower than the cut (off.Narrow: a replay checks it
// at the cut, admitting a time to live changed by less than two cuts at
// some reads, as a date cut to a day admits yesterday), for a caller whose
// replay compares it (Window.ComparesOffsets), where the echo could read
// the test case's own request (unread: why not) and the call's recorded
// answer does not hold it (rec, its recorded moment, in resp: an answer
// keploy would serve with the recorded value); refused otherwise, the
// reason naming the offset, or its range where it is no narrower, and enc,
// the resolution its values' encoding writes, beside a cut. A cut is never
// refused (founder decision, 2026-10-27; a clock read plus a constant
// offset, then cut, is a clock read, 2026-10-28): thirty days cut to the
// second is learned, and a replay checks it at the second.
func (dc *DriftCache) clockOffset(ln Learned, off models.ClockOffset, enc time.Duration, w *Window, unread string, rec moment, resp []Leaf) Learned {
	path := fieldName(ln.Recorded)
	fallBack := dc.offsetFallBackWhy(path, off, enc, w)
	switch {
	case off.Unit() <= time.Second:
		ln.Kind, ln.Why = KindUnaccounted, clockOffsetFineWhy(path, off)
	case !off.Narrow():
		ln.Kind, ln.Why = KindUnaccounted, clockOffsetWideWhy(path, off, enc)
	case fallBack != "":
		ln.Kind, ln.Why = KindUnaccounted, fallBack
	case !w.offsets:
		ln.Kind, ln.Why = KindUnaccounted, clockOffsetUncomparedWhy(path, off)
	case unread != "":
		ln.Kind, ln.Why = KindUnaccounted, unreadRequestWhy(unread)
	case len(dc.instantEchoes(rec, resp)) > 0:
		ln.Kind, ln.Why = KindUnaccounted, clockOffsetEchoedWhy(path, off)
	default:
		ln.Kind, ln.Offset, ln.Why = KindClockOffset, &off, clockOffsetEvidence(path, off)
	}
	return ln
}

// textClockOffset is classifyRead's reading, in keploy mock's client tests,
// of a drifted text (r as recorded, l as record's full round saw it, v what
// changed) that its call's window w holds as no plain read of the
// application's clock (clientTestUnread), and, in any test, of one the lead
// or a window shows a plain read of it, tried as an offset first (a value
// made before its call's window: classifyRead): where each is one instant
// whole that names its moment (readClockTextWall: an epoch's digits, a time
// with its zone), both written in one layout, and w holds the two as a read of
// each run's clock plus one constant offset (offsetReading), that offset,
// learned as classifyInstant learns a typed one (clockOffset: only for a
// caller whose replay compares it, in a unit coarser than a second, and the
// rest of its checks), in the text's layout, which a replay reads its value
// in. ok is false where no offset is learned (the value is no such instant,
// a wall clock of no zone, which stands from the clock by its zone's offset
// as much as by one the application adds; the two runs wrote two layouts; w
// shows no one offset; or clockOffset refuses it), and the client test's
// refusal, or the plain read, stands, as the typed reading's does
// (classifyInstant); cutWhy, where the cut the two values show leaves the
// offset ambiguous (offsetReading's cutAmbiguousWhy, naming both cuts),
// which a client test's refusal names instead (clientCutAmbiguousWhy).
func (dc *DriftCache) textClockOffset(r, l Leaf, v *valueDrift, w *Window, resp []Leaf) (got Learned, cutWhy string, ok bool) {
	rat, rres, rwall, okR := readClockTextWall(r.Value)
	lat, lres, lwall, okL := readClockTextWall(l.Value)
	if !okR || !okL || rwall || lwall {
		return Learned{}, "", false
	}
	lay := models.ClockOffset{Layout: clockTextLayout(r.Value)}.MeetLayout(clockTextLayout(l.Value))
	if lay == "" || lay == models.LayoutApart {
		return Learned{}, "", false
	}
	off, cutWhy, ok := offsetReading(fieldName(r), typedInstant{at: rat, res: rres, names: namesMoment}, typedInstant{at: lat, res: lres, names: namesMoment}, w, dc.zone)
	if !ok {
		return Learned{}, cutWhy, false
	}
	// The recorded moment the echo check reads the call's answer for.
	for _, p := range v.instants {
		if p.rec.key.at == rat.UnixNano() {
			off.Layout = lay
			got := dc.clockOffset(Learned{Kind: KindNoise, Recorded: r, Live: l}, off, max(rres, lres), w, "", p.rec, resp)
			return got, "", got.Kind == KindClockOffset
		}
	}
	return Learned{}, "", false
}

// clientCutAmbiguousWhy is a client test's refusal of a changed instant
// whose two values offsetReading reads as a read of the application's clock
// plus an offset at two cuts (cutWhy, cutAmbiguousWhy's, which names both
// and how to correct it): it names the ambiguity, never only that no clock
// read matched it (learning legible), and adds the client test's own
// correction, for a value the test's harness supplies.
func clientCutAmbiguousWhy(cutWhy string) string {
	return cutWhy + ". If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it"
}

// fieldName is the leaf's name as keploy.yml's noise writes it (Leaf.Noise:
// "body.expiresAt", not its encoding's "...expiresAt.$date.$numberLong"),
// its path where it has none.
func fieldName(l Leaf) string {
	if l.Noise != "" {
		return l.Noise
	}
	return l.Path
}

// offsetNamed is what the offset off is to the application, as a reason
// names it: a time to live (a due date, an expiry) where it is above zero,
// a look-back (a cutoff such as createdAt >= now - 2m) where it is below,
// an offset where its range holds both.
func offsetNamed(off models.ClockOffset) string {
	switch {
	case off.Min > 0:
		return "a time to live"
	case off.Max < 0:
		return "a look-back"
	}
	return "an offset"
}

// clockOffsetSeen is what the learn step saw of a value at off from the
// application's clock: "expires_at = clock + 30m ...".
func clockOffsetSeen(path string, off models.ClockOffset) string {
	return path + " = " + off.String() + ": in the recording and in record's full round alike, it is a read of the application's clock in its call's window plus that offset"
}

// clockOffsetEvidence is a KindClockOffset's Why: what was seen, and how the
// replay compares it. Of an offset cut to an hour or a day that no unit
// holds three of (models.ClockOffset.CoarseCut: a day's cut, or an hour's
// that reads in hours or minutes, clock + 2h), what a replay checking it at
// the cut admits (models.ClockOffset.Checkable): as a date cut to a day
// admits yesterday, a time to live changed by less than two cuts can pass,
// by where the replay's read falls in its cut (one changed by one cut
// passes at every read where the application cuts the value down to the
// cut, Go's Truncate; where it cuts and then adds time, only at some), one
// changed by two only near the cut's turn, and one changed by three or more
// never, on a day of 24 hours; of an hour's cut, a replay refuses the value
// near an hour the application's zone shows twice (cutExceptions).
func clockOffsetEvidence(path string, off models.ClockOffset) string {
	if off.CoarseCut() {
		w := unitWord(off.Res)
		outside, on := cutExceptions(off.Res)
		return clockOffsetSeen(path, off) + ", cut to " + aUnit(off.Res) + ". A replay checks the offset at the " + w + ", read in the zone the value shows a whole " + w + " in, else the application's, as standing for its whole " + w + ", within one " + w + " of its call's window plus the offset on either side, as a date cut to a day passes from yesterday: " + offsetNamed(off) + " changed by less than two " + w + "s can pass there, by where the replay's read falls in its " + w + ", " + outside + "one changed by two " + w + "s fails unless the replay's read falls within its call's window and the offset's range (" + models.ShortDuration(off.Max-off.Min) + ") of the " + w + "'s turn, and one changed by three " + w + "s or more fails" + on + "; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	return clockOffsetSeen(path, off) + ". A replay compares the offset: " + offsetNamed(off) + " changed by " + aUnit(off.Unit()) + " or more still fails; near a wall time the application's zone shows twice as its clocks go back (any zone's, where keploy knows none of the application's) a replay refuses the value, since an application that resolves that wall time to its first showing writes a value an hour early there (two hours in Antarctica/Troll); if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// offsetFallBackWhy is why an instant at the clock plus the offset off,
// checked at a cut finer than a day (none, a second, a minute, an hour:
// CheckedAt at enc, the resolution its values' encoding writes), is
// refused where a run's call's window, or the values a check of it admits
// there, meets a wall time the application's zone (dc.zone) shows twice as
// its clocks go back (models.ClockOffset.FallBack), or, where the learn
// step knows no zone of the application's, one any zone does
// (FallBackAnywhere): an application that resolves that wall time to its
// first showing writes a value an hour early there (two in Troll), so
// where both runs fall in the second showing the offset they show stands
// an hour short of the one the application adds (clock + 1h of clock +
// 2h), and a replay would then pass a time to live made an hour shorter,
// at any read, and refuse the right one. "" where none does.
//
// At an hour's cut, only a turn whose wall time shown twice holds the
// start of one of the zone's wall hours strictly inside it is refused
// (FallBackAcrossHour, FallBackAnywhereAcrossHour): there two runs in the
// second showing on either side of that start cut to different hours,
// which resolve to different showings, or both to an early one
// (acrossHourNamed). Pacific/Chatham's 02:45 to 03:45 holds 03:00: a run in
// the second showing at 02:59 cuts to 02:00, shown once, and writes the
// right value, while its full round at 03:00 writes 03:00's first showing,
// an hour early, and the two show clock + 1h of clock + 2h.
// Antarctica/Troll's 01:00 to 03:00 holds 02:00, and its 01:00 is shown
// twice too: runs at 01:59 and 02:00 in the second showing write 01:00's
// and 02:00's first showings, both two hours early, and show clock + 1h of
// clock + 3h. Where the wall time shown twice is one whole hour
// (Europe/Berlin's 02:00 to 03:00), both runs in the second showing cut
// into one hour and write one value, which shows no read of the clock, and
// one in each show no one offset, so a right learning near such a turn is
// not refused.
func (dc *DriftCache) offsetFallBackWhy(path string, off models.ClockOffset, enc time.Duration, w *Window) string {
	c := off.CheckedAt(enc)
	if w == nil || c >= 24*time.Hour {
		return ""
	}
	fallBack, anywhere, hour := off.FallBack, off.FallBackAnywhere, c >= time.Hour
	if hour {
		fallBack, anywhere = off.FallBackAcrossHour, off.FallBackAnywhereAcrossHour
	}
	for _, s := range []Span{w.Rec, w.F} {
		named, early := "", ""
		if turn, back, met := fallBack(enc, s.From, s.To, dc.zone); met {
			named, early = fallBackNamed(turn, back, dc.zone)+acrossHourNamed(hour, turn, dc.zone), backWords(back)
		} else if dc.zone == nil || dc.zoneNoName {
			if turn, back, loc, met := anywhere(enc, s.From, s.To); met {
				named, early = fallBackNamed(turn, back, loc)+acrossHourNamed(hour, turn, loc)+" (keploy knows no zone of the application's here, so it reads every zone's clocks going back)", backWords(back)
			}
		}
		if named != "" {
			return clockOffsetSeen(path, off) + ", but a run's call's window, or the values a check of it admits there, meets " + named + ": an application that resolves a wall time shown twice to its first showing (JavaScript's Date setters; java.time's LocalDateTime then atZone) writes a value " + early + " early there, so the offset a run there shows can stand " + early + " from the one it adds, and keploy refuses it rather than learn it. Record the test again outside that hour; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
		}
	}
	return ""
}

// acrossHourNamed is what offsetFallBackWhy adds, at an hour's cut (hour),
// to the turn named, at which loc's clocks go back and whose wall time
// shown twice holds the start of one of loc's wall hours
// (models.AcrossHour): that hour, and why two runs a minute apart in the
// second showing, either side of it, show an offset that stands from the
// one the application adds. Where the wall time shown twice starts within
// an hour (Pacific/Chatham's 02:45 holds 03:00), the hour before the one
// held is shown once, so a run before it writes the right value and one
// after it the held hour's first showing, early by what the clocks go
// back; where it starts on the hour (Antarctica/Troll's 01:00 to 03:00
// holds 02:00), that hour is shown twice too, so both write their hour's
// first showing and both are early by it. "" elsewhere.
func acrossHourNamed(hour bool, turn time.Time, loc *time.Location) string {
	if !hour || !models.AcrossHour(turn, loc) {
		return ""
	}
	after := turn.In(loc)
	_, b := turn.Add(-time.Nanosecond).In(loc).Zone()
	_, a := after.Zone()
	early := backWords(time.Duration(b-a) * time.Second)
	into := time.Duration(after.Minute())*time.Minute + time.Duration(after.Second())*time.Second + time.Duration(after.Nanosecond())
	start := after.Add(time.Hour - into).Format("15:04")
	if into != 0 {
		return ", which holds the start of the wall hour " + start + ": cut to the hour, a run in the second showing before " + start + " writes the hour before it, shown once, and so the right value, and one after it " + start + "'s first showing, " + early + " early"
	}
	return ", which holds the start of the wall hour " + start + ": cut to the hour, a run in the second showing before " + start + " writes " + after.Format("15:04") + "'s first showing, shown twice too, and one after it " + start + "'s, so both are " + early + " early and the two show one offset " + early + " short of the one the application adds"
}

// backWords names what a zone's clocks go back by at a turn, back: "an
// hour", "two hours" (Antarctica/Troll), "half an hour" (Australia/Lord_Howe),
// else its short form.
func backWords(back time.Duration) string {
	switch back {
	case 30 * time.Minute:
		return "half an hour"
	case time.Hour:
		return "an hour"
	case 2 * time.Hour:
		return "two hours"
	}
	return models.ShortDuration(back)
}

// clockOffsetFineWhy is why an instant at the clock plus an offset off is
// refused where off reads in a second or a finer unit, read after "<field>
// changed, and ": an application that cuts its value to that unit (a whole
// second) makes a time to live changed by one look the same.
func clockOffsetFineWhy(path string, off models.ClockOffset) string {
	return clockOffsetSeen(path, off) + ", but the offset reads in " + unitWord(off.Unit()) + "s, and an application that cuts its value to the " + unitWord(off.Unit()) + " makes " + offsetNamed(off) + " changed by " + aUnit(off.Unit()) + " look the same, so keploy refuses an offset that reads in a second or finer. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// clockOffsetWideWhy is why an instant at the clock plus an offset in off is
// refused where the learn step cannot learn off (off.Narrow), read after
// "<field> changed, and ": off and three of its resolution (off.Res) are no
// narrower than the unit the offset reads in (off.Within), or, of an off cut
// to an hour or a day that no unit holds three of (off.CoarseCut), its range
// alone is no narrower than the cut: its call's windows, or the cut its
// values show, bound the offset too loosely for a replay to tell a time to
// live changed by that unit, or by hours or days.
func clockOffsetWideWhy(path string, off models.ClockOffset, enc time.Duration) string {
	if off.CoarseCut() {
		w := unitWord(off.Res)
		return path + " = " + off.Range() + " at a resolution of " + resolutionOf(off.Res, enc) + ": in the recording and in record's full round alike, it is a read of the application's clock in its call's window plus an offset in that range, but the range is not narrower than the " + w + " the value is cut to, so a replay checking it at the " + w + " could not tell " + offsetNamed(off) + " changed by " + w + "s from it, and keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	return path + " = " + off.Range() + " at a resolution of " + resolutionOf(off.Res, enc) + ": in the recording and in record's full round alike, it is a read of the application's clock in its call's window plus an offset in that range, but the range and three of its resolution (a replay checks the value within one of it of its window plus the offset, on either side) are not narrower than the " + unitWord(off.Unit()) + " the offset reads in, so a replay could not tell " + offsetNamed(off) + " changed by " + aUnit(off.Unit()) + " from it, and keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// resolutionOf is res as a reason reads it, saying what set it: the
// encoding's (enc), or the cut both runs' values show where that is
// coarser (offsetReading).
func resolutionOf(res, enc time.Duration) string {
	if res > enc {
		return models.ShortDuration(res) + " (the cut both runs' values show, coarser than the " + models.ShortDuration(enc) + " their encoding writes)"
	}
	return models.ShortDuration(res) + " (the one its encoding writes)"
}

// cutExceptions are where a reason's claims on a time to live changed by
// two or three of the cut c, an hour or a day, do not hold
// (models.ClockOffset.Checkable): outside goes before both claims, on after
// the last. A daylight-saving turn makes a day 23 or 25 hours long; near the
// hour the clocks go back an application that cuts to the hour's first
// showing (JavaScript; java.time's LocalDateTime then atZone) writes a value
// up to two hours before the instant it cuts, so a replay refuses an hour's
// cut there (models.ClockOffset.FallBack).
func cutExceptions(c time.Duration) (outside, on string) {
	if c == time.Hour {
		return "", ", and near an hour the application's zone shows twice as its clocks go back a replay refuses the value (an application that cuts to that hour's first showing writes a value up to two hours early there)"
	}
	return "", " on a day of 24 hours"
}

// aUnit is a models.ClockOffset.Unit with its article: "a minute", "an
// hour".
func aUnit(u time.Duration) string {
	if u == time.Hour {
		return "an hour"
	}
	return "a " + unitWord(u)
}

// unitWord names a models.ClockOffset.Unit: "minute".
func unitWord(u time.Duration) string {
	switch u {
	case 24 * time.Hour:
		return "day"
	case time.Hour:
		return "hour"
	case time.Minute:
		return "minute"
	case time.Second:
		return "second"
	case time.Millisecond:
		return "millisecond"
	case time.Microsecond:
		return "microsecond"
	}
	return "nanosecond"
}

// clockOffsetUncomparedWhy is why such an instant is refused for a caller
// whose replay does not compare an offset (Window.ComparesOffsets), read
// after "<field> changed, and ".
func clockOffsetUncomparedWhy(path string, off models.ClockOffset) string {
	return clockOffsetSeen(path, off) + ", and this protocol's replay does not compare such an offset yet: as noise, " + offsetNamed(off) + " changed by any amount would pass, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// clockOffsetEchoedWhy is why such an instant is refused where the call's
// recorded answer holds it, read after "<field> changed, and ".
func clockOffsetEchoedWhy(path string, off models.ClockOffset) string {
	return clockOffsetSeen(path, off) + ", and the call's recorded answer holds the moment it was recorded with, which keploy would serve as recorded while comparing the value's offset, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
}

// instantServedFrom is the leaf of an exchange F served earlier that the
// drifted instant (r; rec and live its moments) came from, ranked as
// appearedIn ranks a value's places (placeRank): a leaf F saw drift that
// was, as recorded, that instant (its text, or, in any form, one instant and
// nothing else, of the moment it was recorded with: an equal instant is
// followed whatever its provenance, as in a payload), or that held the
// moment it was recorded with as recorded and the moment F saw as F saw it,
// neither on its other side (it drifted with it, rule 1). Whole, of the
// place, is its recorded text's being r's: where the value can be bound. The
// test case's own request is such a place too, where keploy test sent a part
// of it with template values (Earlier.Given): nothing binds there.
func (dc *DriftCache) instantServedFrom(r Leaf, rec, live moment, served []Earlier) (Place, bool) {
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if s.Live == s.Leaf.Value {
				continue
			}
			whole := s.Leaf.Value == r.Value
			exact := whole || dc.isInstant(s.Leaf.Value, rec) || seenIs(s.Leaf, s.Leaf.Value, rec)
			if !exact && !dc.instantCoDrifted(s.Leaf.Value, s.Live, rec, live) {
				continue
			}
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
				// Never marked a whole copy of an answer (Place.givenWhole):
				// this search takes the leaf as recorded, with no check that
				// F's view of it held the instant F saw in the value. Where
				// it did, the copy rule's second classification, without the
				// source searches (skipBoth), takes the leaf again as the
				// echo, which keeps the variable; where it did not, the full
				// round refutes the copy, and the copy rule learns the read
				// of the clock since the test began that both runs hold.
				best.givenWhole = false
			}
		}
	}
	return best, rank < 7
}

// seenMoment is the moment value names where l, a leaf of an earlier
// exchange, is one its protocol types as one instant (readTyped), as the
// drifted instant's is (typedInstant.moment): a time of day's too, whose
// form the text reader never reads, its designator T glued to its digits.
// ok is false for a leaf of another type, or a value no form writes.
func seenMoment(l Leaf, value string) (moment, bool) {
	if l.Type != Instant {
		return moment{}, false
	}
	ti, ok := readTyped(Leaf{Type: Instant, Value: value})
	if !ok {
		return moment{}, false
	}
	return ti.moment(ti.res), true
}

// seenIs reports whether value, l's as a side holds it, is an instant its
// protocol types (seenMoment) of moment m, exactly: one instant and nothing
// else, as isInstant reads a text.
func seenIs(l Leaf, value string, m moment) bool {
	sm, ok := seenMoment(l, value)
	return ok && sm.key == m.key
}

// heldSeen files in held the moment value names where l is a leaf its
// protocol types as one instant (seenMoment), as a place holds one: the
// echo reads a typed time of day an earlier exchange holds as it reads a
// text's, at its own precision.
func heldSeen(held *heldMoments, l Leaf, value string) {
	if m, ok := seenMoment(l, value); ok {
		m.typed = false
		held.addMoment(m)
	}
}

// seenNamesApart reports whether value, l's as a side holds it, is an
// instant its protocol types (seenMoment) that names m by a rule of rules
// (any, nil) by which it does not name other, as namesApart reads a text's:
// the places the echo names read a typed time of day as the echo reads it
// (heldSeen), so a copy it found there is bound or refused naming that leaf.
func seenNamesApart(l Leaf, value string, m, other moment, rules map[cutRule]bool) bool {
	sm, ok := seenMoment(l, value)
	return ok && namesMomentApart(sm, m, other, rules)
}

// seenHoldsRecorded is holdsRecorded of a typed leaf's value (seenNamesApart).
func seenHoldsRecorded(l Leaf, value string, ms []echoMatch) bool {
	for _, m := range ms {
		if seenNamesApart(l, value, m.rec, m.live, m.rules) {
			return true
		}
	}
	return false
}

// seenHoldsLive is holdsLive of a typed leaf's value (seenNamesApart).
func seenHoldsLive(l Leaf, value string, ms []echoMatch) bool {
	for _, m := range ms {
		if seenNamesApart(l, value, m.live, m.rec, m.rules) {
			return true
		}
	}
	return false
}

// instantSource reports whether v, a drifted value read word by word, came
// from s, a leaf F saw drift whose protocol types it as one instant (Instant):
// v held the moment s was recorded with as recorded and the moment F saw
// there as F saw it, in any form, neither on its other side.
func (dc *DriftCache) instantSource(s Seen, v *valueDrift) bool {
	rt, ok := readTyped(s.Leaf)
	if !ok {
		return false
	}
	lt, ok := readTyped(Leaf{Type: Instant, Value: s.Live})
	if !ok {
		return false
	}
	return dc.instantCoDrifted(v.rec, v.live, rt.moment(0), lt.moment(0))
}

// instantWhole reports whether recorded, a value's text as recorded, is in
// one of its declared forms the instant leaf s, its protocol's typed
// instant, was recorded with, whole (isInstant): a whole copy of it in
// another layout (epoch seconds of a typed createdAt).
func (dc *DriftCache) instantWhole(s Leaf, recorded string) bool {
	rt, ok := readTyped(s)
	return ok && dc.isInstant(recorded, rt.moment(rt.res))
}

// isInstant reports whether text, in one of its declared forms (textForms),
// is one instant and nothing else, of moment m to the nanosecond: of a time
// of day too, where m is one (a typed time of day), as holdsExact reads it.
func (dc *DriftCache) isInstant(text string, m moment) bool {
	for _, f := range textForms(text) {
		f = strings.TrimSpace(f)
		sp := dc.spansOf(f)
		if len(sp) == 1 && sp[0].keyed && sp[0].start == 0 && sp[0].end == len(f) && sp[0].key == m.key {
			return true
		}
	}
	return false
}

// instantCoDrifted reports whether a leaf F saw drift (rec as recorded, live
// as F saw it) held moment rm as recorded and lm as F saw it, in any form,
// and neither on its other side.
func (dc *DriftCache) instantCoDrifted(rec, live string, rm, lm moment) bool {
	in := func(text string, m moment) bool {
		return dc.holdsExact(text, m) || dc.holdsExact(dc.rawExtra(text), m)
	}
	return in(rec, rm) && in(live, lm) && !in(live, rm) && !in(rec, lm)
}

// instantEchoFrom is the earlier place holding the moment the drifted
// instant r was recorded with, as an instant or its time of day, by the rule
// the echo found it a copy by (echoed, holdsRecorded, as appearedIn's echo
// reads the places): a leaf of an exchange F served earlier, a typed time of
// day there as the echo reads it (seenHoldsRecorded), ranked by placeRank,
// then a payload served or recorded before at, its field where one holds it.
func (dc *DriftCache) instantEchoFrom(r Leaf, echoed []echoMatch, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if !dc.holdsRecorded(s.Leaf.Value, echoed) && !dc.holdsRecorded(dc.rawExtra(s.Leaf.Value), echoed) && !seenHoldsRecorded(s.Leaf, s.Leaf.Value, echoed) {
				continue
			}
			whole := s.Leaf.Value == r.Value
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
				// No givenWhole here: classifyInstant's re-read without the
				// source rules (skipBoth, the copy rule's) takes this echo
				// again, so the copy rule never reads it as a clock read.
				best.Echo = true
			}
		}
	}
	if rank < 7 {
		return best, true
	}
	for _, x := range raw {
		if !x.earlierThan(at) {
			continue
		}
		xb := x.payload()
		for _, f := range xb.fields {
			if !f.key && dc.holdsRecorded(xb.text[f.start:f.end], echoed) {
				return Place{Mock: x.mock, Field: f.describe(), Echo: true, unserved: !x.served}, true
			}
		}
		if dc.holdsRecorded(xb.text, echoed) || dc.holdsRecorded(dc.rawExtra(xb.text), echoed) {
			return Place{Mock: x.mock, Echo: true, unserved: !x.served}, true
		}
	}
	return Place{}, false
}

// instantLeafFrom is the leaf of an exchange F served earlier, leaf by leaf,
// that holds rec, the moment the drifted instant r was recorded with, as an
// instant of its own, exactly (not its time of day, nor a finer instant cut
// to it), in any form, as recorded, whatever F saw there, ranked by placeRank:
// as a payload that holds it is (instantRawFrom), an exchange served leaf by
// leaf is an earlier place, and an instant equal to one it holds is followed
// as a value from there, whether or not it is a read of the clock. An answer's
// own clock stamp served as recorded is none (Seen.Stamp, rule 3), nor is the
// test case's own request, which requestHolds reads.
func (dc *DriftCache) instantLeafFrom(r Leaf, rec moment, served []Earlier) (Place, bool) {
	var best Place
	rank := 7
	for _, e := range served {
		if e.Given {
			continue
		}
		for _, s := range e.Seen {
			if stamped(s) || !dc.holdsExact(s.Leaf.Value, rec) && !dc.holdsExact(dc.rawExtra(s.Leaf.Value), rec) && !seenIs(s.Leaf, s.Leaf.Value, rec) {
				continue
			}
			whole := s.Leaf.Value == r.Value
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
				// Never marked a whole copy of an answer (Place.givenWhole):
				// the echo comes first, so a leaf F saw hold the instant F
				// saw in the value is never this search's. One it reaches is
				// a copy the full round refutes, and the copy rule learns the
				// read of the clock since the test began that both runs hold.
				best.givenWhole = false
			}
		}
	}
	return best, rank < 7
}

// instantRawFrom is the first payload served earlier in the test or recorded
// before at that holds rec, the moment the drifted instant r was recorded
// with, as an instant of its own in any layout, exactly (not its time of
// day, nor a finer instant cut to it: those are the echo's, which F's moment
// gates), in any form, with a protocol-phase answer's own clock reads and an
// HTTP answer's stamp blanked (searched: rules 2 and 3), and the field that
// holds it: the one, of its answer's fields that hold it, that the full round
// does not refute the copy of, else the first. live is the moment F saw in r
// (copiedInBoth).
func (dc *DriftCache) instantRawFrom(r Leaf, rec, live moment, at time.Time, raw []Raw) (Place, bool) {
	for _, x := range raw {
		if !x.earlierThan(at) || !dc.mayHoldInstant(x, rec) {
			continue
		}
		texts := dc.searched(x, false)
		if !dc.holdsExact(texts[0], rec) && !dc.holdsExact(texts[1], rec) {
			continue
		}
		field, given, found := "", false, false
		in := func(own string) bool { return dc.holdsExact(own, live) }
		for _, f := range x.payload().fields {
			if f.key || f.end > len(texts[0]) {
				continue
			}
			own := texts[0][f.start:f.end]
			if !dc.holdsExact(own, rec) && !dc.holdsExact(dc.rawExtra(own), rec) {
				continue
			}
			// Marked a whole copy of x's answer (Place.givenWhole) only
			// where the full round does not refute it (copiedInBoth): F's
			// view of x held, at a field of its answer that holds rec, the
			// instant F saw in the value, or x has no view; and then that
			// field. Else the first field that holds rec, unmarked (an
			// upsert's createdAt, older in F's view, before its updatedAt,
			// which held the value F saw: the copy is the updatedAt's).
			if !found {
				field, found = f.describe(), true
			}
			if answerPath(f.path) && dc.copiedInBoth(x, f, in) {
				field, given = f.describe(), true
				break
			}
		}
		return Place{Mock: x.mock, Field: field, Held: r.Value, unserved: !x.served, givenWhole: given}, true
	}
	return Place{}, false
}

// instantEchoes are the leaves of the call's recorded answer (resp) that
// hold rec, the moment the drifted instant was recorded with, as an instant
// of their own, exactly, in any form, a typed time of day's among them
// (seenIs), which no text form writes: places keploy answers with the moment
// F saw.
func (dc *DriftCache) instantEchoes(rec moment, resp []Leaf) []Leaf {
	var out []Leaf
	for _, a := range resp {
		if a.Value != "" && (dc.holdsExact(a.Value, rec) || dc.holdsExact(dc.rawExtra(a.Value), rec) || seenIs(a, a.Value, rec)) {
			out = append(out, a)
		}
	}
	return out
}

// holdsExact reports whether text holds m as a moment of its own, in any
// layout (instantSpans), to the nanosecond: an instant, or, where m is a time
// of day (a typed time of day, typedInstant.moment), a time of day, never an
// instant's time of day, whose key is another (instantKey.day). A text holds
// a typed time of day so as it holds a typed instant: a time of day written
// as a text and one its protocol types drifting together are a value and its
// copy, as two texts or two typed leaves are, never noise.
func (dc *DriftCache) holdsExact(text string, m moment) bool {
	if text == "" {
		return false
	}
	for _, sp := range dc.spansOf(text) {
		if sp.keyed && sp.key == m.key {
			return true
		}
	}
	return false
}

// instantEvidence is why the drifted instant is a variable of from, a place
// the echo found or a payload holding the moment it was recorded with, as a
// reason names it: what was found and where, why no read of the clock
// accounts for it, and how to correct it. It names no value.
func instantEvidence(from Place) string {
	at := placeAt(from)
	if from.Echo {
		return echoFound(at) + "; " + echoProvenance + ", so keploy follows it as a value from there. " + correction(from.Mock, from.unserved)
	}
	return "the moment it was recorded with stands in " + at + ", as an instant of its own; an instant equal to one an earlier place holds is followed as a value from there, whether or not it is a read of the clock. " + correction(from.Mock, from.unserved)
}
