package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
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
// the grammar of variables.Parse (body.<json path>, url.path[<n>],
// url.query.<name>, header.<name>), or as a word address of one
// (variables.ParseAddress: a word "#<n>" of its text, after any decode steps
// "|<step>"). Every reader reads a place with variables.ReadPlace: as Parse
// reads it where the recording holds it so, else as a word address.
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
	// Why is, for a variable the learn step wrote, why it is one and how to
	// undo it, for a person or a tool reading the mock (the legibility
	// rule). Only given variables (GivenPrefix) carry one for now
	// (learn.GivenWhy). Nothing replays it.
	Why string `json:"why,omitempty" yaml:"why,omitempty" bson:"why,omitempty"`
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
// (an expiry it sets thirty minutes after now, or a cutoff it queries from
// two minutes before now), which the learn step found,
// in its call's window both in the recording and in record's full round, at
// a read of that run's clock plus one offset from Min up to, not including,
// Max (learn.KindClockOffset). A replay keeps comparing it: the value it
// sees must be a read made in its own call's window plus such an offset
// (Holds), so a time to live changed by its Unit or more still fails (of one
// cut to a day, or to an hour that reads in hours or minutes, by three cuts
// or more, with the exceptions Checkable states: CoarseCut). Min and Max are
// what the two runs' windows bound the offset to, never a tolerance, and
// the learn step learns one only where they, with Res, are narrower than
// the unit the offset reads in (Unit, Within), or, of such a cut, where
// their width alone is narrower than the cut (Narrow): thirty minutes
// bounded to a few milliseconds, never to a few minutes. The offset is
// signed and never zero (signed): above zero for a value the application
// builds after its read (Min above zero: an expiry), below zero for one it
// builds before it (Max below zero: createdAt >= now - 2m), never a range
// around zero, which is a plain read's (Plain: Min and Max both zero); and
// Unit is coarser than a second for any offset the learn step finds.
//
// Res is the resolution the learn step read the value at. Of an offset, the
// one its encoding writes, or the cut its two values show (founder decision,
// 2026-10-27; a clock read plus a constant offset, then cut, is a clock
// read, 2026-10-28): the coarsest step of CutOf above the encoding that both
// values, or the gap between them, are whole multiples of (an application
// that cuts its value and then adds a time to live that is no whole number
// of cuts, 30m30s after a minute's start, puts its two values a whole number
// of cuts apart, though neither is a whole cut), read in UTC, in the
// offsets they write and in the application's zone, as a plain read's: an
// hour or a day, which a zone's offset moves, is read in the application's
// zone (AppZone names it; AppZoneMatters), at learn and at replay. The
// learn step bounds Min and Max at it, as
// standing for its whole cut (truncated: a value v stands for reads from v
// up to v plus Res), and the sums that decide whether a replay can compare
// the offset (Within, at the learn step and in Holds) count it, so thirty
// minutes cut to the minute, which a replay could not tell from thirty-one,
// is refused as too wide, and thirty days cut to the second is learned;
// so is a day cut to the hour (a day's unit holds its width and three
// hours). An offset cut to a day, or to an hour where it reads in hours or
// minutes (clock + 2h), which no unit holds three of (CoarseCut), is
// learned where its width is narrower than the cut (Narrow) and checked at
// the cut (Checkable), refused only where its width is the cut or more,
// never for the three cuts no unit holds (founder decision, 2026-10-27: a
// cut is checked, never refused; ruled for a day's cut and an hour's that
// reads in hours 2026-10-29): as yesterday's date passes a date cut to a
// day, a time to live changed by less than two cuts can pass there, by
// where the replay's read falls in its cut.
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
// moment cut coarser than a minute, to an hour or a day, a plain read or
// one plus an offset, which a zone's offset moves (an hour in UTC+05:30 is
// no hour in UTC). It is "UTC" or a
// fixed offset ("+05:30") of a zone with no change of offset ahead, else
// an IANA name ("Europe/Berlin"), as ZoneNameOf names it (ZoneLocation
// reads each). A replay reads the application's zone as
// the one its process writes its clock in in that run, where keploy reads
// it from the process (a wall clock of no zone is the application's own),
// else as AppZone, on any machine (learn's CheckClockRead): a recording
// learned on a laptop in IST checks the same cut in IST on a CI machine in
// UTC whose application's zone keploy cannot read, and in UTC where it
// reads the application now writing in UTC. Either is the zone the process
// started with, as its options (a JVM's user.timezone), its environment
// (TZ) and its files (/etc/localtime) name it, never one it sets later
// (learn's ProcZones).
// "" where keploy could not read the zone of the process that made the
// call, or could not name it (a zone file copied with no name, whose offset
// changes): the learn step read the value in the process's zone where it
// read one it could not name, else in the agent's own, either of which may
// be any zone; a replay that cannot read the application's zone either
// reads it in the agent's own, except that it refuses a read cut to an
// hour or coarser (plain, or plus an offset), whose cut the zone moves,
// read in the application's zone alone (ZoneApp, or an instant that names
// its moment); one in either zone (ZoneEither) it checks at its cut in UTC
// and the agent's own. A recording learned before AppZone was reads the
// same. Two learnings of one place read with the application's zone as
// two zones, two named ones or a named one and one of no name, meet as
// AppZoneApart (Meet), which names none.
type ClockOffset struct {
	Min     time.Duration `json:"min" yaml:"min" bson:"min"`
	Max     time.Duration `json:"max" yaml:"max" bson:"max"`
	Res     time.Duration `json:"res" yaml:"res" bson:"res"`
	Zone    string        `json:"zone,omitempty" yaml:"zone,omitempty" bson:"zone,omitempty"`
	Layout  string        `json:"layout,omitempty" yaml:"layout,omitempty" bson:"layout,omitempty"`
	AppZone string        `json:"app_zone,omitempty" yaml:"app_zone,omitempty" bson:"app_zone,omitempty"`
	// Since is where a replay's span for the read starts: "" at its call's
	// window (after the previous data-plane call); SinceTestBegin at the
	// running test's begin, for a read the learn step saw made before its
	// call's window in both runs and after the test began (keploy mock's
	// client tests: an application that reads its clock once for a request
	// and writes it at a later call). A replay that cannot tell where the
	// running test began on the application's clock refuses such a read, and
	// one of a Since this keploy does not read holds nothing (SinceKnown).
	Since string `json:"since,omitempty" yaml:"since,omitempty" bson:"since,omitempty"`
	// Word, of a read that is one word of a longer text (keploy mock's
	// client test naming an app "[E2E] stop-<UnixNano>" by its own clock
	// read since the test began), is where that word stands in the text as
	// recorded (ClockWord): a replay checks the value holds the recorded
	// text's bytes before the word and after it (by their length and
	// SHA-256) with one such read between them, so only the word is read as
	// the clock and every other byte is still compared. Zero (IsZero) for a
	// read that is its value whole, as every learning before Word was, and
	// then omitted: an old file reads the same. A value, not a pointer, so
	// two learnings of one read compare equal (==). A keploy that does not
	// read Word checks the whole value as one read, which a text around the
	// word is not, so it refuses it.
	Word ClockWord `json:"word,omitzero" yaml:"word,omitempty" bson:"word,omitempty"`
	// Why is the line the learn step wrote for the read: what, where, both
	// values, the rule and the undo, for a person or a tool reading the mock.
	// Nothing replays it. Meet keeps the lesser of two that are set, so one
	// line in either order; after a Meet that narrows the offset, that line
	// may quote the wider range one learning saw, until the learn step
	// writes Why again from the met offset.
	Why string `json:"why,omitempty" yaml:"why,omitempty" bson:"why,omitempty"`
	// Step is the cut the application made its clock read to before it added
	// the offset (a read cut to the hour, plus thirty days): a replay checks
	// the offset from that cut of the call's span (holdsStep). 0 for none,
	// as every learning before Step was, and then omitted: an old file reads
	// the same. Two learnings of one place at two Steps meet as StepApart
	// (Empty), which holds nothing.
	Step time.Duration `json:"step,omitempty" yaml:"step,omitempty" bson:"step,omitempty"`
}

// StepApart is the Step of the Meet of two learnings of one place that saw
// the application cut its read to two different steps: no one step holds
// both, so it holds nothing (Empty).
const StepApart time.Duration = -1

// holdsStep is Holds of o, whose Step is above zero. Until a keploy reads
// such a value, it holds nothing (fail closed).
var holdsStep = func(o ClockOffset, at time.Time, res time.Duration, from, to time.Time) bool { return false }

// ClockWord is where a read of the clock stands in a longer text
// (ClockOffset.Word), as recorded: the word at bytes Start to End of the
// text (End exclusive), After bytes of text after it, and the SHA-256, in
// lower-case hex, of the text before it (its first Start bytes:
// BeforeSHA256) and of the text after it (its last After bytes:
// AfterSHA256). It holds no byte of the text, so what the learn step
// writes is the same few bytes whatever the text's length, and nothing the
// recording holds (which may be a secret) is copied into what was learned:
// the recording holds the text, and "the word at bytes Start-End" names
// where to read it there. A word read always has text on one side at
// least (the learn step learns a read of a value whole otherwise), so the
// zero ClockWord is a read whole. The word's own length may differ at a
// replay (a time with a fraction cut of its trailing zeros), so a replay
// reads its word between the first Start bytes and the last After bytes
// of its value (WordOf); End only says where the recording's word ends.
type ClockWord struct {
	Start        int    `json:"start" yaml:"start" bson:"start"`
	End          int    `json:"end" yaml:"end" bson:"end"`
	After        int    `json:"after" yaml:"after" bson:"after"`
	BeforeSHA256 string `json:"before_sha256" yaml:"before_sha256" bson:"before_sha256"`
	AfterSHA256  string `json:"after_sha256" yaml:"after_sha256" bson:"after_sha256"`
	// Form is the text the word stands in: "" the value as it is; Decoded
	// (ClockWordDecoded) the value decoded once, its escapes read and its
	// base64 runs decoded where they stand (a percent-encoded instant, one
	// written with "\u002b" for its '+', one inside a base64 cursor), where
	// the word and its sides are read, so the sides are compared as decoded
	// text. Any other form is no place of a text (Valid). WordOf does not
	// decode yet: the replay that reads Decoded lands before any learn step
	// writes it, and until then nothing writes it. A Decoded word read raw
	// fails its decoded sides' hashes, or, as the whole value, is the raw
	// text checked whole, which the learned layout does not read.
	Form string `json:"form,omitempty" yaml:"form,omitempty" bson:"form,omitempty"`
}

// ClockWordDecoded is the Form of a word read in its value decoded once.
const ClockWordDecoded = "decoded"

// ClockWordOf is the ClockWord of the word at bytes start to end (end
// exclusive) of text, a value as recorded, 0 <= start < end <= len(text).
func ClockWordOf(text string, start, end int) ClockWord {
	return ClockWord{Start: start, End: end, After: len(text) - end, BeforeSHA256: textSHA256(text[:start]), AfterSHA256: textSHA256(text[end:])}
}

// IsZero reports whether w is no word: the read is its value whole. The
// json, yaml and bson encoders omit a zero Word by it.
func (w ClockWord) IsZero() bool { return w == ClockWord{} }

// Valid reports whether w is a word a replay can check: a word of one byte
// or more (Start < End), at bytes of a text (none negative), with text on
// one side of it at least or read in the value's decoded form, both hashes
// a SHA-256 in lower-case hex, in a form this keploy reads.
// What a file holds otherwise (a hand edit, a cut copy) is no place of a
// text, and a replay refuses the value there whatever it is.
func (w ClockWord) Valid() bool {
	return w.Start >= 0 && w.End > w.Start && w.After >= 0 && (w.Start > 0 || w.After > 0 || w.Form == ClockWordDecoded) && isSHA256Hex(w.BeforeSHA256) && isSHA256Hex(w.AfterSHA256) && (w.Form == "" || w.Form == ClockWordDecoded)
}

// WordOf is the word of live, a replay's text at a place learned as w (a
// Valid one): live with its first w.Start bytes and its last w.After bytes
// taken off, where those are the bytes the recorded text held before its
// word and after it, compared by length and SHA-256, so every byte of
// them is still compared. Otherwise changed says what is not as recorded:
// "short" for a live too short to hold both apart (they would overlap, or
// leave no word between them), else "before" or "after", the side of the
// word whose bytes changed; changed is "" for a word.
func (w ClockWord) WordOf(live string) (word, changed string) {
	switch {
	case len(live) <= w.Start+w.After:
		return "", "short"
	case textSHA256(live[:w.Start]) != w.BeforeSHA256:
		return "", "before"
	case textSHA256(live[len(live)-w.After:]) != w.AfterSHA256:
		return "", "after"
	}
	return live[w.Start : len(live)-w.After], ""
}

// textSHA256 is the SHA-256 of s in lower-case hex.
func textSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// isSHA256Hex reports whether s is a SHA-256 in lower-case hex (textSHA256).
func isSHA256Hex(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// meetWord is the Word two learnings of one place read it at, a and b, and
// whether they share one: both whole (zero), or both the same word of the
// same text. A read whole and one inside a text, or two at different places
// of a text, are two forms of the value: their Meet is LayoutApart (Empty).
func meetWord(a, b ClockWord) (ClockWord, bool) {
	return a, a == b
}

// AppZoneMatters reports whether what the learn step learned of o depends
// on the application's zone, so it names that zone in AppZone: a read a
// replay reads in that zone (CutInAppZone), or a read plus an offset at any
// cut, whose check fails closed near the zone's backward turns (FallBack:
// an application that resolves a wall time its zone shows twice to the
// first showing writes a value an hour early there, two in Troll, so a time
// to live made that much longer would pass).
func (o ClockOffset) AppZoneMatters() bool {
	return o.CutInAppZone() || !o.Plain()
}

// CutInAppZone reports whether a replay reads o's value in the
// application's zone: a wall clock of no zone read in it (ZoneApp,
// ZoneEither), or a value that names its moment cut coarser than a minute
// (an hour, a day), whose cut it reads there: a plain read's beside UTC and
// the offset it writes (learn's cutZones), a read plus an offset's there
// alone (learn's checkReadIn: the hour or the day its read plus the offset
// was cut to). A minute or any finer cut is one in every zone (zones'
// offsets are whole minutes); a wall clock read in UTC (ZoneUTC) is cut in
// UTC alone. Of an o that is not, AppZone serves FallBack alone.
func (o ClockOffset) CutInAppZone() bool {
	return o.Zone == ZoneApp || o.Zone == ZoneEither || o.Zone == "" && o.Res > time.Minute
}

// ZoneLocation is the zone name names (ClockOffset.AppZone): "UTC", a fixed
// offset ("+05:30", "-03:00") or an IANA name ("Asia/Kolkata"), read from
// the host's zone files, else from the ones built into keploy (time/tzdata),
// so a replay reads it on any machine; IANA's names of UTC (Etc/UTC,
// Universal: utcNames) UTC with no file. An error for any other name, and
// for an IANA name whose file in keploy's own zone files is mounted from
// another zone's (ownZoneFileMounted: IsOwnZoneMount), as Go would read
// that file's rules for name, which a JVM or Node reading name do not.
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
	if utcNames[name] {
		// UTC in every release of every runtime's zone data (Java's tzdb,
		// ICU, IANA's), whatever file keploy's own zone files hold for it.
		return time.UTC, nil
	}
	if z, ok := zoneLocations.Load(name); ok {
		return z.(zoneLoad).loc, z.(zoneLoad).err
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	if err := ownZoneFileMounted(name); err != nil {
		if IsOwnZoneMount(err) {
			zoneLocations.Store(name, zoneLoad{err: err})
		}
		return nil, err
	}
	zoneLocations.Store(name, zoneLoad{loc: loc})
	return loc, nil
}

// zoneLocations are the IANA zones ZoneLocation has read, by name, and
// those it refused for their file's mount (IsOwnZoneMount; an error where
// keploy could not tell is read again): a replay checks a place's AppZone
// at every call, and reads its file and keploy's mounts once. Only names
// Go reads a zone of are kept.
var zoneLocations sync.Map

// zoneLoad is ZoneLocation's answer for one IANA name.
type zoneLoad struct {
	loc *time.Location
	err error
}

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
// zone held (ZoneEither), which both held in UTC and it checks there, at its
// cut, whatever the cut.
const AppZoneApart = "apart"

// SinceTestBegin is a ClockOffset's Since for a read checked from the
// running test's begin rather than from its call's window (ClockOffset).
const SinceTestBegin = "test"

// SinceKnown reports whether o's Since is one a replay reads: "" (its call's
// window) or SinceTestBegin. A replay refuses a value at a place of any
// other, from whatever keploy wrote it, rather than check it in a span its
// learn step did not read.
func (o ClockOffset) SinceKnown() bool { return o.Since == "" || o.Since == SinceTestBegin }

// meetSince is the Since a replay reads a place from that two learnings read
// from a and b: SinceTestBegin where either did (a span from the test's
// begin holds every read its call's window does), and one this keploy does
// not read where either is (SinceKnown fails it closed); of two such, the
// later in byte order, so the meet is one whatever order they come in.
func meetSince(a, b string) string {
	switch {
	case a == b || b == "" || b == SinceTestBegin && a != "":
		return a
	case a == "" || a == SinceTestBegin:
		return b
	}
	return max(a, b)
}

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
// to live changed by a unit cannot hold. Of an offset cut to an hour or a
// day that no unit holds three of (CoarseCut: a day's cut, or an hour's
// that reads in hours or minutes, clock + 2h), the window and o's width
// narrower than the cut: a cut is checked at the cut all the same (founder
// decision, 2026-10-27: a cut is checked, never refused; ruled for a day's
// cut and an hour's that reads in hours 2026-10-29), as a date cut to a day
// is, so a time to live changed by less than two cuts can hold, by where
// the replay's read falls in its cut (one changed by one cut at every read where the value is the
// read plus the offset cut down to the cut; where the application adds time
// after its cut, only at some), one changed by two holds only where the
// replay's read falls within the window and o's width of the cut's turn,
// and one changed by three or more never does, but for a day a
// daylight-saving turn shortens to 23 hours, still one day (holdsRead):
// there three days can hold where the window and o's width are within an
// hour of the day, a call some 23 hours long (a stated limit). Where the zone
// the value is read in shows a wall time twice as its clocks go back, Holds
// holds nothing near that turn, at any cut finer than a day (FallBack). A
// Plain o is always checkable.
func (o ClockOffset) Checkable(res time.Duration, from, to time.Time) bool {
	if o.Plain() {
		return true
	}
	m := max(o.CheckedAt(res), o.Res)
	if o.CoarseChecked(res) {
		return to.Sub(from) < m && o.Max-o.Min < m-to.Sub(from)
	}
	return o.Within(to.Sub(from), o.Max-o.Min, m, m, m)
}

// CoarseChecked reports whether a replay checks a value of o written at res
// at its cut alone (Checkable's cut branch): o is cut to an hour or a day
// that no unit holds three of (CoarseCut) and Holds checks the value at
// that cut (CheckedAt(res) is o.Res). A value whose encoding writes a step
// coarser than the cut (two days; or a day, of an offset cut to the hour;
// no encoding keploy reads does either) is checked at its own resolution,
// where three of it count against the unit (Within), and holds nothing.
func (o ClockOffset) CoarseChecked(res time.Duration) bool {
	return o.CoarseCut() && o.CheckedAt(res) == o.Res
}

// Narrow reports whether the learn step can learn o, an offset it found
// (learn's clockOffset): o's width and three of its Res are narrower than
// its Unit (Within), so a replay can tell a time to live changed by a unit,
// or, of an o cut to an hour or a day that no unit holds three of
// (CoarseCut), its width alone is narrower than the cut: a replay checks it
// at the cut, as standing for its whole cut, within one cut of its window
// plus the offset on either side (Holds, Checkable).
func (o ClockOffset) Narrow() bool {
	if o.CoarseCut() {
		return o.Max-o.Min < o.Res
	}
	return o.Within(o.Max-o.Min, o.Res, o.Res, o.Res)
}

// CoarseCut reports whether o is an offset the learn step read cut to an
// hour or a day that no unit holds three of with o's width: a day's cut (a
// time to live cut to the day, in the zone the value shows a whole day in:
// UTC's, Go's Truncate; the offset it writes; or the application's; an
// expiry at the start of the day thirty days on), or an hour's whose offset
// reads in hours or a finer unit (Unit at most an hour: clock + 2h cut to
// the hour, or a cut hour plus 90 minutes, which a range at least a full
// round's lead wide reads in minutes; of clock + 1d cut to the hour, a day
// holds three hours and Within decides). The learn step learns it where its
// width is narrower than the cut (Narrow), and a replay checks it at the
// cut (Checkable), never refusing it for the three cuts no unit holds
// (founder decision, 2026-10-27; ruled for a day's cut and an hour's that
// reads in hours 2026-10-29): as yesterday's date passes a date cut to a
// day, one changed by less than two cuts can pass there, by where the
// replay's read falls in its cut. Only a
// Res of exactly an hour or a day, steps of models.CutOf: a Res no step is
// (96 hours, a hand-edited mock's) is no cut, and its sums count three of
// it against the unit as before.
func (o ClockOffset) CoarseCut() bool {
	if o.Plain() {
		return false
	}
	switch o.Res {
	case 24 * time.Hour:
		return true
	case time.Hour:
		return o.Unit() <= time.Hour
	}
	return false
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
// Unit. The learn step learns o, other than one cut to an hour or a day
// that no unit holds three of (CoarseCut, which Narrow and Checkable decide
// alone), only where its width and three of its resolution do
// (o.Within(o.Max-o.Min, o.Res, o.Res, o.Res)), so it holds one multiple of
// its unit, the one String names; Holds asks the same of the replay's
// window with them, at the resolution it checks the value at (of a value it
// checks at that cut, Checkable asks instead: CoarseChecked),
// so a time to live changed by a unit or more never holds there, however the
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
// on the application's clock) plus an offset of o; of an o with a Step, as
// holdsStep says (until a keploy reads one, nothing); of a Plain o (a zero
// offset, a plain read of the clock), a read made there itself (holdsRead).
// Of any other o, a read of the clock plus an offset, then cut, is a clock
// read (2026-10-28): at is checked as a plain read of the span from
// from+o.Min to to+o.Max, at c, the cut o.Res where it is one coarser than
// res (CheckedAt), else res: it holds within one c of that span on either
// side (holdsRead: from the start of the c that holds from+o.Min, less c, up
// to to+o.Max plus c), so a value a read in the window plus the offset
// writes holds wherever the read fell in its cut, cut down or rounded, and
// so does one up to one cut stale or ahead; one more than a cut past either
// side never does, however the replay's read falls. It holds nothing where
// the window, o's width and three of the coarser of c and o.Res add up to
// o's Unit or more (Within): there a value whose time to live changed by a
// unit could hold, and the replay fails closed. Of a cut to an hour or a day
// that no unit holds three of (CoarseCut), where the window and o's width
// add up to the cut or more (Checkable); where they are narrower, the cut
// admits as a date's admits yesterday: a time to live changed by less than
// two cuts can hold, by where the read falls in its cut, one changed by two
// only near the cut's turn, and one changed by three or more never, but
// for the 23-hour day (Checkable). Nor, at any cut finer than a day, where
// the call's window or that span meets the wall time at's zone shows twice
// as its clocks go back (FallBack): an application that resolves that wall
// time to its first showing writes a value an hour early there (two in
// Troll), and up to one cut more where it cuts. Nor
// for an o whose Unit is a second or finer: an application that cuts its
// value to that unit makes a time to live changed by one look the same. Nor
// for an o whose Zone a replay reads no value in (ZoneKnown: ZoneApart, or one
// this keploy does not read). at is the value's instant as read in o's Zone:
// the caller reads
// a wall clock of no zone there (learn.CheckClockRead), and under ZoneEither
// in each of UTC and the application's zone, passing the value where either
// holds. A window with no start (a zero to is before any from), one that
// ends before it starts, a res of none or an empty o (it has no Unit) holds
// nothing, and so does an o the learn step never finds (offsets not all on
// one side of zero, signed: a range around zero, whose width could overflow
// or whose Unit could swallow the sign of the offset; or a Res of none or
// below: one built without the resolution it was learned at could have its
// sums read finer than the application cuts its value).
func (o ClockOffset) Holds(at time.Time, res time.Duration, from, to time.Time) bool {
	if !o.ZoneKnown() || !o.LayoutKnown() {
		return false
	}
	if o.Step != 0 {
		return o.Step > 0 && holdsStep(o, at, res, from, to)
	}
	if o.Plain() {
		return o.holdsRead(at, res, from, to)
	}
	if !o.signed() || o.Res <= 0 || o.Unit() <= time.Second || from.IsZero() || to.Before(from) || res <= 0 {
		return false
	}
	if !o.Checkable(res, from, to) {
		return false
	}
	if _, _, met := o.FallBack(res, from, to, at.Location()); met {
		return false
	}
	c := o.CheckedAt(res)
	return ClockOffset{Res: c}.holdsRead(at, c, from.Add(o.Min), to.Add(o.Max))
}

// FallBack is the turn at which loc's clocks go back, and by how much, that
// a check of o, a read plus an offset checked at a cut finer than a day
// (CheckedAt(res): none, a millisecond, a second, a minute or an hour), in
// a call's window from from to to meets: one whose wall time shown twice
// (from the turn less what the clocks go back by up to the turn plus it:
// both showings) meets the window itself or the values Holds admits there
// (from one cut before the cut that holds from+o.Min, up to one cut after
// to+o.Max). An application that resolves a wall time shown twice to its
// first showing (JavaScript's Date setters, setMilliseconds(0),
// setSeconds(0, 0) and setMinutes(0, 0, 0) alike; java.time's
// LocalDateTime then atZone, with no cut at all) writes, of an instant in
// the second showing, a value an hour before it (two in Troll's turn), and
// up to one cut more where it cuts: of a read in the window where it cuts
// (or converts) and then adds its time to live, of the value itself where
// it adds and then cuts. Checked against the call's span plus the offset,
// a time to live made an hour longer would pass there at any read, so
// Holds holds nothing where a turn is met, and a replay refuses such a
// value, naming the turn (learn's checkReadIn): a false red some hours a
// year. A day's cut meets none: the cut keeps the date alone, which both
// showings share, so the first showing's day is the instant's.
// met is false where none is, for a Plain o (no time to live to change), a
// nil loc and a window with no start or that ends before it starts.
func (o ClockOffset) FallBack(res time.Duration, from, to time.Time, loc *time.Location) (turn time.Time, back time.Duration, met bool) {
	return o.fallBack(res, from, to, loc, false)
}

// FallBackAcrossHour is FallBack kept to the turns whose wall time shown
// twice holds the start of one of loc's wall hours strictly inside it
// (AcrossHour: Pacific/Chatham's 02:45 to 03:45 holds 03:00, Troll's 01:00
// to 03:00 holds 02:00; Europe/Berlin's 02:00 to 03:00 and Lord Howe's
// 01:30 to 02:00 hold none). The learn step reads it at an hour's cut
// (learn.DriftCache.offsetFallBackWhy): there two runs in the second
// showing on either side of that hour's start cut to different hours,
// which resolve to different showings or both to an early one, so a
// recording and its full round a minute apart can show an offset that
// stands from the one the application adds. In Chatham the hour before
// 03:00 is shown once, so a run before it writes the right value and one
// after it 03:00's first showing, an hour early: clock + 1h of clock + 2h.
// In Troll 01:00 is shown twice too, so both write their hour's first
// showing, two hours early: clock + 1h of clock + 3h.
func (o ClockOffset) FallBackAcrossHour(res time.Duration, from, to time.Time, loc *time.Location) (turn time.Time, back time.Duration, met bool) {
	return o.fallBack(res, from, to, loc, true)
}

// fallBack is FallBack, kept to the turns AcrossHour holds where across.
func (o ClockOffset) fallBack(res time.Duration, from, to time.Time, loc *time.Location, across bool) (turn time.Time, back time.Duration, met bool) {
	lo, hi, ok := o.fallBackSpan(res, from, to, loc)
	if !ok || loc == nil {
		return time.Time{}, 0, false
	}
	for _, s := range [][2]time.Time{{from, to}, {lo, hi}} {
		if turn, back, met := fallBackIn(s[0], s[1], loc, across); met {
			return turn, back, true
		}
	}
	return time.Time{}, 0, false
}

// AcrossHour reports whether the wall time loc shows twice as its clocks
// go back at turn, from the wall time they show just after it up to the one
// just before it, holds the start of one of loc's wall hours strictly
// inside it: a wall clock cut to the hour in the second showing resolves
// that hour to its first showing, and the hour before it to another
// showing (Chatham's 02:00, shown once) or to its own first showing too
// (Troll's 01:00, shown twice).
func AcrossHour(turn time.Time, loc *time.Location) bool {
	if loc == nil {
		return false
	}
	_, before := turn.Add(-time.Nanosecond).In(loc).Zone()
	_, after := turn.In(loc).Zone()
	return acrossHour(turn, before, after)
}

// acrossHour is AcrossHour of a turn whose zone's offset east of UTC goes
// from before to after (seconds) there.
func acrossHour(turn time.Time, before, after int) bool {
	lo, hi := turn.Unix()+int64(after), turn.Unix()+int64(before)
	h := lo / 3600
	if lo%3600 < 0 {
		h--
	}
	return (h+1)*3600 < hi
}

// FallBackAnywhere is FallBack read in every zone of the tz database
// (tzNames), for a replay that knows no zone of the application's: the
// first turn any zone's clocks go back at that the window or the values a
// check of o admits meet, by how much, and that zone. The values' span is
// read one cut wider before (a zone whose cut starts elsewhere): that is
// reached at an hour's cut alone, by the learn step's
// FallBackAnywhereAcrossHour (an hour in Pacific/Chatham, at UTC+12:45,
// starts at a quarter past one of UTC's), since a replay with no zone known
// refuses an hour's cut before it reads this (learn.CheckClockRead), and at
// a minute's cut or finer every zone's cuts are UTC's (offsets are whole
// minutes). That is a handful of dates a year, a few hours each,
// where such a replay refuses the value: false reds, never a pass of a
// value an application in an unknown zone may have written an hour early.
func (o ClockOffset) FallBackAnywhere(res time.Duration, from, to time.Time) (turn time.Time, back time.Duration, loc *time.Location, met bool) {
	return o.fallBackAnywhere(res, from, to, false)
}

// FallBackAnywhereAcrossHour is FallBackAnywhere kept to the turns whose
// wall time shown twice holds the start of one of their zone's wall hours
// strictly inside it (AcrossHour; FallBackAcrossHour), for the learn step
// at an hour's cut where it knows no zone of the application's: today
// Pacific/Chatham's and Antarctica/Troll's, some hours a year.
func (o ClockOffset) FallBackAnywhereAcrossHour(res time.Duration, from, to time.Time) (turn time.Time, back time.Duration, loc *time.Location, met bool) {
	return o.fallBackAnywhere(res, from, to, true)
}

// fallBackAnywhere is FallBackAnywhere, kept to the turns AcrossHour holds
// where across.
func (o ClockOffset) fallBackAnywhere(res time.Duration, from, to time.Time, across bool) (turn time.Time, back time.Duration, loc *time.Location, met bool) {
	lo, hi, ok := o.fallBackSpan(res, from, to, time.UTC)
	if !ok {
		return time.Time{}, 0, nil, false
	}
	lo = lo.Add(-o.CheckedAt(res))
	for _, s := range [][2]time.Time{{from, to}, {lo, hi}} {
		for y := s[0].Add(-maxFallBack).Year(); y <= s[1].Add(maxFallBack).Year(); y++ {
			for _, f := range yearFallBacks(y) {
				if (f.across || !across) && !f.at.Add(-f.back).After(s[1]) && f.at.Add(f.back).After(s[0]) {
					return f.at, f.back, f.loc, true
				}
			}
		}
	}
	return time.Time{}, 0, nil, false
}

// fallBackSpan is the span of values a check of o at res admits in a
// call's window from from to to, read in loc (Holds: from one cut before
// the cut that holds from+o.Min up to one cut after to+o.Max), and whether
// FallBack reads o at all: a read plus an offset checked at a cut finer
// than a day, in a window with a start that ends no earlier.
func (o ClockOffset) fallBackSpan(res time.Duration, from, to time.Time, loc *time.Location) (lo, hi time.Time, ok bool) {
	c := o.CheckedAt(res)
	if o.Plain() || o.Res <= 0 || res <= 0 || c >= 24*time.Hour || o.Res >= 24*time.Hour || from.IsZero() || to.Before(from) {
		return time.Time{}, time.Time{}, false
	}
	if loc == nil {
		loc = time.UTC
	}
	return CutBefore(ReadCut(from.Add(o.Min), c, loc), c, loc), CutAfter(to.Add(o.Max), c, loc), true
}

// maxFallBack is more than any zone's clocks go back by at one turn
// (Troll's two hours are the most).
const maxFallBack = 3 * time.Hour

// fallBackIn is the first turn at which loc's clocks go back whose wall
// time shown twice (from the turn less what they go back by up to the turn
// plus it) meets the span from lo to hi, both included, and what they go
// back by there; where across, only a turn AcrossHour holds.
func fallBackIn(lo, hi time.Time, loc *time.Location, across bool) (time.Time, time.Duration, bool) {
	for t := lo.Add(-maxFallBack); ; {
		_, end := t.In(loc).ZoneBounds()
		if end.IsZero() || !end.After(t) || end.After(hi.Add(maxFallBack)) {
			return time.Time{}, 0, false
		}
		_, before := end.Add(-time.Nanosecond).In(loc).Zone()
		_, after := end.In(loc).Zone()
		if back := time.Duration(before-after) * time.Second; back > 0 && (!across || acrossHour(end, before, after)) && !end.Add(-back).After(hi) && end.Add(back).After(lo) {
			return end, back, true
		}
		t = end
	}
}

// zoneFallBack is one turn at which a zone's clocks go back (at), by how
// much (back), the zone (loc), and whether its wall time shown twice holds
// the start of one of loc's wall hours strictly inside it (AcrossHour).
type zoneFallBack struct {
	at     time.Time
	back   time.Duration
	loc    *time.Location
	across bool
}

// fallBackYears keeps yearFallBacks by year, read once per process.
var fallBackYears = struct {
	sync.Mutex
	m map[int][]zoneFallBack
}{m: map[int][]zoneFallBack{}}

// yearFallBacks is every turn in year y (UTC) at which a zone of tzNames
// goes back, earliest first, one per instant, step and AcrossHour (the
// first zone, by name, that turns there so: Pacific/Chatham's turn, at
// Auckland's instant and step, is kept beside Auckland's), as the zone
// files keploy reads hold them (the host's, else its embedded copy). A name
// no file holds is skipped.
func yearFallBacks(y int) []zoneFallBack {
	fallBackYears.Lock()
	defer fallBackYears.Unlock()
	if fs, ok := fallBackYears.m[y]; ok {
		return fs
	}
	start, end := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
	type key struct {
		at     int64
		back   time.Duration
		across bool
	}
	seen := map[key]bool{}
	var fs []zoneFallBack
	for _, name := range tzNames {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		for t := start.Add(-time.Nanosecond); ; {
			_, e := t.In(loc).ZoneBounds()
			if e.IsZero() || !e.After(t) || !e.Before(end) {
				break
			}
			_, before := e.Add(-time.Nanosecond).In(loc).Zone()
			_, after := e.In(loc).Zone()
			if back := time.Duration(before-after) * time.Second; back > 0 {
				k := key{e.UnixNano(), back, acrossHour(e, before, after)}
				if !seen[k] {
					seen[k] = true
					fs = append(fs, zoneFallBack{at: e, back: back, loc: loc, across: k.across})
				}
			}
			t = e
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].at.Before(fs[j].at) })
	fallBackYears.m[y] = fs
	return fs
}

// Plain reports whether o is a zero offset: a plain read of the
// application's clock (learn.Learned.Read), its Min and Max both zero, which
// Holds reads as this call's own now. An o the learn step learned with an
// offset (learn.KindClockOffset) is signed.
func (o ClockOffset) Plain() bool { return o.Min == 0 && o.Max == 0 }

// signed reports whether o's offsets, from Min up to, not including, Max,
// lie all on one side of zero: all above it (Min above zero: a value the
// application builds after its read, an expiry) or all below it (Max below
// zero: one it builds before its read, a cutoff it queries from), with Max
// above Min. Every offset the learn step learns is (learn.KindClockOffset);
// a range around zero is none, its Unit would swallow the offset's sign (a
// day around zero holds any value), and Holds holds nothing for it.
func (o ClockOffset) signed() bool {
	return (o.Min > 0 || o.Max < 0) && o.Min < o.Max
}

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
// meetLayouts), whole at one and a word of a text at another, or at two
// places of a text (meetWord), meet as an Empty one, which holds nothing; ZoneEither meets
// UTC or the application's zone as that one, the zone both hold, and a
// Layout of "" (learned before Layout was) meets another as that one. Two
// that read the application's zone as two zones (AppZoneApart: two named
// ones, or one named and one keploy could not name) meet as one that names
// no zone: a replay checks it in the application's own zone where it reads
// that from the process, a wall clock either zone held cut finer than an
// hour in UTC, and refuses any other (learn's CheckClockRead). Meet is the
// same in either order and however a place's learnings are grouped, so a
// recording paired many times keeps one answer whatever order its
// pairings came in. Two at different Steps meet as StepApart (Empty). Why
// is the lesser of the two that are set (nothing replays it): one learned
// line, whatever the order.
func (o ClockOffset) Meet(p ClockOffset) ClockOffset {
	m := ClockOffset{Min: max(o.Min, p.Min), Max: min(o.Max, p.Max), Res: max(o.Res, p.Res), Zone: meetZones(o.Zone, p.Zone), Layout: meetLayouts(o.Layout, p.Layout), Since: meetSince(o.Since, p.Since), Why: o.Why, Step: o.Step}
	if m.Why == "" || p.Why != "" && p.Why < m.Why {
		m.Why = p.Why
	}
	if o.Step != p.Step {
		m.Step = StepApart
	}
	// One read whole and one inside a text, or at two places of a text
	// (meetWord), were written in two forms: no one layout holds both.
	if word, ok := meetWord(o.Word, p.Word); ok {
		m.Word = word
	} else {
		m.Layout = LayoutApart
	}
	// A learning whose value a replay reads in no zone of the application's
	// (CutInAppZone: one cut to the minute or finer), that names none, says
	// nothing of the zone a meet read in it holds: the other's reading says
	// which (as before such learnings named their zone for FallBack). Met
	// with another of its kind, it names no one zone, and FallBack reads
	// every zone's turns (learn's CheckClockRead).
	silent := func(x ClockOffset) bool { return x.AppZone == "" && !x.CutInAppZone() && m.CutInAppZone() }
	switch {
	case !o.AppZoneMatters() || silent(o):
		// o's learning read no zone of the application's (a plain read cut
		// to the second, a wall clock in UTC), so p's reading says which.
		m.AppZone = p.AppZone
	case !p.AppZoneMatters() || silent(p) || p.AppZone == o.AppZone:
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
// no offset: a Max not above its Min, other than a Plain o's (a Plain and
// an offset meet so, and so do two offsets apart, of one sign or of two);
// that read it in zones no one zone holds (ZoneApart); or that saw it
// written in different layouts (LayoutApart); or at different Steps
// (StepApart). Holds holds nothing for it, and String says why.
func (o ClockOffset) Empty() bool {
	return !o.Plain() && o.Max <= o.Min || o.Zone == ZoneApart || o.Layout == LayoutApart || o.Step < 0
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
// than a nanosecond apart: "clock + 30m (from 29m59.998s to 30m0.001s)";
// one below zero as "clock - 2m (from -2m0.04s to -1m59.989s)".
// Where they hold more than one multiple, or lie around zero (no offset the
// learn step finds: signed), it names no offset: "clock plus an
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
	case !o.SinceKnown():
		return "clock read from a start this keploy does not read (" + strconv.Quote(o.Since) + ")"
	case o.Step < 0:
		return "clock reads cut to no one step (one place learned as a read cut to different steps before its offset at different calls)"
	}
	s := o.offsetString() + o.zoneString() + o.layoutString()
	if o.Step > 0 {
		s += ", read cut to " + o.Step.String() + " before the offset"
	}
	if !o.Word.IsZero() {
		// Where the word stands, never what the text holds.
		s += ", as the word at bytes " + strconv.Itoa(o.Word.Start) + "-" + strconv.Itoa(o.Word.End) + " of the text as recorded, with " + strconv.Itoa(o.Word.After) + " bytes after it"
	}
	if o.Since == SinceTestBegin {
		s += ", read after its test began and before its call"
	}
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
	if u == 0 || !o.signed() || ceilTo(o.Min, u)+u < o.Max {
		return o.Range()
	}
	s := "clock + " + ShortDuration(ceilTo(o.Min, u))
	if n := ceilTo(o.Min, u); n < 0 {
		s = "clock - " + ShortDuration(-n)
	}
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
		if cur.Why == "" {
			cur.Why = v.Why
		}
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

// RunVariablePrefix begins the name of a run's variable: one bound for the
// whole test set, by its start-up or by an earlier test's call, which every
// later test of the set reads (learn.Tracker.RunName). No test's own variable
// is named with it, so a replay keeps a binding of such a name for the set
// (variables.Table), and forgets every other one when the next test begins.
const RunVariablePrefix = "run-"

// RunVariable reports whether name is a run's variable's (RunVariablePrefix).
func RunVariable(name string) bool { return strings.HasPrefix(name, RunVariablePrefix) }

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
