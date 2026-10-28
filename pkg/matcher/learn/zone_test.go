package learn

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ReadZone reads a process's zone as the C library and Go do, with its name
// where its files give one (coordinator's ruling QE, 2026-10-28 00:30): TZ
// first ("", "UTC", an IANA name with or without ":", a path), else root's
// /etc/localtime, a link into zoneinfo or a copied file /etc/timezone names;
// a copied file of no name keeps its rules (named by its fixed offset where
// it has one, models.ZoneNameOf); a TZ of a POSIX rule is an error, never
// UTC. A link's absolute target is read under root, never keploy's own.
func TestReadZoneNamesTheZoneAProcessWritesItsWallClockIn(t *testing.T) {
	kolkata, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kolkata")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	berlin, err := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	root := func(t *testing.T, files map[string][]byte, links map[string]string) string {
		t.Helper()
		r := t.TempDir()
		for name, data := range files {
			p := filepath.Join(r, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, target := range links {
			p := filepath.Join(r, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	zi := "usr/share/zoneinfo/"
	for _, row := range []struct {
		what  string
		files map[string][]byte
		links map[string]string
		tz    string
		set   bool
		want  string // ZoneNameOf the zone; "!" an error
		off   int    // its offset at at, in seconds
		// named: the zone ReadZone gives carries a name (loc.String()), the
		// one its files give; "" none.
	}{
		{"a link into zoneinfo", map[string][]byte{zi + "Asia/Kolkata": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Asia/Kolkata"}, "", false, "+05:30", 19800},
		{"a relative link", map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "../" + zi + "Europe/Berlin"}, "", false, "Europe/Berlin", 3600},
		{"a posix/ link", map[string][]byte{zi + "posix/Asia/Kolkata": kolkata}, map[string]string{"etc/localtime": "/" + zi + "posix/Asia/Kolkata"}, "", false, "+05:30", 19800},
		{"a copied file /etc/timezone names", map[string][]byte{"etc/localtime": berlin, "etc/timezone": []byte("Europe/Berlin\n"), zi + "Europe/Berlin": berlin}, nil, "", false, "Europe/Berlin", 3600},
		{"a copied file /etc/timezone misnames", map[string][]byte{"etc/localtime": berlin, "etc/timezone": []byte("Asia/Kolkata\n"), zi + "Asia/Kolkata": kolkata}, nil, "", false, "", 3600},
		{"a link to a link into zoneinfo", map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/etc/alt/localtime", "etc/alt/localtime": "/" + zi + "Europe/Berlin"}, "", false, "Europe/Berlin", 3600},
		// Under root, the file the link names holds Berlin's rules: they are
		// read, never keploy's own Asia/Kolkata.
		{"a link into zoneinfo, its file under root another zone's", map[string][]byte{zi + "Asia/Kolkata": berlin}, map[string]string{"etc/localtime": "/" + zi + "Asia/Kolkata"}, "", false, "Asia/Kolkata", 3600},
		{"a link to no file", nil, map[string]string{"etc/localtime": "/" + zi + "Asia/Kolkata"}, "", false, "!", 0},
		{"a link climbing above root", map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "../../../../../../" + zi + "Europe/Berlin"}, "", false, "Europe/Berlin", 3600},
		{"a copied file of no name, a fixed offset", map[string][]byte{"etc/localtime": kolkata}, nil, "", false, "+05:30", 19800},
		{"a copied file of no name, summer time", map[string][]byte{"etc/localtime": berlin}, nil, "", false, "", 3600},
		{"no zone file", nil, nil, "", false, "!", 0},
		{"TZ empty", map[string][]byte{"etc/localtime": berlin}, nil, "", true, "UTC", 0},
		{"TZ UTC", nil, nil, "UTC", true, "UTC", 0},
		{"TZ an IANA name", map[string][]byte{"etc/localtime": berlin}, nil, "Asia/Kolkata", true, "+05:30", 19800},
		{"TZ an IANA name with a colon", nil, nil, ":Europe/Berlin", true, "Europe/Berlin", 3600},
		{"TZ a path under root", map[string][]byte{zi + "Asia/Kolkata": kolkata}, nil, "/" + zi + "Asia/Kolkata", true, "+05:30", 19800},
		{"TZ a POSIX rule", nil, nil, "IST-5:30", true, "!", 0},
	} {
		r := root(t, row.files, row.links)
		loc, err := ReadZone(r, row.tz, row.set)
		if row.want == "!" {
			if err == nil {
				t.Errorf("%s: %v; want an error", row.what, loc)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", row.what, err)
			continue
		}
		if got := models.ZoneNameOf(loc, at); got != row.want && !strings.Contains(row.what, "another zone's") {
			t.Errorf("%s: named %q; want %q", row.what, got, row.want)
		}
		if strings.Contains(row.what, "misnames") && loc.String() != "" {
			t.Errorf("%s: ReadZone named it %q; want no name (the bytes are another zone's)", row.what, loc.String())
		}
		if _, off := at.In(loc).Zone(); off != row.off {
			t.Errorf("%s: offset %d; want %d", row.what, off, row.off)
		}
	}
	// An absolute link's target is the process's path, read under its root,
	// of no zoneinfo path too.
	r := root(t, map[string][]byte{"z/" + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/z/Europe/Berlin"})
	if loc, err := ReadZone(r, "", false); err != nil {
		t.Errorf("an absolute link to a file of no zoneinfo path: %v", err)
	} else if _, off := at.In(loc).Zone(); off != 3600 {
		t.Errorf("an absolute link read under root: offset %d; want Berlin's 3600", off)
	}
}

// A clock read whose learning depends on the application's zone is learned
// naming it (models.ClockOffset.AppZone), and a replay checks it in that
// zone on any machine (coordinator's ruling QE, 2026-10-28 00:30): learned
// in IST, a $date the application cuts to the IST hour passes at every one
// of 2,000 replays whose own tracker reads UTC (a CI machine), where the
// old file, which names no zone, is read in the replay's UTC and refused by
// when in the hour the replay ran. Learned in UTC, a wall clock of no zone
// cut to the hour is read in UTC on a laptop in IST: the application's UTC
// wall clock passes, an IST one is refused, where the old file passes it.
// A minute's cut, which no zone moves, names none. A zone this keploy
// cannot read refuses, naming it.
func TestACutLearnedInTheApplicationsZoneIsCheckedInItOnAnyMachine(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const tp = "body.at.$date.$numberLong"
	shape := func(path string, l Leaf) Shape { return Shape{Leaves: map[string]Leaf{path: l}} }
	hourIn := func(loc *time.Location, at time.Time) time.Time {
		l := at.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	}
	typed := func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at, time.Millisecond) }
	rec := time.Date(2026, 10, 20, 9, 47, 7, 600e6, time.UTC)
	learnIn := func(zone *time.Location, path string, leaf func(time.Time) Leaf, cut func(time.Time) time.Time) models.ClockOffset {
		t.Helper()
		dc := clockedDrifts()
		dc.zone = zone
		live := rec.Add(3*time.Hour + 7*time.Minute)
		ln := ClassifyIn(dc, []string{path}, shape(path, leaf(cut(live))), shape(path, leaf(cut(rec))), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !ok {
			t.Fatalf("learned %s (%s); want a read of the clock", ln.Kind, ln.Why)
		}
		return o
	}

	// Learned in IST, replayed on a machine in UTC.
	o := learnIn(ist, tp, typed, func(t time.Time) time.Time { return hourIn(ist, t) })
	if o.Res != time.Hour || o.AppZone != "+05:30" {
		t.Fatalf("a $date cut to the IST hour, learned in IST: %+v; want an hour's cut naming +05:30", o)
	}
	old := o
	old.AppZone = ""
	rng := rand.New(rand.NewSource(7))
	base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	oldRefused := 0
	for i := 0; i < 2000; i++ {
		from := base.Add(time.Duration(rng.Int63n(int64(10 * 24 * time.Hour))))
		s := Span{From: from, To: from.Add(time.Duration(rng.Int63n(int64(10 * time.Minute)))), Zone: time.UTC}
		v := typed(hourIn(ist, s.From))
		if ok, why := CheckClockRead(tp, o, v, s, true); !ok {
			t.Fatalf("learned in IST, replayed in UTC at %s: the IST hour %s refused (%s)", s.From, v.Value, why)
		}
		if ok, _ := CheckClockRead(tp, o, typed(hourIn(ist, s.From).Add(-2*time.Hour)), s, true); ok {
			t.Fatalf("learned in IST, replayed in UTC at %s: a value two hours stale passed", s.From)
		}
		if ok, _ := CheckClockRead(tp, old, v, s, true); !ok {
			oldRefused++
		}
	}
	if oldRefused == 0 {
		t.Errorf("the old file (no zone named) replayed in UTC: never refused the IST hour; the row no longer shows what the stored zone fixes")
	}
	if ok, why := CheckClockRead(tp, o, typed(hourIn(ist, base)), Span{From: base, To: base.Add(time.Minute)}, true); !ok {
		t.Errorf("learned in IST, a replay whose tracker cannot tell the application's zone: refused (%s); want checked in IST", why)
	}
	// Refused on the CI machine in UTC (a value two hours stale), the
	// reason says the replay runs in another zone, and how to replay in the
	// recorded one: a fixed offset is no TZ value.
	ci := Span{From: base, To: base.Add(time.Minute), Zone: time.UTC}
	if ok, why := CheckClockRead(tp, o, typed(hourIn(ist, base).Add(-2*time.Hour)), ci, true); ok || !strings.Contains(why, "This replay runs in UTC, not +05:30, the zone the test was recorded in") || !strings.Contains(why, "run it in a zone at UTC+05:30 as it was recorded") || strings.Contains(why, "TZ=+") {
		t.Errorf("learned in IST, refused on a machine in UTC: %v %q; want the hint naming both zones, with no TZ=+05:30", ok, why)
	}
	// A replay whose own zone has no name but IST's rules: no hint.
	if ok, why := CheckClockRead(tp, o, typed(hourIn(ist, base).Add(-2*time.Hour)), Span{From: base, To: base.Add(time.Minute), Zone: time.FixedZone("", 5*3600+1800)}, true); ok || strings.Contains(why, "This replay runs in") {
		t.Errorf("learned in IST, refused in an unnamed zone of IST's rules: %v %q; want no hint", ok, why)
	}

	// A tracker that could not read its zone by name (SetAppZoneUnnamed)
	// learns no name; one that could (SetAppZone) learns it.
	for _, named := range []bool{false, true} {
		tr := NewTracker()
		if named {
			tr.SetAppZone(ist)
		} else {
			tr.SetAppZoneUnnamed(ist)
		}
		dc := tr.Drifts()
		dc.appClock = true
		live := rec.Add(3*time.Hour + 7*time.Minute)
		ln := ClassifyIn(dc, []string{tp}, shape(tp, typed(hourIn(ist, live))), shape(tp, typed(hourIn(ist, rec))), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
		got, ok := ln.Clock()
		if want := map[bool]string{true: "+05:30", false: ""}[named]; !ok || got.Res != time.Hour || got.AppZone != want {
			t.Errorf("a tracker whose zone is named %v: learned %+v (%s); want an hour's cut naming %q", named, got, ln.Why, want)
		}
	}

	// Learned in UTC, replayed on a laptop in IST.
	const wp = "body.seenAt"
	wall := func(loc *time.Location) func(time.Time) Leaf {
		return func(at time.Time) Leaf {
			return Leaf{Path: wp, Noise: wp, Type: String, Value: at.In(loc).Format("2006-01-02 15:04:05")}
		}
	}
	u := learnIn(time.UTC, wp, wall(time.UTC), func(t time.Time) time.Time { return t.Truncate(time.Hour) })
	if u.Res != time.Hour || u.Zone != models.ZoneEither || u.AppZone != "UTC" {
		t.Fatalf("a wall clock cut to the UTC hour, learned in UTC: %+v; want an hour's cut in either zone naming UTC", u)
	}
	now := time.Date(2026, 10, 28, 4, 45, 0, 0, time.UTC)
	s := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}
	if ok, why := CheckClockRead(wp, u, wall(time.UTC)(now.Truncate(time.Hour)), s, true); !ok {
		t.Errorf("learned in UTC, replayed in IST: the application's UTC hour refused (%s)", why)
	}
	istHour := wall(ist)(hourIn(ist, now))
	if ok, why := CheckClockRead(wp, u, istHour, s, true); ok || !strings.Contains(why, "read with the application's zone as UTC") || !strings.Contains(why, "This replay runs in Asia/Kolkata, not UTC, the zone the test was recorded in") || !strings.Contains(why, "run it with TZ=UTC as it was recorded") {
		t.Errorf("learned in UTC, replayed in IST: the IST hour %s: %v %q; want refused, read in UTC as learned, naming it", istHour.Value, ok, why)
	}
	oldU := u
	oldU.AppZone = ""
	// Neither zone known: a wall clock either zone held is checked at its
	// cut in UTC and in the agent's zone, and passes where either reading
	// holds, never refused for its cut (2026-10-28: a cut read is checked).
	for _, h := range []Leaf{istHour, wall(time.UTC)(now.Truncate(time.Hour))} {
		if ok, why := CheckClockRead(wp, oldU, h, s, true); !ok {
			t.Errorf("FALSE RED: the old file, replayed where the agent is in IST and the application's zone is unread: the hour %s refused (%s); want checked at its cut in either zone, and passed", h.Value, why)
		}
	}
	for _, h := range []Leaf{wall(ist)(hourIn(ist, now.Add(-3*time.Hour))), wall(time.UTC)(now.Add(-3 * time.Hour).Truncate(time.Hour))} {
		if ok, why := CheckClockRead(wp, oldU, h, s, true); ok || !strings.Contains(why, "it changed") || !strings.Contains(why, "so it checked the value in UTC and in the agent's own zone (Asia/Kolkata)") {
			t.Errorf("the old file, replayed where the agent is in IST and the application's zone is unread: the hour %s, three hours stale in both zones: %v %q; want refused as changed, saying where it was read", h.Value, ok, why)
		}
	}
	own := s
	own.Own = true
	if ok, why := CheckClockRead(wp, oldU, istHour, own, true); !ok {
		t.Errorf("the old file, replayed where the application's own zone is IST: the IST hour refused (%s); want it read in the application's zone", why)
	}

	// A minute's cut is one in every zone: no zone named.
	if m := learnIn(ist, tp, typed, func(t time.Time) time.Time { return t.Truncate(time.Minute) }); m.AppZone != "" {
		t.Errorf("a $date cut to the minute, learned in IST: %+v; want no zone named", m)
	}
	bad := o
	bad.AppZone = "Mars/Olympus"
	if ok, why := CheckClockRead(tp, bad, typed(hourIn(ist, base)), Span{From: base, To: base.Add(time.Minute), Zone: ist}, true); ok || !strings.Contains(why, "Mars/Olympus") {
		t.Errorf("a zone this keploy cannot read: %v %q; want refused, naming it", ok, why)
	}
}

// A wall clock of no zone learned in the application's zone alone
// (models.ZoneApp) and checked in the zone the learn step named, because
// keploy cannot read the zone of the process that made the call (keploy's
// docker and compose modes, a pod without hostPID, a process exiting as
// keploy reads it), holds at a cut of an hour or coarser only where it holds
// read in UTC too (2026-10-29). Read in the stored
// zone alone, a date a recording made natively in New York learned passed
// two days stale where the application now writes in UTC (replayed at
// 2026-10-29 02:00Z, 2026-10-27 passed). Now: the
// application's own date passes at every hour of the day whichever of five
// zones it writes in (no zone's date is more than a day from UTC's), and so
// does the day before in a call's window that crosses its midnight; a date
// a day or two back passes only where it holds read in both zones, so one
// two days stale in New York, UTC or Honolulu is refused at every hour, and
// one two days stale in a zone ahead of both (Kolkata, Tokyo) passes only
// where that zone's date is a day ahead of both readings' (the stated
// limit); learned in Tokyo, a date a day ahead of Tokyo's is refused in
// Tokyo's morning, as UTC's date is a day behind. An hour's cut learned in
// Kolkata, or in Berlin in summer (two hours from UTC), is refused whatever
// its value, saying so: in Berlin even the hour before the application's
// own, which both readings would pass (in Kolkata none would). So is one
// read since its test began in Berlin before the clocks went back
// (2026-10-25 01:00Z) whose call came after, its span starting two hours
// from UTC and ending one hour from it, though both readings pass the
// application's hour at the call; one whose test began after passes it, and
// a call's window across that instant is refused, saying it began two
// hours from UTC. A value that shows only a finer cut is checked
// at it in the stored zone. Learned in Berlin in winter, its own hour
// passes, one hour stale too, and one an hour ahead is refused (UTC's hour
// is an hour behind; a time of day alike); learned in the Azores
// (UTC-01:00), one an hour ahead passes and one stale is refused; two hours
// stale is refused anywhere. The check in UTC is at the cut the stored
// zone's was: a date and time at the first instant of a day whose midnight
// Santiago skips stands for its day, never for the hour it shows in UTC.
// With the process's zone read (Span.Own) nothing changes.
func TestADateOfNoZoneCheckedInTheStoredZoneHoldsOnlyWhereItHoldsInUTCToo(t *testing.T) {
	load := func(name string) *time.Location {
		t.Helper()
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	ny := load("America/New_York")
	const tp = "body.day"
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{tp: l}} }
	dateOf := func(at time.Time, loc *time.Location) time.Time {
		l := at.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	}
	day := func(d time.Time) Leaf { return WallClockLeaf(tp, "body.day", d, 24*time.Hour) }
	rec := time.Date(2026, 10, 20, 2, 30, 0, 0, time.UTC) // New York 10-19 22:30, UTC 10-20
	live := rec.Add(3 * time.Hour)                        // New York 10-20 01:30, UTC 10-20
	dc := clockedDrifts()
	dc.zone = ny
	ln := ClassifyIn(dc, []string{tp}, shape(day(dateOf(live, ny))), shape(day(dateOf(rec, ny))), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	o, ok := ln.Clock()
	if !ok || o.Zone != models.ZoneApp || o.AppZone != "America/New_York" || o.Res != 24*time.Hour {
		t.Fatalf("a date of no zone learned in New York: %+v %v (%s); want a day's cut, zone app, naming America/New_York", o, ok, ln.Why)
	}
	const stale = "the value this call carries passes in America/New_York but not in UTC, so keploy refuses it"
	holds := func(v Leaf, s Span) bool {
		ok, _ := CheckClockRead(tp, o, v, s, true)
		return ok
	}

	// Learned in New York, replayed at 02:00Z, the application now in UTC.
	from := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	s := Span{From: from, To: from.Add(time.Minute), Zone: time.UTC}
	for _, row := range []struct {
		d    int
		pass bool
		why  string
	}{{29, true, ""}, {28, true, ""}, {27, false, stale}, {26, false, "it changed"}} {
		ok, why := CheckClockRead(tp, o, day(time.Date(2026, 10, row.d, 0, 0, 0, 0, time.UTC)), s, true)
		if ok != row.pass || !strings.Contains(why, row.why) {
			t.Errorf("replayed at %s, the application's zone unread: 2026-10-%d: %v %q; want %v %q", from, row.d, ok, why, row.pass, row.why)
		}
	}
	if ok, why := CheckClockRead(tp, o, day(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)), s, true); ok || !strings.Contains(why, "read in America/New_York, the application's zone the learn step read it in") || !strings.Contains(why, "A value cut to 1d") || !strings.Contains(why, "--pid=host") || !strings.Contains(why, "TZ=UTC") || strings.Contains(why, "This replay runs in") {
		t.Errorf("FALSE GREEN or unclear: a date two days stale, the application's zone unread: %v %q; want refused, naming the stored zone, the cut and what to do in docker too", ok, why)
	}
	own := s
	own.Own = true
	if ok, _ := CheckClockRead(tp, o, day(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)), own, true); ok {
		t.Errorf("FALSE GREEN: a date two days stale, the application's own zone read as UTC: passed")
	}
	if ok, why := CheckClockRead(tp, o, day(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)), own, true); !ok {
		t.Errorf("the application's own date, its zone read as UTC: refused (%s)", why)
	}

	// Every hour of a day, whichever zone the application now writes in.
	// A date one or two days back passes only where it holds read in both
	// zones; for an application in New York, UTC or Honolulu, two days back
	// never does.
	for _, name := range []string{"America/New_York", "UTC", "Pacific/Honolulu", "Asia/Kolkata", "Asia/Tokyo"} {
		app := load(name)
		for h := 0; h < 24; h++ {
			now := time.Date(2026, 11, 3, h, 10, 0, 0, time.UTC)
			s := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: time.UTC}
			at := now.Add(5 * time.Millisecond)
			if d := dateOf(at, app); !holds(day(d), s) {
				_, why := CheckClockRead(tp, o, day(d), s, true)
				t.Errorf("FALSE RED: an application in %s at %02d:10Z, its zone unread: its own date %s refused (%s)", name, h, d.Format("2006-01-02"), why)
			}
			lo := func(loc *time.Location) time.Time { return dateOf(s.From, loc).AddDate(0, 0, -1) }
			for _, back := range []int{1, 2} {
				d := dateOf(at, app).AddDate(0, 0, -back)
				want := !d.Before(lo(ny)) && !d.Before(lo(time.UTC))
				if back == 2 && want && (name == "America/New_York" || name == "UTC" || name == "Pacific/Honolulu") {
					t.Fatalf("the row's own expectation: a date two days stale in %s at %02d:10Z expected to pass", name, h)
				}
				if ok, why := CheckClockRead(tp, o, day(d), s, true); ok != want {
					t.Errorf("an application in %s at %02d:10Z, its zone unread: its date %d days back %s: %v (%s); want %v", name, h, back, d.Format("2006-01-02"), ok, why, want)
				}
			}
		}
		// A read just before the application's midnight, in a call's
		// window that crosses it: the day before passes, in every zone.
		next := dateOf(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), app).AddDate(0, 0, 1)
		midnight := time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, app)
		s := Span{From: midnight.Add(-time.Second), To: midnight.Add(10 * time.Millisecond), Zone: time.UTC}
		if ok, why := CheckClockRead(tp, o, day(next.AddDate(0, 0, -1)), s, true); !ok {
			t.Errorf("FALSE RED: an application in %s whose call's window crosses its midnight, its zone unread: the day before refused (%s)", name, why)
		}
	}

	// Learned in Tokyo, replayed in Tokyo's morning (2026-11-04 05:00, UTC
	// still on 11-03): Tokyo's date and UTC's pass, one a day ahead of
	// Tokyo's is refused by the reading in UTC.
	tk := o
	tk.AppZone = "Asia/Tokyo"
	morning := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
	ts := Span{From: morning, To: morning.Add(20 * time.Millisecond), Zone: time.UTC}
	for _, row := range []struct {
		d    int
		pass bool
		why  string
	}{{4, true, ""}, {3, true, ""}, {5, false, "the value this call carries passes in Asia/Tokyo but not in UTC, so keploy refuses it"}, {6, false, "it changed"}} {
		if ok, why := CheckClockRead(tp, tk, day(time.Date(2026, 11, row.d, 0, 0, 0, 0, time.UTC)), ts, true); ok != row.pass || !strings.Contains(why, row.why) {
			t.Errorf("a date learned in Tokyo, replayed at %s, the application's zone unread: 2026-11-%02d: %v %q; want %v %q", morning, row.d, ok, why, row.pass, row.why)
		}
	}

	// A date and time at the first instant of 2026-09-06 in Santiago, whose
	// midnight its clocks skip (01:00), cut to a day: checked in UTC at the
	// day too, where it shows only an hour.
	sg := models.ClockOffset{Res: 24 * time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "America/Santiago"}
	noon := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	first := Leaf{Path: "body.seenAt", Noise: "body.seenAt", Type: String, Value: "2026-09-06 01:00:00"}
	if ok, why := CheckClockRead("body.seenAt", sg, first, Span{From: noon, To: noon.Add(20 * time.Millisecond), Zone: time.UTC}, true); !ok {
		t.Errorf("FALSE RED: the first instant of a day whose midnight Santiago skips, cut to a day, its zone unread: refused (%s); want checked at the day in UTC too", why)
	}

	// An hour of no zone. Learned in Kolkata or in Berlin in summer (more
	// than an hour from UTC): refused whatever it is, the hour before too,
	// but a value that shows only a minute's cut is checked at it in the
	// stored zone. Learned in Berlin in winter (UTC+01:00): its own hour
	// and the one before pass, one an hour ahead is refused by the reading
	// in UTC, a time of day alike, and one two hours stale is refused.
	// Learned in the Azores (UTC-01:00): its own hour and the one after
	// pass, one an hour stale is refused by the reading in UTC.
	const wp = "body.seenAt"
	wall := func(loc *time.Location, at time.Time) Leaf {
		l := at.In(loc)
		return Leaf{Path: wp, Noise: wp, Type: String, Value: time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")}
	}
	now := time.Date(2026, 11, 3, 4, 45, 0, 0, time.UTC)
	minute := func(loc *time.Location, at time.Time) Leaf {
		return Leaf{Path: wp, Noise: wp, Type: String, Value: at.In(loc).Truncate(time.Minute).Format("2006-01-02 15:04:05")}
	}
	tod := func(loc *time.Location, at time.Time) Leaf {
		return Leaf{Path: wp, Noise: wp, Type: String, Value: at.In(loc).Truncate(time.Hour).Format("15:04:05")}
	}
	summer := time.Date(2026, 7, 1, 4, 45, 0, 0, time.UTC) // Berlin 06:45, at UTC+02:00
	const whatever, notUTC = "so keploy refuses the value whatever it is", "the value this call carries passes in "
	for _, row := range []struct {
		zone       string
		at         time.Time
		ago        time.Duration
		leaf       func(*time.Location, time.Time) Leaf
		layout     string
		pass       bool
		why, avoid string
	}{
		{"Asia/Kolkata", now, 0, wall, "", false, "only where the zone stands within an hour of UTC: Asia/Kolkata stood at UTC+05:30 at this call, " + whatever, "This replay runs in"},
		{"Asia/Kolkata", now, time.Hour, wall, "", false, whatever, "This replay runs in"},
		{"Asia/Kolkata", now, 0, minute, "", true, "", ""},
		{"Europe/Berlin", summer, 0, wall, "", false, "Europe/Berlin stood at UTC+02:00 at this call, " + whatever, ""},
		{"Europe/Berlin", summer, time.Hour, wall, "", false, whatever, ""},
		{"Europe/Berlin", now, 0, wall, "", true, "", ""},
		{"Europe/Berlin", now, time.Hour, wall, "", true, "", ""},
		{"Europe/Berlin", now, -time.Hour, wall, "", false, notUTC + "Europe/Berlin but not in UTC", ""},
		{"Europe/Berlin", now, 2 * time.Hour, wall, "", false, "it changed", ""},
		{"Europe/Berlin", now, 0, tod, "time-second", true, "", ""},
		{"Europe/Berlin", now, -time.Hour, tod, "time-second", false, notUTC + "Europe/Berlin but not in UTC", ""},
		{"Atlantic/Azores", now, 0, wall, "", true, "", ""},
		{"Atlantic/Azores", now, -time.Hour, wall, "", true, "", ""},
		{"Atlantic/Azores", now, time.Hour, wall, "", false, notUTC + "Atlantic/Azores but not in UTC", ""},
	} {
		layout := "iso-second-space"
		if row.layout != "" {
			layout = row.layout
		}
		h := models.ClockOffset{Res: time.Hour, Zone: models.ZoneApp, Layout: layout, AppZone: row.zone}
		s := Span{From: row.at, To: row.at.Add(20 * time.Millisecond), Zone: time.UTC}
		v := row.leaf(load(row.zone), row.at.Add(-row.ago))
		ok, why := CheckClockRead(wp, h, v, s, true)
		if ok != row.pass || !strings.Contains(why, row.why) || row.avoid != "" && strings.Contains(why, row.avoid) {
			t.Errorf("an hour of no zone (%s) learned in %s, replayed at %s, the application's zone unread: %q, read at the call less %v: %v %q; want %v %q", layout, row.zone, row.at, v.Value, row.ago, ok, why, row.pass, row.why)
		}
	}

	// Read since its test began, in Berlin, across 2026-10-25 01:00Z, when
	// its clocks went back from UTC+02:00 to UTC+01:00: the span starts more
	// than an hour from UTC and ends within one, so it is refused whatever
	// the value, though both readings pass the application's hour at the
	// call (02:00Z read in Berlin, inside the span, and 03:00Z in UTC,
	// within its hour's allowance past the call at 02:30Z); a test begun
	// after the clocks went back passes it, and a call's own window across
	// that instant is refused, saying it began far from UTC.
	berlin := load("Europe/Berlin")
	back := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	call := back.Add(90 * time.Minute) // Berlin 03:30, at UTC+01:00
	since := models.ClockOffset{Res: time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "Europe/Berlin", Since: models.SinceTestBegin}
	plain := since
	plain.Since = ""
	atCall := wall(berlin, call)
	for _, row := range []struct {
		o        models.ClockOffset
		from     time.Time
		began    time.Time
		pass     bool
		why, say string
	}{
		{since, call, back.Add(-30 * time.Minute), false, "Europe/Berlin stood at UTC+02:00 when this test began, " + whatever, "refused whatever it is"},
		{since, call, back.Add(30 * time.Minute), true, "", "passed"},
		{plain, back.Add(-10 * time.Millisecond), time.Time{}, false, "Europe/Berlin stood at UTC+02:00 as this call's window began, " + whatever, "refused whatever it is"},
	} {
		s := Span{From: row.from, To: call, Zone: time.UTC, Began: row.began}
		if row.o.Since == "" {
			s.To = back.Add(10 * time.Millisecond)
		}
		if ok, why := CheckClockRead(wp, row.o, atCall, s, true); ok != row.pass || !strings.Contains(why, row.why) {
			t.Errorf("an hour of no zone learned in Berlin, %q, span %s to %s (test began %s), the application's zone unread: %v %q; want it %s", atCall.Value, s.From, s.To, row.began, ok, why, row.say)
		}
	}
}
