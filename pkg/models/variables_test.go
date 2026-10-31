package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
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

// An offset below zero (a cutoff the application queries from: createdAt
// >= now - 2m) holds as one above zero does: where some read of its call's
// window, less the offset the learn step bounded, cuts to it, within one
// resolution of it on either side (the train's check at the cut,
// 2026-10-28): one resolution past the edges of both ranges holds, two do
// not, and a changed offset (two minutes become one or three), the same
// offset above zero and a plain read do not. It reads as "clock - 2m". Two learnings of it meet as what both
// hold, and as Empty with one apart (of either sign) or with a plain read.
// A range around zero is no offset the learn step finds: it names none.
// Stored in YAML and JSON, it reads back as it was.
func TestAClockOffsetBelowZeroHoldsOnlyAReadOfItsWindowLessThatOffset(t *testing.T) {
	ms, min2 := time.Millisecond, 2*time.Minute
	from := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	to := from.Add(50 * ms)
	o := ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms}
	for _, row := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"a read in the window less two minutes", from.Add(20*ms - min2), true},
		{"the earliest read plus the least offset", from.Add(o.Min), true},
		{"a millisecond before it (one resolution stale)", from.Add(o.Min - ms), true},
		{"two milliseconds before it", from.Add(o.Min - 2*ms), false},
		{"the latest read plus the greatest offset, less a millisecond", to.Add(o.Max - ms), true},
		{"the latest read plus the greatest offset, plus a millisecond (one resolution ahead)", to.Add(o.Max + ms), true},
		{"the latest read plus the greatest offset, plus two milliseconds", to.Add(o.Max + 2*ms), false},
		{"a read less one minute", from.Add(20*ms - time.Minute), false},
		{"a read less three minutes", from.Add(20*ms - 3*time.Minute), false},
		{"a read plus two minutes", from.Add(20*ms + min2), false},
		{"a plain read", from.Add(20 * ms), false},
	} {
		if got := o.Holds(row.at, ms, from, to); got != row.want {
			t.Errorf("%s: Holds = %v, want %v", row.name, got, row.want)
		}
	}
	if u := o.Unit(); u != time.Minute {
		t.Errorf("Unit %s, want a minute", u)
	}
	if got, want := o.String(), "clock - 2m (from -2m0.04s to -1m59.989s)"; got != want {
		t.Errorf("String %q, want %q", got, want)
	}
	if got, want := (ClockOffset{Min: -time.Hour, Max: -time.Hour + 1}).String(), "clock - 1h"; got != want {
		t.Errorf("String %q, want %q", got, want)
	}

	b := ClockOffset{Min: -min2 - 5*ms, Max: -min2 + 30*ms, Res: ms}
	if got, want := o.Meet(b), (ClockOffset{Min: b.Min, Max: o.Max, Res: ms}); got != want || got.Empty() || !got.Holds(from.Add(20*ms-min2), ms, from, to) {
		t.Errorf("two learnings of two minutes before meet as %+v; want %+v, holding a read less two minutes", got, want)
	}
	minute := ClockOffset{Min: -time.Minute - 40*ms, Max: -time.Minute + 11*ms, Res: ms}
	after := ClockOffset{Min: min2 - 40*ms, Max: min2 + 11*ms, Res: ms}
	plain := ClockOffset{Res: ms}
	for name, e := range map[string]ClockOffset{"one minute before": o.Meet(minute), "two minutes after": o.Meet(after), "a plain read": plain.Meet(o)} {
		if !e.Empty() || e.Plain() {
			t.Errorf("two minutes before meet %s as %+v: want Empty", name, e)
		}
		if e.Holds(from.Add(20*ms-min2), ms, from, to) || e.Holds(from.Add(20*ms-time.Minute), ms, from, to) {
			t.Errorf("two minutes before met with %s holds a value", name)
		}
		if s := e.String(); !strings.HasPrefix(s, "clock reads no one offset holds") {
			t.Errorf("two minutes before met with %s reads %q", name, s)
		}
	}
	if got, want := (ClockOffset{Min: -ms, Max: ms, Res: ms}).String(), "clock plus an offset from -1ms to 1ms"; got != want {
		t.Errorf("a range around zero reads %q, want %q", got, want)
	}

	y, err := yaml.Marshal(map[string]ClockOffset{"q": o})
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(map[string]ClockOffset{"q": o})
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML, fromJSON map[string]ClockOffset
	if err := yaml.Unmarshal(y, &fromYAML); err != nil || fromYAML["q"] != o {
		t.Errorf("YAML %s read back as %+v (%v), want %+v", y, fromYAML["q"], err, o)
	}
	if err := json.Unmarshal(j, &fromJSON); err != nil || fromJSON["q"] != o {
		t.Errorf("JSON %s read back as %+v (%v), want %+v", j, fromJSON["q"], err, o)
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

// An offset cut to a day (founder decision, 2026-10-27: a cut is checked at
// the cut; ruled for a day's cut 2026-10-29) is learned where its width is
// narrower than the day (Narrow), though no unit holds three days, and a
// replay checks it at the day where its window and the offset's width are
// narrower than the day (Checkable), within one day of the window plus the
// offset on either side: thirty days, at every read in the widest such
// window, holds the read plus thirty days and the value one day stale or
// ahead (as yesterday's date passes a date cut to a day), never three days
// off; a window a nanosecond wider holds nothing. Cut to an hour, an
// offset that reads in days still counts three cuts against the unit (a day
// cut to the hour is learned by Within, its width under 21 hours), and one
// that reads in hours is checked at the hour, as a day's cut is at the day
// (TestAClockOffsetCutToAnHourThatReadsInHoursIsCheckedAtTheHour).
func TestAClockOffsetCutToADayIsCheckedAtTheDay(t *testing.T) {
	ms, day := time.Millisecond, 24*time.Hour
	o := ClockOffset{Min: 30*day - 31*time.Second, Max: 30*day + 30*time.Second, Res: day}
	if !o.CoarseCut() || !o.Narrow() || o.Unit() != day {
		t.Fatalf("thirty days at the day: CoarseCut %v, Narrow %v, Unit %s; want a day's cut, learned, reading in days", o.CoarseCut(), o.Narrow(), o.Unit())
	}
	from := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	widest := day - (o.Max - o.Min) - 1
	to := from.Add(widest)
	if !o.Checkable(ms, from, to) || o.Checkable(ms, from, to.Add(1)) {
		t.Errorf("Checkable: want the widest window with the offset's width within the day, a nanosecond more not")
	}
	for _, read := range []time.Time{from, from.Add(widest / 2), to} {
		v := read.Add(30 * day).Truncate(day)
		for _, row := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"the read plus thirty days, cut to the day", v, true},
			{"one day stale", v.Add(-day), true},
			{"one day ahead", v.Add(day), true},
			{"three days stale", v.Add(-3 * day), false},
			{"three days ahead", v.Add(3 * day), false},
		} {
			if got := o.Holds(row.at, ms, from, to); got != row.want {
				t.Errorf("a read at %s, %s: held %v, want %v", read.Sub(from), row.name, got, row.want)
			}
		}
	}
	if o.Holds(from.Add(30*day).Truncate(day), ms, from, to.Add(1)) {
		t.Errorf("a window a nanosecond wider, with the offset's width the day: held, want nothing held")
	}
	for _, c := range []struct {
		name string
		o    ClockOffset
		want bool
	}{
		{"thirty days at the day, its width a day", ClockOffset{Min: 30 * day, Max: 31 * day, Res: day}, false},
		{"thirty days at the day, its width just under a day", ClockOffset{Min: 30 * day, Max: 31*day - 1, Res: day}, true},
		{"a day at the hour", ClockOffset{Min: day - 31*time.Second, Max: day + 30*time.Second, Res: time.Hour}, true},
		{"a day at the hour, its width 21 hours", ClockOffset{Min: 12 * time.Hour, Max: 33 * time.Hour, Res: time.Hour}, false},
		{"two hours at the hour", ClockOffset{Min: 2*time.Hour - 31*time.Second, Max: 2*time.Hour + 30*time.Second, Res: time.Hour}, true},
	} {
		if got := c.o.Narrow(); got != c.want {
			t.Errorf("Narrow of %s: %v, want %v", c.name, got, c.want)
		}
	}
	// A Res of no step (96 hours, a hand-edited mock's) is no day's cut: a
	// replay checks the value at its own millisecond, where three of Res
	// count against the unit, so nothing holds, not a value two days off.
	hand := ClockOffset{Min: 28 * day, Max: 31*day + 23*time.Hour, Res: 96 * time.Hour}
	if hand.CoarseCut() || hand.Checkable(ms, from, from.Add(time.Minute)) || hand.Holds(from.Add(32*day), ms, from, from.Add(time.Minute)) || hand.Holds(from.Add(30*day), ms, from, from.Add(time.Minute)) {
		t.Errorf("FALSE GREEN: a hand-edited offset at a Res of 96 hours: CoarseCut %v, Checkable %v; want no day's cut, and nothing held", hand.CoarseCut(), hand.Checkable(ms, from, from.Add(time.Minute)))
	}
	// A value written coarser than a day (two days: no encoding keploy
	// reads writes one) is no day's check: it is read at its own
	// resolution, where three of it count against the day the offset reads
	// in, so nothing holds, not the read plus thirty days.
	if o.CoarseChecked(2*day) || o.Checkable(2*day, from, from.Add(time.Minute)) || o.Holds(from.Add(30*day).Truncate(day), 2*day, from, from.Add(time.Minute)) || !o.CoarseChecked(day) || !o.CoarseChecked(ms) {
		t.Errorf("thirty days at the day, a value written at two days: CoarseChecked %v, Checkable %v; want checked at the day only for a value written at a day or finer, and nothing held at two days", o.CoarseChecked(2*day), o.Checkable(2*day, from, from.Add(time.Minute)))
	}
	if (ClockOffset{Res: day}).CoarseCut() || (ClockOffset{Res: time.Hour}).CoarseCut() {
		t.Errorf("CoarseCut of a plain read cut to a day or an hour: true, want false (a plain read's cut is no offset's)")
	}
}

// An offset cut to an hour that reads in hours (clock + 2h, the hour cut
// down: Go's Truncate) is checked at the hour as a day's cut is at the day
// (founder decision, 2026-10-27; ruled for an hour's cut 2026-10-29): the
// learn step learns it where its width is narrower than the hour (Narrow),
// though no unit holds three hours, and a replay checks it where its window
// and the offset's width are narrower than the hour (Checkable), within one
// hour of the window plus the offset on either side. At every read in the
// widest such window it holds the read plus two hours and the value one
// hour stale or ahead (the stated cost: a time to live changed by one hour
// is not caught), never three hours off; two hours off holds only where the
// read falls within the window and the width of the hour's turn, and is
// refused at a read away from it. A window a nanosecond wider holds nothing.
// An hour's cut that reads in minutes (a cut hour plus 90 minutes, or any
// time to live of no whole number of hours) is checked so too; one that
// reads in days is not (Within decides it, three hours against the day),
// nor is a Res of two hours, which is no step.
func TestAClockOffsetCutToAnHourThatReadsInHoursIsCheckedAtTheHour(t *testing.T) {
	ms, hour := time.Millisecond, time.Hour
	o := ClockOffset{Min: 2*hour - 62*time.Second, Max: 2*hour + 30*time.Second, Res: hour}
	if !o.CoarseCut() || !o.Narrow() || o.Unit() != hour || !o.CoarseChecked(ms) || !o.CoarseChecked(hour) || o.CoarseChecked(2*hour) {
		t.Fatalf("two hours at the hour: CoarseCut %v, Narrow %v, Unit %s; want an hour's cut, learned, reading in hours, checked at the hour for a value written at an hour or finer", o.CoarseCut(), o.Narrow(), o.Unit())
	}
	from := time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)
	widest := hour - (o.Max - o.Min) - 1
	to := from.Add(widest)
	if !o.Checkable(ms, from, to) || o.Checkable(ms, from, to.Add(1)) {
		t.Errorf("Checkable: want the widest window with the offset's width within the hour, a nanosecond more not")
	}
	for _, read := range []time.Time{from, from.Add(widest / 2), to} {
		v := read.Add(2 * hour).Truncate(hour)
		lo, hi := from.Add(o.Min).Truncate(hour).Add(-hour), to.Add(o.Max).Add(hour)
		for _, row := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"the read plus two hours, cut to the hour", v, true},
			{"one hour stale", v.Add(-hour), true},
			{"one hour ahead", v.Add(hour), true},
			{"two hours stale", v.Add(-2 * hour), !v.Add(-2 * hour).Before(lo)},
			{"two hours ahead", v.Add(2 * hour), !v.Add(2 * hour).After(hi)},
			{"three hours stale", v.Add(-3 * hour), false},
			{"three hours ahead", v.Add(3 * hour), false},
		} {
			if got := o.Holds(row.at, ms, from, to); got != row.want {
				t.Errorf("a read at %s, %s: held %v, want %v", read.Sub(from), row.name, got, row.want)
			}
		}
	}
	// A short call away from the hour's turn: two hours off either way is
	// refused.
	at := time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)
	v := at.Add(2 * hour).Truncate(hour)
	if !o.Holds(v.Add(-hour), ms, at, at.Add(time.Second)) || o.Holds(v.Add(-2*hour), ms, at, at.Add(time.Second)) || o.Holds(v.Add(2*hour), ms, at, at.Add(time.Second)) {
		t.Errorf("a call at 06:30: want one hour stale held, two hours off either way not")
	}
	if o.Holds(from.Add(2*hour).Truncate(hour), ms, from, to.Add(1)) {
		t.Errorf("a window a nanosecond wider, with the offset's width the hour: held, want nothing held")
	}
	for _, c := range []struct {
		name   string
		o      ClockOffset
		coarse bool
		narrow bool
	}{
		{"two hours at the hour, its width an hour", ClockOffset{Min: 2 * hour, Max: 3 * hour, Res: hour}, true, false},
		{"two hours at the hour, its width just under an hour", ClockOffset{Min: 2 * hour, Max: 3*hour - 1, Res: hour}, true, true},
		{"a cut hour plus 90 minutes, reading in minutes", ClockOffset{Min: 90*time.Minute - 31*time.Second, Max: 90*time.Minute + 31*time.Second, Res: hour}, true, true},
		{"a look-back of two hours at the hour", ClockOffset{Min: -2*hour - 30*time.Second, Max: -2*hour + 31*time.Second, Res: hour}, true, true},
		{"a day at the hour", ClockOffset{Min: 24*hour - 31*time.Second, Max: 24*hour + 30*time.Second, Res: hour}, false, true},
		{"two hours at two hours (no step)", ClockOffset{Min: 2*hour - 31*time.Second, Max: 2*hour + 30*time.Second, Res: 2 * hour}, false, false},
		{"thirty minutes at the minute", ClockOffset{Min: 30*time.Minute - 31*time.Second, Max: 30*time.Minute + 30*time.Second, Res: time.Minute}, false, false},
	} {
		if got, n := c.o.CoarseCut(), c.o.Narrow(); got != c.coarse || n != c.narrow {
			t.Errorf("%s: CoarseCut %v, Narrow %v; want %v, %v", c.name, got, n, c.coarse, c.narrow)
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
	if (ClockOffset{Res: time.Minute}).AppZoneMatters() || (ClockOffset{Res: time.Hour, Zone: ZoneUTC}).AppZoneMatters() {
		t.Errorf("AppZoneMatters is true of a plain read cut to the minute, or a wall clock in UTC")
	}
	// A read plus an offset at any cut names the zone (its check fails
	// closed near that zone's backward turns: FallBack), though a replay
	// reads a minute's cut, or a wall clock in UTC, in no zone of the
	// application's.
	for _, off := range []ClockOffset{{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Minute}, {Min: time.Hour, Max: time.Hour + time.Second, Res: time.Millisecond}, {Min: time.Hour, Max: time.Hour + time.Second, Res: time.Hour, Zone: ZoneUTC}} {
		if !off.AppZoneMatters() || off.CutInAppZone() {
			t.Errorf("%+v: AppZoneMatters %v, CutInAppZone %v; want the zone named, the value read in none of the application's", off, off.AppZoneMatters(), off.CutInAppZone())
		}
	}
	// A read plus an offset cut to an hour or a day: its cut is read in the
	// application's zone (an hour in UTC+05:30 is no hour in UTC).
	if !(ClockOffset{Min: 24 * time.Hour, Max: 25 * time.Hour, Res: time.Hour}).AppZoneMatters() || !(ClockOffset{Min: -49 * time.Hour, Max: -47 * time.Hour, Res: 24 * time.Hour}).AppZoneMatters() {
		t.Errorf("AppZoneMatters is false of an offset cut to an hour or a day")
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
	// A read plus an offset cut to an hour reads the application's zone too,
	// so its learnings meet as a plain read's: one cut to the minute (no
	// zone of the application's) met with one cut to the IST hour, in either
	// order, is the hour's cut and zone over the bounds both hold, and an
	// hour's in IST met with one of no name or one in UTC names no one zone.
	// Learned before the hour's offsets read the zone, each of these met as
	// naming none.
	day := 24 * time.Hour
	offMin := ClockOffset{Min: day - 2*time.Minute, Max: day + time.Minute, Res: time.Minute, Layout: LayoutTyped}
	offIST := ClockOffset{Min: day - time.Hour, Max: day + time.Hour, Res: time.Hour, Layout: LayoutTyped, AppZone: "+05:30"}
	want := ClockOffset{Min: day - 2*time.Minute, Max: day + time.Minute, Res: time.Hour, Layout: LayoutTyped, AppZone: "+05:30"}
	if m := offMin.Meet(offIST); m != want {
		t.Errorf("an offset cut to the minute meets one cut to the IST hour: %+v; want %+v", m, want)
	}
	if m := offIST.Meet(offMin); m != want {
		t.Errorf("an offset cut to the IST hour meets one cut to the minute: %+v; want %+v", m, want)
	}
	for _, other := range []string{"", "UTC"} {
		p := offIST
		p.AppZone = other
		if m := offIST.Meet(p); m.AppZone != AppZoneApart || m.Empty() {
			t.Errorf("an offset cut to the IST hour meets one cut to the hour read with the zone as %q: %+v; want AppZoneApart", other, m)
		}
		if m := p.Meet(offIST); m.AppZone != AppZoneApart {
			t.Errorf("an offset cut to the hour read with the zone as %q meets one cut to the IST hour: %+v; want AppZoneApart", other, m)
		}
	}
	if m := MergeClocks(map[string]ClockOffset{"body.exp": offMin}, map[string]ClockOffset{"body.exp": offIST})["body.exp"]; m != want {
		t.Errorf("MergeClocks of an offset cut to the minute and one cut to the IST hour: %+v; want %+v", m, want)
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

// firstShowingCut is an hour cut of x the way JavaScript's setMinutes(0, 0,
// 0) and java.time's LocalDateTime truncatedTo then atZone make it in loc:
// the start of x's wall hour there at its first showing, in the hour the
// clocks go back an hour (or two) before the instant keploy reads the cut
// at (ReadCut).
func firstShowingCut(x time.Time, loc *time.Location) time.Time {
	l := x.In(loc)
	c := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	for e := c.Add(-time.Hour); ; e = e.Add(-time.Hour) {
		if el := e.In(loc); el.Hour() != c.In(loc).Hour() || el.Day() != c.In(loc).Day() {
			break
		}
		c = e
	}
	return c
}

// An offset cut to an hour (clock + 2h) in Europe/Berlin around the hour its
// clocks go back (2026-10-25 01:00Z, 03:00 CEST to 02:00 CET), the value
// read in Berlin, of an application that cuts to the hour's first showing,
// before adding the two hours or after. Without FallBack a time to live two
// hours longer holds at every read in the second showing (cut, then add)
// and in the hour four hours before the turn (add, then cut). With it,
// Holds holds nothing where the call's window or the values it admits meet
// the hour shown twice, which FallBack names (the turn, an hour back), a
// time to live two hours off holds only near the hour's turn, three never,
// and the right value holds at every read more than four hours before the
// turn or two after it: the rest of the day is checked as before.
// Troll's turn goes back two hours, and UTC and an offset cut to a day meet
// none.
func TestAnHourCutOffsetHoldsNothingNearTheHourTheClocksGoBack(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	ms, hour := time.Millisecond, time.Hour
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	o := ClockOffset{Min: 2*hour - 62*time.Second, Max: 2*hour + 30*time.Second, Res: hour}
	for _, shape := range []string{"cut, then add", "add, then cut"} {
		val := func(read time.Time, ttl time.Duration) time.Time {
			if shape == "cut, then add" {
				return firstShowingCut(read, ber).Add(ttl).In(ber)
			}
			return firstShowingCut(read.Add(ttl), ber).In(ber)
		}
		far := map[int]int{}
		for from := turn.Add(-8 * hour); from.Before(turn.Add(5 * hour)); from = from.Add(time.Minute) {
			to := from.Add(time.Second)
			bucket := int(from.Sub(turn) / hour)
			if from.Before(turn) {
				bucket--
			}
			got, back, met := o.FallBack(ms, from, to, ber)
			if met && (!got.Equal(turn) || back != hour) {
				t.Fatalf("%s, read at %s: FallBack names %s back %s; want the turn %s, an hour back", shape, from, got, back, turn)
			}
			near := !from.Before(turn.Add(-4*hour)) && from.Before(turn.Add(2*hour))
			if met && !near {
				t.Errorf("FALSE RED: %s, read at %s, %s from the turn: FallBack met; want checked as on any other hour", shape, from, from.Sub(turn))
			}
			if from.Before(turn.Add(-3*hour)) == false && from.Before(turn.Add(hour)) && !met {
				t.Errorf("FALSE GREEN: %s, read at %s, %s from the turn: FallBack not met", shape, from, from.Sub(turn))
			}
			if right := o.Holds(val(from, 2*hour), ms, from, to); right == met {
				t.Errorf("%s, read at %s (FallBack met %v): the right value holds %v", shape, from, met, right)
			}
			for _, k := range []time.Duration{2 * hour, -2 * hour} {
				if o.Holds(val(from, 2*hour+k), ms, from, to) {
					far[bucket]++
				}
			}
			for _, k := range []time.Duration{3 * hour, -3 * hour} {
				if o.Holds(val(from, 2*hour+k), ms, from, to) {
					t.Errorf("FALSE GREEN: %s, read at %s: a time to live %s off holds", shape, from, k)
				}
			}
		}
		for b, n := range far {
			if n > 4 {
				t.Errorf("FALSE GREEN: %s, reads in the hour %d from the turn: a time to live two hours longer or shorter holds %d times in 60 reads; want only near the hour's turn", shape, b, n)
			}
		}
	}
	// Troll's clocks go back two hours (03:00 to 01:00 local): every read
	// from three hours before the turn to two hours after it (the values'
	// span reaches back five) meets it, two hours back, and none later.
	troll := zoneOr(t, "Antarctica/Troll")
	for from := turn.Add(-3 * hour); from.Before(turn.Add(3 * hour)); from = from.Add(time.Minute) {
		got, back, met := o.FallBack(ms, from, from.Add(time.Second), troll)
		if want := from.Before(turn.Add(2 * hour)); met != want || met && (!got.Equal(turn) || back != 2*hour) {
			t.Errorf("Antarctica/Troll, read at %s, %s from the turn: FallBack %s back %s met %v; want met %v, the turn %s, two hours back", from, from.Sub(turn), got, back, met, want, turn)
		}
	}
	// Lord Howe's go back thirty minutes (2026-04-05, 02:00 +11 to 01:30
	// +10:30): clock + 2h meets the turn from three and a half hours before
	// it to thirty minutes after, and clock - 2h (the values' span one cut
	// before the hour that holds the read less two hours, whose hours start
	// thirty minutes after the turn) from thirty minutes before it to three
	// and a half hours after.
	lhi := zoneOr(t, "Australia/Lord_Howe")
	lturn := time.Date(2026, 4, 4, 15, 0, 0, 0, time.UTC)
	neg := ClockOffset{Min: -2*hour - 30*time.Second, Max: -2*hour + 62*time.Second, Res: hour}
	for _, row := range []struct {
		name   string
		o      ClockOffset
		lo, hi time.Duration
	}{
		{"clock + 2h", o, -3*hour - 30*time.Minute, 30 * time.Minute},
		{"clock - 2h", neg, -30 * time.Minute, 3*hour + 31*time.Minute},
	} {
		for from := lturn.Add(-5 * hour); from.Before(lturn.Add(5 * hour)); from = from.Add(time.Minute) {
			got, back, met := row.o.FallBack(ms, from, from.Add(time.Second), lhi)
			if want := !from.Before(lturn.Add(row.lo)) && from.Before(lturn.Add(row.hi)); met != want || met && (!got.Equal(lturn) || back != 30*time.Minute) {
				t.Errorf("Australia/Lord_Howe, %s read at %s, %s from the turn: FallBack %s back %s met %v; want met %v, the turn %s, thirty minutes back", row.name, from, from.Sub(lturn), got, back, met, want, lturn)
			}
		}
	}
	at := turn.Add(30 * time.Minute)
	if _, _, met := o.FallBack(ms, at, at.Add(time.Second), time.UTC); met {
		t.Errorf("UTC: FallBack met; UTC's clocks never go back")
	}
	if _, _, met := (ClockOffset{Min: 30*24*hour - 62*time.Second, Max: 30*24*hour + 30*time.Second, Res: 24 * hour}).FallBack(ms, at, at.Add(time.Second), ber); met {
		t.Errorf("an offset cut to a day, read in Berlin's second showing: FallBack met; a day's start shows once")
	}
}

// firstShowingAt is x's wall clock in loc cut to c, resolved to the first
// instant loc shows that wall time at, as JavaScript's Date setters
// (setMilliseconds(0), setSeconds(0, 0), setMinutes(0, 0, 0)) and
// java.time's LocalDateTime then atZone (c of a millisecond: no cut)
// resolve a wall time shown twice: in the second showing of the hour
// Berlin's clocks go back, an hour before x (two in Troll).
func firstShowingAt(x time.Time, c time.Duration, loc *time.Location) time.Time {
	_, off := x.In(loc).Zone()
	wall := x.Add(time.Duration(off) * time.Second).UTC().Truncate(c)
	first := wall.Add(-time.Duration(off) * time.Second)
	for _, d := range []time.Duration{30 * time.Minute, time.Hour, 90 * time.Minute, 2 * time.Hour} {
		e := first.Add(-d)
		if _, eo := e.In(loc).Zone(); e.Add(time.Duration(eo) * time.Second).UTC().Equal(wall) {
			first = e
		}
	}
	return first.In(loc)
}

// A read plus an offset checked at a cut finer than an hour (a second, a
// minute, none) in Europe/Berlin around the hour its clocks go back, of an
// application that resolves the wall time shown twice to its first showing
// before adding the time to live or after. Without FallBack a time to live
// an hour longer holds at every read whose cut (or converted) instant lies
// in the second showing. With it, Holds holds nothing where the call's
// window or the values it admits meet the hour shown twice, the right value
// holds at every read more than four hours before the turn or two after,
// and FallBackAnywhere, for a replay that knows no zone, meets the
// European Union's turn there and none in July. A plain read and a day's
// cut meet none.
func TestAReadPlusAnOffsetHoldsNothingNearAWallTimeShownTwiceAtAnyCut(t *testing.T) {
	ber := zoneOr(t, "Europe/Berlin")
	ms, hour := time.Millisecond, time.Hour
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name  string
		cut   time.Duration
		o     ClockOffset
		shape string
	}{
		{"cut to the second", time.Second, ClockOffset{Min: 2*hour - 300*ms, Max: 2*hour + 255*ms, Res: time.Second}, "cut, then add"},
		{"cut to the second", time.Second, ClockOffset{Min: 2*hour - 300*ms, Max: 2*hour + 255*ms, Res: time.Second}, "add, then cut"},
		{"cut to the minute", time.Minute, ClockOffset{Min: 2*hour - 7300*ms, Max: 2*hour + 51255*ms, Res: time.Minute}, "cut, then add"},
		{"cut to the minute", time.Minute, ClockOffset{Min: 2*hour - 7300*ms, Max: 2*hour + 51255*ms, Res: time.Minute}, "add, then cut"},
		{"not cut (LocalDateTime then atZone)", ms, ClockOffset{Min: 2*hour - 30*ms, Max: 2*hour + 71*ms, Res: ms}, "cut, then add"},
	} {
		val := func(read time.Time, ttl time.Duration) time.Time {
			if row.shape == "cut, then add" {
				return firstShowingAt(read, row.cut, ber).Add(ttl)
			}
			return firstShowingAt(read.Add(ttl), row.cut, ber)
		}
		second := 0
		for from := turn.Add(-5 * hour); from.Before(turn.Add(5 * hour)); from = from.Add(37 * time.Second) {
			to := from.Add(time.Second)
			read := from.Add(500 * ms)
			x := read
			if row.shape == "add, then cut" {
				x = read.Add(2*hour + hour)
			}
			_, _, met := row.o.FallBack(ms, from, to, ber)
			if !x.Before(turn) && x.Before(turn.Add(hour)) {
				second++
				if row.o.Holds(val(read, 3*hour), ms, from, to) {
					t.Errorf("FALSE GREEN: %s, %s, read at %s (FallBack met %v): a time to live an hour longer holds in the second showing", row.name, row.shape, read, met)
				}
			}
			if right := row.o.Holds(val(read, 2*hour), ms, from, to); right == met {
				t.Errorf("%s, %s, read at %s, %s from the turn (FallBack met %v): the right value holds %v", row.name, row.shape, read, from.Sub(turn), met, right)
			}
			if far := from.Before(turn.Add(-4*hour)) || !from.Before(turn.Add(2*hour)); far && met {
				t.Errorf("FALSE RED: %s, %s, read at %s, %s from the turn: FallBack met; want checked as on any other hour", row.name, row.shape, read, from.Sub(turn))
			}
		}
		if second < 90 {
			t.Fatalf("%s, %s: %d reads in the second showing; want 90 or more", row.name, row.shape, second)
		}
	}
	o := ClockOffset{Min: 2*hour - 300*ms, Max: 2*hour + 255*ms, Res: time.Second}
	at := turn.Add(30 * time.Minute)
	if got, back, loc, met := o.FallBackAnywhere(ms, at, at.Add(time.Second)); !met || !got.Equal(turn) || back != hour || loc == nil || loc.String() != "Europe/Amsterdam" {
		t.Errorf("FallBackAnywhere at %s: %s back %s in %v met %v; want the European Union's turn %s, an hour back, named as Europe/Amsterdam", at, got, back, loc, met, turn)
	}
	july := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if got, _, loc, met := o.FallBackAnywhere(ms, july, july.Add(time.Second)); met {
		t.Errorf("FALSE RED: FallBackAnywhere in July: met %s in %v; no zone's clocks go back then", got, loc)
	}
	for _, none := range []ClockOffset{{Res: time.Second}, {Min: 30*24*hour - 62*time.Second, Max: 30*24*hour + 30*time.Second, Res: 24 * hour}} {
		if _, _, met := none.FallBack(ms, at, at.Add(time.Second), ber); met {
			t.Errorf("%+v, read in Berlin's second showing: FallBack met; a plain read has no time to live, and a day's cut keeps the date both showings share", none)
		}
		if _, _, _, met := none.FallBackAnywhere(ms, at, at.Add(time.Second)); met {
			t.Errorf("%+v, read in Berlin's second showing: FallBackAnywhere met", none)
		}
	}
}

// AcrossHour holds where a zone's wall time shown twice as its clocks go
// back holds the start of one of its wall hours strictly inside it
// (Pacific/Chatham's 02:45 to 03:45 holds 03:00; Antarctica/Troll's 01:00
// to 03:00 holds 02:00), and nowhere it is whole hours or within one
// (Europe/Berlin's 02:00 to 03:00, Newfoundland's 01:00 to 02:00, Lord
// Howe's 01:30 to 02:00). The learn step reads the turns it holds at an
// hour's cut: FallBackAcrossHour in the zone known, and with none every
// zone's (FallBackAnywhereAcrossHour), which keeps Chatham's turn though
// Auckland turns at the same instant by the same hour and holds none. A
// window at Chatham's turn meets it; one at Berlin's meets none in Berlin,
// and among every zone's meets Troll's, at the same instant; one at New
// York's meets none.
func TestAWallTimeShownTwiceHoldingAnHoursStartIsReadAtAnHoursCut(t *testing.T) {
	hour := time.Hour
	for _, c := range []struct {
		zone string
		turn time.Time
		want bool
	}{
		{"Pacific/Chatham", time.Date(2026, 4, 4, 14, 0, 0, 0, time.UTC), true},
		{"Antarctica/Troll", time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), true},
		{"Pacific/Auckland", time.Date(2026, 4, 4, 14, 0, 0, 0, time.UTC), false},
		{"Europe/Berlin", time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), false},
		{"America/St_Johns", time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC), false},
		{"Australia/Lord_Howe", time.Date(2026, 4, 4, 15, 0, 0, 0, time.UTC), false},
	} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		if _, end := c.turn.Add(-time.Second).In(loc).ZoneBounds(); !end.Equal(c.turn) {
			t.Fatalf("%s: the zone's next change after %s is at %s, not then", c.zone, c.turn.Add(-time.Second), end)
		}
		if got := AcrossHour(c.turn, loc); got != c.want {
			t.Errorf("AcrossHour(%s, %s) = %v; want %v", c.turn, c.zone, got, c.want)
		}
	}
	o := ClockOffset{Min: 2*hour - time.Minute, Max: 2*hour + time.Minute, Res: hour}
	chat, _ := time.LoadLocation("Pacific/Chatham")
	ber, _ := time.LoadLocation("Europe/Berlin")
	at := time.Date(2026, 4, 4, 14, 14, 0, 0, time.UTC)
	if turn, back, met := o.FallBackAcrossHour(hour, at, at.Add(time.Second), chat); !met || !turn.Equal(time.Date(2026, 4, 4, 14, 0, 0, 0, time.UTC)) || back != hour {
		t.Errorf("FallBackAcrossHour at %s in Chatham = %s, %s, %v; want its turn at 14:00 UTC, an hour", at, turn, back, met)
	}
	turn, back, loc, met := o.FallBackAnywhereAcrossHour(hour, at, at.Add(time.Second))
	if !met || !turn.Equal(time.Date(2026, 4, 4, 14, 0, 0, 0, time.UTC)) || back != hour || loc == nil || !AcrossHour(turn, loc) {
		t.Errorf("FallBackAnywhereAcrossHour at %s = %s, %s, %v, %v; want Chatham's turn at 14:00 UTC, an hour, in a zone whose wall time shown twice holds an hour's start", at, turn, back, loc, met)
	}
	if _, _, l, met := o.FallBackAnywhere(hour, at, at.Add(time.Second)); !met || AcrossHour(turn, l) {
		t.Errorf("FallBackAnywhere at %s names %v (met %v); want the first zone by name that turns there, Auckland's rules, as before", at, l, met)
	}
	bt := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	if turn, _, met := o.FallBackAcrossHour(hour, bt, bt.Add(time.Second), ber); met {
		t.Errorf("FallBackAcrossHour at %s in Berlin met its turn at %s; want none: its wall time shown twice is a whole hour", bt, turn)
	}
	if _, _, met := o.FallBack(hour, bt, bt.Add(time.Second), ber); !met {
		t.Errorf("FallBack at %s in Berlin met no turn; want its turn", bt)
	}
	if turn, _, l, met := o.FallBackAnywhereAcrossHour(hour, bt, bt.Add(time.Second)); !met || l == nil || l.String() != "Antarctica/Troll" || !turn.Equal(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("FallBackAnywhereAcrossHour at %s = %s, %v, %v; want Antarctica/Troll's turn, at Berlin's instant", bt, turn, l, met)
	}
	ny, _ := time.LoadLocation("America/New_York")
	nt := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	if _, _, met := o.FallBack(hour, nt, nt.Add(time.Second), ny); !met {
		t.Errorf("FallBack at %s in New York met no turn; want its turn", nt)
	}
	if turn, _, l, met := o.FallBackAnywhereAcrossHour(hour, nt, nt.Add(time.Second)); met {
		t.Errorf("FallBackAnywhereAcrossHour at %s met %s's turn at %s; want none of a zone whose wall time shown twice holds an hour's start", nt, l, turn)
	}
}

// A read of the clock that is its value whole writes no Word in json, yaml
// or bson, so a file learned before Word reads and writes the same; one
// that is a word of a text keeps where the word stands through each (its
// bytes, the bytes after it, and the SHA-256 of each side), never a byte of
// the text, and two learnings of one word read compare equal (Word is a
// value, not a pointer).
func TestClockWordIsOmittedWholeAndKeptAsAWord(t *testing.T) {
	const text = "[E2E] generate-stop-1793336961102639022"
	whole := ClockOffset{Res: time.Nanosecond, Since: SinceTestBegin}
	word := whole
	word.Word = ClockWordOf(text, 20, len(text))
	if word.Word.Start != 20 || word.Word.End != 39 || word.Word.After != 0 {
		t.Fatalf("ClockWordOf = %+v; want the word at bytes 20-39 with none after it", word.Word)
	}
	if word.Word.BeforeSHA256 != sha256Of("[E2E] generate-stop-") || word.Word.AfterSHA256 != sha256Of("") || !word.Word.Valid() {
		t.Fatalf("ClockWordOf hashes %+v; want the SHA-256 of each side in lower-case hex", word.Word)
	}
	type enc struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}
	for _, e := range []enc{
		{"json", json.Marshal, json.Unmarshal},
		{"yaml", yaml.Marshal, yaml.Unmarshal},
		{"bson", bson.Marshal, bson.Unmarshal},
	} {
		b, err := e.marshal(whole)
		if err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if strings.Contains(string(b), "word") {
			t.Errorf("%s: a whole read wrote a word: %q", e.name, b)
		}
		b, err = e.marshal(word)
		if err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if strings.Contains(string(b), "generate") || strings.Contains(string(b), "E2E") {
			t.Errorf("%s: a word read wrote the text around it: %q", e.name, b)
		}
		var back ClockOffset
		if err := e.unmarshal(b, &back); err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if back != word {
			t.Errorf("%s: a word read came back as %+v, want %+v", e.name, back, word)
		}
	}
	again := whole
	again.Word = ClockWordOf(text, 20, len(text))
	if again != word || !whole.Word.IsZero() || word.Word.IsZero() {
		t.Errorf("two learnings of one word read compare apart, or IsZero is wrong")
	}
}

// WordOf takes a replay's word from between the recorded text's sides,
// compared by length and SHA-256: a word of another length between them is
// still the word (a time cut of its trailing zeros), and a side changed in
// one byte, its length kept, is named.
func TestClockWordOfComparesEachSideByLengthAndHash(t *testing.T) {
	w := ClockWordOf("job 2026-10-30T12:00:00.5Z queued", 4, 26)
	for _, tc := range []struct{ live, word, changed string }{
		{"job 2026-10-30T12:00:01.25Z queued", "2026-10-30T12:00:01.25Z", ""},
		{"jab 2026-10-30T12:00:01.25Z queued", "", "before"},
		{"job 2026-10-30T12:00:01.25Z queuee", "", "after"},
		{"job  queued", "", "short"},
		{"job queued", "", "short"},
	} {
		if word, changed := w.WordOf(tc.live); word != tc.word || changed != tc.changed {
			t.Errorf("WordOf(%q) = %q, %q; want %q, %q", tc.live, word, changed, tc.word, tc.changed)
		}
	}
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Two recordings that hold a variable at one place inside other text say
// two things about the place: the merge keeps neither, so the place is
// compared whole and a replay refuses it; parts at other places, or the
// same part twice, are kept once. A part at a place the variable does not
// name is stray.
func TestMergePartsDropsAPlaceTwoRecordingsDisagreeOn(t *testing.T) {
	app := VariablePart{Place: "body.key", Before: "APP:"}
	answer := VariablePart{Place: "body.key", Answer: true, Before: "APP:"}
	org := VariablePart{Place: "body.key", Before: "ORG:"}
	if got := MergeParts([]VariablePart{app}, []VariablePart{app, answer}); !reflect.DeepEqual(got, []VariablePart{app, answer}) {
		t.Fatalf("merged %+v", got)
	}
	if got := MergeParts([]VariablePart{app, answer}, []VariablePart{org}); !reflect.DeepEqual(got, []VariablePart{answer}) {
		t.Fatalf("a place two recordings disagree on is dropped: %+v", got)
	}
	if got := MergeParts([]VariablePart{app}, []VariablePart{org}); got != nil {
		t.Fatalf("nothing left is nil: %+v", got)
	}
	a := MockVariables{"app": {Recorded: "a-1", Use: []string{"body.key"}, Within: []VariablePart{app}}}
	b := MockVariables{"app": {Recorded: "a-1", Use: []string{"body.key"}, Within: []VariablePart{org}}}
	if got := MergeVariables(a, b)["app"]; got.Within != nil || !reflect.DeepEqual(got.Use, []string{"body.key"}) {
		t.Fatalf("merged variable %+v", got)
	}
	if c := a.Clone(); &c["app"].Within[0] == &a["app"].Within[0] {
		t.Fatal("a clone shares its parts")
	}
	if p, stray := (MockVariable{Use: []string{"body.key"}, Within: []VariablePart{answer}}).StrayPart(); !stray || p != answer {
		t.Fatalf("an answer part with no answer place is stray: %+v %v", p, stray)
	}
	if _, stray := a["app"].StrayPart(); stray {
		t.Fatal("a part at a use place is not stray")
	}
}

// A kept answer place is a note: one per place, the first recording's why
// first; a merge of two learns keeps both's places, and a clone shares none.
func TestMergeKeptKeepsEachPlaceOnce(t *testing.T) {
	label := VariableKept{Place: "body.label", Why: "the value is shorter than 8 characters"}
	again := VariableKept{Place: "body.label", Why: "another learn's words"}
	link := VariableKept{Place: "body.link", Why: "the value is all digits"}
	if got := MergeKept([]VariableKept{label}, []VariableKept{again, link}); !reflect.DeepEqual(got, []VariableKept{label, link}) {
		t.Fatalf("merged %+v", got)
	}
	if got := MergeKept(nil, nil); got != nil {
		t.Fatalf("nothing is nil: %+v", got)
	}
	a := MockVariables{"page": {Recorded: "1", Answer: []string{"body.key"}, Kept: []VariableKept{label}}}
	b := MockVariables{"page": {Recorded: "1", Answer: []string{"body.key"}, Kept: []VariableKept{link}}}
	if got := MergeVariables(a, b)["page"]; !reflect.DeepEqual(got.Kept, []VariableKept{label, link}) {
		t.Fatalf("merged variable %+v", got)
	}
	if c := a.Clone(); &c["page"].Kept[0] == &a["page"].Kept[0] {
		t.Fatal("a clone shares its kept places")
	}
}
