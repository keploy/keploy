package learn

import (
	"strings"
	"testing"
	"time"
)

// zoneAfterSpace are texts that write an instant with its zone after a
// space, as Sequelize writes a date where a timezone option is set
// ("YYYY-MM-DD HH:mm:ss.SSS Z"), as Go's time.Time.String() and Ruby's
// Time#to_s write one, each with the instant it names (at) and the time of
// day it writes (wall, in its own zone), and the zone's offset east of UTC.
var zoneAfterSpace = []struct {
	text string
	at   time.Time
	res  time.Duration
	zone time.Duration
}{
	{"2026-11-01 18:12:37.172 +00:00", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 0},
	{"2026-11-01 23:42:37.172 +05:30", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 5*time.Hour + 30*time.Minute},
	{"2026-11-01 23:57:37.172 +05:45", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 5*time.Hour + 45*time.Minute},
	{"2026-11-01 15:12:37.172 -03:00", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, -3 * time.Hour},
	// A zone with no colon (Go's and Ruby's), with the zone's name after it
	// (Go's).
	{"2026-11-01 23:42:37.172 +0530", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 5*time.Hour + 30*time.Minute},
	{"2026-11-01 23:42:37.172 +0530 IST", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 5*time.Hour + 30*time.Minute},
	{"2026-11-01 18:12:37.172 +0000 UTC", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 0},
	{"2026-11-01 23:42:37 +0530", time.Date(2026, 11, 1, 18, 12, 37, 0, time.UTC), time.Second, 5*time.Hour + 30*time.Minute},
	{"2026-11-01 23:42 +05:30", time.Date(2026, 11, 1, 18, 12, 0, 0, time.UTC), time.Minute, 5*time.Hour + 30*time.Minute},
	{"2026-11-01T23:42:37.172 +05:30", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 5*time.Hour + 30*time.Minute},
	// Z, written after the time as ISO 8601 writes it.
	{"2026-11-01 18:12:37.172Z", time.Date(2026, 11, 1, 18, 12, 37, 172e6, time.UTC), time.Millisecond, 0},
}

// A text that writes its zone after a space holds the instant that zone
// places it at: one span over the text's date, time and zone, zoned, at the
// zone's offset, writing its own time of day, and no time of day read of
// the zone's digits ("05:30").
func TestAZoneAfterASpaceIsTheInstantsZone(t *testing.T) {
	for _, tc := range zoneAfterSpace {
		spans := instantSpans(tc.text)
		if len(spans) != 1 {
			t.Errorf("%q: %d instants, want 1: %+v", tc.text, len(spans), spans)
			continue
		}
		sp := spans[0]
		// Go writes the zone's name after its offset: the offset places
		// the instant, and the name stands after the span.
		want := len(strings.TrimSuffix(strings.TrimSuffix(tc.text, " IST"), " UTC"))
		if sp.start != 0 || sp.end != want {
			t.Errorf("%q: span [%d, %d), want [0, %d)", tc.text, sp.start, sp.end, want)
		}
		if !sp.keyed || sp.key != (instantKey{at: tc.at.UnixNano()}) {
			t.Errorf("%q: names %v, want %v", tc.text, time.Unix(0, sp.key.at).UTC(), tc.at)
		}
		if !sp.zoned || sp.zone != int64(tc.zone) {
			t.Errorf("%q: zoned %v at %v, want zoned at %v", tc.text, sp.zoned, time.Duration(sp.zone), tc.zone)
		}
		if sp.wall != clockOfDay(tc.at.In(time.FixedZone("", int(tc.zone/time.Second)))) {
			t.Errorf("%q: writes %v of its day", tc.text, time.Duration(sp.wall))
		}
		if sp.res != tc.res {
			t.Errorf("%q: resolution %v, want %v", tc.text, sp.res, tc.res)
		}
	}
}

// An untyped stamp the application sent that writes its zone after a space
// echoes the cell its protocol types as that instant (a Postgres
// timestamptz it was stored in), and never a cell at its own time of day
// read as UTC, another instant wherever its zone is not UTC's.
func TestAZoneAfterASpaceEchoesItsTypedCell(t *testing.T) {
	for _, tc := range zoneAfterSpace {
		r := Leaf{Path: "query.bind[2]", Type: String, Value: tc.text}
		cell := InstantLeaf("rows[0].created_at", "rows[0].created_at", tc.at, tc.res)
		if e := Echoes(nil, r, []Leaf{cell}); len(e) != 1 {
			t.Errorf("%q: the cell %s it was stored in is no echo of it", tc.text, cell.Value)
		}
		if tc.zone == 0 {
			continue
		}
		wall := tc.at.In(time.FixedZone("", int(tc.zone/time.Second)))
		other := InstantLeaf("rows[0].created_at", "rows[0].created_at",
			time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), time.UTC), tc.res)
		if e := Echoes(nil, r, []Leaf{other}); len(e) != 0 {
			t.Errorf("%q: echoes %s, its time of day read as UTC: another instant", tc.text, other.Value)
		}
	}
}

// A zone after a space is read after a time alone, written whole (four
// digits, with or without a colon): after a date alone, or as two digits,
// a signed number after a space is no zone, and the instant before it is
// read as it was.
func TestASignedNumberAfterASpaceIsNoZoneUnlessItIsOne(t *testing.T) {
	for _, tc := range []struct {
		text     string
		end      int
		at       time.Time
		res      time.Duration
		zoned    bool
		instants int
	}{
		// A date, then a sum: the date as a date, of no zone.
		{"2026-11-01 -1500.00", 10, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 24 * time.Hour, false, 1},
		{"2026-11-01 +05:30", 10, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 24 * time.Hour, false, 2},
		// Two digits after a space name no zone.
		{"2026-11-01 23:42:37 +05", 19, time.Date(2026, 11, 1, 23, 42, 37, 0, time.UTC), time.Second, false, 1},
		{"2026-11-01 23:42:37 +05:3", 19, time.Date(2026, 11, 1, 23, 42, 37, 0, time.UTC), time.Second, false, 1},
		// Two spaces.
		{"2026-11-01 23:42:37  +05:30", 19, time.Date(2026, 11, 1, 23, 42, 37, 0, time.UTC), time.Second, false, 2},
	} {
		spans := instantSpans(tc.text)
		if len(spans) != tc.instants {
			t.Errorf("%q: %d instants, want %d: %+v", tc.text, len(spans), tc.instants, spans)
			continue
		}
		sp := spans[0]
		if sp.start != 0 || sp.end != tc.end || sp.key != (instantKey{at: tc.at.UnixNano()}) || sp.res != tc.res || sp.zoned != tc.zoned {
			t.Errorf("%q: %+v, want [0, %d) at %v to %v, zoned %v", tc.text, sp, tc.end, tc.at, tc.res, tc.zoned)
		}
	}
}
