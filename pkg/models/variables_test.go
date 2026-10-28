package models

import (
	"encoding/json"
	"os"
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
// its call's window, plus an offset the learn step bounded, cuts to it,
// within one resolution of it on either side (2026-10-28): one resolution
// past the edges of both ranges holds, two do not, and a changed time
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
		{"a millisecond before it (one resolution stale)", from.Add(o.Min - ms), ms, o, from, to, true},
		{"two milliseconds before it", from.Add(o.Min - 2*ms), ms, o, from, to, false},
		{"the latest read plus the greatest offset, less a nanosecond", to.Add(o.Max - 1), time.Nanosecond, ns, from, to, true},
		{"the latest read plus the greatest offset, plus a nanosecond (one resolution ahead)", to.Add(o.Max + 1), time.Nanosecond, ns, from, to, true},
		{"the latest read plus the greatest offset, plus two nanoseconds", to.Add(o.Max + 2), time.Nanosecond, ns, from, to, false},
		{"a value at a nanosecond's encoding, the offset learned at a millisecond's, checked at the millisecond", to.Add(o.Max + ms), time.Nanosecond, o, from, to, true},
		{"a nanosecond value more than a millisecond past the greatest", to.Add(o.Max + ms + 1), time.Nanosecond, o, from, to, false},
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
	widest := time.Minute - (o.Max - o.Min) - 3*ms - 1
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
	if !o.Within(widest, o.Max-o.Min, ms, ms, ms) || o.Within(widest+1, o.Max-o.Min, ms, ms, ms) || !o.Within() {
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

// B1 of the offset rule's second and third reviews: Holds counts three of
// the coarser of the resolution it checks the value at and the one the
// learn step read it at (Res) in its sum. With Res half a second (no step:
// two values shared it by chance, before cuts were learned), the value is
// read at the replay's own millisecond: the read plus thirty minutes holds,
// a value 400 ms before it does not, and thirty-one or twenty-nine minutes
// never do; a window that with three of Res adds up to a minute holds
// nothing. A cut the learn step read (a second) is checked at the cut,
// within one of it of the span on either side, at every read position. An
// offset that reads in seconds or finer holds nothing (a cut to the unit
// makes a time to live changed by one look the same), and neither does one
// with a Res of none or below, which the learn step never finds: one built
// without it (from the API, or a hand-edited mock) would have its sums read
// finer than it was learned at.
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
	// A cut the learn step read (a second, a step of cutSteps) is checked at
	// the second, at every read position in it: a value cut down or rounded
	// holds, the edges of the widened span hold (from the second that holds
	// the earliest read plus Min, less a second, up to the latest plus Max,
	// plus a second), a millisecond past either does not, and thirty-one or
	// twenty-nine minutes never do. Thirty minutes cut to the minute holds
	// nothing: three minutes and its width add up to more than the minute it
	// reads in. A Res of 2 s or 31 s (no step: two values shared it by
	// chance) is checked at the replay's millisecond.
	cut := ClockOffset{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: sec}
	for step := time.Duration(0); step < sec; step += 10 * ms {
		from := base.Add(step)
		to, read := from.Add(10*ms), from.Add(5*ms)
		lo, hi := from.Add(cut.Min).Truncate(sec).Add(-sec), to.Add(cut.Max).Add(sec)
		for _, row := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"cut down", read.Add(30 * time.Minute).Truncate(sec), true}, {"rounded", read.Add(30 * time.Minute).Round(sec), true},
			{"the widened span's start", lo, true}, {"its end", hi, true},
			{"a millisecond before it", lo.Add(-ms), false}, {"a millisecond after it", hi.Add(ms), false},
			{"thirty-one minutes", read.Add(31 * time.Minute).Truncate(sec), false}, {"twenty-nine", read.Add(29 * time.Minute).Truncate(sec), false},
		} {
			if got := cut.Holds(row.at, ms, from, to); got != row.want {
				t.Errorf("%s: thirty minutes cut to the second, %s, the read %s into its second: holds %v, want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], row.name, read.Sub(read.Truncate(sec)), got, row.want)
			}
		}
	}
	if cut.Holds(base.Add(30*time.Minute), ms, base, base.Add(57*sec)) {
		t.Errorf("FALSE GREEN? a window 57 s wide, which with the offset's width and three seconds adds up to more than a minute: held, want nothing held")
	}
	if !cut.Holds(base.Add(30*time.Minute), ms, base, base.Add(56*sec)) {
		t.Errorf("FALSE RED: a window 56 s wide, which with the offset's width and three seconds adds up to less than a minute: not held")
	}
	late := base.Add(59*sec + 900*ms)
	if wide := (ClockOffset{Min: 30*time.Minute - 600*ms, Max: 30*time.Minute + 600*ms, Res: time.Minute}); wide.Holds(late.Truncate(time.Minute).Add(30*time.Minute), ms, late.Add(-10*ms), late) {
		t.Errorf("FALSE GREEN? thirty minutes cut to the minute held: want nothing held")
	}
	chance := ClockOffset{Min: 30*time.Minute - 155*ms, Max: 30*time.Minute + 368*ms, Res: 2 * sec}
	if !chance.Holds(late.Add(30*time.Minute), ms, late.Add(-10*ms), late) || chance.Holds(late.Truncate(sec).Add(30*time.Minute-sec), ms, late.Add(-10*ms), late) {
		t.Errorf("%+v, a Res no step: want the read plus thirty minutes held at the millisecond, and a value a second and more before it not", chance)
	}
	if chance.Res = 31 * sec; chance.Holds(late.Add(30*time.Minute), ms, late.Add(-10*ms), late) {
		t.Errorf("%+v: three of a Res of 31 s add up to more than a minute: held, want nothing held", chance)
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

// A replay checks a value of an offset at its Res where that is a step of
// cutSteps coarser than the value's encoding (a cut the learn step read: a
// second or a minute above a millisecond, a millisecond above a
// microsecond, 10 ms above a microsecond), else at the value's own: a Res
// of the encoding itself, or one learned before cuts were (500 ms or 2 s two
// values shared by chance), is none. An hour learned at a minute's encoding
// is checked at the minute at a replay of a millisecond's, as standing for
// its whole minute: a value read in the window holds wherever its read
// fell in its minute, and one more than a minute past either side does not.
func TestAnOffsetIsCheckedAtTheCutItsValuesShowed(t *testing.T) {
	us, ms, sec, min := time.Microsecond, time.Millisecond, time.Second, time.Minute
	for _, row := range []struct {
		res, enc, want time.Duration
	}{
		{sec, ms, sec}, {min, ms, min}, {ms, us, ms}, {10 * ms, us, 10 * ms}, {min, sec, min},
		{2 * sec, ms, ms}, {500 * ms, ms, ms}, {31 * sec, ms, ms}, {2 * sec, sec, sec},
		{ms, ms, ms}, {sec, sec, sec}, {ms, sec, sec}, {0, ms, ms},
		{10 * ms, ms, ms}, {100 * ms, ms, ms}, {100 * ms, 100 * us, 100 * ms}, {10 * us, us, 10 * us},
	} {
		if got := (ClockOffset{Res: row.res}).CheckedAt(row.enc); got != row.want {
			t.Errorf("Res %s at an encoding of %s: checked at %s, want %s", row.res, row.enc, got, row.want)
		}
	}
	o := ClockOffset{Min: time.Hour - 17155*ms, Max: time.Hour + 41368*ms, Res: min}
	from := time.Date(2026, 10, 26, 13, 0, 7, 0, time.UTC)
	for _, enc := range []time.Duration{min, ms} {
		for step := time.Duration(0); step < min; step += 250 * ms {
			read := from.Add(step)
			if !o.Holds(read.Add(time.Hour).Truncate(enc), enc, read, read.Add(50*ms)) {
				t.Errorf("FALSE RED: an hour cut to the minute, at a replay of %s's encoding, its read %s into its minute: not held", enc, read.Sub(read.Truncate(min)))
			}
			for _, off := range []time.Duration{-2*min - 17200*ms, 2*min + 41500*ms, 2 * time.Hour, 0} {
				if o.Holds(read.Add(time.Hour+off).Truncate(enc), enc, read, read.Add(50*ms)) != (off == 0) {
					t.Errorf("an hour cut to the minute plus %s, at a replay of %s's encoding, its read %s into its minute: held %v", off, enc, read.Sub(read.Truncate(min)), off == 0)
				}
			}
		}
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
		{"a read a resolution past the end (a writer that rounds)", to.Add(ms), ms, o, from, to, true},
		{"a read a resolution before the start", from.Add(-ms), ms, o, from, to, true},
		{"a read two resolutions past the end", to.Add(2 * ms), ms, o, from, to, false},
		{"a read two resolutions before the start", from.Add(-2 * ms), ms, o, from, to, false},
		{"m10: thirty days before", from.Add(-30 * 24 * time.Hour), ms, o, from, to, false},
		{"m10b: the epoch", time.Unix(0, 0), ms, o, from, to, false},
		{"a second's value whose second meets the window", from.Truncate(time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, true},
		{"a second's value a second later (rounded up)", from.Truncate(time.Second).Add(time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, true},
		{"a second's value a second stale", from.Truncate(time.Second).Add(-time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, true},
		{"a second's value two seconds later", from.Truncate(time.Second).Add(2 * time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, false},
		{"a second's value two seconds stale", from.Truncate(time.Second).Add(-2 * time.Second), time.Second, ClockOffset{Res: time.Second}, from, to, false},
		{"no Res", from.Add(5 * ms), ms, ClockOffset{}, from, to, false},
		{"no res", from.Add(5 * ms), 0, o, from, to, false},
		{"a window with no start", from.Add(5 * ms), ms, o, time.Time{}, to, false},
		{"a window that ends before it starts", from.Add(5 * ms), ms, o, to, from, false},
		{"a cut to the second at a millisecond's encoding", from.Truncate(time.Second), ms, ClockOffset{Res: time.Second}, from.Add(-600 * ms), to, false},
		{"a millisecond's Res at a nanosecond's replay", from.Add(5 * ms), time.Nanosecond, o, from, to, false},
		{"a Res of a tenth (learned at an encoding of tenths) at a millisecond's replay", from.Add(5 * ms), ms, ClockOffset{Res: 100 * ms}, from, to, false},
		{"a Res of 10 ms at a millisecond's replay", from.Add(5 * ms), ms, ClockOffset{Res: 10 * ms}, from, to, false},
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

// A plain read checked at a cut of an hour or a day reads the cut in the
// zone its value carries (the zone its cut is read in), and a day is one of
// that zone's calendar: in UTC+05:30 an hour starts at :30 in UTC, and in
// Berlin the day the clocks go back (2026-10-25) is 25 hours long, so a read
// late in it still holds that day's date and yesterday's, and the day
// before yesterday's never does; the morning after, yesterday's date (its
// day began 25 hours before today's) holds; and late on 2026-03-28, the day
// before a day of 23 hours, the next day's date holds and the one after it
// never does (24 hours later is already past its midnight).
func TestAPlainClockReadIsCheckedAtItsCutInItsZone(t *testing.T) {
	ist := time.FixedZone("", 5*3600+1800)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zone data: %v", err)
	}
	h, d := time.Hour, 24*time.Hour
	o := ClockOffset{Res: h}
	from := time.Date(2026, 10, 27, 10, 50, 0, 0, ist)
	to := from.Add(time.Minute)
	late := time.Date(2026, 10, 25, 23, 40, 0, 0, berlin)
	morning := time.Date(2026, 10, 26, 8, 0, 0, 0, berlin)
	spring := time.Date(2026, 3, 28, 23, 30, 0, 0, berlin)
	for _, row := range []struct {
		what     string
		at       time.Time
		res      time.Duration
		o        ClockOffset
		from, to time.Time
		want     bool
	}{
		{"the IST hour of the read", time.Date(2026, 10, 27, 10, 0, 0, 0, ist), h, o, from, to, true},
		{"the IST hour before it", time.Date(2026, 10, 27, 9, 0, 0, 0, ist), h, o, from, to, true},
		{"two IST hours before it", time.Date(2026, 10, 27, 8, 0, 0, 0, ist), h, o, from, to, false},
		{"the next IST hour (rounded up)", time.Date(2026, 10, 27, 11, 0, 0, 0, ist), h, o, from, to, true},
		{"two IST hours after it", time.Date(2026, 10, 27, 12, 0, 0, 0, ist), h, o, from, to, false},
		{"the day the clocks go back, read late in it", time.Date(2026, 10, 25, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, late, late.Add(time.Minute), true},
		{"the day before it", time.Date(2026, 10, 24, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, late, late.Add(time.Minute), true},
		{"two days before it", time.Date(2026, 10, 23, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, late, late.Add(time.Minute), false},
		{"the next day", time.Date(2026, 10, 26, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, late, late.Add(time.Minute), true},
		{"two days after it", time.Date(2026, 10, 27, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, late, late.Add(time.Minute), false},
		{"yesterday's date the morning after the clocks went back (25 hours before the day's start)", time.Date(2026, 10, 25, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, morning, morning.Add(time.Minute), true},
		{"the date two days ahead, late on the day before the clocks go forward (a day of 23 hours)", time.Date(2026, 3, 30, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, spring.Add(-time.Minute), spring, false},
		{"the next day's date, late on the day before the clocks go forward", time.Date(2026, 3, 29, 0, 0, 0, 0, berlin), d, ClockOffset{Res: d}, spring.Add(-time.Minute), spring, true},
	} {
		if got := row.o.Holds(row.at, row.res, row.from, row.to); got != row.want {
			t.Errorf("%s: Holds = %v, want %v", row.what, got, row.want)
		}
	}
	// Adelaide's clocks go back at 2026-04-04T16:30Z (03:00 ACDT to 02:00
	// ACST): its wall clock shows 02:00-03:00 twice. Read in the second
	// showing, the hour cut is 02:00 ACST (Java's truncatedTo, Go's
	// time.Date) or 02:00 ACDT, an hour before (JavaScript's setMinutes,
	// Python's fold 0); 01:00 ACDT is two hours before the read's hour.
	if adelaide, err := time.LoadLocation("Australia/Adelaide"); err == nil {
		read := time.Date(2026, 4, 4, 16, 45, 0, 0, time.UTC)
		for _, row := range []struct {
			what string
			at   time.Time
			want bool
		}{
			{"Adelaide: the read's hour, at its second showing", time.Date(2026, 4, 4, 16, 30, 0, 0, time.UTC), true},
			{"Adelaide: the read's hour, at its first showing", time.Date(2026, 4, 4, 15, 30, 0, 0, time.UTC), true},
			{"Adelaide: the hour before the first showing (two hours before the read's hour)", time.Date(2026, 4, 4, 14, 30, 0, 0, time.UTC), false},
		} {
			if got := o.Holds(row.at.In(adelaide), h, read, read.Add(time.Minute)); got != row.want {
				t.Errorf("%s: Holds = %v, want %v", row.what, got, row.want)
			}
		}
		if got, want := ReadCut(read, h, adelaide), time.Date(2026, 4, 4, 16, 30, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("ReadCut of %v to an hour in Adelaide = %v, want %v (the hour's second showing)", read, got, want)
		}
	}
	// Chatham's clocks skip 02:45 to 03:45 at 2026-09-26T14:00Z. Read at
	// 03:45 +13:45, the hour's start at the read's offset is 03:00 +13:45
	// (13:15Z): 01:00 +12:45 is one hour before it, 00:00 +12:45 two.
	if chatham, err := time.LoadLocation("Pacific/Chatham"); err == nil {
		read := time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
		for _, row := range []struct {
			what string
			at   time.Time
			want bool
		}{
			{"Chatham: one hour stale, read as the clocks skip", time.Date(2026, 9, 26, 12, 15, 0, 0, time.UTC), true},
			{"Chatham: two hours stale, read as the clocks skip", time.Date(2026, 9, 26, 11, 15, 0, 0, time.UTC), false},
		} {
			if got := o.Holds(row.at.In(chatham), h, read, read.Add(time.Minute)); got != row.want {
				t.Errorf("%s: Holds = %v, want %v", row.what, got, row.want)
			}
		}
	}
	// Amman's clocks went back at 01:00 on 2010-10-29, so its wall clock
	// showed that day's midnight twice (21:00Z and 22:00Z the day before;
	// Go's time.Date gives the second). Read after the second, the day still
	// starts at the first, and yesterday's date is one cut stale.
	if amman, err := time.LoadLocation("Asia/Amman"); err == nil {
		read := time.Date(2010, 10, 28, 22, 30, 0, 0, time.UTC)
		yesterday := time.Date(2010, 10, 27, 21, 0, 0, 0, time.UTC).In(amman)
		if !(ClockOffset{Res: d}).Holds(yesterday, d, read, read.Add(time.Minute)) {
			t.Errorf("Amman: yesterday's date (%v) read after the day's second midnight: Holds = false, want true", yesterday)
		}
	}
	// Santiago's clocks skip 00:00-01:00 on 2026-09-06: read at 00:10 the
	// day before, tomorrow's date (that day's first instant, 01:00 -03) is
	// one cut ahead; the same time of day tomorrow does not exist there.
	if santiago, err := time.LoadLocation("America/Santiago"); err == nil {
		read := time.Date(2026, 9, 5, 0, 10, 0, 0, santiago)
		tomorrow := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC).In(santiago)
		if !(ClockOffset{Res: d}).Holds(tomorrow, d, read, read.Add(time.Minute)) {
			t.Errorf("Santiago: tomorrow's date (%v) read at 00:10 the day before its clocks skip midnight: Holds = false, want true", tomorrow)
		}
	}
	if got, want := ReadCut(late, d, berlin), time.Date(2026, 10, 25, 0, 0, 0, 0, berlin); !got.Equal(want) || late.Sub(got) != 24*time.Hour+40*time.Minute {
		t.Errorf("ReadCut of %v to a day in Berlin = %v (%v before), want %v, 24h40m (23h40m of wall clock and the hour the clocks went back)", late, got, late.Sub(got), want)
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

// A read's Layout meets another as the one both saw, a Layout of "" (learned
// before Layout was) as the other, and two layouts as LayoutApart, which is
// Empty, holds nothing and says so, in either order and grouped either way.
// A Layout this keploy does not name holds nothing. The Layout survives a
// YAML, JSON and BSON-tagged round trip, and a read of none writes no field.
func TestAClockReadsLayoutMeetsAsOneOrHoldsNothing(t *testing.T) {
	ms := time.Millisecond
	sec, epoch, old := ClockOffset{Res: ms, Layout: "iso-second"}, ClockOffset{Res: ms, Layout: "epoch-ms"}, ClockOffset{Res: ms}
	at, from, to := time.Unix(100, 0), time.Unix(100, 0), time.Unix(101, 0)
	if got := sec.Meet(ClockOffset{Res: time.Second, Layout: "iso-second"}); got != (ClockOffset{Res: time.Second, Layout: "iso-second"}) || got.Empty() {
		t.Errorf("two reads in one layout met as %+v; want that layout at the coarser Res", got)
	}
	for _, got := range []ClockOffset{sec.Meet(old), old.Meet(sec)} {
		if got.Layout != "iso-second" || got.Empty() || !got.Holds(at, ms, from, to) {
			t.Errorf("a read of no Layout met one in iso-second as %+v; want iso-second, holding", got)
		}
	}
	for _, pair := range [][2]ClockOffset{{sec, epoch}, {epoch, sec}, {sec, ClockOffset{Res: ms, Layout: LayoutTyped}}} {
		got := pair[0].Meet(pair[1])
		if got.Layout != LayoutApart || !got.Empty() || got.Holds(at, ms, from, to) {
			t.Errorf("%+v meet %+v = %+v (Empty %v); want LayoutApart, Empty, holding nothing", pair[0], pair[1], got, got.Empty())
		}
		if s := got.String(); !strings.Contains(s, "no one form") {
			t.Errorf("String of %+v = %q; want it to say no one form holds it", got, s)
		}
	}
	if a, b := sec.Meet(epoch).Meet(old), sec.Meet(epoch.Meet(old)); a != b {
		t.Errorf("Meet grouped two ways: %+v and %+v", a, b)
	}
	unknown := ClockOffset{Res: ms, Layout: "iso-week"}
	if unknown.LayoutKnown() || unknown.Holds(at, ms, from, to) || !strings.Contains(unknown.String(), "a layout this keploy does not read") {
		t.Errorf("an unknown layout: known %v, holds %v, %q; want unknown, holding nothing, saying so", unknown.LayoutKnown(), unknown.Holds(at, ms, from, to), unknown.String())
	}
	for _, o := range []ClockOffset{sec, old, {Res: ms, Layout: LayoutTyped}} {
		if !o.LayoutKnown() {
			t.Errorf("%+v: layout unknown; want known", o)
		}
	}
	if s := sec.String(); s != "clock, written as an ISO 8601 date and time to the second or a fraction of it" {
		t.Errorf("String of %+v = %q", sec, s)
	}
	if s := (ClockOffset{Res: ms, Layout: LayoutTyped}).String(); s != "clock" {
		t.Errorf("String of a typed read = %q; want clock", s)
	}
	for _, o := range []ClockOffset{sec, old} {
		y, err := yaml.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var back ClockOffset
		if err := yaml.Unmarshal(y, &back); err != nil || back != o {
			t.Errorf("YAML round trip of %+v = %+v (%v)", o, back, err)
		}
		if strings.Contains(string(y), "layout") != (o.Layout != "") {
			t.Errorf("YAML of %+v = %q; want a layout field only where Layout is set", o, y)
		}
		j, _ := json.Marshal(o)
		var jb ClockOffset
		if err := json.Unmarshal(j, &jb); err != nil || jb != o {
			t.Errorf("JSON round trip of %+v = %+v (%v)", o, jb, err)
		}
	}
	if f, ok := reflect.TypeOf(ClockOffset{}).FieldByName("Layout"); !ok || f.Tag.Get("bson") != "layout,omitempty" {
		t.Errorf("Layout's bson tag = %q; want layout,omitempty", f.Tag.Get("bson"))
	}
}

// A clock read whose learning depends on the application's zone names it
// (ClockOffset.AppZone, coordinator's ruling QE, 2026-10-28 00:30): an IANA
// name, a fixed offset or UTC, which ZoneLocation reads back on any machine
// (time/tzdata), legible on disk; an old file without it decodes with none
// and writes none back. Two learnings in different named zones meet as
// ZoneApart, which holds nothing; one of no name meets the other's.
func TestAClockOffsetNamesTheApplicationsZoneItWasLearnedIn(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("time/tzdata is built in, yet: %v", err)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	for _, row := range []struct {
		what string
		loc  *time.Location
		want string
	}{
		{"UTC", time.UTC, "UTC"},
		// A zone with no change of offset ahead is named by its offset, so
		// its aliases are one name.
		{"an IANA zone of one offset ahead", ist, "+05:30"},
		{"its alias", zoneOr(t, "Asia/Calcutta"), "+05:30"},
		{"UTC's alias", zoneOr(t, "Etc/UTC"), "UTC"},
		{"a zone with summer time", berlin, "Europe/Berlin"},
		{"a fixed zone of no name", time.FixedZone("", 5*3600+1800), "+05:30"},
		{"a fixed zone west of UTC", time.FixedZone("", -(3*3600 + 1800)), "-03:30"},
		{"a fixed zone named as no IANA zone", time.FixedZone("IST", 5*3600+1800), "+05:30"},
		// The IANA CET has summer time; a fixed UTC+01:00 named CET is not it.
		{"a fixed zone named as an IANA zone with summer time", time.FixedZone("CET", 3600), "+01:00"},
		{"a fixed zone of no offset", time.FixedZone("", 0), "UTC"},
		{"no zone", nil, ""},
	} {
		got := ZoneNameOf(row.loc, at)
		if got != row.want {
			t.Errorf("%s: ZoneNameOf %q; want %q", row.what, got, row.want)
			continue
		}
		if got == "" {
			continue
		}
		back, err := ZoneLocation(got)
		if err != nil {
			t.Errorf("%s: ZoneLocation(%q): %v", row.what, got, err)
			continue
		}
		for _, when := range []time.Time{at, at.AddDate(0, 6, 0)} {
			if _, a := when.In(back).Zone(); true {
				if _, b := when.In(row.loc).Zone(); a != b && row.what != "a fixed zone named as an IANA zone with summer time" {
					t.Errorf("%s at %s: %q reads back at offset %d, the zone writes %d", row.what, when, got, a, b)
				}
			}
		}
	}
	// A zone file of no name whose offset changes ahead cannot be named.
	if data, err := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin"); err == nil {
		if loc, err := time.LoadLocationFromTZData("", data); err == nil {
			if got := ZoneNameOf(loc, at); got != "" {
				t.Errorf("an unnamed Berlin zone file: ZoneNameOf %q; want none", got)
			}
		}
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "+5:30", "+05:3x", "+15:00", "+-5:30", "05:30"} {
		if loc, err := ZoneLocation(bad); err == nil {
			t.Errorf("ZoneLocation(%q) = %v; want an error", bad, loc)
		}
	}

	o := ClockOffset{Res: time.Hour, Zone: ZoneApp, AppZone: "Asia/Kolkata"}
	if !o.AppZoneMatters() || !(ClockOffset{Res: 24 * time.Hour}).AppZoneMatters() || !(ClockOffset{Res: time.Millisecond, Zone: ZoneEither}).AppZoneMatters() {
		t.Errorf("AppZoneMatters is false for a wall clock in the application's zone, or a day's cut")
	}
	if (ClockOffset{Res: time.Minute}).AppZoneMatters() || (ClockOffset{Res: time.Hour, Zone: ZoneUTC}).AppZoneMatters() || (ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: 24 * time.Hour}).AppZoneMatters() {
		t.Errorf("AppZoneMatters is true of a minute's cut, a wall clock in UTC, or an offset")
	}
	if s := o.String(); !strings.HasSuffix(s, ", read with the application's zone as Asia/Kolkata") {
		t.Errorf("String %q; want it to name the zone", s)
	}
	y, err := yaml.Marshal(map[string]ClockOffset{"body.at": o})
	if err != nil || !strings.Contains(string(y), "app_zone: Asia/Kolkata") {
		t.Errorf("on disk %q (%v); want app_zone: Asia/Kolkata", y, err)
	}
	var old map[string]ClockOffset
	if err := yaml.Unmarshal([]byte("body.at:\n    min: 0s\n    max: 0s\n    res: 1h0m0s\n    zone: app\n"), &old); err != nil || old["body.at"].AppZone != "" || old["body.at"].Res != time.Hour {
		t.Errorf("an old file: %+v (%v); want its offset with no AppZone", old, err)
	}
	if y, _ := yaml.Marshal(old); strings.Contains(string(y), "app_zone") {
		t.Errorf("an old file written back: %q; want no app_zone", y)
	}
	j, _ := json.Marshal(o)
	if !strings.Contains(string(j), `"app_zone":"Asia/Kolkata"`) {
		t.Errorf("JSON %s; want app_zone", j)
	}

	// A learning of no name was read in a zone keploy could not name (the
	// process's, or the agent's own where it could not read the process's),
	// which may be any: met with a named one it names no zone
	// (AppZoneApart), rather than carry a name it was never read in. A
	// replay checks it only in the application's own zone (learn's
	// CheckClockRead).
	unnamed := o
	unnamed.AppZone = ""
	if m := unnamed.Meet(o); m.AppZone != AppZoneApart || m.Empty() {
		t.Errorf("a learning of no name meets a named one: %+v; want AppZoneApart", m)
	}
	if m := o.Meet(unnamed); m.AppZone != AppZoneApart || m.Empty() {
		t.Errorf("a named learning meets one of no name: %+v; want AppZoneApart", m)
	}
	if s := unnamed.Meet(o).String(); !strings.Contains(s, "unknown to keploy") {
		t.Errorf("String %q; want it to say the zone was unknown at one call", s)
	}
	if m := unnamed.Meet(unnamed); m != unnamed {
		t.Errorf("two learnings of no name meet as %+v; want %+v", m, unnamed)
	}
	// One whose learning read no zone of the application's (a cut to the
	// second) carries no name and takes the other's.
	second, hour := ClockOffset{Res: time.Second}, ClockOffset{Res: time.Hour, AppZone: "Asia/Kolkata"}
	if m := second.Meet(hour); m != hour {
		t.Errorf("a second's cut meets a named hour's: %+v; want the named zone", m)
	}
	if m := hour.Meet(second); m != hour {
		t.Errorf("a named hour's cut meets a second's: %+v; want the named zone", m)
	}
	utc := o
	utc.AppZone = "UTC"
	if m := o.Meet(utc); m.AppZone != AppZoneApart {
		t.Errorf("learnings in IST and UTC meet as %+v; want AppZoneApart", m)
	}
	// Meet is the same however a place's learnings are grouped: a wall
	// clock either zone held, in IST and in UTC, met with one in UTC.
	eIST := ClockOffset{Res: time.Millisecond, Zone: ZoneEither, AppZone: "Asia/Kolkata"}
	eUTC := ClockOffset{Res: time.Millisecond, Zone: ZoneEither, AppZone: "UTC"}
	eNone := ClockOffset{Res: time.Millisecond, Zone: ZoneEither}
	inUTC := ClockOffset{Res: time.Millisecond, Zone: ZoneUTC}
	for _, three := range [][3]ClockOffset{{eIST, eUTC, inUTC}, {eIST, eNone, inUTC}} {
		a, b, c := three[0], three[1], three[2]
		want := a.Meet(b).Meet(c)
		for i, got := range []ClockOffset{a.Meet(b.Meet(c)), a.Meet(c).Meet(b), b.Meet(c).Meet(a), c.Meet(a).Meet(b), b.Meet(a).Meet(c)} {
			if got != want {
				t.Errorf("%v, %v, %v grouped %d: %+v; want %+v", a, b, c, i, got, want)
			}
		}
		if want != inUTC {
			t.Errorf("%v, %v, %v meet as %+v; want %+v, read in UTC alone", a, b, c, want, inUTC)
		}
	}
	if m := MergeClocks(map[string]ClockOffset{"body.at": o}, map[string]ClockOffset{"body.at": utc})["body.at"]; m.AppZone != AppZoneApart {
		t.Errorf("merged learnings in IST and UTC: %+v; want one that names no zone", m)
	}
	if m := o.Meet(o); m != o {
		t.Errorf("a learning meets itself as %+v", m)
	}
	// A meet that reads no zone of the application's names none: a name
	// this keploy cannot read would refuse a value it never reads there.
	either := ClockOffset{Res: time.Millisecond, Zone: ZoneEither, AppZone: "Mars/Olympus"}
	if m := either.Meet(ClockOffset{Res: time.Millisecond, Zone: ZoneUTC}); m.Zone != ZoneUTC || m.AppZone != "" {
		t.Errorf("either zone meets UTC as %+v; want UTC, naming no zone", m)
	}
}

// zoneOr is the IANA zone name, built into keploy (time/tzdata).
func zoneOr(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return loc
}
