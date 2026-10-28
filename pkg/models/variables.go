package models

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	// A replay reads a ClockOffset's AppZone on any machine, one with no
	// zone files too (ZoneLocation).
	_ "time/tzdata"
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
// one its encoding writes, or the cut its two values show (founder decision,
// 2026-10-27; a clock read plus a constant offset, then cut, is a clock
// read, 2026-10-28): the coarsest step of CutOf above the encoding that both
// values, or the gap between them, are whole multiples of (an application
// that cuts its value and then adds a time to live that is no whole number
// of cuts, 30m30s after a minute's start, puts its two values a whole number
// of cuts apart, though neither is a whole cut), no coarser than a minute
// (learn's offsetReading: a cut to an hour or a day, which a zone's offset
// moves, shows no offset). The learn step bounds Min and Max at it, as
// standing for its whole cut (truncated: a value v stands for reads from v
// up to v plus Res), and the sums that decide whether a replay can compare
// the offset (Within, at the learn step and in Holds) count it, so thirty
// minutes cut to the minute, which a replay could not tell from thirty-one,
// is refused as too wide, and thirty days cut to the second is learned.
// Holds checks a replay's value at Res where it is such a step coarser than
// the value's own encoding (a cut), as standing for its whole cut, within
// one Res of the call's span plus the offset on either side, as a plain
// read's; at the value's own resolution otherwise. A Res learned before
// cuts were (the granularity two values shared by chance, 500 ms or 2 s) is
// no step: Holds reads it only in its sums, to refuse more.
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
// end in zeros never coarsens Res. A cut, of any step (founder decision,
// 2026-10-27), is learned, never refused: a value cut to the second, the
// minute, the day, or to 10 or 100 ms. A replay reads a Plain Res above its
// value's encoding as a cut only where its value is a whole multiple of a
// step above its encoding, no coarser than Res, too (CutOf; learn's
// CheckClockRead): one that is, is checked at that cut, as standing for its
// whole cut: it holds within one cut of the call's span on either side (a
// value v cut to c, where the start of the cut that holds from, less c, <=
// v <= to+c: holdsRead); one that is not is checked so at its own
// resolution, which is stricter. A value read in the call's span holds
// either way, wherever its read fell in its cut, cut down or rounded, and so
// does one up to one cut stale or ahead. Both the learn step and a replay
// read a cut in UTC, in the offset the value writes, and in the
// application's zone (learn's zonedRes, CheckClockRead), so an hour or a
// day the application cuts in its own zone (an hour in UTC+05:30, a day in
// UTC+02:00) is read as one.
//
// Zone says in which zone a replay reads the value at the place, where the
// learn step learned a wall clock of no zone there (a text, or a type of its
// protocol, that names no zone: "2026-10-27 09:00:00", a Postgres
// timestamp): ZoneUTC in UTC, ZoneApp in the application's own zone, the
// one the replay reads from the application's process, else the one
// AppZone names, else in the agent's own (learn.Span); ZoneEither in either of
// the two, where the recording could not tell them apart at the value's
// resolution (a date whose day is one in both zones at its call, or an
// application whose zone is UTC there); "" for a value that names its
// moment, read as it names it, as a recording learned before Zone was read
// is. A replay reads a wall clock of no zone only under ZoneUTC, ZoneApp or
// ZoneEither, the zones the learn step read it in, and refuses one under
// ZoneApp or ZoneEither that names no AppZone where it cannot tell the
// application's zone, so it
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
//
// AppZone names the application's zone the learn step read the value in,
// where what it learned depends on that zone (AppZoneMatters): a wall clock
// of no zone read in it (ZoneApp, ZoneEither), or a value that names its
// moment cut coarser than a minute, to an hour or a day, which a zone's
// offset moves (an hour in UTC+05:30 is no hour in UTC). It is "UTC" or a
// fixed offset ("+05:30") of a zone with no change of offset ahead, else
// an IANA name ("Europe/Berlin"), as ZoneNameOf names it (ZoneLocation
// reads each). A replay reads the application's zone as
// the one its process writes its clock in in that run, where keploy reads
// it from the process (a wall clock of no zone is the application's own),
// else as AppZone, on any machine (learn's CheckClockRead): a recording
// learned on a laptop in IST checks the same cut in IST on a CI machine in
// UTC whose application's zone keploy cannot read, and in UTC where it
// reads the application now writing in UTC. Either is the zone the process
// started with, as its environment (TZ) and its files (/etc/localtime)
// name it, never one it sets later (learn's ProcZones).
// "" where keploy could not read the zone of the process that made the
// call, or could not name it (a zone file copied with no name, whose offset
// changes): the learn step read the value in the process's zone where it
// read one it could not name, else in the agent's own, either of which may
// be any zone; a replay that cannot read the application's zone either
// reads it in the agent's own, except that it refuses a plain read cut to
// an hour or coarser, whose cut the zone moves. A recording learned before
// AppZone was reads the same. Two learnings of one place read with the
// application's zone as two zones, two named ones or a named one and one
// of no name, meet as AppZoneApart (Meet), which names none.
type ClockOffset struct {
	Min     time.Duration `json:"min" yaml:"min" bson:"min"`
	Max     time.Duration `json:"max" yaml:"max" bson:"max"`
	Res     time.Duration `json:"res" yaml:"res" bson:"res"`
	Zone    string        `json:"zone,omitempty" yaml:"zone,omitempty" bson:"zone,omitempty"`
	Layout  string        `json:"layout,omitempty" yaml:"layout,omitempty" bson:"layout,omitempty"`
	AppZone string        `json:"app_zone,omitempty" yaml:"app_zone,omitempty" bson:"app_zone,omitempty"`
}

// AppZoneMatters reports whether what the learn step learned of o depends
// on the application's zone, so it names that zone in AppZone: a wall clock
// of no zone read in it (ZoneApp, ZoneEither), or a plain read of a value
// that names its moment cut coarser than a minute (an hour, a day), whose
// cut a replay reads in the application's zone beside UTC and the offset it
// writes (learn's cutZones). A minute or any finer cut is one in every zone
// (zones' offsets are whole minutes); a wall clock read in UTC (ZoneUTC) is
// cut in UTC alone.
func (o ClockOffset) AppZoneMatters() bool {
	return o.Zone == ZoneApp || o.Zone == ZoneEither || o.Zone == "" && o.Plain() && o.Res > time.Minute
}

// ZoneLocation is the zone name names (ClockOffset.AppZone): "UTC", a fixed
// offset ("+05:30", "-03:00") or an IANA name ("Asia/Kolkata"), read from
// the host's zone files, else from the ones built into keploy (time/tzdata),
// so a replay reads it on any machine. An error for any other name.
func ZoneLocation(name string) (*time.Location, error) {
	if name == "UTC" {
		return time.UTC, nil
	}
	if len(name) == 6 && (name[0] == '+' || name[0] == '-') && name[3] == ':' {
		h, herr := strconv.Atoi(name[1:3])
		m, merr := strconv.Atoi(name[4:6])
		if herr != nil || merr != nil || h > 14 || m > 59 || !isDigits(name[1:3]) || !isDigits(name[4:6]) {
			return nil, fmt.Errorf("%q is no fixed offset keploy reads", name)
		}
		off := h*3600 + m*60
		if name[0] == '-' {
			off = -off
		}
		return time.FixedZone(name, off), nil
	}
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("%q names no zone", name)
	}
	if loc, ok := zoneLocations.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zoneLocations.Store(name, loc)
	return loc, nil
}

// zoneLocations are the IANA zones ZoneLocation has read, by name: a
// replay checks a place's AppZone at every call, and reads its file once.
var zoneLocations sync.Map

// sameZoneAt reports whether a and b write at with one offset and next
// change it at one instant (time.Time.ZoneBounds; none in either).
func sameZoneAt(a, b *time.Location, at time.Time) bool {
	ta, tb := at.In(a), at.In(b)
	_, oa := ta.Zone()
	_, ob := tb.Zone()
	_, ea := ta.ZoneBounds()
	_, eb := tb.ZoneBounds()
	return oa == ob && ea.Equal(eb)
}

// isDigits reports whether s is ASCII digits alone (strconv.Atoi takes a
// sign, which a fixed offset's hours or minutes never carry).
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// ZoneNameOf is the name ClockOffset.AppZone stores for loc, which
// ZoneLocation reads back as the same zone from at on: of a zone whose
// offset never changes after at, "UTC" or its fixed offset ("+05:30" of
// Asia/Kolkata, its alias Asia/Calcutta, or a zone file copied with no
// name), so names of one such zone are one name (Etc/UTC and UTC); of one
// whose offset changes, its IANA name ("Europe/Berlin"); "" where it has
// none keploy reads back as it (a zone file of summer time copied with no
// name) or loc is nil.
func ZoneNameOf(loc *time.Location, at time.Time) string {
	if loc == nil {
		return ""
	}
	if loc == time.UTC {
		return "UTC"
	}
	t := at.In(loc)
	if _, end := t.ZoneBounds(); !end.IsZero() {
		// A name another zone's file holds ("CET" of a rule IANA's CET
		// does not follow) names loc only where the two agree at at and on
		// its next change of offset.
		if n := loc.String(); n != "" && n != "Local" {
			if l, err := ZoneLocation(n); err == nil && sameZoneAt(l, loc, at) {
				return n
			}
		}
		return ""
	}
	_, off := t.Zone()
	if off == 0 {
		return "UTC"
	}
	sign := byte('+')
	if off < 0 {
		sign, off = '-', -off
	}
	if off%60 != 0 {
		return ""
	}
	return fmt.Sprintf("%c%02d:%02d", sign, off/3600, off%3600/60)
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

// AppZoneApart is the AppZone of the Meet of two learnings of one place
// that depend on the application's zone (ClockOffset.AppZoneMatters) and
// read it as two zones: two named ones, or one named and one keploy could
// not name. No zone is named so. A replay that reads the application's own
// zone from its process checks the place in it, as it does either
// learning; one that cannot names no zone both learnings were read in, so
// it refuses the value (learn's CheckClockRead), except a wall clock either
// zone held (ZoneEither) cut finer than an hour, which both held in UTC and
// it checks there.
const AppZoneApart = "apart"

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

// CheckedAt is the resolution Holds checks a replay's value of an offset
// o at, a value whose own encoding writes res: o.Res where it is a step of
// cutSteps coarser than res that CutOf would read as a cut at res (a unit,
// a millisecond or finer, or a decimal step at least 1,000 times res: the
// cut the learn step read in both of its values or their gap, or an
// encoding coarser than the replay's), as standing for its whole cut,
// whatever cut the value itself shows (one whose time to live is no whole
// number of cuts shows a finer one, and its verdict must not turn on
// that); res otherwise: a Res learned before cuts were (a granularity two
// values shared by chance: 500 ms, 2 s, or 10 ms or 100 ms at a
// millisecond's encoding) is read only in the sums.
func (o ClockOffset) CheckedAt(res time.Duration) time.Duration {
	if o.Res > res && CutOf(int64(o.Res), res, o.Res) == o.Res {
		return o.Res
	}
	return res
}

// Checkable reports whether a replay can check a value of the offset o,
// written at res, in a call's window from from to to (Holds): the window,
// o's width and three of the coarser of the resolution it checks the value
// at (CheckedAt) and o.Res are narrower than o's Unit (Within), so a time
// to live changed by a unit cannot hold. A Plain o is always checkable.
func (o ClockOffset) Checkable(res time.Duration, from, to time.Time) bool {
	if o.Plain() {
		return true
	}
	m := max(o.CheckedAt(res), o.Res)
	return o.Within(to.Sub(from), o.Max-o.Min, m, m, m)
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
// times enc: a millisecond's value falls on a whole 10 ms one time in ten
// by chance (on a whole 100 ms one in a hundred), and a step learned or
// read by chance is a looser check (a value it holds may stand up to one
// step stale: learn's CheckClockRead), so two values of a writer of
// milliseconds would be learned at 10 ms about one slow recording in a
// hundred. A unit (a second, a minute, an hour, a day) counts at any ratio:
// applications cut to units, and two values of a writer of seconds both
// fall on a whole minute only where record's full round read its clock
// half a second or more off the recording's read (learn's FullRoundLead).
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
// Unit. The learn step learns o only where its width and three of its
// resolution do (o.Within(o.Max-o.Min, o.Res, o.Res, o.Res)), so it holds
// one multiple of its unit, the one String names; Holds asks the same of
// the replay's window with them, at the resolution it checks the value at,
// so a time to live changed by a unit or more never holds, however the
// replay's read falls in its window: a value checked at a cut c is held
// from the start of the cut that holds the earliest read plus Min, less c,
// up to the latest plus Max, plus c (holdsRead), up to the window, the
// width and three cuts wide, and one whose time to live changed by a unit
// lies outside that. A time to live changed by less than that sum may
// hold, by where the read falls.
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

// Holds reports whether at, a value written at resolution res (its own
// encoding's), is a read of the clock made from from to to (a call's window
// on the application's clock) plus an offset of o; of a Plain o (a zero
// offset, a plain read of the clock), a read made there itself (holdsRead).
// Of any other o, a read of the clock plus an offset, then cut, is a clock
// read (2026-10-28): at is checked as a plain read of the span from
// from+o.Min to to+o.Max, at c, the cut o.Res where it is one coarser than
// res (CheckedAt), else res: it holds within one c of that span on
// either side (holdsRead: from the start of the c that holds from+o.Min,
// less c, up to to+o.Max plus c), so a value a read in the window plus the
// offset writes holds wherever the read fell in its cut, cut down or
// rounded, and so does one up to one cut stale or ahead; one more than a
// cut past either side never does, however the replay's read falls. It
// holds nothing where the window, o's width and three of the coarser of c
// and o.Res add up to o's Unit or more (Within): there a value whose time
// to live changed by a unit could hold, and the replay fails closed. Nor
// for an o whose Unit is a second or finer: an application that cuts its
// value to that unit makes a time to live changed by one look the same.
// Nor for an o whose Zone a replay reads no value in (ZoneKnown: ZoneApart,
// or one this keploy does not read). at is the value's instant as read in
// o's Zone: the caller reads a wall clock of no zone there
// (learn.CheckClockRead), and under ZoneEither in each of UTC and the
// application's zone, passing the value where either holds. A window with
// no start (a zero to is before any from), one that ends before it starts,
// a res of none or an empty o (it has no Unit) holds nothing, and so does an
// o the learn step never finds (a Min of zero or below, a Max not above
// Min, or a Res of none or below: one built without the resolution it was
// learned at could have its sums read finer than the application cuts its
// value), whose width could overflow or whose Unit could swallow the sign
// of the offset.
func (o ClockOffset) Holds(at time.Time, res time.Duration, from, to time.Time) bool {
	if !o.ZoneKnown() || !o.LayoutKnown() {
		return false
	}
	if o.Plain() {
		return o.holdsRead(at, res, from, to)
	}
	if o.Min <= 0 || o.Max <= o.Min || o.Res <= 0 || o.Unit() <= time.Second || from.IsZero() || to.Before(from) || res <= 0 {
		return false
	}
	if !o.Checkable(res, from, to) {
		return false
	}
	c := o.CheckedAt(res)
	return ClockOffset{Res: c}.holdsRead(at, c, from.Add(o.Min), to.Add(o.Max))
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
// value holds it only where both do; a coarser Res is a cut (or an
// encoding's resolution), at which a replay checks a value, of a Plain read
// one that shows it, of an offset any (Holds), a read within one cut of its
// span on either side (CheckClockRead in learn). Two that share no offset
// (a Plain and one with an offset, or two offsets apart), that read it in
// zones no one zone holds (UTC and the application's: ZoneApart,
// meetZones), or that saw it written in different layouts (LayoutApart,
// meetLayouts) meet as an Empty one, which holds nothing; ZoneEither meets
// UTC or the application's zone as that one, the zone both hold, and a
// Layout of "" (learned before Layout was) meets another as that one. Two
// that read the application's zone as two zones (AppZoneApart: two named
// ones, or one named and one keploy could not name) meet as one that names
// no zone: a replay checks it in the application's own zone where it reads
// that from the process, a wall clock either zone held cut finer than an
// hour in UTC, and refuses any other (learn's CheckClockRead). Meet is the
// same in either order and however a place's learnings are grouped, so a
// recording paired many times keeps one answer whatever order its
// pairings came in.
func (o ClockOffset) Meet(p ClockOffset) ClockOffset {
	m := ClockOffset{Min: max(o.Min, p.Min), Max: min(o.Max, p.Max), Res: max(o.Res, p.Res), Zone: meetZones(o.Zone, p.Zone), Layout: meetLayouts(o.Layout, p.Layout)}
	switch {
	case !o.AppZoneMatters():
		// o's learning read no zone of the application's (a read cut to
		// the second, a wall clock in UTC), so p's reading says which.
		m.AppZone = p.AppZone
	case !p.AppZoneMatters() || p.AppZone == o.AppZone:
		m.AppZone = o.AppZone
	default:
		// Read with the application's zone as two zones (two named ones,
		// recordings learned on two machines), or as one keploy named and
		// one it could not name (read in a zone of no name, which may be
		// any): no one zone is known to hold both readings. A replay that
		// reads the application's own zone checks the place in it, as it
		// would each learning; one that cannot refuses it (learn's
		// CheckClockRead). The conflict stays in AppZone, not Zone, so a
		// meet that then reads no zone of the application's (UTC with a
		// wall clock either zone held) holds, as it does met in any other
		// order.
		m.AppZone = AppZoneApart
	}
	if !m.AppZoneMatters() {
		// A meet that reads no zone of the application's (a wall clock
		// either zone held, met with one in UTC) names none.
		m.AppZone = ""
	}
	return m
}

// Empty reports whether o is the Meet of learnings of one place that share
// no offset: a Min above zero and a Max not above it; that read it in
// zones no one zone holds (ZoneApart); or that saw it written in different
// layouts (LayoutApart). Holds holds nothing for it, and String says why.
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

// holdsRead is Holds of a Plain o: at, a value written at resolution res
// (its own encoding's, or a cut it shows: learn's CheckClockRead), lies
// within one res of from to to, the call's window on the application's
// clock, on either side (founder decision, 2026-10-27): from the start of
// the res that holds from, less one res, up to to plus one res, both
// included, read on the instant at writes, in the zone at carries (its
// Location: the zone its res is read in, where a value cut to an hour or a
// day shows that cut, ReadCut). A day is a day of that zone's calendar
// (CutBefore, CutAfter): a day of 25 hours, when its clocks go back, is one
// day, never 24 hours. So a value a read in the window writes holds
// wherever the read fell in its res: cut down (Go's Truncate, a database
// column of seconds) it lies at or after the start of the res that holds
// from; rounded (JavaScript's Math.round(Date.now()/1000)) up to one res
// after to. One stale by up to one res holds too (a timestamp cached for
// under a second at a second's res, yesterday's date at a day's), and one
// moved by more than one res past either side never does, however the
// replay's read falls: the verdict does not turn on timing. It holds
// nothing for a res of none, a window with no start or that ends before it
// starts, an o with no Res (one built without the resolution it was learned
// at), or an o whose Res is coarser than res: a cut the value does not
// show, or one learned at an encoding coarser than the replay's. Learn's
// CheckClockRead decides those first, by the cut the value itself shows,
// and asks Holds of o read at the resolution it checks the value at. There
// is no unit to tell apart, so the window's width bounds nothing more.
func (o ClockOffset) holdsRead(at time.Time, res time.Duration, from, to time.Time) bool {
	if o.Res <= 0 || res <= 0 || from.IsZero() || to.Before(from) || o.Res > res {
		return false
	}
	loc := at.Location()
	return !at.Before(CutBefore(ReadCut(from, res, loc), res, loc)) && !at.After(CutAfter(to, res, loc))
}

// ReadCut is the start of the cut of resolution res that holds t, read in
// loc: for a day, the first instant of t's calendar day there (UnitStart:
// 01:00 on a day whose clocks skip midnight); for an hour or a finer res,
// t's wall clock in loc, at t's own offset, cut down to a whole res (zones'
// offsets are whole minutes). In an hour the wall clock shows twice (the
// clocks went back) that is the start of the showing t lies in, never the
// first showing's, an hour or two earlier. Of res none, t.
func ReadCut(t time.Time, res time.Duration, loc *time.Location) time.Time {
	t = t.In(loc)
	switch {
	case res <= 0:
		return t
	case res >= 24*time.Hour:
		return UnitStart(t, 24*time.Hour, loc)
	}
	r := WallNanos(t, loc) % int64(res)
	if r < 0 {
		r += int64(res)
	}
	return t.Add(-time.Duration(r))
}

// UnitStart is the first instant of t's hour (unit an hour) or calendar day
// (a day) in loc: the instant its wall clock there turns to t's hour or
// date, as Java's truncatedTo and atStartOfDay and JavaScript's
// setHours(0) cut a time (Go's time.Date too in a zone ahead of UTC; behind
// it, Go gives 23:00 the day before a skipped midnight): midnight, or, on a
// day whose clocks skip midnight (Cairo, Santiago, Havana on the day summer
// time starts), the first instant after the skipped hour. In an hour the
// clocks show again more than an hour later (Troll's two-hour change), it
// is the start of the showing t lies in.
func UnitStart(t time.Time, unit time.Duration, loc *time.Location) time.Time {
	l := t.In(loc)
	y, m, d := l.Date()
	h := 0
	if unit < 24*time.Hour {
		h = l.Hour()
	}
	same := func(x time.Time) bool {
		xl := x.In(loc)
		xy, xm, xd := xl.Date()
		return xy == y && xm == m && xd == d && (unit >= 24*time.Hour || xl.Hour() == h)
	}
	s := time.Date(y, m, d, h, 0, 0, 0, loc)
	if same(s) && !s.After(l) && !same(s.Add(-1)) {
		return s
	}
	// The wall clock skipped the unit's start (time.Date then gives an
	// instant outside it): the first instant at which it shows the unit,
	// at most a day and a few hours before t.
	lo, hi := l.Add(-unit-6*time.Hour), l
	if s.Before(l) && !same(s) {
		lo = s
	}
	for hi.Sub(lo) > 1 {
		if mid := lo.Add(hi.Sub(lo) / 2); same(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// CutIn is the cut at's wall clock in loc shows (CutOf of WallNanos), or an
// hour or a day where at is the first instant of its hour or calendar day
// there (UnitStart), which the wall clock does not show where the zone's
// clocks skipped its start: above enc and no coarser than upTo, as CutOf.
func CutIn(at time.Time, loc *time.Location, enc, upTo time.Duration) time.Duration {
	w := WallNanos(at, loc)
	c := CutOf(w, enc, upTo)
	if w%int64(time.Minute) != 0 {
		return c
	}
	for _, u := range []time.Duration{24 * time.Hour, time.Hour} {
		if u > c && u > enc && u <= upTo && at.Equal(UnitStart(at, u, loc)) {
			return u
		}
	}
	return c
}

// CutBefore is t one res earlier, read in loc: for a res of a day, the
// first instant of the calendar day before t's there (t is a day's start:
// ReadCut), a day of 25 or 23 hours one day; otherwise t less res.
func CutBefore(t time.Time, res time.Duration, loc *time.Location) time.Time {
	if res >= 24*time.Hour {
		for n := res / (24 * time.Hour); n > 0; n-- {
			t = UnitStart(UnitStart(t, 24*time.Hour, loc).Add(-1), 24*time.Hour, loc)
		}
		return t
	}
	return t.Add(-res)
}

// CutAfter is t one res later, read in loc: for a res of a day, the same
// time of day on the next calendar day there (24 hours later, or 25 or
// 23), and never before that day's first instant (where the next day's
// clocks skip t's time of day); otherwise t plus res.
func CutAfter(t time.Time, res time.Duration, loc *time.Location) time.Time {
	if res >= 24*time.Hour {
		next, start := t.In(loc), t
		for n := res / (24 * time.Hour); n > 0; n-- {
			next = next.AddDate(0, 0, 1)
			start = UnitStart(UnitStart(start, 24*time.Hour, loc).Add(26*time.Hour), 24*time.Hour, loc)
		}
		if next.Before(start) {
			return start
		}
		return next
	}
	return t.Add(res)
}

// WallNanos is t's wall clock in loc as nanoseconds from the Unix epoch
// (t's instant plus loc's offset at it): a value cut to an hour or a day
// in loc is a whole number of them there, whatever its offset from UTC.
func WallNanos(t time.Time, loc *time.Location) int64 {
	_, off := t.In(loc).Zone()
	return t.UnixNano() + int64(off)*int64(time.Second)
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
	s := o.offsetString() + o.zoneString() + o.layoutString()
	switch o.AppZone {
	case "":
	case AppZoneApart:
		s += ", read with the application's zone as no one zone (learned at different calls with the application's zone read as two zones, or named at one and unknown to keploy at another)"
	default:
		s += ", read with the application's zone as " + o.AppZone
	}
	return s
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
