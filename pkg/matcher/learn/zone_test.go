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
	if ok, why := CheckClockRead(wp, oldU, istHour, s, true); !ok {
		t.Errorf("the old file, replayed in IST: the IST hour refused (%s); want it read in the replay's zone, as before", why)
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
