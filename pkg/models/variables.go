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
// Res is the resolution the learn step read the value at: the coarser of
// the one its encoding writes and the granularity both runs' values share
// (the greatest duration both are whole multiples of, from the Unix epoch).
// An application that cuts its value coarser than its encoding
// (Truncate(time.Second) stored as a millisecond $date) shows that cut in
// both values, and the sums that decide whether a replay can compare the
// offset (Within, at the learn step and in Holds) count it, never the
// encoding's finer one: a value cut to the minute, its time to live thirty
// minutes, read late in its minute stands only thirty minutes and a
// fraction of a second from its read even with a time to live of
// thirty-one, and a resolution of a millisecond would hold it. Res is read
// only in those sums and in the refusal of a cut (CutCoarserThan), so only
// to refuse more: two values that happen to
// share a coarser granularity make it coarser, never finer, the learn step
// bounds Min and Max at the encoding's resolution, not at Res, and Holds
// reads the replay's value at the replay's own resolution, not at Res. A
// Res that is a cut the application made (CutCoarserThan: a whole number of
// the unit above the encoding's, a value cut to the second stored at the
// millisecond) is never learned: read at its encoding's resolution, a
// replay's value cut so would hold only where its read falls early in its
// cut, by when the replay ran, and keploy does not read the application's
// cut from two values to hold it anywhere. Nor are two values a whole
// number of that unit apart (learn's apartByUnits): a cut to it plus a time
// to live that is no whole number of cuts, which Res does not show.
type ClockOffset struct {
	Min time.Duration `json:"min" yaml:"min" bson:"min"`
	Max time.Duration `json:"max" yaml:"max" bson:"max"`
	Res time.Duration `json:"res" yaml:"res" bson:"res"`
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
// 3,600 at a second's, and are refused the same. A Res above enc that is no
// whole number of the next unit (half a second two millisecond values share
// by chance, or 2 s two second values do) is none: the sums count it, and
// the replay's value is read at its own resolution. A Res of enc itself
// (an hour's time to live at a minute's encoding) is none either.
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
// fell in the cut. A window with no start (a zero to
// is before any from), one that ends before it starts, a res of none or an
// empty o (it has no Unit) holds nothing, and so does an o the learn step
// never finds (a Min of zero or below, a Max not above Min, or a Res of
// none or below: one built without the resolution it was learned at could
// have its sums read finer than the application cuts its value), whose
// width could overflow or whose Unit could swallow the sign of the offset.
func (o ClockOffset) Holds(at time.Time, res time.Duration, from, to time.Time) bool {
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
// value holds it only where both do; a Res read coarser refuses more,
// never less. Two that share no offset (a Plain and one with an offset, or
// two offsets apart) meet as an Empty one, which holds nothing. Meet is the
// same in either order and however a place's learnings are grouped, so a
// recording paired many times keeps one answer whatever order its
// pairings came in.
func (o ClockOffset) Meet(p ClockOffset) ClockOffset {
	return ClockOffset{Min: max(o.Min, p.Min), Max: min(o.Max, p.Max), Res: max(o.Res, p.Res)}
}

// Empty reports whether o is the Meet of learnings of one place that share
// no offset: a Min above zero and a Max not above it. Holds holds nothing
// for it, and String says why.
func (o ClockOffset) Empty() bool { return o.Min > 0 && o.Max <= o.Min }

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
// is a cut coarser than res's encoding (CutCoarserThan: a value the
// application cut to the second, stored at the millisecond, would hold only
// where its read fell early enough in its second, by when the replay ran),
// nor one learned at an encoding coarser than the replay's. There is no
// unit to tell apart, so the window's width bounds nothing more.
func (o ClockOffset) holdsRead(at time.Time, res time.Duration, from, to time.Time) bool {
	if o.Res <= 0 || res <= 0 || from.IsZero() || to.Before(from) || o.CutCoarserThan(res) {
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
// "clock reads no one offset holds (from 30m up to 0s)". It never decides
// anything.
func (o ClockOffset) String() string {
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
