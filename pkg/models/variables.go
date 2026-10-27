package models

import (
	"strconv"
	"strings"
	"time"
)

// MockVariables names, by variable, the places of a recorded dependency call
// that hold the value of a test variable: a value the application makes anew
// on every run and carries from where it first appears into later calls (an id
// it mints, a token it signs). The learn step writes it; the recording keeps
// the bytes it was recorded with. Replay reads it only under the proven switch
// (package variables says how a replay follows it).
//
// On disk, a top-level block of the mock document:
//
//	variables:
//	    orderId:
//	        recorded: o3fa9c21b7d4e
//	        bind: body.id
//	        answer: [body.id]
//
// Names are per test (a test case in keploy test, a test in keploy mock
// replay) and may repeat across tests. One namespace holds a test's mock
// variables and its test case's template keys (config.yaml `template:`), so:
//
//   - A variable that carries a template key's value is named after that key:
//     the same value when recorded and in each run the learn step watched,
//     not merely an equal recorded value (two counters both at 1 when
//     recorded are two values). A template key's name is reserved in every
//     test of the set: no other variable takes it. Its recorded value is the
//     key's config.yaml value; a dependency call that binds it is checked
//     against what the test case captures under the key, and a call that
//     names it with another recorded value is refused. Replay cannot tell,
//     from values, a misnamed variable from another value recorded equal: a
//     capture is checked only against a variable of its key's name.
//   - A value that first appears in the test case's own request (a template
//     key the request sends) is bound by the test case: the mocks name it with
//     use places only, no bind. keploy test sends those values for HTTP test
//     cases; a gRPC test case's request binds none.
type MockVariables map[string]MockVariable

// MockVariable is where one variable is in one call. A place is written in
// the grammar of variables.Parse: body.<json path>, url.path[<n>],
// url.query.<name>, header.<name>.
type MockVariable struct {
	// Recorded is the variable's value in the recording: every place below
	// held it when the call was recorded.
	Recorded string `json:"recorded" yaml:"recorded" bson:"recorded"`
	// Bind is the request place where the variable first appears in its test:
	// the live value there is the variable's value for the rest of the test.
	Bind string `json:"bind,omitempty" yaml:"bind,omitempty" bson:"bind,omitempty"`
	// Use lists request places that must hold the variable's value.
	Use []string `json:"use,omitempty" yaml:"use,omitempty" bson:"use,omitempty"`
	// Answer lists response places that hold the variable's value: they are
	// served with the value the variable has in the replay.
	Answer []string `json:"answer,omitempty" yaml:"answer,omitempty" bson:"answer,omitempty"`
	// Echo marks a value that belongs to this call alone (a request id the
	// dependency echoes): bound at Bind for this exchange, answered, and
	// never asked of another call.
	Echo bool `json:"echo,omitempty" yaml:"echo,omitempty" bson:"echo,omitempty"`
	// Clock is, for a variable that the learn step saw drift at its Bind
	// only with F's clock, the clock reads its recorded value held there
	// that moved by the full round's lead (learn.ClockReads): F's evidence
	// that it is a read of the app's clock, and when the read was made. Two
	// variables recorded with two values may share one live value in a
	// replay only when both are clock reads made in one second, as their
	// recordings say (ReadInOneSecond): a creation time and an expiry an hour
	// later both move with F's clock, and one written as the other is a
	// regression. Each is still asserted at every one of its places. Only the
	// learn step sets it, with a Bind; none for any other variable.
	Clock []ClockRead `json:"clock,omitempty" yaml:"clock,omitempty" bson:"clock,omitempty"`
}

// ClockRead is one clock read a recorded value holds, as the learn step read
// it: the instant it says (At), to the resolution it says it at (Res: the
// read was made in [At, At+Res)), and whether it is a time of day with no
// date (Day), compared modulo a day.
type ClockRead struct {
	At  time.Time     `json:"at" yaml:"at" bson:"at"`
	Res time.Duration `json:"res" yaml:"res" bson:"res"`
	Day bool          `json:"day,omitempty" yaml:"day,omitempty" bson:"day,omitempty"`
}

// ReadInOneSecond reports whether a and b, the clock reads of two variables
// (MockVariable.Clock), were made in one second, as their recordings say:
// each holds reads, as many as the other, and at each place the earlier
// read's resolution ends less than a second before the later one begins.
// Only two such variables may share one live value though recorded with
// two values (variables.Table.BindAll): two reads of one clock in one second.
func ReadInOneSecond(a, b []ClockRead) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].inOneSecond(b[i]) {
			return false
		}
	}
	return true
}

// inOneSecond reports whether reads r and o can have been made less than a
// second apart.
func (r ClockRead) inOneSecond(o ClockRead) bool {
	if r.Day != o.Day {
		return false
	}
	d := o.At.Sub(r.At)
	if r.Day {
		for d > 12*time.Hour {
			d -= 24 * time.Hour
		}
		for d <= -12*time.Hour {
			d += 24 * time.Hour
		}
	}
	first := r.Res
	if d < 0 {
		d, first = -d, o.Res
	}
	return d-first < time.Second
}

// ClockOffset is how far a value stands from a read of the application's
// clock: a value the application builds on its clock by a constant offset
// (an expiry it sets thirty minutes after now), which the learn step found,
// in its call's window both in the recording and in record's full round, at
// a read of that run's clock plus one offset from Min up to, not including,
// Max (learn.KindClockOffset). A replay keeps comparing it: the value it
// sees must be a read made in its own call's window plus such an offset
// (Holds), so a time to live changed by its Unit or more still fails. Min
// and Max are what the two runs' windows bound the offset to, never a
// tolerance, and the learn step learns one only where they, with Res, are
// narrower than the unit the offset reads in (Unit, Within): thirty minutes
// bounded to a few milliseconds, never to a few minutes. Min is above zero
// and Unit coarser than a second for any offset the learn step finds.
//
// Res is the resolution the learn step read the value at. Of an offset, the
// coarser of the one its encoding writes and the granularity both runs'
// values share (the greatest duration both are whole multiples of, from the
// Unix epoch). An application that cuts its value coarser than its
// encoding (Truncate(time.Second) stored as a millisecond $date) shows that
// cut in both values, and the sums that decide whether a replay can compare
// the offset (Within, at the learn step and in Holds) count it, never the
// encoding's finer one: a value cut to the minute, its time to live thirty
// minutes, read late in its minute stands only thirty minutes and a
// fraction of a second from its read even with a time to live of
// thirty-one, and a resolution of a millisecond would hold it. An offset's
// Res is read only in those sums and in the refusal of a cut
// (CutCoarserThan), so only to refuse more: two values that happen to share
// a coarser granularity make it coarser, never finer, the learn step bounds
// Min and Max at the encoding's resolution, not at Res, and Holds reads the
// replay's value at the replay's own resolution, not at Res. An offset
// whose Res is a cut the application made (CutCoarserThan: a whole number
// of the unit above the encoding's, a value cut to the second stored at the
// millisecond) is never learned: read at its encoding's resolution, a
// replay's value cut so would hold only where its read falls early in its
// cut, by when the replay ran. Nor are two values a whole number of that
// unit apart (learn's apartByUnits): a cut to it plus a time to live that
// is no whole number of cuts, which Res does not show.
//
// Of a Plain read, Res is the resolution its encoding writes, or, where both
// values are whole multiples of a step above it (CutOf: a unit, or a
// decimal step between units, such as 10 ms or 100 µs), the coarsest such
// step: a cut, the application's or one both values fell on by chance,
// never a span the two share between steps (492 ms), so the same recording
// learns the same Res whichever value record's full round read. Of a text
// or a number, the encoding's is the finer of the two values' forms (learn's
// textReadRes): a writer that drops a fraction's trailing zeros writes fewer
// digits where its read fell on a whole unit, so a fraction's digits bound
// its writer's resolution from above only, and one value that happens to
// end in zeros never coarsens Res. The learn step keeps only a cut of a
// millisecond or finer. One coarser it refuses, naming the cut (learn's
// cutWhy), and the recording's test with it, whatever value a replay would
// carry: checked at its encoding's resolution a value cut so would pass or
// fail by where its read fell in the cut, and checked at the cut it could
// stand a whole cut stale. A replay reads a Plain Res above its value's
// encoding as a cut only where its value is a whole multiple of a step
// above its encoding, no coarser than Res, too (CutOf; learn's
// CheckClockRead): a value that is not is checked at its own resolution,
// which is stricter; one that shows a cut of a millisecond or finer is
// checked at that cut (it stands for its whole cut, so a read it holds may
// lie up to one cut, a millisecond at most, before the call's span); one
// that shows a coarser cut (against a Res learned at an encoding coarser
// than the replay's value writes) is refused, naming the cut.
//
// Zone says in which zone a replay reads the value at the place, where the
// learn step learned a wall clock of no zone there (a text, or a type of its
// protocol, that names no zone: "2026-10-27 09:00:00", a Postgres
// timestamp): ZoneUTC in UTC, ZoneApp in the application's own zone, as
// the replay's tracker knows it (learn.Span.Zone); ZoneEither in either of
// the two, where the recording could not tell them apart at the value's
// resolution (a date whose day is one in both zones at its call, or an
// application whose zone is UTC there); "" for a value that names its
// moment, read as it names it, as a recording learned before Zone was read
// is. A replay reads a wall clock of no zone only under ZoneUTC, ZoneApp or
// ZoneEither, the zones the learn step read it in, and refuses one under
// ZoneApp or ZoneEither where it cannot tell the application's zone, so it
// never checks a value in a zone the learn step did not read it in.
// ZoneApart is the Meet of two learnings that read the place in zones no
// one zone holds, which holds nothing (Empty), and so does a Zone this
// keploy does not read (ZoneKnown).
//
// Layout is the form the learn step saw the value written in, in both of
// its runs, which a replay's value must be written in too: LayoutTyped for a
// type of its protocol (a BSON datetime, a Postgres timestamp), or the
// layout of a text or a number, which names the resolution its form writes
// (an ISO 8601 date, its time to the minute or to the second, an epoch in
// seconds or in milliseconds, a time of day: ClockLayoutNames), or
// LayoutTypedTimeOfDay for a type of its protocol that writes a time of day.
// A time of day names no day (LayoutTimeOfDay): a replay reads it modulo a
// day, in the zone Zone names (never "": the learn step reads the zone a
// time of day was read in, as it does a wall clock's). A fraction of a second is
// no part of it: a writer that drops a fraction's trailing zeros (Go's
// RFC3339Nano) writes a value to the second, or with fewer digits, where
// its clock read fell on a whole second, which a layout that held the
// fraction would refuse by where the read fell. So is no zone a value
// names part of it (Zone reads that). "" for a read learned before Layout
// was, which a replay reads in any layout, as it did then. LayoutApart is
// the Meet of two learnings that saw the place in different layouts, which
// holds nothing (Empty).
type ClockOffset struct {
	Min    time.Duration `json:"min" yaml:"min" bson:"min"`
	Max    time.Duration `json:"max" yaml:"max" bson:"max"`
	Res    time.Duration `json:"res" yaml:"res" bson:"res"`
	Zone   string        `json:"zone,omitempty" yaml:"zone,omitempty" bson:"zone,omitempty"`
	Layout string        `json:"layout,omitempty" yaml:"layout,omitempty" bson:"layout,omitempty"`
}

// The layouts a ClockOffset's Layout names beside those of a text or a
// number (ClockLayoutNames).
const (
	// LayoutTyped: a type of the value's protocol, which writes its own
	// resolution.
	LayoutTyped = "typed"
	// LayoutApart: two learnings of one place saw it in different
	// layouts (Meet); it holds nothing.
	LayoutApart = "apart"
	// LayoutTypedTimeOfDay: a type of the value's protocol that writes a
	// time of day (a Postgres time), which names no day: a replay reads it
	// modulo a day (LayoutTimeOfDay).
	LayoutTypedTimeOfDay = "typed-time-of-day"
)

// LayoutTimeOfDay reports whether layout, a Layout, writes a time of day,
// which names no day: LayoutTypedTimeOfDay, or a text's "time-minute" or
// "time-second" (ClockLayoutNames). A replay reads a value at a place
// learned so as its time into its day, in the zone Zone names, against its
// call's span read modulo a day (the day it begins on and the one it ends
// on), and reads a value of any other layout there as none; nor does it
// read a time of day where the place was learned in a layout that names a
// day.
func LayoutTimeOfDay(layout string) bool {
	return layout == LayoutTypedTimeOfDay || layout == "time-minute" || layout == "time-second"
}

// ClockLayoutNames are, by Layout, the forms of a text or a number a read of
// the clock is learned in, as a person reads them; LayoutTyped and
// LayoutApart are not among them. A Layout this keploy does not name holds
// nothing (LayoutKnown).
var ClockLayoutNames = map[string]string{
	"rfc1123":          "an HTTP date (RFC 1123), to the second",
	"iso-date":         "an ISO 8601 date",
	"iso-hour":         "an ISO 8601 date and hour",
	"iso-minute":       "an ISO 8601 date and time to the minute",
	"iso-second":       "an ISO 8601 date and time to the second or a fraction of it",
	"iso-minute-space": "an ISO 8601 date and time to the minute, with a space before the time",
	"iso-second-space": "an ISO 8601 date and time to the second or a fraction of it, with a space before the time",
	"compact":          "a compact date and time (yyyymmddThhmmss), to the second",
	"uuid1":            "a version 1 UUID's time",
	"uuid7":            "a version 7 UUID's time",
	"epoch-s":          "a number of seconds since the epoch, whole or with a fraction",
	"epoch-ms":         "a number of milliseconds since the epoch",
	"epoch-us":         "a number of microseconds since the epoch",
	"epoch-ns":         "a number of nanoseconds since the epoch",
	"time-minute":      "a time of day to the minute",
	"time-second":      "a time of day to the second or a fraction of it",
}

// LayoutKnown reports whether o's Layout is one a replay reads a value's
// form against: "" (any form, as learned before Layout was), LayoutTyped,
// LayoutTypedTimeOfDay or one of ClockLayoutNames. Holds holds nothing for any other (LayoutApart,
// or a Layout this keploy does not name), which it never checks in some
// other form.
func (o ClockOffset) LayoutKnown() bool {
	if o.Layout == "" || o.Layout == LayoutTyped || o.Layout == LayoutTypedTimeOfDay {
		return true
	}
	_, ok := ClockLayoutNames[o.Layout]
	return ok
}

// LayoutName is how a person reads layout, a Layout or the layout of a
// replay's value: "a type of its protocol" for LayoutTyped, a name of
// ClockLayoutNames, or the layout quoted where keploy names none.
func LayoutName(layout string) string {
	switch layout {
	case LayoutTyped:
		return "a type of its protocol"
	case LayoutTypedTimeOfDay:
		return "a time of day its protocol types"
	}
	if n, ok := ClockLayoutNames[layout]; ok {
		return n
	}
	return strconv.Quote(layout)
}

// MeetLayout is the Layout two learnings of one place saw it in, o's and
// layout (meetLayouts), as Meet reads it.
func (o ClockOffset) MeetLayout(layout string) string { return meetLayouts(o.Layout, layout) }

// meetLayouts is the Layout a replay reads a place's form against that two
// learnings saw it in, a and b: the one both saw, the other where one is ""
// (learned before Layout was: any form), LayoutApart where they differ.
func meetLayouts(a, b string) string {
	switch {
	case a == b || b == "":
		return a
	case a == "":
		return b
	}
	return LayoutApart
}

// The zones a ClockOffset's Zone names (ClockOffset).
const (
	// ZoneUTC: a wall clock of no zone, read in UTC.
	ZoneUTC = "utc"
	// ZoneApp: a wall clock of no zone, read in the application's zone.
	ZoneApp = "app"
	// ZoneEither: a wall clock of no zone the recording could not tell
	// UTC's from the application's at its resolution, read in each of the
	// two: a replay passes it where either reading holds.
	ZoneEither = "either"
	// ZoneApart: two learnings of one place read it in zones no one zone
	// holds (Meet); it holds nothing.
	ZoneApart = "apart"
)

// ZoneKnown reports whether o's Zone is one a replay reads a value in: ""
// (as the value names its moment), ZoneUTC, ZoneApp or ZoneEither. Holds
// holds nothing for any other: ZoneApart, or a Zone this keploy does not
// read, which it never checks in some other zone.
func (o ClockOffset) ZoneKnown() bool {
	return o.Zone == "" || o.Zone == ZoneUTC || o.Zone == ZoneApp || o.Zone == ZoneEither
}

// meetZones is the zone a replay reads a place in that two learnings read
// in a and b: the one zone both hold (ZoneEither holds UTC and the
// application's, so its meet with either one is that one); ZoneApart where
// none does (UTC and the application's zone, or a wall clock of no zone and
// a value that names its zone).
func meetZones(a, b string) string {
	switch {
	case a == b:
		return a
	case a == ZoneEither && (b == ZoneUTC || b == ZoneApp):
		return b
	case b == ZoneEither && (a == ZoneUTC || a == ZoneApp):
		return a
	}
	return ZoneApart
}

// offsetUnits are the units an offset reads in, coarsest first.
var offsetUnits = []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second, time.Millisecond, time.Microsecond, time.Nanosecond}

// Unit is the coarsest unit (a day, an hour, a minute, a second, a
// millisecond, a microsecond, a nanosecond) one of whose multiples lies from
// Min up to, not including, Max: the unit String reads the offset in ("clock
// + 30m": a minute). 0 for an empty o.
func (o ClockOffset) Unit() time.Duration {
	for _, u := range offsetUnits {
		if ceilTo(o.Min, u) < o.Max {
			return u
		}
	}
	return 0
}

// CutCoarserThan reports whether o's Res is a cut the application made
// coarser than an encoding that writes enc: above enc and a whole number of
// the next unit of offsetUnits above enc (a second above a millisecond, a
// millisecond above a microsecond, a minute above a second, an hour above a
// minute), or above an enc no unit is above. Both runs' values cut to the
// second and stored at the millisecond share a whole number of seconds;
// two values the application does not cut share one by chance, one time in
// about a million at a millisecond's or a microsecond's encoding and one in
// 3,600 at a second's, and an offset's are refused the same (learn's
// clockOffset). A Res above enc that is no whole number of the next unit
// (half a second two millisecond values share by chance, or 2 s two second
// values do) is none: the sums count it, and the replay's value is read at
// its own resolution. A Res of enc itself (an hour's time to live at a
// minute's encoding) is none either. A Plain read's Res is read by its
// steps (CutOf), not by this (learn's CheckClockRead).
func (o ClockOffset) CutCoarserThan(enc time.Duration) bool {
	if o.Res <= enc {
		return false
	}
	u := UnitAbove(enc)
	return u == 0 || o.Res%u == 0
}

// UnitAbove is the finest of the units an offset reads in (a day, an hour,
// a minute, a second, a millisecond, a microsecond, a nanosecond) coarser
// than enc, an encoding's resolution: a second above a millisecond, a minute
// above a second. 0 above a day.
func UnitAbove(enc time.Duration) time.Duration {
	for i := len(offsetUnits) - 1; i >= 0; i-- {
		if u := offsetUnits[i]; u > enc {
			return u
		}
	}
	return 0
}

// cutSteps are the steps a Plain read's cut is read in, coarsest first:
// the units an offset reads in (offsetUnits) and, between the units below a
// second, the decimal steps a fraction's digits write (100 ms and 10 ms,
// 100 µs and 10 µs, 100 ns and 10 ns): a database column of two fraction
// digits cuts its value to 10 ms. Each is a whole number of the next finer
// one.
var cutSteps = []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second, 100 * time.Millisecond, 10 * time.Millisecond, time.Millisecond, 100 * time.Microsecond, 10 * time.Microsecond, time.Microsecond, 100 * time.Nanosecond, 10 * time.Nanosecond, time.Nanosecond}

// CutOf is the coarsest of cutSteps above enc and no coarser than upTo that
// n, nanoseconds counted from the Unix epoch (or into a day), is a whole
// multiple of: the cut a value written at enc shows, whose encoding cannot
// show one at enc or finer. 0 where n shows none. A decimal step coarser
// than a millisecond (10 ms, 100 ms) counts only where it is at least 1,000
// times enc: a millisecond's value falls on a whole 100 ms one time in ten
// by chance, and the learn step refuses a cut coarser than a millisecond
// (learn's cutWhy), so two values of a writer of milliseconds would be
// refused one recording in a hundred. A unit (a second, a minute, an hour,
// a day) counts at any ratio: applications cut to units, and two values of
// a writer of seconds both fall on a whole minute one recording in 3,600.
// Steps nest, so a value a whole multiple of a step is one of every finer
// step, and CutOf is the one cut all values that are whole multiples of it
// share.
func CutOf(n int64, enc, upTo time.Duration) time.Duration {
	for _, u := range cutSteps {
		if u > enc && u <= upTo && n%int64(u) == 0 && (u <= time.Millisecond || u >= time.Second || u >= 1000*enc) {
			return u
		}
	}
	return 0
}

// ceilTo is the least multiple of u not below d.
func ceilTo(d, u time.Duration) time.Duration {
	q := d / u
	if q*u < d {
		q++
	}
	return q * u
}

// Within reports whether spans, each at least zero, add up to less than o's
// Unit. The learn step learns o only where its width and its resolution are
// (o.Within(o.Max-o.Min, o.Res)), so it holds one multiple of its unit, the
// one String names; Holds asks the same of the replay's window with them,
// so a time to live changed by a unit or more never holds, however the
// replay's read falls in its window, as long as the application cuts its
// value no coarser at the replay than in the two runs the learn step read,
// and no coarser than Res there: a value cut to a whole number of the unit
// above its encoding the learn step refuses (CutCoarserThan, and learn's
// apartByUnits), but one cut finer than that unit (10 ms at a
// millisecond's encoding) plus a time to live that is no whole number of
// cuts stands up to a whole cut from its read, which Res does not count. A
// time to live changed by less than that sum may hold, by where the read
// falls.
func (o ClockOffset) Within(spans ...time.Duration) bool {
	left := o.Unit()
	for _, s := range spans {
		if s >= left {
			return false
		}
		left -= s
	}
	return true
}

// Holds reports whether at, a value cut to resolution res (truncated: it
// stands for [at, at+res)), is a read of the clock made from from to to (a
// call's window on the application's clock) plus an offset of o; of a Plain
// o (a zero offset, a plain read of the clock), a read made there itself
// (holdsRead). Of any other o: some read
// t in [from, to] and offset d in [o.Min, o.Max) put t+d in [at, at+res),
// so [from+o.Min, to+o.Max) and [at, at+res) meet. It reads at at res, the
// replay's own resolution, never at o.Res, so a granularity the learn step
// read in both runs' values accepts nothing more. It holds nothing where
// the window, o's width and the coarser of res and o.Res add up to o's Unit
// or more (Within): there a value whose time to live changed by a unit
// could hold, by where the read fell, and the replay fails closed. Nor for
// an o whose Unit is a second or finer: an application that cuts its value
// to that unit makes a time to live changed by one look the same. Nor for
// an o whose Res is a cut coarser than res's encoding (CutCoarserThan),
// which the learn step never learns, or one it learned at an encoding
// coarser than the replay's (a millisecond's Res at a nanosecond's
// replay): the replay's value, read at res, would hold by where its read
// fell in the cut. Nor for an o whose Zone a replay reads no value in
// (ZoneKnown: ZoneApart, or one this keploy does not read). at is the
// value's instant as read in o's Zone: the caller reads a wall clock of no
// zone there (learn.CheckClockRead), and under ZoneEither in each of UTC and
// the application's zone, passing the value where either holds. A window with no start (a zero to
// is before any from), one that ends before it starts, a res of none or an
// empty o (it has no Unit) holds nothing, and so does an o the learn step
// never finds (a Min of zero or below, a Max not above Min, or a Res of
// none or below: one built without the resolution it was learned at could
// have its sums read finer than the application cuts its value), whose
// width could overflow or whose Unit could swallow the sign of the offset.
func (o ClockOffset) Holds(at time.Time, res time.Duration, from, to time.Time) bool {
	if !o.ZoneKnown() || !o.LayoutKnown() {
		return false
	}
	if o.Plain() {
		return o.holdsRead(at, res, from, to)
	}
	if o.Min <= 0 || o.Max <= o.Min || o.Res <= 0 || o.Unit() <= time.Second || from.IsZero() || to.Before(from) || res <= 0 || o.CutCoarserThan(res) {
		return false
	}
	if !o.Within(to.Sub(from), o.Max-o.Min, max(res, o.Res)) {
		return false
	}
	return from.Add(o.Min).Before(at.Add(res)) && at.Before(to.Add(o.Max))
}

// Plain reports whether o is a zero offset: a plain read of the
// application's clock (learn.Learned.Read), its Min and Max both zero, which
// Holds reads as this call's own now. An o the learn step learned with an
// offset (learn.KindClockOffset) has a Min above zero.
func (o ClockOffset) Plain() bool { return o.Min == 0 && o.Max == 0 }

// Meet is o and p, two learnings of one place's clock read, as one: the
// offsets both hold (from the later Min up to the earlier Max: a Plain o
// holds the offset zero alone) at the coarser of their resolutions. Each
// holds the true offset of a read its learn step saw, so the one place's
// value holds it only where both do; of an offset, a Res read coarser
// refuses more, never less; of a Plain read, a coarser Res is a cut of a
// millisecond or finer (or an encoding's resolution), at which a replay
// checks a value that shows it, a read up to one cut before its span
// (CheckClockRead in learn). Two that share no offset (a Plain and one with an offset, or
// two offsets apart), or that read it in zones no one zone holds (UTC and
// the application's: ZoneApart, meetZones), meet as an Empty one, which
// holds nothing; ZoneEither meets UTC or the application's zone as that
// one, the zone both hold. So do two that saw it written in different
// layouts (LayoutApart, meetLayouts); a Layout of "" (learned before Layout
// was) meets another as that one. Meet is the
// same in either order and however a place's learnings are grouped, so a
// recording paired many times keeps one answer whatever order its
// pairings came in.
func (o ClockOffset) Meet(p ClockOffset) ClockOffset {
	return ClockOffset{Min: max(o.Min, p.Min), Max: min(o.Max, p.Max), Res: max(o.Res, p.Res), Zone: meetZones(o.Zone, p.Zone), Layout: meetLayouts(o.Layout, p.Layout)}
}

// Empty reports whether o is the Meet of learnings of one place that share
// no offset: a Min above zero and a Max not above it; or that read it in
// zones no one zone holds (ZoneApart). Holds holds nothing for it, and String says
// why.
func (o ClockOffset) Empty() bool {
	return o.Min > 0 && o.Max <= o.Min || o.Zone == ZoneApart || o.Layout == LayoutApart
}

// MergeClocks is a and b, two sets of the places a call's learn steps
// learned as clock reads (MockSpec.Clocks), as one fresh map: every place
// of either, a place of both as the Meet of its two offsets. nil when
// neither holds a place.
func MergeClocks(a, b map[string]ClockOffset) map[string]ClockOffset {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]ClockOffset, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if cur, ok := out[k]; ok {
			v = cur.Meet(v)
		}
		out[k] = v
	}
	return out
}

// holdsRead is Holds of a Plain o: at, cut to resolution res (it stands for
// [at, at+res)), is a read of the clock made from from to to, the call's
// window on the application's clock, itself: some read t in [from, to] lies
// in [at, at+res). A read the application made for the call lies in its
// window whenever the replay ran, so a value the replay's own clock wrote
// always holds and one moved by a resolution past the window never does:
// the verdict does not turn on timing. It holds nothing for a res of none,
// a window with no start or that ends before it starts, an o with no Res
// (one built without the resolution it was learned at), or an o whose Res
// is coarser than res: a cut (a value the application cut to the second,
// stored at the millisecond, would hold only where its read fell early
// enough in its second, by when the replay ran), or one learned at an
// encoding coarser than the replay's. Learn's CheckClockRead decides those
// first, by the cut the value itself shows, and asks Holds of o read at the
// resolution it checks the value at. There is no unit to tell apart, so the
// window's width bounds nothing more.
func (o ClockOffset) holdsRead(at time.Time, res time.Duration, from, to time.Time) bool {
	if o.Res <= 0 || res <= 0 || from.IsZero() || to.Before(from) || o.Res > res {
		return false
	}
	return from.Before(at.Add(res)) && !at.After(to)
}

// String is o as a person reads it: "clock" for a Plain o; "clock + 30m",
// the one multiple from Min
// up to Max of its Unit, and the two bounds after it where they are more
// than a nanosecond apart: "clock + 30m (from 29m59.998s to 30m0.001s)".
// Where they hold more than one multiple, it names no offset: "clock plus an
// offset from 29m59.96s to 32m59.956s"; an Empty o says it holds none:
// "clock reads no one offset holds (from 30m up to 0s)". A Zone of a wall
// clock of no zone follows it: "clock, a wall clock of no zone in the
// application's zone"; one of ZoneApart, or one this keploy does not read,
// says so instead. It never decides anything.
func (o ClockOffset) String() string {
	switch {
	case o.Zone == ZoneApart:
		return "clock reads in no one zone (one place learned as a wall clock of no zone in UTC, in the application's zone, or as a value that names its zone, at different calls)"
	case !o.ZoneKnown():
		return "clock read in a zone this keploy does not read (" + strconv.Quote(o.Zone) + ")"
	case o.Layout == LayoutApart:
		return "clock reads in no one form (one place learned written in different layouts at different calls)"
	case !o.LayoutKnown():
		return "clock read written in a layout this keploy does not read (" + strconv.Quote(o.Layout) + ")"
	}
	return o.offsetString() + o.zoneString() + o.layoutString()
}

// layoutString is the form o's Layout says a replay's value is written in,
// as String writes it after the zone: "" for a Layout of "" (any form), and
// for LayoutTyped and LayoutTypedTimeOfDay, whose type its protocol's place
// already says (and zoneString that it is a time of day).
func (o ClockOffset) layoutString() string {
	if o.Layout == "" || o.Layout == LayoutTyped || o.Layout == LayoutTypedTimeOfDay {
		return ""
	}
	return ", written as " + LayoutName(o.Layout)
}

// zoneString is where o's Zone says a replay reads a wall clock of no zone,
// or a time of day (LayoutTimeOfDay), as String writes it after the offset:
// "" for a value that names its zone.
func (o ClockOffset) zoneString() string {
	if LayoutTimeOfDay(o.Layout) {
		switch o.Zone {
		case ZoneUTC:
			return ", a time of day in UTC"
		case ZoneApp:
			return ", a time of day in the application's zone"
		case ZoneEither:
			return ", a time of day in UTC or in the application's zone"
		}
		return ""
	}
	switch o.Zone {
	case ZoneUTC:
		return ", a wall clock of no zone in UTC"
	case ZoneApp:
		return ", a wall clock of no zone in the application's zone"
	case ZoneEither:
		return ", a wall clock of no zone in UTC or in the application's zone"
	}
	return ""
}

// offsetString is String of o's offset alone.
func (o ClockOffset) offsetString() string {
	if o.Plain() {
		return "clock"
	}
	if o.Empty() {
		return "clock reads no one offset holds (from " + ShortDuration(o.Min) + " up to " + ShortDuration(o.Max) + ")"
	}
	u := o.Unit()
	if u == 0 || ceilTo(o.Min, u)+u < o.Max {
		return o.Range()
	}
	s := "clock + " + ShortDuration(ceilTo(o.Min, u))
	if o.Max-o.Min > 1 {
		s += " (from " + ShortDuration(o.Min) + " to " + ShortDuration(o.Max) + ")"
	}
	return s
}

// Range is o's bounds as a person reads them, naming no offset: "clock plus
// an offset from 29m42.845s to 30m41.368s". The learn step's refusal of an
// o too wide for a replay to compare (Within) names it so.
func (o ClockOffset) Range() string {
	return "clock plus an offset from " + ShortDuration(o.Min) + " to " + ShortDuration(o.Max)
}

// ShortDuration is d as time.Duration writes it, without its trailing zero
// units ("30m", "1h", "1h30m"), and a whole number of days as days ("30d").
func ShortDuration(d time.Duration) string {
	if d != 0 && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
	}
	return s
}

// Clone returns a copy of v that shares no map or slice with it; nil for an
// empty v.
func (v MockVariables) Clone() MockVariables {
	if len(v) == 0 {
		return nil
	}
	out := make(MockVariables, len(v))
	for name, mv := range v {
		mv.Use = append([]string(nil), mv.Use...)
		mv.Answer = append([]string(nil), mv.Answer...)
		mv.Clock = append([]ClockRead(nil), mv.Clock...)
		out[name] = mv
	}
	return out
}

// MergeVariables is a and b as one block (neither is changed): a variable in
// both keeps a's bind and both's uses and answers, and is an echo only when
// both say so. Clock goes with the bind it was read at: a's bind keeps a's,
// one taken from b keeps b's, and a variable both bind is a clock read only
// when both say so.
func MergeVariables(a, b MockVariables) MockVariables {
	out := a.Clone()
	for name, v := range b {
		if out == nil {
			out = MockVariables{}
		}
		cur, ok := out[name]
		if !ok {
			out[name] = MockVariables{name: v}.Clone()[name]
			continue
		}
		if cur.Bind == "" {
			cur.Bind, cur.Clock = v.Bind, append([]ClockRead(nil), v.Clock...)
		} else if v.Bind != "" && len(v.Clock) == 0 {
			cur.Clock = nil
		}
		cur.Use = appendNewPlaces(cur.Use, v.Use...)
		cur.Answer = appendNewPlaces(cur.Answer, v.Answer...)
		cur.Echo = cur.Echo && v.Echo
		out[name] = cur
	}
	return out
}

// MergeBinds is a and b, variables bound at other mocks by mock, as one
// (neither is changed).
func MergeBinds(a, b map[string]MockVariables) map[string]MockVariables {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]MockVariables, len(a)+len(b))
	for m, v := range a {
		out[m] = v.Clone()
	}
	for m, v := range b {
		out[m] = MergeVariables(out[m], v)
	}
	return out
}

// appendNewPlaces appends to list each of vs it does not hold yet.
func appendNewPlaces(list []string, vs ...string) []string {
	for _, v := range vs {
		dup := false
		for _, have := range list {
			if have == v {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, v)
		}
	}
	return list
}

// TestVariablesResp is the answer of GET /agent/variables.
type TestVariablesResp struct {
	Variables []VariableBinding `json:"variables"`
}

// VariableBinding is the value a test variable has in a test, and where it
// was taken: a dependency call the test made (Mock is the mock's name), or
// the test case itself (Mock is "test case <name>"). keploy test sends the
// agent the values its test case's request carries, and reads back what the
// test bound, for its report.
type VariableBinding struct {
	Name string `json:"name" yaml:"name"`
	// Value is the live value.
	Value string `json:"value" yaml:"value"`
	// Recorded is the value the recording held where it was taken.
	Recorded string `json:"recorded" yaml:"recorded"`
	Mock     string `json:"mock" yaml:"mock"`
	Place    string `json:"place" yaml:"place"`
	// Given marks a value the test case's request gave (its template
	// value), not one the application made: it binds the variable for the
	// calls that use it, and is never another variable's value.
	Given bool `json:"given,omitempty" yaml:"given,omitempty"`
	// Clock is, for a binding of a clock variable, its clock reads
	// (MockVariable.Clock): it may share its live value with another clock
	// variable's binding whose reads were made in the same second
	// (ReadInOneSecond), and with no other.
	Clock []ClockRead `json:"clock,omitempty" yaml:"clock,omitempty"`
}
