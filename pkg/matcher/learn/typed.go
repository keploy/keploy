package learn

import (
	"fmt"
	"strings"
	"time"
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
// time of day (moved). Outside 0 to 24 hours it is written as it is, a text
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
	instantAfterCall = "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, and it moved by the lead that record's full round runs ahead of the recording, but the moment it was recorded with is later than its call, and a read of the clock a call carries is made no later than the call: it is built on the clock or another instant by an offset (a time to live, a due date), which keploy cannot follow yet. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
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
func (dc *DriftCache) classifyInstant(r, l Leaf, at time.Time, served []Earlier, raw []Raw, resp []Leaf, w *Window, unread string, places func() *offsetPlaces) Learned {
	ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
	rt, okR := readTyped(r)
	lt, okL := readTyped(l)
	if !okR || !okL {
		// keploy's limit only where it cannot decode its own recording of
		// an instant both sides type as one; a live value it cannot decode,
		// or one typed so on one side alone, is the app's.
		ln.Kind, ln.Why, ln.NotFollowed = KindUnaccounted, instantUnread(r, l), r.Type == Instant && l.Type == Instant && !okR
		return ln
	}
	rec, live := rt.moment(rt.res), lt.moment(lt.res)
	if from, ok := dc.instantServedFrom(r, rec, live, served); ok {
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
	if from, ok := dc.instantLeafFrom(r, rec, served); ok {
		ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
		return ln
	}
	if from, ok := dc.instantRawFrom(r, rec, at, raw); ok {
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
	if m, ok := places().offsetFrom([]driftedInstant{{rec: rec, live: live}}); ok {
		// A place keploy could not read is keploy's limit.
		ln.Kind, ln.Why, ln.NotFollowed = KindUnaccounted, offsetWhy(m), m.unread != ""
		return ln
	}
	// A copy of what F gave the application in another test or in a
	// recording made after the call, or a pick from it (the echo of those
	// places), before any provenance excuses it.
	if why := dc.echoedElsewhere([]driftedInstant{{rec: rec, live: live}}, at, served, raw, places); why != "" {
		ln.Kind, ln.Why, ln.NotFollowed = KindUnaccounted, why, places().unread != "" && why == places().unread
		return ln
	}
	if !instantClock(rt, lt, w, !dc.clientTests, at, dc.zone) {
		ln.Kind, ln.Why = KindUnaccounted, instantNotClock
		switch {
		case rt.names != lt.names:
			ln.Why = instantFormChangedWhy(rt.names, lt.names)
		case dc.clientTests && rt.names == namesTimeOfDay:
			ln.Why = timeOfDayClientTestWhy
		case dc.clientTests:
			ln.Why = clientTestWhy("", w != nil)
		case inClockRange(rt.at) && inClockRange(lt.at) && leadMoved(rt.at, lt.at, rt.res, lt.res, false):
			// Moved by the lead, as a read made after its call would
			// (readNoLaterThan): the reason says so, never that it did
			// not move by it.
			ln.Why = instantPastCall
			if rt.afterCall(at, dc.zone) {
				ln.Why = instantAfterCall
			}
		}
		return ln
	}
	// Noise, or an echo, only where the echo could read the test case's own
	// request, where the moment F saw may stand: an instant the clock
	// accounts for, which the unread request may hold too, is keploy's
	// limit. One the clock does not account for was refused above as the
	// app's: an ambiguous case never gets keploy's cause.
	if unread != "" {
		ln.Kind, ln.Why, ln.NotFollowed = KindUnaccounted, unreadRequestWhy(unread), true
		return ln
	}
	if echo := dc.instantEchoes(rec, resp); len(echo) > 0 {
		ln.Kind, ln.Echo = KindEcho, echo
	}
	return ln
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
			if !whole && !dc.isInstant(s.Leaf.Value, rec) && !seenIs(s.Leaf, s.Leaf.Value, rec) && !dc.instantCoDrifted(s.Leaf.Value, s.Live, rec, live) {
				continue
			}
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
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
		for _, f := range x.fields {
			if !f.key && dc.holdsRecorded(x.text[f.start:f.end], echoed) {
				return Place{Mock: x.mock, Field: f.describe(), Echo: true}, true
			}
		}
		if dc.holdsRecorded(x.text, echoed) || dc.holdsRecorded(dc.rawExtra(x.text), echoed) {
			return Place{Mock: x.mock, Echo: true}, true
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
// holds it.
func (dc *DriftCache) instantRawFrom(r Leaf, rec moment, at time.Time, raw []Raw) (Place, bool) {
	for _, x := range raw {
		if !x.earlierThan(at) {
			continue
		}
		texts := dc.searched(x, false)
		if !dc.holdsExact(texts[0], rec) && !dc.holdsExact(texts[1], rec) {
			continue
		}
		field := ""
		for _, f := range x.fields {
			if f.key || f.end > len(texts[0]) {
				continue
			}
			own := texts[0][f.start:f.end]
			if dc.holdsExact(own, rec) || dc.holdsExact(dc.rawExtra(own), rec) {
				field = f.describe()
				break
			}
		}
		return Place{Mock: x.mock, Field: field, Held: r.Value}, true
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
		return echoFound(at) + "; " + echoProvenance + ", so keploy follows it as a value from there. " + correction(from.Mock)
	}
	return "the moment it was recorded with stands in " + at + ", as an instant of its own; an instant equal to one an earlier place holds is followed as a value from there, whether or not it is a read of the clock. " + correction(from.Mock)
}
