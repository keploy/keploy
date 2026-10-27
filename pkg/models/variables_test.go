package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// read is a clock read recorded at at (RFC 3339) to resolution res.
func read(t *testing.T, at string, res time.Duration) ClockRead {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatal(err)
	}
	return ClockRead{At: tm, Res: res}
}

// A clock read goes with the bind it was read at: a's bind keeps a's reads,
// a bind taken from b keeps b's, and a variable both bind is a clock read only
// when both say so.
func TestMergeVariablesKeepsAClockReadWithItsBind(t *testing.T) {
	at := []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Second)}
	for _, c := range []struct {
		name string
		a, b MockVariable
		want []ClockRead
	}{
		{"a's bind, marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, at},
		{"a bind taken from b, marked", MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, at},
		{"both bind, both marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, at},
		{"both bind, one marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Bind: "body.at"}, nil},
		{"both bind, the other marked", MockVariable{Recorded: "r", Bind: "body.at"}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, nil},
		{"a use marked is no clock read", MockVariable{Recorded: "r", Bind: "body.at"}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}, Clock: at}, nil},
	} {
		in := MockVariables{"at": c.a}
		got := MergeVariables(in, MockVariables{"at": c.b})["at"]
		if !reflect.DeepEqual(got.Clock, c.want) && !(len(got.Clock) == 0 && len(c.want) == 0) {
			t.Errorf("%s: merged Clock %v, want %v", c.name, got.Clock, c.want)
		}
		if len(got.Clock) > 0 {
			got.Clock[0].Res = time.Hour
			if in["at"].Clock != nil && in["at"].Clock[0].Res == time.Hour {
				t.Errorf("%s: the merged block shares its reads with a", c.name)
			}
		}
	}
}

// FD-3's case is two reads of one clock made in one second: the earlier
// read's resolution ends less than a second before the later one begins, as
// the recordings say. A creation time and an expiry an hour later are both
// clock reads, and are no such pair; nor is a variable with no reads, nor two
// that hold a different number of reads, nor a time of day and a date.
func TestReadInOneSecond(t *testing.T) {
	sec := func(at string) []ClockRead { return []ClockRead{read(t, at, time.Second)} }
	for _, c := range []struct {
		name string
		a, b []ClockRead
		want bool
	}{
		{"one second, straddling a tick", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:01Z"), true},
		{"the other way round", sec("2026-10-14T10:00:01Z"), sec("2026-10-14T10:00:00Z"), true},
		{"the same instant", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z"), true},
		{"two seconds apart", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:02Z"), false},
		{"an expiry an hour later", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T11:00:00Z"), false},
		{"milliseconds less than a second apart", []ClockRead{read(t, "2026-10-14T10:00:00.100Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:01.099Z", time.Millisecond)}, true},
		{"milliseconds a second apart", []ClockRead{read(t, "2026-10-14T10:00:00.100Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:01.101Z", time.Millisecond)}, false},
		{"minutes straddling a tick", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:01:00Z", time.Minute)}, true},
		{"minutes two apart", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:02:00Z", time.Minute)}, false},
		// The earlier read's resolution, whichever of the two it is.
		{"a minute, then a millisecond read at the next minute", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:01:00.500Z", time.Millisecond)}, true},
		{"a millisecond read, then the minute before it", []ClockRead{read(t, "2026-10-14T10:01:00.500Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, true},
		{"a millisecond read, then a minute read after it", []ClockRead{read(t, "2026-10-14T10:00:58.500Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:01:00Z", time.Minute)}, false},
		{"no reads", nil, nil, false},
		{"one with no reads", sec("2026-10-14T10:00:00Z"), nil, false},
		{"the other with no reads", nil, sec("2026-10-14T10:00:00Z"), false},
		{"another number of reads", sec("2026-10-14T10:00:00Z"), append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z")...), false},
		{"two reads each, the second an hour apart", append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z")...), append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T11:00:00Z")...), false},
		{"times of day across midnight", []ClockRead{{At: time.Date(0, 1, 1, 23, 59, 59, 0, time.UTC), Res: time.Second, Day: true}}, []ClockRead{{At: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, true},
		{"times of day an hour apart", []ClockRead{{At: time.Date(0, 1, 1, 10, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, []ClockRead{{At: time.Date(0, 1, 1, 11, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, false},
		{"a time of day and a date", []ClockRead{{At: time.Date(0, 1, 1, 10, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, sec("0000-01-01T10:00:00Z"), false},
	} {
		if got := ReadInOneSecond(c.a, c.b); got != c.want {
			t.Errorf("%s: ReadInOneSecond %v, want %v", c.name, got, c.want)
		}
	}
}

// A variable's clock reads are stored with its mock, in YAML and JSON, and
// read back as they were.
func TestAVariablesClockReadsRoundTrip(t *testing.T) {
	in := MockVariables{"at": {Recorded: "r", Bind: "body.at", Clock: []ClockRead{
		read(t, "2026-10-14T10:00:00.250Z", time.Millisecond),
		{At: time.Date(0, 1, 1, 10, 0, 41, 0, time.UTC), Res: time.Second, Day: true},
	}}}
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML MockVariables
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("%v in\n%s", err, y)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON MockVariables
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for format, got := range map[string]MockVariables{"yaml": fromYAML, "json": fromJSON} {
		g, w := got["at"].Clock, in["at"].Clock
		if len(g) != len(w) {
			t.Fatalf("%s: read back %v, want %v", format, g, w)
		}
		for i := range w {
			if !g[i].At.Equal(w[i].At) || g[i].Res != w[i].Res || g[i].Day != w[i].Day {
				t.Errorf("%s: read %d back as %+v, want %+v", format, i, g[i], w[i])
			}
		}
	}
}

// A value at the clock plus an offset (ClockOffset) holds where some read of
// its call's window, plus an offset the learn step bounded, cuts to it: the
// edges of both ranges hold, a step past either does not, and a changed time
// to live (thirty minutes become thirty-one, or an hour) does not. No window,
// no resolution and an empty offset hold nothing.
func TestAClockOffsetHoldsOnlyAReadOfItsWindowPlusThatOffset(t *testing.T) {
	from := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	to := from.Add(50 * time.Millisecond)
	o := ClockOffset{Min: 30*time.Minute - 10*time.Millisecond, Max: 30*time.Minute + 5*time.Millisecond, Res: time.Millisecond}
	ms := time.Millisecond
	ns := o
	ns.Res = time.Nanosecond
	for _, row := range []struct {
		name string
		at   time.Time
		res  time.Duration
		o    ClockOffset
		from time.Time
		to   time.Time
		want bool
	}{
		{"a read in the window plus thirty minutes", from.Add(20*ms + 30*time.Minute), ms, o, from, to, true},
		{"the earliest read plus the least offset", from.Add(o.Min), ms, o, from, to, true},
		{"a millisecond before it", from.Add(o.Min - ms), ms, o, from, to, false},
		{"the latest read plus the greatest offset, less a nanosecond", to.Add(o.Max - 1), time.Nanosecond, ns, from, to, true},
		{"the latest read plus the greatest offset", to.Add(o.Max), time.Nanosecond, ns, from, to, false},
		{"a value at a nanosecond's encoding, the offset learned at a millisecond's (a cut coarser than it)", to.Add(o.Max - 1), time.Nanosecond, o, from, to, false},
		{"a millisecond value whose millisecond holds that edge", to.Add(o.Max - 1).Truncate(ms), ms, o, from, to, true},
		{"thirty-one minutes", from.Add(20*ms + 31*time.Minute), ms, o, from, to, false},
		{"an hour", from.Add(20*ms + time.Hour), ms, o, from, to, false},
		{"the minute of a read plus thirty minutes: the window, the offset's width and a minute add up to more than a minute", from.Add(30 * time.Minute).Truncate(time.Minute), time.Minute, o, from, to, false},
		{"no window", from.Add(20*ms + 30*time.Minute), ms, o, time.Time{}, time.Time{}, false},
		{"a window with no start", from.Add(20*ms + 30*time.Minute), ms, o, time.Time{}, to, false},
		{"a window with no end", from.Add(20*ms + 30*time.Minute), ms, o, from, time.Time{}, false},
		{"a window that ends before it starts", from.Add(20*ms + 30*time.Minute), ms, o, to, from, false},
		{"a window that ends 10 ms before it starts", from.Add(30*time.Minute + ms), ms, o, from.Add(10 * ms), from, false},
		{"no resolution", from.Add(20*ms + 30*time.Minute), 0, o, from, to, false},
		{"an empty offset", from.Add(20*ms + 30*time.Minute), ms, ClockOffset{Min: 30 * time.Minute, Max: 30 * time.Minute}, from, to, false},
	} {
		if got := row.o.Holds(row.at, row.res, row.from, row.to); got != row.want {
			t.Errorf("%s: Holds = %v, want %v", row.name, got, row.want)
		}
	}
}

// A replay's verdict on a ClockOffset never turns on where its read falls
// for a time to live changed by its unit or more: it holds only where its
// window, the offset's width and the value's resolution add up to less than
// the unit (a minute, for thirty minutes), and then a value a minute off
// holds at no read in the window, at either edge; at the unit or more,
// nothing holds, not even the value itself (the replay fails closed).
func TestAClockOffsetHoldsOnlyWhereItsWindowIsNarrowerThanItsUnit(t *testing.T) {
	ms := time.Millisecond
	o := ClockOffset{Min: 30*time.Minute - 10*ms, Max: 30*time.Minute + 5*ms, Res: ms}
	from := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	widest := time.Minute - (o.Max - o.Min) - ms - 1
	to := from.Add(widest)
	for _, read := range []time.Time{from, to} {
		if !o.Holds(read.Add(30*time.Minute).Truncate(ms), ms, from, to) {
			t.Errorf("a read at %s plus thirty minutes, the window as wide as it may be: not held, want held", read.Sub(from))
		}
		for _, ttl := range []time.Duration{31 * time.Minute, 29 * time.Minute} {
			if o.Holds(read.Add(ttl).Truncate(ms), ms, from, to) {
				t.Errorf("a read at %s plus %s, the window as wide as it may be: held, want not", read.Sub(from), ttl)
			}
		}
	}
	if o.Holds(from.Add(30*time.Minute), ms, from, to.Add(1)) {
		t.Errorf("a window a nanosecond wider, adding up to a minute: held, want nothing held")
	}
	if !o.Within(widest, o.Max-o.Min, ms) || o.Within(widest+1, o.Max-o.Min, ms) || !o.Within() {
		t.Errorf("Within: want the widest window within a minute, a nanosecond more not, and no span within")
	}
	// N1 of the review of 8e4346458: an offset the learn step never finds
	// holds nothing. Its width overflowing to below zero, or a unit of a day
	// around zero, would hold any value.
	for _, bad := range []ClockOffset{
		{Min: -(1 << 62), Max: 1<<63 - 1, Res: ms},
		{Min: -time.Hour, Max: time.Hour, Res: ms},
		{Min: 0, Max: ms, Res: ms},
		{Min: 30 * time.Minute, Max: 30 * time.Minute, Res: ms},
		{Min: 30*time.Minute + ms, Max: 30 * time.Minute, Res: ms},
	} {
		for _, at := range []time.Time{from, from.Add(30 * time.Minute), from.Add(-time.Hour), from.Add(1 << 61)} {
			if bad.Holds(at, ms, from, from) {
				t.Errorf("FALSE GREEN? %+v holds %s: want nothing held", bad, at.Sub(from))
			}
		}
	}
}

// B1 of the offset rule's second and third reviews: Holds counts the
// coarser of the replay's resolution and the one the learn step read the
// value at (Res, the granularity both runs' values shared) in its sum, and
// reads the replay's value at the replay's own, so a grain the two values
// shared by chance only ever refuses more. With Res half a second, the read
// plus thirty minutes holds, a value 400 ms before it does not, and
// thirty-one or twenty-nine minutes never do; a window that with Res adds up
// to a minute holds nothing. An offset whose Res is a cut coarser than the
// replay's encoding (a second, 2 s, 31 s or a minute at a millisecond's:
// CutCoarserThan), which the learn step never learns, holds nothing, not
// even thirty minutes, so thirty-one minutes read late in its minute, or
// twenty-nine minutes and fifty-nine seconds at 2 s, never holds. An offset
// that reads in seconds or finer holds nothing (a cut to the unit makes a
// time to live changed by one look the same), and neither does one with a
// Res of none or below, which the learn step never finds: one built without
// it (from the API, or a hand-edited mock) would have its sums read finer
// than it was learned at.
func TestAClockOffsetHoldsAtTheReplaysOwnResolution(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	o := ClockOffset{Min: 30*time.Minute - 10*ms, Max: 30*time.Minute + 21*ms, Res: 500 * ms}
	for step := time.Duration(0); step < sec; step += 10 * ms {
		from := base.Add(step)
		to, read := from.Add(50*ms), from.Add(20*ms)
		for _, row := range []struct {
			at   time.Time
			want bool
		}{{read.Add(30 * time.Minute), true}, {read.Add(30*time.Minute - 400*ms), false}, {read.Add(31 * time.Minute), false}, {read.Add(29 * time.Minute), false}} {
			if got := o.Holds(row.at, ms, from, to); got != row.want {
				t.Errorf("%s: Res half a second, a value %s from a read %s into its second: holds %v, want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], row.at.Sub(read), read.Sub(read.Truncate(sec)), got, row.want)
			}
		}
	}
	if wide := time.Minute - (o.Max - o.Min) - o.Res; o.Holds(base.Add(30*time.Minute), ms, base, base.Add(wide)) {
		t.Errorf("a window that with Res adds up to a minute: held, want nothing held")
	}
	late := base.Add(59*sec + 900*ms)
	for _, cut := range []ClockOffset{
		{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: sec},
		{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: 2 * sec},
		{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: 31 * sec},
		{Min: 30*time.Minute - 600*ms, Max: 30*time.Minute + 600*ms, Res: time.Minute},
	} {
		for _, at := range []time.Time{late.Add(30 * time.Minute), late.Truncate(sec).Add(30 * time.Minute), late.Truncate(2 * sec).Add(30*time.Minute - sec), late.Truncate(time.Minute).Add(30 * time.Minute), late.Truncate(time.Minute).Add(31 * time.Minute)} {
			if cut.Holds(at, ms, late.Add(-10*ms), late) {
				t.Errorf("FALSE GREEN? %+v, a cut coarser than the replay's millisecond, holds %s from its read: want nothing held", cut, at.Sub(late))
			}
		}
	}
	for _, bad := range []ClockOffset{
		{Min: 45*sec - 155*ms, Max: 45*sec + 368*ms, Res: 500 * ms},
		{Min: 45*sec - 155*ms, Max: 45*sec + 368*ms, Res: ms},
		{Min: ms, Max: 52 * ms, Res: ms},
		{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms, Res: -ms},
		{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms},
		{Min: 30*time.Minute - 600*ms, Max: 30*time.Minute + 600*ms},
	} {
		from := base.Add(100 * ms)
		read := from.Add(20 * ms)
		for _, at := range []time.Time{read.Add(bad.Min).Truncate(ms), read.Add(bad.Min).Truncate(sec), read.Add(45 * sec), read.Add(30 * time.Minute).Truncate(ms)} {
			if bad.Holds(at, ms, from, from.Add(50*ms)) {
				t.Errorf("FALSE GREEN? %+v holds %s: want nothing held", bad, at.Sub(read))
			}
		}
	}
}

// A Res is a cut the application made where it is above the encoding's
// resolution and a whole number of the unit above it: a second, 2 s, 31 s
// or a minute above a millisecond; a millisecond above a microsecond; a
// minute above a second; an hour above a minute. Half a second above a
// millisecond, 2 s above a second, 30.751 s above a millisecond and the
// encoding's own resolution are none: an hour's time to live at a minute's
// encoding (Res a minute) is no cut. Above a day, or any resolution no unit
// is above, every coarser Res is, and the encoding's own is not.
func TestAResIsACutWhereItIsAWholeNumberOfTheUnitAboveTheEncoding(t *testing.T) {
	us, ms, sec, min := time.Microsecond, time.Millisecond, time.Second, time.Minute
	for _, row := range []struct {
		res, enc time.Duration
		want     bool
	}{
		{sec, ms, true}, {2 * sec, ms, true}, {31 * sec, ms, true}, {min, ms, true},
		{ms, us, true}, {7 * ms, us, true}, {min, sec, true}, {2 * min, sec, true}, {time.Hour, min, true},
		{500 * ms, ms, false}, {30751 * ms, ms, false}, {2 * sec, sec, false}, {31 * sec, sec, false},
		{ms, ms, false}, {sec, sec, false}, {min, min, false}, {2 * min, min, false}, {500 * us, us, false},
		{sec, 10 * ms, true}, {20 * ms, 10 * ms, false}, {2 * time.Nanosecond, time.Nanosecond, false}, {us, time.Nanosecond, true},
		{48 * time.Hour, 24 * time.Hour, true}, {24 * time.Hour, 24 * time.Hour, false}, {ms, 2 * ms, false}, {0, ms, false},
	} {
		if got := (ClockOffset{Res: row.res}).CutCoarserThan(row.enc); got != row.want {
			t.Errorf("Res %s at an encoding of %s: a cut %v, want %v", row.res, row.enc, got, row.want)
		}
	}
	// Holds reads it against the replay's resolution: an hour learned at a
	// minute's encoding holds at a minute's, and nothing at a millisecond's.
	o := ClockOffset{Min: time.Hour - 17155*ms, Max: time.Hour + 41368*ms, Res: min}
	from := time.Date(2026, 10, 26, 13, 0, 7, 0, time.UTC)
	read := from.Add(20 * ms)
	if !o.Holds(read.Add(time.Hour).Truncate(min), min, from, from.Add(50*ms)) {
		t.Errorf("FALSE RED: an hour at a minute's encoding, at a replay of a minute's: not held")
	}
	if o.Holds(read.Add(time.Hour).Truncate(ms), ms, from, from.Add(50*ms)) {
		t.Errorf("an hour learned at a minute's encoding held at a replay of a millisecond's: want nothing held")
	}
}

// A ClockOffset's unit is the coarsest whole unit one of whose multiples
// lies in it; an empty one has none.
func TestAClockOffsetsUnitIsTheCoarsestWithAMultipleInIt(t *testing.T) {
	ms := time.Millisecond
	for _, row := range []struct {
		o    ClockOffset
		want time.Duration
	}{
		{ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms}, time.Minute},
		{ClockOffset{Min: 30*time.Minute - 40*ms, Max: 33*time.Minute - 44*ms}, time.Minute},
		{ClockOffset{Min: 30*24*time.Hour - time.Second, Max: 30*24*time.Hour + time.Second}, 24 * time.Hour},
		{ClockOffset{Min: 59 * time.Minute, Max: time.Hour + 1}, time.Hour},
		{ClockOffset{Min: 59 * time.Minute, Max: time.Hour}, time.Minute},
		{ClockOffset{Min: 1500 * time.Microsecond, Max: 1500*time.Microsecond + 2}, time.Microsecond},
		{ClockOffset{Min: 1, Max: 2}, time.Nanosecond},
		{ClockOffset{Min: -90 * time.Second, Max: -30 * time.Second}, time.Minute},
		{ClockOffset{Min: 30 * time.Minute, Max: 30 * time.Minute}, 0},
	} {
		if got := row.o.Unit(); got != row.want {
			t.Errorf("%+v: Unit %s, want %s", row.o, got, row.want)
		}
	}
}

// A ClockOffset reads plainly: the offset as the coarsest whole unit in it,
// and the bounds where they differ; one that holds more than one multiple of
// its unit names no offset, only its bounds.
func TestAClockOffsetReadsAsTheClockPlusItsOffset(t *testing.T) {
	ms := time.Millisecond
	for _, row := range []struct {
		o    ClockOffset
		want string
	}{
		{ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + 1}, "clock + 30m"},
		{ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms}, "clock + 30m (from 29m59.96s to 30m0.011s)"},
		{ClockOffset{Min: 30*time.Minute - 40*ms, Max: 33*time.Minute - 44*ms}, "clock plus an offset from 29m59.96s to 32m59.956s"},
		{ClockOffset{Min: 30 * time.Minute, Max: 31 * time.Minute}, "clock + 30m (from 30m to 31m)"},
		{ClockOffset{Min: 30 * time.Minute, Max: 31*time.Minute + 1}, "clock plus an offset from 30m to 31m0.000000001s"},
		{ClockOffset{Min: 30 * time.Minute, Max: 30 * time.Minute}, "clock reads no one offset holds (from 30m up to 30m)"},
		{ClockOffset{Res: ms}.Meet(ClockOffset{Min: 30 * time.Minute, Max: 31 * time.Minute, Res: ms}), "clock reads no one offset holds (from 30m up to 0s)"},
		{ClockOffset{Min: 30*24*time.Hour - time.Second, Max: 30*24*time.Hour + time.Second}, "clock + 30d (from 719h59m59s to 720h0m1s)"},
		{ClockOffset{Min: 90*time.Minute - ms, Max: 90*time.Minute + ms}, "clock + 1h30m (from 1h29m59.999s to 1h30m0.001s)"},
		{ClockOffset{Min: 1500 * time.Microsecond, Max: 1500*time.Microsecond + 2}, "clock + 1.5ms (from 1.5ms to 1.500002ms)"},
	} {
		if got := row.o.String(); got != row.want {
			t.Errorf("%+v: %q, want %q", row.o, got, row.want)
		}
	}
	// N4: Range names no offset, even where one multiple is in it.
	if got, want := (ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms}).Range(), "clock plus an offset from 29m59.96s to 30m0.011s"; got != want {
		t.Errorf("Range: %q, want %q", got, want)
	}
}

// A zero offset (Plain) is a plain read of the application's clock: it
// holds a value its call's window holds at the value's own resolution, at
// either end, and nothing a resolution past it, thirty days before it or at
// the epoch (the false green of a clock read learned as noise, which no
// replay compared again). A second's value holds where its second meets the
// window. It holds nothing with no Res, no res, a window with no start or
// that ends before it starts, or a Res that is a cut coarser than the
// replay's encoding (a value cut to the second, stored at the millisecond,
// would hold by where its read fell in its second). Its String is "clock".
func TestAPlainClockReadHoldsOnlyAReadOfItsOwnWindow(t *testing.T) {
	ms := time.Millisecond
	from := time.Date(2026, 10, 26, 10, 0, 0, 500*int(ms), time.UTC)
	to := from.Add(20 * ms)
	o := ClockOffset{Res: ms}
	if !o.Plain() || (ClockOffset{Min: ms, Res: ms}).Plain() || o.String() != "clock" {
		t.Fatalf("Plain/String: %v %v %q", o.Plain(), (ClockOffset{Min: ms, Res: ms}).Plain(), o.String())
	}
	for _, row := range []struct {
		what     string
		at       time.Time
		res      time.Duration
		o        ClockOffset
		from, to time.Time
		want     bool
	}{
		{"a read inside the window", from.Add(5 * ms), ms, o, from, to, true},
		{"a read at the window's start", from, ms, o, from, to, true},
		{"a read at the window's end", to, ms, o, from, to, true},
		{"a read a resolution past the end", to.Add(ms), ms, o, from, to, false},
		{"a read a resolution before the start", from.Add(-ms), ms, o, from, to, false},
		{"m10: thirty days before", from.Add(-30 * 24 * time.Hour), ms, o, from, to, false},
		{"m10b: the epoch", time.Unix(0, 0), ms, o, from, to, false},
		{"a second's value whose second meets the window", from.Truncate(time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, true},
		{"a second's value a second later", from.Truncate(time.Second).Add(time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, false},
		{"no Res", from.Add(5 * ms), ms, ClockOffset{}, from, to, false},
		{"no res", from.Add(5 * ms), 0, o, from, to, false},
		{"a window with no start", from.Add(5 * ms), ms, o, time.Time{}, to, false},
		{"a window that ends before it starts", from.Add(5 * ms), ms, o, to, from, false},
		{"a cut to the second at a millisecond's encoding", from.Truncate(time.Second), ms, ClockOffset{Res: time.Second}, from.Add(-600 * ms), to, false},
		{"a millisecond's Res at a nanosecond's replay", from.Add(5 * ms), time.Nanosecond, o, from, to, false},
		{"a wall clock of no zone read in the application's zone", from.Add(5 * ms), ms, ClockOffset{Res: ms, Zone: ZoneApp}, from, to, true},
		{"a wall clock of no zone read in UTC", from.Add(5 * ms), ms, ClockOffset{Res: ms, Zone: ZoneUTC}, from, to, true},
		{"two learnings in different zones", from.Add(5 * ms), ms, ClockOffset{Res: ms, Zone: ZoneApart}, from, to, false},
		{"a zone this keploy does not read", from.Add(5 * ms), ms, ClockOffset{Res: ms, Zone: "pst"}, from, to, false},
		{"an offset in a zone this keploy does not read", from.Add(30*time.Minute + 5*ms), ms, ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + ms, Res: ms, Zone: "pst"}, from, to, false},
	} {
		if got := row.o.Holds(row.at, row.res, row.from, row.to); got != row.want {
			t.Errorf("%s: Holds = %v, want %v", row.what, got, row.want)
		}
	}
}

// Two learnings of one place's clock read meet as the offsets both hold, at
// the coarser resolution, in either order and however grouped (MergeClocks
// of many pairings gives one answer whatever order they came in). A plain
// read met with an offset, or two offsets apart, is Empty: it holds nothing
// (not even the value the replay's own clock wrote), and says so.
func TestTwoLearningsOfAClockReadMeetAsWhatBothHold(t *testing.T) {
	ms := time.Millisecond
	plain, plainUs := ClockOffset{Res: ms}, ClockOffset{Res: time.Microsecond}
	a := ClockOffset{Min: 30*time.Minute - 20*ms, Max: 30*time.Minute + 10*ms, Res: time.Microsecond}
	b := ClockOffset{Min: 30*time.Minute - 5*ms, Max: 30*time.Minute + 40*ms, Res: ms}
	far := ClockOffset{Min: 31 * time.Minute, Max: 31*time.Minute + 10*ms, Res: ms}
	if got, want := a.Meet(b), (ClockOffset{Min: b.Min, Max: a.Max, Res: ms}); got != want || b.Meet(a) != want || got.Empty() {
		t.Fatalf("a meet b = %+v, b meet a = %+v; want %+v, not Empty", got, b.Meet(a), want)
	}
	if got := plainUs.Meet(plain); !got.Plain() || got.Res != ms || got.Empty() {
		t.Fatalf("two plain reads meet as %+v; want a plain read at 1ms", got)
	}
	from := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	for _, e := range []ClockOffset{plain.Meet(a), a.Meet(plain), a.Meet(far), far.Meet(a)} {
		if !e.Empty() || e.Plain() {
			t.Fatalf("%+v: want Empty, not Plain", e)
		}
		if e.Holds(from.Add(5*ms), ms, from, from.Add(10*ms)) || e.Holds(from.Add(30*time.Minute), ms, from, from.Add(10*ms)) {
			t.Fatalf("%+v holds a value", e)
		}
		if s := e.String(); !strings.HasPrefix(s, "clock reads no one offset holds") {
			t.Fatalf("%+v reads %q", e, s)
		}
	}
	if (ClockOffset{}).Empty() || plain.Empty() || a.Empty() {
		t.Fatal("a learning on its own is not Empty")
	}
	learnings := []map[string]ClockOffset{{"x": a, "p": plain}, {"x": b}, {"x": a, "y": far}, nil, {"p": plainUs}}
	want := map[string]ClockOffset{"x": a.Meet(b), "p": plain, "y": far}
	for _, order := range [][]int{{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}} {
		var got map[string]ClockOffset
		for _, i := range order {
			got = MergeClocks(got, learnings[i])
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order %v merges as %+v, want %+v", order, got, want)
		}
	}
	if MergeClocks(nil, map[string]ClockOffset{}) != nil {
		t.Fatal("MergeClocks of none is not nil")
	}
	in := map[string]ClockOffset{"x": a}
	if out := MergeClocks(in, map[string]ClockOffset{"x": b}); out["x"] == a || in["x"] != a {
		t.Fatalf("MergeClocks wrote its input: in %+v, out %+v", in, out)
	}
}

// A plain clock read holds nothing in a window that ends before it starts.
func TestAPlainClockReadHoldsNothingInAWindowThatEndsBeforeItStarts(t *testing.T) {
	from := time.Date(2026, 10, 27, 12, 0, 0, 500*int(time.Millisecond), time.UTC)
	o := ClockOffset{Res: time.Second}
	if !o.Holds(from.Add(10*time.Millisecond), time.Second, from, from.Add(40*time.Millisecond)) {
		t.Fatal("the window's own read refused")
	}
	if o.Holds(from.Add(-300*time.Millisecond), time.Second, from, from.Add(-200*time.Millisecond)) {
		t.Fatal("a window that ends before it starts held a read")
	}
}

// A place's zone (ClockOffset.Zone) is carried through Meet: two learnings
// in one zone keep it, two in different zones meet as ZoneApart, which is
// Empty and holds nothing, in either order and however grouped. String
// names the zone a replay reads a wall clock of no zone in, and says so of
// ZoneApart and of a zone this keploy does not read; ZoneKnown is true of
// "", ZoneUTC and ZoneApp alone.
func TestAClockReadsZoneMeetsAsOneOrHoldsNothing(t *testing.T) {
	ms := time.Millisecond
	app, utc, named := ClockOffset{Res: ms, Zone: ZoneApp}, ClockOffset{Res: time.Second, Zone: ZoneUTC}, ClockOffset{Res: ms}
	if got := app.Meet(ClockOffset{Res: time.Second, Zone: ZoneApp}); got != (ClockOffset{Res: time.Second, Zone: ZoneApp}) || got.Empty() {
		t.Errorf("two reads in the application's zone met as %+v (Empty %v); want one there at the coarser Res", got, got.Empty())
	}
	for _, pair := range [][2]ClockOffset{{app, utc}, {utc, app}, {app, named}, {named, utc}} {
		got := pair[0].Meet(pair[1])
		if got.Zone != ZoneApart || !got.Empty() || got.Holds(time.Unix(0, 0), ms, time.Unix(0, 0), time.Unix(1, 0)) {
			t.Errorf("%+v meet %+v = %+v (Empty %v); want ZoneApart, Empty, holding nothing", pair[0], pair[1], got, got.Empty())
		}
		if s := got.String(); !strings.Contains(s, "no one zone") {
			t.Errorf("String of %+v = %q; want it to say no one zone holds it", got, s)
		}
	}
	if a, b := app.Meet(utc).Meet(app), app.Meet(utc.Meet(app)); a != b {
		t.Errorf("Meet grouped two ways: %+v and %+v", a, b)
	}
	// A read in either zone meets one in UTC or in the application's zone as
	// that one, the zone both hold, in either order; another of either as
	// either; one that names its zone as no zone at all. Grouped any way,
	// either, UTC and the application's meet as ZoneApart.
	either := ClockOffset{Res: ms, Zone: ZoneEither}
	for _, row := range []struct {
		with ClockOffset
		want string
	}{{utc, ZoneUTC}, {app, ZoneApp}, {either, ZoneEither}, {named, ZoneApart}, {ClockOffset{Res: ms, Zone: "pst"}, ZoneApart}} {
		for _, got := range []ClockOffset{either.Meet(row.with), row.with.Meet(either)} {
			if got.Zone != row.want || got.Empty() != (row.want == ZoneApart) {
				t.Errorf("either meet %+v = %+v (Empty %v); want zone %q", row.with, got, got.Empty(), row.want)
			}
		}
	}
	for _, got := range []ClockOffset{either.Meet(utc).Meet(app), either.Meet(utc.Meet(app)), app.Meet(either).Meet(utc), utc.Meet(app.Meet(either))} {
		if got.Zone != ZoneApart || !got.Empty() {
			t.Errorf("either, UTC and the application's zone met as %+v; want ZoneApart", got)
		}
	}
	for _, row := range []struct {
		o    ClockOffset
		want string
	}{
		{named, "clock"},
		{app, "clock, a wall clock of no zone in the application's zone"},
		{utc, "clock, a wall clock of no zone in UTC"},
		{either, "clock, a wall clock of no zone in UTC or in the application's zone"},
		{ClockOffset{Res: ms, Zone: "pst"}, `clock read in a zone this keploy does not read ("pst")`},
	} {
		if got := row.o.String(); got != row.want {
			t.Errorf("String of %+v = %q; want %q", row.o, got, row.want)
		}
	}
	for zone, want := range map[string]bool{"": true, ZoneUTC: true, ZoneApp: true, ZoneEither: true, ZoneApart: false, "pst": false} {
		if got := (ClockOffset{Res: ms, Zone: zone}).ZoneKnown(); got != want {
			t.Errorf("ZoneKnown of %q = %v; want %v", zone, got, want)
		}
	}
}
