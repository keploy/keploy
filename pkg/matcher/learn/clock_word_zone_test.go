package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// noZoneWall writes r as a wall clock of no zone in z, to the microsecond,
// as MySQL's DATETIME(6) text and Python's str(datetime) write one.
func noZoneWall(r time.Time, z *time.Location) string {
	return r.In(z).Format("2006-01-02 15:04:05.000000")
}

// A text whose one changed word is a wall clock of no zone is found as that
// word (ClockWordIn) where the call's window holds the word read in UTC, or
// in the zone the application writes such a clock in (an application in
// India writes now() in IST): the same two readings a whole value's instant
// gets (Window.holdsRead), so a word is never refused for a zone its text
// was not even tried in. With no zone known (nil) a word is read in UTC
// alone, as before. A word an hour before the window in that zone, or one
// whose text names its zone (read as it names it, never again in the
// application's), is no read; nor is a text the layout matches and cannot
// read, a 30th of February, in a window around the instant keploy would
// take it for.
func TestAClockWordOfNoZoneIsReadInUTCAndInTheApplicationsZone(t *testing.T) {
	read := time.Date(2026, 11, 4, 0, 16, 15, 682701000, time.UTC)
	fread := read.Add(3 * time.Minute)
	span := func(r time.Time) Span { return Span{From: r.Add(-time.Second), To: r.Add(time.Second)} }
	w := &Window{Rec: span(read), F: span(fread)}
	epoch := time.Unix(0, 0).UTC()
	around1970 := &Window{Rec: Span{From: epoch.Add(-time.Second), To: epoch.Add(2 * time.Second)}, F: Span{From: epoch.Add(-time.Second), To: epoch.Add(2 * time.Second)}}
	for _, tc := range []struct {
		name      string
		rec, live string
		w         *Window
		zone      *time.Location
		found     bool
	}{
		{"in UTC, no zone known", noZoneWall(read, time.UTC), noZoneWall(fread, time.UTC), w, nil, true},
		{"in IST, no zone known", noZoneWall(read, ist), noZoneWall(fread, ist), w, nil, false},
		{"in IST, the application in IST", noZoneWall(read, ist), noZoneWall(fread, ist), w, ist, true},
		{"in UTC, the application in IST", noZoneWall(read, time.UTC), noZoneWall(fread, time.UTC), w, ist, true},
		{"in IST an hour early, the application in IST", noZoneWall(read.Add(-time.Hour), ist), noZoneWall(fread.Add(-time.Hour), ist), w, ist, false},
		{"its zone named, UTC, the application in IST", noZoneWall(read, ist) + "Z", noZoneWall(fread, ist) + "Z", w, ist, false},
		{"recorded on the 30th of February", "1970-02-30 00:00:00", "1970-01-01 00:00:01", around1970, nil, false},
		{"seen on the 30th of February", "1970-01-01 00:00:01", "1970-02-30 00:00:00", around1970, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, rw, lw, after, ok := ClockWordIn("note-"+tc.rec+" (cache)", "note-"+tc.live+" (cache)", tc.w, tc.zone)
			if ok != tc.found {
				t.Fatalf("found %v (%q, %q); want %v", ok, rw, lw, tc.found)
			}
			if ok && (before != "note-" || after != " (cache)" || rw != tc.rec || lw != tc.live) {
				t.Fatalf("found %q | %q / %q | %q; want the word between \"note-\" and \" (cache)\"", before, rw, lw, after)
			}
		})
	}
}

// keploy mock's client tests learn the one changed word of a text as a read
// of the clock (wordRead) in the application's zone too: a name
// "note-<now()>" an application in IST writes, in its call's window and in
// the window since its test began (sinceWord), is a checked read of that
// word, the rest of the text compared. With no zone known it is learned as
// before: only a word read in UTC.
func TestAClientTestLearnsAWordOfNoZoneTheApplicationWritesInItsZone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write *time.Location
		app   *time.Location
		read  bool
	}{
		{"written in IST, the application in IST", ist, ist, true},
		{"written in IST, no zone known", ist, nil, false},
		{"written in UTC, no zone known", time.UTC, nil, true},
	} {
		for _, prev := range []time.Duration{-100 * time.Millisecond, time.Second} {
			ln, rec := classifyWordAt(clientTestCacheIn(tc.app), func(r time.Time) string { return "note-" + noZoneWall(r, tc.write) }, prev, false)
			o, ok := ln.Clock()
			got := ln.Kind == KindClockRead && ok && o.Plain() && o.Word == (sides{"note-", ""}).at(t, rec) && (o.Since == models.SinceTestBegin) == (prev > 0)
			if got != tc.read {
				t.Errorf("%s, the window opening %v after the read: %s Read=%+v (%s); want a checked read of the word: %v", tc.name, prev, ln.Kind, ln.Read, ln.Why, tc.read)
			}
		}
	}
}
