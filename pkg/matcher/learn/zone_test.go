package learn

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		// Under root, the file the link names holds Berlin's rules: not
		// Asia/Kolkata's, so unknown, never keploy's own Asia/Kolkata nor
		// Berlin under Kolkata's name (TestAZoneFileWhoseRulesAreNotItsNamesIsUnknown).
		{"a link into zoneinfo, its file under root another zone's", map[string][]byte{zi + "Asia/Kolkata": berlin}, map[string]string{"etc/localtime": "/" + zi + "Asia/Kolkata"}, "", false, "!", 0},
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

// tzif is a zone file (version 1) of the offsets offs, in seconds east of
// UTC: offs[0] before at[0], offs[i] from at[i-1] on.
func tzif(offs []int, at []time.Time) []byte {
	var b []byte
	put := func(v int32) { b = append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	b = append(b, "TZif"...)
	b = append(b, make([]byte, 16)...)
	for _, n := range []int{0, 0, 0, len(at), len(offs), 2} {
		put(int32(n))
	}
	for _, t := range at {
		put(int32(t.Unix()))
	}
	for i := range at {
		b = append(b, byte(i+1))
	}
	for _, off := range offs {
		put(int32(off))
		b = append(b, 0, 0)
	}
	return append(b, "Z\x00"...)
}

// tzifRules is the zone file data's rules from 1901 to 2038 hold, written
// as a version 1 file (tzif): other bytes of the same rules there.
func tzifRules(t *testing.T, data []byte) []byte {
	t.Helper()
	loc, err := time.LoadLocationFromTZData("", data)
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Unix(-1<<31, 0), time.Unix(1<<31-1, 0)
	_, off := from.In(loc).Zone()
	offs, at := []int{off}, []time.Time(nil)
	for tt := from; ; {
		_, end := tt.In(loc).ZoneBounds()
		if end.IsZero() || !end.Before(to) {
			break
		}
		_, off := end.In(loc).Zone()
		offs, at, tt = append(offs, off), append(at, end), end
	}
	b := tzif(offs, at)
	if bytes.Equal(b, data) {
		t.Fatal("tzifRules wrote the bytes it read")
	}
	return b
}

// A zone file whose rules are not those of the zone its link or its
// /etc/timezone names is unknown, a lasting error that names both
// (coordinator's ruling, 2026-10-29 12:13): Node and a JVM read the zone
// by the name, the C library, Go and Python by the file's rules. A host's
// /etc/localtime bind-mounted over a Debian image's link to Etc/UTC gives
// that shape. A file that holds the name's rules from 1970 to 2038 is
// named, whatever it holds outside them; a change of offset either makes
// is read, so the first instant they differ at is the one named. A JVM
// reads the link's name (TestAJVMWithNoZoneOptionReadsItsZoneAsTheJDKDoes).
func TestAZoneFileWhoseRulesAreNotItsNamesIsUnknown(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile("/usr/share/zoneinfo/" + name)
		if err != nil {
			t.Skipf("no host zone files: %v", err)
		}
		return b
	}
	kolkata, utc, phoenix := read("Asia/Kolkata"), read("Etc/UTC"), read("America/Phoenix")
	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	zi := "usr/share/zoneinfo/"
	toUTC := map[string]string{"etc/localtime": "/" + zi + "Etc/UTC"}
	for _, row := range []struct {
		what  string
		files map[string][]byte
		links map[string]string
		why   string // a text the error holds; "" none
		name  string // the zone's name (loc.String()) where it is read
		off   int    // its offset at at
	}{
		{"a link to Etc/UTC whose file holds Kolkata's rules (a host's /etc/localtime bound over the image's link)", map[string][]byte{zi + "Etc/UTC": kolkata}, toUTC,
			`named "Etc/UTC" (by TZ, the /etc/localtime link or /etc/timezone) does not hold "Etc/UTC"'s rules: their offsets first differ on 1970-01-01T00:00:00Z, where it reads UTC+05:30 and "Etc/UTC" UTC+00:00. Node and a JVM read the zone by its name, the C library, Go and Python by the file's rules`, "", 0},
		{"a link to Etc/UTC whose file holds its rules", map[string][]byte{zi + "Etc/UTC": utc}, toUTC, "", "Etc/UTC", 0},
		{"a link to Asia/Kolkata whose file holds its rules", map[string][]byte{zi + "Asia/Kolkata": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Asia/Kolkata"}, "", "Asia/Kolkata", 19800},
		{"/etc/timezone names Etc/UTC, whose file under root is the copy's Kolkata rules", map[string][]byte{"etc/localtime": kolkata, "etc/timezone": []byte("Etc/UTC\n"), zi + "Etc/UTC": kolkata}, nil,
			`named "Etc/UTC" (by TZ, the /etc/localtime link or /etc/timezone) does not hold "Etc/UTC"'s rules: their offsets first differ on 1970-01-01T00:00:00Z, where it reads UTC+05:30`, "", 0},
		{"/etc/timezone names Etc/UTC over a copied file of Kolkata's rules, its Etc/UTC UTC's: no name", map[string][]byte{"etc/localtime": kolkata, "etc/timezone": []byte("Etc/UTC\n"), zi + "Etc/UTC": utc}, nil, "", "", 19800},
		{"a link to America/Denver whose file holds Phoenix's rules, which Denver's first change of offset parts", map[string][]byte{zi + "America/Denver": phoenix}, map[string]string{"etc/localtime": "/" + zi + "America/Denver"},
			`first differ on 1970-04-26T09:00:00Z, where it reads UTC-07:00 and "America/Denver" UTC-06:00`, "", 0},
		{"a link to Etc/UTC whose file changes offset in 2026", map[string][]byte{zi + "Etc/UTC": tzif([]int{0, 3600}, []time.Time{day(2026, 3, 1)})}, toUTC,
			`first differ on 2026-03-01T00:00:00Z, where it reads UTC+01:00 and "Etc/UTC" UTC+00:00`, "", 0},
		{"a link to Etc/UTC whose file differs before 1970 alone", map[string][]byte{zi + "Etc/UTC": tzif([]int{19800, 0}, []time.Time{day(1969, 12, 31)})}, toUTC, "", "Etc/UTC", 0},
		{"a link to Etc/UTC whose file differs from 2038 alone", map[string][]byte{zi + "Etc/UTC": tzif([]int{0, 3600}, []time.Time{day(2038, 1, 1)})}, toUTC, "", "Etc/UTC", 0},
		{"a link to Etc/UTC whose file holds an offset of seconds", map[string][]byte{zi + "Etc/UTC": tzif([]int{-2670}, nil)}, toUTC,
			`first differ on 1970-01-01T00:00:00Z, where it reads UTC-00:44:30 and "Etc/UTC" UTC+00:00`, "", 0},
		// A name keploy's zone data does not hold, or "Local" (keploy's own
		// zone): the file's rules, with no name.
		{"a link to a zone keploy's zone data does not hold", map[string][]byte{zi + "Mars/Olympus": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Mars/Olympus"}, "", "", 19800},
		{"a link to a file named Local", map[string][]byte{zi + "Local": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Local"}, "", "", 19800},
	} {
		r := t.TempDir()
		for name, data := range row.files {
			p := filepath.Join(r, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, target := range row.links {
			p := filepath.Join(r, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}
		loc, err := ReadZone(r, "", false)
		if row.why != "" {
			// Read twice: the second, of the same name and bytes, is unknown too.
			for i := 0; i < 2; i++ {
				if err == nil || loc != nil {
					t.Errorf("%s (read %d): %v %v; want unknown", row.what, i+1, loc, err)
				} else if !isLasting(err) || !strings.Contains(err.Error(), row.why) {
					t.Errorf("%s (read %d): lasting %v, %q; want lasting, holding %q", row.what, i+1, isLasting(err), err, row.why)
				}
				loc, err = ReadZone(r, "", false)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", row.what, err)
			continue
		}
		if _, off := at.In(loc).Zone(); loc.String() != row.name || off != row.off {
			t.Errorf("%s: %q at %d; want %q at %d", row.what, loc.String(), off, row.name, row.off)
		}
	}
}

// Where keploy's own file of a zone is another zone's, mounted over it
// (native mode in an application's container run with the host's
// /etc/localtime bind-mounted, which lands on the file the image's link
// names), keploy cannot read that zone by its name: Go would read the
// mounted zone's rules for it, which a JVM or Node reading the name in
// their own zone data do not. So each place below that reads a zone by
// its name refuses it, naming the mount, rather than read it in the
// mounted zone's rules: a JVM's user.timezone (javaZone) and a zone file
// named by TZ, the /etc/localtime link or /etc/timezone (loadZone), each
// an error lasting the process's life, whatever the file's bytes; and a
// replay's stored zone where keploy cannot read the application's own
// (CheckClockRead), which refuses the value, though the application wrote
// it in that zone. With nothing mounted, the same reads name the zone and
// the value passes. A directory and a mount table of the row's own stand
// in for keploy's zone files and /proc/self/mountinfo
// (models.SetOwnZoneFilesForTest).
func TestAZoneWhoseFileInKeploysOwnIsAnotherZonesIsRefusedByName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no mount table off Linux")
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "usr/share/zoneinfo")
	// The mount lines below are written by hand with tmp in them as is: a
	// TMPDIR holding whitespace, which mountinfo writes escaped (\040),
	// mis-writes them. A stated limit of these rows, not of keploy.
	if err := os.MkdirAll(filepath.Join(dir, "Europe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Europe/Berlin"), []byte("TZif"), 0o644); err != nil {
		t.Fatal(err)
	}
	table := filepath.Join(t.TempDir(), "mountinfo")
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	mount := func(mounts string) {
		if err := os.WriteFile(table, []byte(mounts), 0o644); err != nil {
			t.Fatal(err)
		}
		// Forgets the zones models.ZoneLocation read through the last table.
		t.Cleanup(models.SetOwnZoneFilesForTest(table, []string{dir + "/"}))
	}
	t.Setenv("ZONEINFO", "")
	// The bytes of a zone file named Europe/Berlin: refused before they are
	// read, so a host with no zone files gives the same refusal.
	data, _ := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin")
	const wp = "body.seenAt"
	o := models.ClockOffset{Res: time.Second, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "Europe/Berlin"}
	call := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	s := Span{From: call.Add(-10 * time.Millisecond), To: call, Zone: time.UTC}
	v := Leaf{Path: wp, Noise: wp, Type: String, Value: call.In(berlin).Format("2006-01-02 15:04:05")}

	mount(root + "2 1 8:1 /usr/share/zoneinfo/Asia/Kolkata " + dir + "/Europe/Berlin ro - xfs /dev/sda1 rw\n")
	const mounted = "which is a file mounted from /usr/share/zoneinfo/Asia/Kolkata, the zone file of \"Asia/Kolkata\""
	if loc, err := javaZone("Europe/Berlin"); loc != nil || !isLasting(err) || !strings.Contains(err.Error(), mounted) {
		t.Errorf("a JVM's user.timezone Europe/Berlin, keploy's own file of it Kolkata's: %v %v; want a lasting error naming the mount", loc, err)
	}
	if loc, err := loadZone("Europe/Berlin", data); loc != nil || !isLasting(err) || !strings.Contains(err.Error(), `cannot be checked against "Europe/Berlin"'s rules`) || !strings.Contains(err.Error(), mounted) {
		t.Errorf("a zone file named Europe/Berlin, keploy's own file of it Kolkata's: %v %v; want a lasting error naming the mount", loc, err)
	}
	if ok, why := CheckClockRead(wp, o, v, s, true); ok || !strings.Contains(why, "the application's zone the learn step read it in, but keploy's own file of that zone is another zone's here") || !strings.Contains(why, mounted) {
		t.Errorf("a wall clock learned in Europe/Berlin, %q, checked in the stored zone, keploy's own file of it Kolkata's: %v %q; want refused, naming the mount", v.Value, ok, why)
	}

	mount(root)
	if loc, err := javaZone("Europe/Berlin"); err != nil || loc.String() != "Europe/Berlin" {
		t.Errorf("a JVM's user.timezone Europe/Berlin, nothing mounted: %v %v; want Europe/Berlin", loc, err)
	}
	if data != nil {
		if loc, err := loadZone("Europe/Berlin", data); err != nil || loc.String() != "Europe/Berlin" {
			t.Errorf("a zone file named Europe/Berlin holding its rules, nothing mounted: %v %v; want Europe/Berlin", loc, err)
		}
	}
	if ok, why := CheckClockRead(wp, o, v, s, true); !ok {
		t.Errorf("a wall clock learned in Europe/Berlin, %q, checked in the stored zone, nothing mounted: %v %q; want passed", v.Value, ok, why)
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
	// pass, one an hour stale is refused by the reading in UTC. Learned in
	// New York (UTC-05:00), as far west of UTC as Kolkata is east: refused
	// whatever it is, the hour after too, naming the zone's offset.
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
		{"America/New_York", now, 0, wall, "", false, "only where the zone stands within an hour of UTC: America/New_York stood at UTC-05:00 at this call, " + whatever, ""},
		{"America/New_York", now, -time.Hour, wall, "", false, "America/New_York stood at UTC-05:00 at this call, " + whatever, ""},
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

	// Learned in New York, checked in the stored zone over a call's window
	// six hours long (06:00 to 12:00 in New York, at UTC-04:00): each
	// reading admits the span's hours and one on either side (05:00 to 13:00
	// read in New York, 09:00 to 17:00 read in UTC), so New York's hour at
	// the call and the three before it (12:00 to 09:00) lie in both, stale
	// or not; keploy refuses each, and the one four hours before, naming New
	// York's offset, rather than pass a stale one.
	nyFrom := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	nyLong := Span{From: nyFrom, To: nyFrom.Add(6 * time.Hour), Zone: time.UTC}
	nyHour := models.ClockOffset{Res: time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "America/New_York"}
	for back := 0; back <= 4; back++ {
		v := wall(ny, nyLong.To.Add(-time.Duration(back)*time.Hour))
		if ok, why := CheckClockRead(wp, nyHour, v, nyLong, true); ok || !strings.Contains(why, "America/New_York stood at UTC-04:00 at this call, "+whatever) {
			t.Errorf("an hour of no zone learned in New York, a six-hour span, the application's zone unread: %q, %d hours before the call: %v %q; want refused whatever it is, naming UTC-04:00", v.Value, back, ok, why)
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

// A read plus an offset cut to an hour or a day is a clock read too, its
// cut checked, never refused (founder decision, 2026-10-27; offsets,
// 2026-10-28), and read in the application's zone: a session the
// application expires a day on, cut to its IST hour (a $date of now plus a
// day, truncated to the hour in Asia/Kolkata, so a whole hour there and a
// half hour in UTC), record's full round across the IST hour's turn
// (17:59:59.05 and 18:01:00.6 IST), is learned as clock + 1d at the hour,
// naming the zone (it was refused for its cut). A replay checks it at the
// hour read in the application's zone, on 2,000 replays across ten days:
// in IST (its process's zone read), the read plus a day cut to the IST
// hour passes, and so do the widened span's two edges (an hour before the
// IST hour that holds the span's start plus the offset, and an hour after
// its end plus the offset), a millisecond past either never does, nor a
// day and three hours, three hours stale, or two days; read in UTC, the
// start's edge would move by half an hour and turn a millisecond past it
// into a pass, or the edge itself into a refusal. An application now
// writing in UTC (its process's zone read) passes its UTC hour. Where
// keploy cannot read the zone of the process, the stored IST is read, at
// the same edges; the far-hour refusal of a wall clock of no zone read in
// a stored zone never reaches it (the value names its moment: a zone moves
// only where its cut starts, within the one cut the check admits).
// Learned where keploy could not read the application's zone, the gap's
// hour is learned with no zone named, and a replay that cannot read it
// either refuses it, saying so; one that reads IST checks it there. (Cut to
// a day: TestAnOffsetCutToADayIsLearnedAndCheckedAtTheDay.)
func TestAnOffsetCutToAnHourInTheApplicationsZoneIsLearnedAndCheckedThere(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	const day = 24 * time.Hour
	ms := time.Millisecond
	cutIn := func(loc *time.Location, at time.Time, u time.Duration) time.Time {
		return models.ReadCut(at, u, loc).UTC()
	}
	learn := func(zone *time.Location, call time.Time, u, ttl, open time.Duration) Learned {
		t.Helper()
		dc := clockedDrifts()
		dc.zone = zone
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-open), To: call}, F: Span{From: fcall.Add(-open), To: fcall}}).ComparesOffsets()
		shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		return ClassifyIn(dc, []string{p}, shape(cutIn(ist, fcall, u).Add(ttl)), shape(cutIn(ist, call, u).Add(ttl)), call, nil, nil, nil, w)[0]
	}
	call := time.Date(2026, 10, 26, 12, 29, 59, 50e6, time.UTC)
	ln := learn(ist, call, time.Hour, day, 100*ms)
	o := ln.Offset
	if ln.Kind != KindClockOffset || o == nil || o.Res != time.Hour || o.AppZone != "+05:30" || o.Min > day || o.Max <= day || !strings.HasPrefix(ln.Why, p+" = clock + 1d (") {
		t.Fatalf("FALSE RED: a day on, cut to the IST hour, learned in IST: %s %+v (%s); want clock + 1d at the hour, naming +05:30", ln.Kind, o, ln.Why)
	}
	n := learn(nil, call, time.Hour, day, 100*ms)
	if n.Kind != KindClockOffset || n.Offset == nil || n.Offset.Res != time.Hour || n.Offset.AppZone != "" {
		t.Fatalf("a day on, cut to the IST hour, learned where keploy could not read the zone: %s %+v (%s); want clock + 1d at the hour, naming no zone", n.Kind, n.Offset, n.Why)
	}
	// A slow call's windows, 70 minutes: read in IST the values are whole
	// hours, learned at the hour; read in UTC alone they show a minute, at
	// which one offset holds both runs too, and their gap is a whole hour:
	// refused as ambiguous, naming both cuts.
	if slow := learn(ist, call, time.Hour, day, 70*time.Minute); slow.Kind != KindClockOffset || slow.Offset == nil || slow.Offset.Res != time.Hour || slow.Offset.AppZone != "+05:30" {
		t.Errorf("FALSE RED: a day on, cut to the IST hour, a slow call's windows, learned in IST: %s %+v (%s); want clock + 1d at the hour", slow.Kind, slow.Offset, slow.Why)
	}
	if slow := learn(nil, call, time.Hour, day, 70*time.Minute); slow.Kind != KindUnaccounted || !strings.Contains(slow.Why, "a whole number of 1h") {
		t.Errorf("a day on, cut to the IST hour, a slow call's windows, the zone unread: %s %+v (%s); want refused as ambiguous, naming the hour", slow.Kind, slow.Offset, slow.Why)
	}
	// So is a client test's (keploy mock), naming the two cuts and how to
	// correct it, with the client test's own correction added, never only
	// that no clock read matched it.
	{
		dc := clockedDrifts()
		dc.clientTests = true
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-70 * time.Minute), To: call}, F: Span{From: fcall.Add(-70 * time.Minute), To: fcall}}).ComparesOffsets()
		shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		c := ClassifyIn(dc, []string{p}, shape(cutIn(ist, fcall, time.Hour).Add(day)), shape(cutIn(ist, call, time.Hour).Add(day)), call, nil, nil, nil, w)[0]
		if c.Kind != KindUnaccounted || c.Offset != nil || !strings.Contains(c.Why, "a whole number of 1h") || !strings.HasSuffix(c.Why, ". If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it") || strings.Contains(c.Why, "no clock read by the app matched it") {
			t.Errorf("a client test's value a day on, cut to the IST hour, a slow call's windows, the zone unread: %s %+v (%s); want refused as ambiguous, naming the hour and how to correct it", c.Kind, c.Offset, c.Why)
		}
	}
	verdict := map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}
	rng := rand.New(rand.NewSource(11))
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		from := base.Add(time.Duration(rng.Int63n(int64(10 * day)))).Truncate(ms)
		to := from.Add(time.Duration(rng.Int63n(int64(10 * time.Minute)))).Truncate(ms)
		read := from.Add(time.Duration(rng.Int63n(int64(to.Sub(from)) + 1))).Truncate(ms)
		lo, hi := cutIn(ist, from.Add(o.Min), time.Hour).Add(-time.Hour), to.Add(o.Max).Add(time.Hour).Truncate(ms)
		v := cutIn(ist, read, time.Hour).Add(day)
		for _, sp := range []struct {
			name string
			s    Span
		}{
			{"its process's zone, IST", Span{From: from, To: to, Zone: ist, Own: true}},
			{"the stored IST (the process's zone unread, the agent's UTC)", Span{From: from, To: to, Zone: time.UTC}},
		} {
			for _, row := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"the read plus a day, cut to the IST hour", v, true},
				{"the widened span's start", lo, true},
				{"a millisecond before it", lo.Add(-ms), false},
				{"the widened span's end", hi, true},
				{"a millisecond after it", hi.Add(ms), false},
				{"a day and three hours", v.Add(3 * time.Hour), false},
				{"an hour stale", v.Add(-time.Hour), true},
				{"three hours stale", v.Add(-3 * time.Hour), false},
				{"two days", v.Add(day), false},
			} {
				if got, why := CheckClockRead(p, *o, InstantLeaf(p, p, row.at, ms), sp.s, true); got != row.want {
					t.Fatalf("%s: clock + 1d at the IST hour, read in %s, span %s to %s, %s (%s): holds %v (%s), want %v", verdict[row.want], sp.name, from, to, row.name, row.at, got, why, row.want)
				}
			}
		}
		if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, cutIn(time.UTC, read, time.Hour).Add(day), ms), Span{From: from, To: to, Zone: time.UTC, Own: true}, true); !ok {
			t.Fatalf("FALSE RED: clock + 1d learned in IST, an application now writing in UTC, its UTC hour plus a day at %s: refused (%s)", read, why)
		}
		if ok, why := CheckClockRead(p, *n.Offset, InstantLeaf(p, p, v, ms), Span{From: from, To: to, Zone: ist, Own: true}, true); !ok {
			t.Fatalf("FALSE RED: clock + 1d learned with no zone named, a replay that reads IST, at %s: refused (%s)", read, why)
		}
	}
	// A Go application in IST cuts to UTC's hour (Truncate counts from
	// UTC), a half hour in IST: learned in IST at the hour, naming IST, and
	// checked in UTC, where its value shows the hour whole. Its value an
	// hour stale passes wherever in the UTC hour the replay ran (at :20 and
	// at :40; read in IST it would pass at :20 and be refused at :40), and
	// one three hours stale never does.
	g := ClassifyIn(func() *DriftCache { dc := clockedDrifts(); dc.zone = ist; return dc }(), []string{p}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, call.Add(FullRoundLead).Add(30*time.Minute).Truncate(time.Hour).Add(day), ms)}}, Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, call.Add(30*time.Minute).Truncate(time.Hour).Add(day), ms)}}, call.Add(30*time.Minute), nil, nil, nil, (&Window{Rec: Span{From: call.Add(30*time.Minute - 100*ms), To: call.Add(30 * time.Minute)}, F: Span{From: call.Add(30*time.Minute + FullRoundLead - 100*ms), To: call.Add(30*time.Minute + FullRoundLead)}}).ComparesOffsets())[0]
	if g.Kind != KindClockOffset || g.Offset == nil || g.Offset.Res != time.Hour || g.Offset.AppZone != "+05:30" {
		t.Fatalf("FALSE RED: a day on, cut to UTC's hour by an application in IST: %s %+v (%s); want clock + 1d at the hour, naming +05:30", g.Kind, g.Offset, g.Why)
	}
	for _, past := range []time.Duration{20 * time.Minute, 40 * time.Minute} {
		for _, own := range []bool{true, false} {
			from := base.Add(10*time.Hour + past)
			s := Span{From: from, To: from.Add(time.Second), Zone: ist, Own: own}
			if !own {
				s.Zone = time.UTC
			}
			v := from.Truncate(time.Hour).Add(day)
			if ok, why := CheckClockRead(p, *g.Offset, InstantLeaf(p, p, v.Add(-time.Hour), ms), s, true); !ok {
				t.Errorf("FALSE RED: clock + 1d cut to UTC's hour in IST (its process's zone read: %v), an hour stale, replayed %s past the hour: refused (%s)", own, past, why)
			}
			if ok, _ := CheckClockRead(p, *g.Offset, InstantLeaf(p, p, v.Add(-3*time.Hour), ms), s, true); ok {
				t.Errorf("FALSE GREEN: clock + 1d cut to UTC's hour in IST (its process's zone read: %v), three hours stale, replayed %s past the hour: passed", own, past)
			}
		}
	}
	if ok, why := CheckClockRead(p, *n.Offset, InstantLeaf(p, p, cutIn(ist, base, time.Hour).Add(day), ms), Span{From: base, To: base.Add(time.Minute), Zone: time.UTC}, true); ok || !strings.Contains(why, "keploy could not read the zone of the process that made this call, and the learn step named none") {
		t.Errorf("clock + 1d at an hour learned with no zone named, a replay that cannot read the application's zone either: %v (%s); want refused, saying so", ok, why)
	}
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, cutIn(ist, base, time.Hour).Add(2*day), ms), Span{From: base, To: base.Add(time.Minute), Zone: ist, Own: true}, true); ok || !strings.Contains(why, "the learn step read it cut to, read here in Asia/Kolkata") {
		t.Errorf("clock + 1d at the IST hour, two days on: %v (%s); want refused, naming the cut and its zone", ok, why)
	}
}

// An offset cut to a day (founder decision, 2026-10-27: a cut is checked at
// the cut, never refused; ruled for a day's cut 2026-10-29): thirty days on,
// cut to the IST day, where record's full round crosses IST's midnight, is
// learned at the day naming +05:30, not refused for the three days no unit
// holds. A replay checks it at the IST day, within one day
// of its window plus the offset on either side, in its process's zone and
// in the stored one: the read plus thirty days passes, and so do the
// widened span's edges and the value one day stale or one day ahead (as
// yesterday's date passes a date cut to a day, a time to live of the read
// plus thirty days, cut down to the day, changed by one day is not caught;
// cut and then six hours added, one day stale is refused at a read after
// 18:00 IST, as the rows of its shape decide); a millisecond past either edge
// never does, nor does a value three days off; one two days off passes only
// where the replay's read falls within its window and the offset's range of
// the IST
// day's turn, and is refused, naming the day's cut and its zone, at a read
// away from it. A call whose windows, with the offset's range, are no
// narrower than the day is refused, at learn and at replay, saying so.
func TestAnOffsetCutToADayIsLearnedAndCheckedAtTheDay(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	const day = 24 * time.Hour
	ms := time.Millisecond
	cutIn := func(at time.Time) time.Time { return models.ReadCut(at, day, ist).UTC() }
	learn := func(zone *time.Location, call time.Time, ttl, open time.Duration) Learned {
		t.Helper()
		dc := clockedDrifts()
		dc.zone = zone
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-open), To: call}, F: Span{From: fcall.Add(-open), To: fcall}}).ComparesOffsets()
		shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		return ClassifyIn(dc, []string{p}, shape(cutIn(fcall).Add(ttl)), shape(cutIn(call).Add(ttl)), call, nil, nil, nil, w)[0]
	}
	call := time.Date(2026, 10, 26, 18, 29, 59, 50e6, time.UTC)
	ln := learn(ist, call, 30*day, 100*ms)
	o := ln.Offset
	if ln.Kind != KindClockOffset || o == nil || o.Res != day || o.AppZone != "+05:30" || o.Min > 30*day || o.Max <= 30*day || !strings.HasPrefix(ln.Why, p+" = clock + 30d (") || !strings.Contains(ln.Why, "cut to a day. A replay checks the offset at the day, read in the zone the value shows a whole day in, else the application's,") || !strings.Contains(ln.Why, "a time to live changed by less than two days can pass there, by where the replay's read falls in its day,") || !strings.Contains(ln.Why, "within its call's window and the offset's range ("+models.ShortDuration(o.Max-o.Min)+") of the day's turn") {
		t.Fatalf("FALSE RED: thirty days on, cut to the IST day, learned in IST: %s %+v (%s); want clock + 30d at the day, naming +05:30, not refused for the three days no unit holds", ln.Kind, o, ln.Why)
	}
	if w := learn(ist, call, 30*day, 25*time.Hour); w.Kind != KindUnaccounted || w.Offset != nil || !strings.Contains(w.Why, "the range is not narrower than the day the value is cut to") {
		t.Errorf("thirty days on, cut to the IST day, a call's windows 25 hours open: %s %+v (%s); want refused, its range no narrower than the day", w.Kind, w.Offset, w.Why)
	}
	// Thirty days and six hours on the IST day's start (06:00 IST, 00:30Z)
	// shows no whole day in any zone: learned at the day (the gap's), and a
	// replay reads its day in the application's zone, IST, never in UTC.
	sixLn := learn(ist, call, 30*day+6*time.Hour, 100*ms)
	if sixLn.Kind != KindClockOffset || sixLn.Offset == nil || sixLn.Offset.Res != day || sixLn.Offset.AppZone != "+05:30" || !strings.HasPrefix(sixLn.Why, p+" = clock + 726h (") {
		t.Fatalf("FALSE RED: thirty days and six hours on the IST day's start, learned in IST: %s %+v (%s); want clock + 726h at the day, naming +05:30", sixLn.Kind, sixLn.Offset, sixLn.Why)
	}
	verdict := map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}
	rng := rand.New(rand.NewSource(13))
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		o, ttl := o, 30*day
		if i%2 == 1 {
			o, ttl = sixLn.Offset, 30*day+6*time.Hour
		}
		from := base.Add(time.Duration(rng.Int63n(int64(10 * day)))).Truncate(ms)
		to := from.Add(time.Duration(rng.Int63n(int64(10 * time.Minute)))).Truncate(ms)
		read := from.Add(time.Duration(rng.Int63n(int64(to.Sub(from)) + 1))).Truncate(ms)
		lo, hi := cutIn(from.Add(o.Min)).Add(-day), to.Add(o.Max).Add(day).Truncate(ms)
		v := cutIn(read).Add(ttl)
		type row struct {
			name string
			at   time.Time
			want bool
		}
		rows := []row{
			{"the read plus thirty days, cut to the IST day", v, true},
			{"the widened span's start", lo, true},
			{"a millisecond before it", lo.Add(-ms), false},
			{"the widened span's end", hi, true},
			{"a millisecond after it", hi.Add(ms), false},
			{"one day stale", v.Add(-day), true},
			{"one day ahead", v.Add(day), true},
			{"two days stale", v.Add(-2 * day), !v.Add(-2 * day).Before(lo)},
			{"two days ahead", v.Add(2 * day), !v.Add(2 * day).After(hi)},
			{"three days stale", v.Add(-3 * day), false},
			{"three days ahead", v.Add(3 * day), false},
		}
		if ttl != 30*day {
			// Cut, then six hours added: the value is the read plus the
			// offset only to within the day it was cut by, and each value
			// of its shape holds where it lies within the widened span read
			// in IST (its day's start, less a day, up to its end plus a day).
			rows = []row{{"the read cut to the IST day, plus thirty days and six hours", v, true}}
			for k := -3; k <= 3; k++ {
				if at := v.Add(time.Duration(k) * day); k != 0 {
					rows = append(rows, row{strconv.Itoa(k) + " days", at, !at.Before(lo) && !at.After(hi)})
				}
			}
		}
		for _, sp := range []struct {
			name string
			s    Span
		}{
			{"its process's zone, IST", Span{From: from, To: to, Zone: ist, Own: true}},
			{"the stored IST (the process's zone unread, the agent's UTC)", Span{From: from, To: to, Zone: time.UTC}},
		} {
			for _, row := range rows {
				if got, why := CheckClockRead(p, *o, InstantLeaf(p, p, row.at, ms), sp.s, true); got != row.want {
					t.Fatalf("%s: clock + %s at the IST day, read in %s, span %s to %s, %s (%s): holds %v (%s), want %v", verdict[row.want], models.ShortDuration(ttl), sp.name, from, to, row.name, row.at, got, why, row.want)
				}
			}
		}
	}
	// At a replay read away from the IST day's turn (11:30 IST), one day
	// stale passes and two days off either way are refused, naming the cut.
	at := base.Add(6 * time.Hour)
	s := Span{From: at, To: at.Add(time.Second), Zone: ist, Own: true}
	v := cutIn(at).Add(30 * day)
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v.Add(-day), ms), s, true); !ok {
		t.Errorf("FALSE RED: clock + 30d at the IST day, one day stale, read at 11:30 IST: refused (%s)", why)
	}
	for _, off := range []time.Duration{-2 * day, 2 * day} {
		if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v.Add(off), ms), s, true); ok || !strings.Contains(why, "the learn step read it cut to, read here in Asia/Kolkata") {
			t.Errorf("FALSE GREEN: clock + 30d at the IST day, %s off, read at 11:30 IST: %v (%s); want refused, naming the day's cut and its zone", models.ShortDuration(off), ok, why)
		}
	}
	// A replay window that, with the offset's range, is no narrower than the
	// day holds nothing, saying why; one a nanosecond narrower is checked.
	widest := day - (o.Max - o.Min) - 1
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v, ms), Span{From: at, To: at.Add(widest), Zone: ist, Own: true}, true); !ok {
		t.Errorf("FALSE RED: clock + 30d at the IST day, a window just narrower than the day with the offset's range: refused (%s)", why)
	}
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v, ms), Span{From: at, To: at.Add(widest + 1), Zone: ist, Own: true}, true); ok || !strings.Contains(why, "with the offset's range is no narrower than the day, so keploy could not tell a time to live changed by days") {
		t.Errorf("clock + 30d at the IST day, a window that with the offset's range is the day: %v (%s); want refused, saying the window is too wide", ok, why)
	}
}

// An expiry two hours on, cut to the IST hour (Java's truncatedTo(HOURS) in
// IST, whose hour turns at half past UTC's), is learned as clock + 2h at the
// hour naming the application's zone (+05:30), though no unit holds three
// hours (founder decision, 2026-10-27: a cut is checked at the cut; ruled
// for an hour's cut that reads in hours 2026-10-29), and a replay checks it
// at the IST hour, within one hour of its window plus the offset on either
// side, in its process's zone and in the stored one: the read plus two
// hours passes, and so do the widened span's edges and the value one hour
// stale or ahead (the stated cost: a time to live of two hours changed by
// one is not caught); a millisecond past either edge never does, nor does a
// value three hours off; one two hours off passes only where the replay's
// read falls within its window and the offset's range of the IST hour's
// turn, and is refused, naming the hour's cut and its zone, at a read away
// from it. Cut and then twenty minutes added, the value shows no whole hour
// in any zone and is learned at the hour all the same (the gap's). A call
// whose windows, with the offset's range, are no narrower than the hour is
// refused, at learn and at replay, saying so.
func TestAnOffsetCutToAnHourThatReadsInHoursIsLearnedAndCheckedAtTheHour(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	const hour = time.Hour
	ms := time.Millisecond
	cutIn := func(at time.Time) time.Time { return models.ReadCut(at, hour, ist).UTC() }
	learn := func(call time.Time, ttl, open time.Duration) Learned {
		t.Helper()
		dc := clockedDrifts()
		dc.zone = ist
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-open), To: call}, F: Span{From: fcall.Add(-open), To: fcall}}).ComparesOffsets()
		shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		return ClassifyIn(dc, []string{p}, shape(cutIn(fcall).Add(ttl)), shape(cutIn(call).Add(ttl)), call, nil, nil, nil, w)[0]
	}
	// 15:59:59.05 IST: record's full round, a lead later, reads the next
	// IST hour.
	call := time.Date(2026, 10, 26, 10, 29, 59, 50e6, time.UTC)
	ln := learn(call, 2*hour, 100*ms)
	o := ln.Offset
	if ln.Kind != KindClockOffset || o == nil || o.Res != hour || o.AppZone != "+05:30" || o.Min > 2*hour || o.Max <= 2*hour || !strings.HasPrefix(ln.Why, p+" = clock + 2h (") || !strings.Contains(ln.Why, "cut to an hour. A replay checks the offset at the hour, read in the zone the value shows a whole hour in, else the application's,") || !strings.Contains(ln.Why, "a time to live changed by less than two hours can pass there, by where the replay's read falls in its hour,") || !strings.Contains(ln.Why, "falls in its hour, one changed by two hours fails unless") || !strings.Contains(ln.Why, "within its call's window and the offset's range ("+models.ShortDuration(o.Max-o.Min)+") of the hour's turn, and one changed by three hours or more fails, and near an hour the application's zone shows twice as its clocks go back a replay refuses the value (an application that cuts to that hour's first showing writes a value up to two hours early there); if the field") {
		t.Fatalf("FALSE RED: two hours on, cut to the IST hour, learned in IST: %s %+v (%s); want clock + 2h at the hour, naming +05:30, not refused for the three hours no unit holds", ln.Kind, o, ln.Why)
	}
	if w := learn(call, 2*hour, 61*time.Minute); w.Kind != KindUnaccounted || w.Offset != nil || !strings.Contains(w.Why, "the range is not narrower than the hour the value is cut to, so a replay checking it at the hour could not tell a time to live changed by hours from it") {
		t.Errorf("two hours on, cut to the IST hour, a call's windows 61 minutes open: %s %+v (%s); want refused, its range no narrower than the hour", w.Kind, w.Offset, w.Why)
	}
	// Two hours and twenty minutes on the IST hour's start (xx:50 UTC, xx:20
	// IST) shows no whole hour in any zone: learned at the hour (the gap's),
	// and a replay reads its hour in the application's zone, IST.
	twentyLn := learn(call, 2*hour+20*time.Minute, 100*ms)
	if twentyLn.Kind != KindClockOffset || twentyLn.Offset == nil || twentyLn.Offset.Res != hour || twentyLn.Offset.AppZone != "+05:30" || !twentyLn.Offset.CoarseCut() {
		t.Fatalf("FALSE RED: two hours and twenty minutes on the IST hour's start, learned in IST: %s %+v (%s); want learned at the hour, naming +05:30", twentyLn.Kind, twentyLn.Offset, twentyLn.Why)
	}
	verdict := map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}
	rng := rand.New(rand.NewSource(17))
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		o, ttl := o, 2*hour
		if i%2 == 1 {
			o, ttl = twentyLn.Offset, 2*hour+20*time.Minute
		}
		from := base.Add(time.Duration(rng.Int63n(int64(5 * 24 * hour)))).Truncate(ms)
		to := from.Add(time.Duration(rng.Int63n(int64(10 * time.Minute)))).Truncate(ms)
		read := from.Add(time.Duration(rng.Int63n(int64(to.Sub(from)) + 1))).Truncate(ms)
		lo, hi := cutIn(from.Add(o.Min)).Add(-hour), to.Add(o.Max).Add(hour).Truncate(ms)
		v := cutIn(read).Add(ttl)
		type row struct {
			name string
			at   time.Time
			want bool
		}
		var rows []row
		if ttl == 2*hour {
			rows = []row{
				{"the read plus two hours, cut to the IST hour", v, true},
				{"the widened span's start", lo, true},
				{"a millisecond before it", lo.Add(-ms), false},
				{"the widened span's end", hi, true},
				{"a millisecond after it", hi.Add(ms), false},
				{"one hour stale", v.Add(-hour), true},
				{"one hour ahead", v.Add(hour), true},
			}
		} else {
			rows = []row{{"the read cut to the IST hour, plus two hours and twenty minutes", v, true}}
		}
		// Each value of the shape, k hours off, holds where it lies within
		// the widened span read in IST; three hours off never does.
		for k := -3; k <= 3; k++ {
			if at := v.Add(time.Duration(k) * hour); k != 0 {
				rows = append(rows, row{strconv.Itoa(k) + " hours", at, !at.Before(lo) && !at.After(hi) && k*k < 9})
			}
		}
		for _, sp := range []struct {
			name string
			s    Span
		}{
			{"its process's zone, IST", Span{From: from, To: to, Zone: ist, Own: true}},
			{"the stored IST (the process's zone unread, the agent's UTC)", Span{From: from, To: to, Zone: time.UTC}},
		} {
			for _, row := range rows {
				if got, why := CheckClockRead(p, *o, InstantLeaf(p, p, row.at, ms), sp.s, true); got != row.want {
					t.Fatalf("%s: clock + %s at the IST hour, read in %s, span %s to %s, %s (%s): holds %v (%s), want %v", verdict[row.want], models.ShortDuration(ttl), sp.name, from, to, row.name, row.at, got, why, row.want)
				}
			}
		}
	}
	// At a replay read away from the IST hour's turn (10:30 IST), one hour
	// stale passes and two hours off either way are refused, naming the cut.
	at := base.Add(5 * hour)
	s := Span{From: at, To: at.Add(time.Second), Zone: ist, Own: true}
	v := cutIn(at).Add(2 * hour)
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v.Add(-hour), ms), s, true); !ok {
		t.Errorf("FALSE RED: clock + 2h at the IST hour, one hour stale, read at 10:30 IST: refused (%s)", why)
	}
	for _, off := range []time.Duration{-2 * hour, 2 * hour} {
		if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v.Add(off), ms), s, true); ok || !strings.Contains(why, "the learn step read it cut to, read here in Asia/Kolkata") {
			t.Errorf("FALSE GREEN: clock + 2h at the IST hour, %s off, read at 10:30 IST: %v (%s); want refused, naming the hour's cut and its zone", models.ShortDuration(off), ok, why)
		}
	}
	// A replay window that, with the offset's range, is no narrower than the
	// hour holds nothing, saying why; one a nanosecond narrower is checked.
	widest := hour - (o.Max - o.Min) - 1
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v, ms), Span{From: at, To: at.Add(widest), Zone: ist, Own: true}, true); !ok {
		t.Errorf("FALSE RED: clock + 2h at the IST hour, a window just narrower than the hour with the offset's range: refused (%s)", why)
	}
	if ok, why := CheckClockRead(p, *o, InstantLeaf(p, p, v, ms), Span{From: at, To: at.Add(widest + 1), Zone: ist, Own: true}, true); ok || !strings.Contains(why, "with the offset's range is no narrower than the hour, so keploy could not tell a time to live changed by hours") {
		t.Errorf("clock + 2h at the IST hour, a window that with the offset's range is the hour: %v (%s); want refused, saying the window is too wide", ok, why)
	}
}

// A client test's text that names its moment, a day after a read of the
// application's clock cut to its IST hour, written with its offset
// ("+05:30") or in UTC ("Z", a Java Instant of the IST hour), is learned as
// clock + 1d at the hour naming +05:30, in the text's layout, and a replay
// checks it at the IST hour in the process's zone and in the stored one:
// the widened span's edges hold and a second past either fails (a value
// off the hour is read in the application's zone, which sets the start),
// an hour stale passes, three hours stale fails. Met with a learning in another zone
// (models.AppZoneApart), it is checked in the zone read from the
// application's process, and refused, saying why, where none is read.
//
// textClockOffset reads the cut in the application's zone: a slow call's
// windows (70 minutes) show the UTC text's hour only read in IST (read in
// UTC alone it shows a minute, at which one offset holds both runs too,
// its gap a whole hour), so with the zone unread no offset is learned: the
// client test's refusal names the two cuts and how to correct it, never
// only that no clock read matched it. A quick call's gap is the hour (one
// offset holds both runs at no finer cut), learned naming no zone, as is
// the "+05:30" text, which shows its hour whole in the offset it writes,
// at either speed. A replay that cannot read the application's zone
// either refuses such a learning, saying so; one that reads IST from the
// process checks it there.
func TestAClientTestsTextOffsetCutToAnHourIsLearnedInTheApplicationsZone(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.docs.msgs.{}.expires_at"
	const day = 24 * time.Hour
	ms := time.Millisecond
	call := time.Date(2026, 10, 26, 12, 29, 59, 50e6, time.UTC)
	fcall := call.Add(FullRoundLead)
	inIST := func(at time.Time) string { return at.In(ist).Format(time.RFC3339) }
	inUTC := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	text := func(f func(time.Time) string, at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: f(at)}
	}
	learn := func(zone *time.Location, f func(time.Time) string, open time.Duration) Learned {
		t.Helper()
		w := (&Window{Rec: Span{From: call.Add(-open), To: call}, F: Span{From: fcall.Add(-open), To: fcall}}).ComparesOffsets()
		dc := clockedDrifts()
		dc.clientTests = true
		dc.zone = zone
		at := func(c time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: text(f, models.ReadCut(c, time.Hour, ist).Add(day))}}
		}
		return ClassifyIn(dc, []string{p}, at(fcall), at(call), call, nil, nil, nil, w)[0]
	}
	verdict := map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}
	from := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	to := from.Add(time.Second)
	v := models.ReadCut(from, time.Hour, ist).Add(day)
	inProcess := Span{From: from, To: to, Zone: ist, Own: true}
	for _, layout := range []struct {
		name string
		f    func(time.Time) string
	}{
		{"written with +05:30", inIST},
		{"written in UTC", inUTC},
	} {
		ln := learn(ist, layout.f, 100*ms)
		o := ln.Offset
		if ln.Kind != KindClockOffset || o == nil || o.Res != time.Hour || o.AppZone != "+05:30" || o.Min > day || o.Max <= day || o.Layout != clockTextLayout(layout.f(call)) {
			t.Fatalf("FALSE RED: a client test's text %s, a day on, cut to the IST hour, learned in IST: %s %+v (%s); want clock + 1d at the hour in the text's layout, naming +05:30", layout.name, ln.Kind, o, ln.Why)
		}
		lo, hi := models.ReadCut(from.Add(o.Min), time.Hour, ist).Add(-time.Hour), to.Add(o.Max).Add(time.Hour).Truncate(time.Second)
		for _, own := range []bool{true, false} {
			s := inProcess
			if !own {
				s = Span{From: from, To: to, Zone: time.UTC}
			}
			for _, row := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"the read plus a day, cut to the IST hour", v, true},
				{"the widened span's start", lo, true},
				{"a second before it", lo.Add(-time.Second), false},
				{"the widened span's end", hi, true},
				{"a second after it", hi.Add(time.Second), false},
				{"an hour stale", v.Add(-time.Hour), true},
				{"three hours stale", v.Add(-3 * time.Hour), false},
			} {
				if got, why := CheckClockRead(p, *o, text(layout.f, row.at), s, true); got != row.want {
					t.Errorf("%s: %s, clock + 1d at the IST hour (its process's zone read: %v), %s (%s): holds %v (%s), want %v", verdict[row.want], layout.name, own, row.name, layout.f(row.at), got, why, row.want)
				}
			}
		}
		apart := *o
		apart.AppZone = models.AppZoneApart
		if ok, why := CheckClockRead(p, apart, text(layout.f, v.Add(-time.Hour)), inProcess, true); !ok {
			t.Errorf("FALSE RED: %s, clock + 1d at the hour learned in two zones, a replay that reads IST from the process, an hour stale: refused (%s)", layout.name, why)
		}
		if ok, _ := CheckClockRead(p, apart, text(layout.f, v.Add(-3*time.Hour)), inProcess, true); ok {
			t.Errorf("FALSE GREEN: %s, clock + 1d at the hour learned in two zones, a replay that reads IST from the process, three hours stale: passed", layout.name)
		}
		if ok, why := CheckClockRead(p, apart, text(layout.f, v), Span{From: from, To: to, Zone: time.UTC}, true); ok || !strings.Contains(why, "the learn step read the application's zone as no one zone") {
			t.Errorf("%s, clock + 1d at the hour learned in two zones, a replay that cannot read the process's zone: %v (%s); want refused, saying so", layout.name, ok, why)
		}
	}
	// The zone guard: only read in IST does a slow call's UTC text show its
	// hour.
	if slow := learn(ist, inUTC, 70*time.Minute); slow.Kind != KindClockOffset || slow.Offset == nil || slow.Offset.Res != time.Hour || slow.Offset.AppZone != "+05:30" {
		t.Fatalf("FALSE RED: a client test's text written in UTC, a slow call's windows, learned in IST: %s %+v (%s); want clock + 1d at the IST hour, naming +05:30", slow.Kind, slow.Offset, slow.Why)
	}
	if slow := learn(nil, inUTC, 70*time.Minute); slow.Kind != KindUnaccounted || slow.Offset != nil || !strings.HasPrefix(slow.Why, p+" = clock + 1d") || !strings.Contains(slow.Why, "stand 1h apart, a whole number of 1h") || !strings.Contains(slow.Why, "keploy cannot tell at which cut a replay must check it, so it refuses it. Record the test again") || !strings.HasSuffix(slow.Why, ". If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it") || strings.Contains(slow.Why, "no clock read by the app matched it") {
		t.Errorf("FALSE GREEN: a client test's text written in UTC, a slow call's windows, the zone unread: %s %+v (%s); want no offset learned, refused naming the two cuts and how to correct it", slow.Kind, slow.Offset, slow.Why)
	}
	for _, row := range []struct {
		name string
		f    func(time.Time) string
		open time.Duration
	}{
		{"written in UTC, a quick call", inUTC, 100 * ms},
		{"written with +05:30, a quick call", inIST, 100 * ms},
		{"written with +05:30, a slow call", inIST, 70 * time.Minute},
	} {
		n := learn(nil, row.f, row.open)
		o := n.Offset
		if n.Kind != KindClockOffset || o == nil || o.Res != time.Hour || o.AppZone != "" || o.Min > day || o.Max <= day || o.Layout != clockTextLayout(row.f(call)) {
			t.Fatalf("FALSE RED: a client test's text %s, the zone unread: %s %+v (%s); want clock + 1d at the hour in the text's layout, naming no zone", row.name, n.Kind, o, n.Why)
		}
		if ok, why := CheckClockRead(p, *o, text(row.f, v), Span{From: from, To: to, Zone: time.UTC}, true); ok || !strings.Contains(why, "the learn step named none") {
			t.Errorf("a client test's text %s, learned with no zone named, a replay that cannot read the application's zone either: %v (%s); want refused, saying so", row.name, ok, why)
		}
		if ok, why := CheckClockRead(p, *o, text(row.f, v.Add(-time.Hour)), inProcess, true); !ok {
			t.Errorf("FALSE RED: a client test's text %s, learned with no zone named, a replay that reads IST from the process, an hour stale: refused (%s)", row.name, why)
		}
		if ok, _ := CheckClockRead(p, *o, text(row.f, v.Add(-3*time.Hour)), inProcess, true); ok {
			t.Errorf("FALSE GREEN: a client test's text %s, learned with no zone named, a replay that reads IST from the process, three hours stale: passed", row.name)
		}
	}
}

// An application in Europe/Berlin that cuts its clock to the hour's first
// showing (JavaScript's setMinutes(0, 0, 0)) and then adds two hours writes,
// in the hour its clocks go back (2026-10-25, 03:00 CEST to 02:00 CET), a
// value an hour earlier than at any other read. The learn step learns the
// right offset at every hour of the day, the turn's too, or none where the
// two runs show one value (both in the hour shown twice, which shows no
// read of the clock) or no one offset (one in it): never a wrong one, so it
// refuses none for the turn. A replay of the offset learned elsewhere, the value
// read in UTC (where it shows its hour whole), in the process's zone and in
// the stored one, refuses a value near that hour, naming the turn (without
// the refusal one two hours longer passes at every read in the second
// showing); away from it the right value passes, one two hours off passes
// only near the hour's turn, and one three hours off never does.
func TestAnOffsetCutToAnHourIsRefusedNearTheHourTheClocksGoBack(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	first := func(x time.Time) time.Time {
		l := x.In(ber)
		c := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, ber)
		if e := c.Add(-hour).In(ber); e.Hour() == c.In(ber).Hour() && e.Day() == c.In(ber).Day() {
			c = c.Add(-hour)
		}
		return c.UTC()
	}
	learn := func(call time.Time) Learned {
		t.Helper()
		dc := clockedDrifts()
		dc.zone = ber
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
		shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		return ClassifyIn(dc, []string{p}, shape(first(fcall).Add(2*hour)), shape(first(call).Add(2*hour)), call, nil, nil, nil, w)[0]
	}
	const named = "meets the hour Europe/Berlin shows twice as its clocks go back from 03:00 CEST to 02:00 CET on 2026-10-25: an application that resolves a wall time shown twice to its first showing"
	var o *models.ClockOffset
	learnedNear := 0
	// At hh:59:59.05 the full round, a lead later, reads the next hour.
	for call := turn.Add(-12*hour - 950*ms); call.Before(turn.Add(11 * hour)); call = call.Add(hour) {
		ln := learn(call)
		near := !call.Before(turn.Add(-4*hour-time.Second)) && call.Before(turn.Add(hour))
		if ln.Kind == KindClockOffset && ln.Offset != nil {
			if ln.Offset.Min > 2*hour || ln.Offset.Max < 2*hour || ln.Offset.Res != hour || ln.Offset.AppZone != "Europe/Berlin" {
				t.Errorf("FALSE GREEN: clock + 2h at the Berlin hour, learned at %s, %s from the turn: %+v; want clock + 2h at the hour, naming Europe/Berlin", call, call.Sub(turn), ln.Offset)
			} else if o == nil && call.After(turn) {
				o = ln.Offset
			}
			if near {
				learnedNear++
			}
			continue
		}
		if !near || strings.Contains(ln.Why, "clocks go back") {
			t.Errorf("FALSE RED: clock + 2h at the Berlin hour, learned at %s, %s from the turn: %s (%s); want learned, or near the turn none for one value or no one offset, never refused for the turn", call, call.Sub(turn), ln.Kind, ln.Why)
		}
	}
	if o == nil || learnedNear < 3 {
		t.Fatalf("learned after the turn: %+v; learned near the turn: %d, want 3 or more", o, learnedNear)
	}
	spans := []struct {
		name string
		s    func(from, to time.Time) Span
	}{
		{"its process's zone, Berlin", func(from, to time.Time) Span { return Span{From: from, To: to, Zone: ber, Own: true} }},
		{"the stored Berlin (the process's zone unread, the agent's UTC)", func(from, to time.Time) Span { return Span{From: from, To: to, Zone: time.UTC} }},
	}
	for _, sp := range spans {
		type key struct {
			k      time.Duration
			bucket int
		}
		far := map[key]int{}
		for from := turn.Add(-8 * hour); from.Before(turn.Add(5 * hour)); from = from.Add(2 * time.Minute) {
			s := sp.s(from, from.Add(time.Second))
			check := func(ttl time.Duration) (bool, string) {
				return CheckClockRead(p, *o, InstantLeaf(p, p, first(from).Add(ttl), ms), s, true)
			}
			ok, why := check(2 * hour)
			near := !from.Before(turn.Add(-4*hour)) && from.Before(turn.Add(2*hour))
			switch {
			case !ok && (!near || !strings.Contains(why, named)):
				t.Errorf("FALSE RED: clock + 2h at the Berlin hour, read in %s at %s, %s from the turn: refused (%s); want passed, or near the turn refused naming it", sp.name, from, from.Sub(turn), why)
			case ok && !from.Before(turn.Add(-hour)) && from.Before(turn.Add(hour)):
				t.Errorf("FALSE GREEN: clock + 2h at the Berlin hour, read in %s at %s, in the hour shown twice: passed; want refused, naming the turn", sp.name, from)
			}
			for _, k := range []time.Duration{2 * hour, -2 * hour, 3 * hour, -3 * hour} {
				if ok, _ := check(2*hour + k); ok {
					far[key{k, int(from.Sub(turn.Add(-8*hour)) / hour)}]++
					if k == 3*hour || k == -3*hour {
						t.Errorf("FALSE GREEN: clock + 2h at the Berlin hour, read in %s at %s: a time to live %s off passed", sp.name, from, k)
					}
				}
			}
		}
		for kb, n := range far {
			if n > 2 {
				t.Errorf("FALSE GREEN: clock + 2h at the Berlin hour, read in %s: a time to live %s off passed at %d of the 30 reads in the hour %d hours after %s; want only near the hour's turn", sp.name, kb.k, n, kb.bucket, turn.Add(-8*hour))
			}
		}
	}
}

// firstShowingIn is x's wall clock in loc cut to c, resolved to the first
// instant loc shows that wall time at, as JavaScript's Date setters
// (setMilliseconds(0), setSeconds(0, 0)) and java.time's LocalDateTime
// then atZone (c of a millisecond: no cut) resolve a wall time shown twice:
// in the second showing of the hour Berlin's clocks go back, an hour before
// x.
func firstShowingIn(x time.Time, c time.Duration, loc *time.Location) time.Time {
	_, off := x.In(loc).Zone()
	wall := x.Add(time.Duration(off) * time.Second).UTC().Truncate(c)
	first := wall.Add(-time.Duration(off) * time.Second)
	for _, d := range []time.Duration{30 * time.Minute, time.Hour, 90 * time.Minute, 2 * time.Hour} {
		e := first.Add(-d)
		if _, eo := e.In(loc).Zone(); e.Add(time.Duration(eo) * time.Second).UTC().Equal(wall) {
			first = e
		}
	}
	return first.UTC()
}

// An application in Europe/Berlin that resolves a wall time shown twice to
// its first showing, cutting its clock to the second or the minute, or not
// at all (java.time's LocalDateTime then atZone), before adding two hours
// or after, writes in the hour its clocks go back (2026-10-25, 03:00 CEST
// to 02:00 CET) a value an hour earlier than at any other read.
//   - A replay of the offset learned on another day, read in the process's
//     zone, in the stored one, and with no zone known (any zone's turns),
//     refuses a value near that hour, naming the turn: without the refusal
//     a time to live an hour longer passes at every read there. Away from
//     it the right value passes, and one an hour longer never does.
//   - The learn step refuses an offset learned there, naming the turn (with
//     no zone known, a zone that turns then): without the refusal both runs
//     in the second showing show clock + 1h of clock + 2h, and a replay
//     would pass a time to live an hour shorter on any other day.
func TestAReadPlusAnOffsetIsRefusedNearAWallTimeShownTwiceAtAnyCut(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	const named = "meets the hour Europe/Berlin shows twice as its clocks go back from 03:00 CEST to 02:00 CET on 2026-10-25: an application that resolves a wall time shown twice to its first showing"
	const anyZone = "keploy knows no zone of the application's"
	for _, row := range []struct {
		name, shape string
		cut         time.Duration
	}{
		{"cut to the second", "cut, then add", time.Second},
		{"cut to the second", "add, then cut", time.Second},
		{"cut to the minute", "cut, then add", time.Minute},
		{"cut to the minute", "add, then cut", time.Minute},
		{"not cut (LocalDateTime then atZone)", "cut, then add", ms},
	} {
		val := func(read time.Time, ttl time.Duration) time.Time {
			if row.shape == "cut, then add" {
				return firstShowingIn(read, row.cut, ber).Add(ttl)
			}
			return firstShowingIn(read.Add(ttl), row.cut, ber)
		}
		learn := func(call time.Time, zone *time.Location) Learned {
			t.Helper()
			dc := clockedDrifts()
			dc.zone = zone
			fcall := call.Add(FullRoundLead)
			w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
			shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
			return ClassifyIn(dc, []string{p}, shape(val(fcall.Add(-30*ms), 2*hour)), shape(val(call.Add(-30*ms), 2*hour)), call, nil, nil, nil, w)[0]
		}
		for _, z := range []struct {
			name string
			loc  *time.Location
			want string
		}{{"Europe/Berlin", ber, named}, {"no zone", nil, anyZone}} {
			for _, call := range []time.Time{turn.Add(10*time.Minute + 7300*ms), turn.Add(40*time.Minute + 7300*ms)} {
				if ln := learn(call, z.loc); ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, z.want) || !strings.Contains(ln.Why, "keploy refuses it rather than learn it") {
					t.Errorf("FALSE GREEN: %s, %s, learned with %s at %s, in the second showing: %s %+v (%.300s); want refused, naming the turn", row.name, row.shape, z.name, call, ln.Kind, ln.Offset, ln.Why)
				}
			}
		}
		spans := []struct {
			name string
			o    models.ClockOffset
			s    func(from, to time.Time) Span
			want string
		}{}
		for _, z := range []struct {
			name string
			loc  *time.Location
		}{{"Europe/Berlin", ber}, {"no zone", nil}} {
			ln := learn(time.Date(2026, 10, 26, 10, 0, 7, 300e6, time.UTC), z.loc)
			if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.Min > 2*hour || ln.Offset.Max < 2*hour || ln.Offset.AppZone != map[bool]string{true: "Europe/Berlin", false: ""}[z.loc != nil] {
				t.Fatalf("FALSE RED: %s, %s, learned with %s on 2026-10-26: %s %+v (%s); want clock + 2h naming the zone learned in", row.name, row.shape, z.name, ln.Kind, ln.Offset, ln.Why)
			}
			o := *ln.Offset
			if z.loc != nil {
				spans = append(spans,
					struct {
						name string
						o    models.ClockOffset
						s    func(from, to time.Time) Span
						want string
					}{"its process's zone, Berlin", o, func(from, to time.Time) Span { return Span{From: from, To: to, Zone: ber, Own: true} }, named},
					struct {
						name string
						o    models.ClockOffset
						s    func(from, to time.Time) Span
						want string
					}{"the stored Berlin (the process's zone unread, the agent's UTC)", o, func(from, to time.Time) Span { return Span{From: from, To: to, Zone: time.UTC} }, named})
				continue
			}
			spans = append(spans, struct {
				name string
				o    models.ClockOffset
				s    func(from, to time.Time) Span
				want string
			}{"no zone known (learned with none, the process's unread, the agent's UTC)", o, func(from, to time.Time) Span { return Span{From: from, To: to, Zone: time.UTC} }, anyZone})
		}
		for _, sp := range spans {
			second := 0
			for from := turn.Add(-5 * hour); from.Before(turn.Add(5 * hour)); from = from.Add(37 * time.Second) {
				s := sp.s(from, from.Add(time.Second))
				read := from.Add(500 * ms)
				x := read
				if row.shape == "add, then cut" {
					x = read.Add(3 * hour)
				}
				longer, why := CheckClockRead(p, sp.o, InstantLeaf(p, p, val(read, 3*hour), ms), s, true)
				if !x.Before(turn) && x.Before(turn.Add(hour)) {
					second++
					if longer || !strings.Contains(why, sp.want) {
						t.Errorf("FALSE GREEN: %s, %s, read in %s at %s, in the second showing: a time to live an hour longer: %v (%.300s); want refused, naming the turn", row.name, row.shape, sp.name, read, longer, why)
					}
				} else if longer {
					t.Errorf("FALSE GREEN: %s, %s, read in %s at %s: a time to live an hour longer passed", row.name, row.shape, sp.name, read)
				}
				// With no zone known, every zone's turns count: Beirut's and
				// Jerusalem's the evening before Berlin's, so only the hours
				// after Berlin's are far from all of them.
				if far := from.Before(turn.Add(-4*hour)) && sp.want != anyZone || !from.Before(turn.Add(2*hour)); far {
					if ok, why := CheckClockRead(p, sp.o, InstantLeaf(p, p, val(read, 2*hour), ms), s, true); !ok {
						t.Errorf("FALSE RED: %s, %s, read in %s at %s, %s from the turn: the right value refused (%s)", row.name, row.shape, sp.name, read, from.Sub(turn), why)
					}
				}
			}
			if second < 90 {
				t.Fatalf("%s, %s, %s: %d reads in the second showing; want 90 or more", row.name, row.shape, sp.name, second)
			}
		}
	}
}

// The learn step's refusal below an hour's cut, by its outcome: with both
// of its runs in the hour Berlin shows twice as its clocks go back
// (2026-10-25), an application that resolves the wall time to its first
// showing writes, of clock + 2h, a value an hour early in each run, so the
// runs agree on clock + 1h. Cut to the second or the minute, or not cut,
// whatever the learn step takes there, a replay on another day, in the
// process's zone or with no zone known, never passes a time to live an
// hour shorter: without the refusal it learns clock + 1h, and that replay
// passes it at every read. Learned the day after, the same replay passes
// the right value at every read and the shorter one at none. (An hour's
// cut learns there, clock + 2h or none: its own row above.)
func TestAnOffsetLearnedInAWallTimeShownTwicePassesNoShorterTimeToLiveElsewhere(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name, shape string
		cut         time.Duration
	}{
		{"cut to the second", "cut, then add", time.Second},
		{"cut to the second", "add, then cut", time.Second},
		{"cut to the minute", "cut, then add", time.Minute},
		{"cut to the minute", "add, then cut", time.Minute},
		{"not cut (LocalDateTime then atZone)", "cut, then add", ms},
	} {
		val := func(read time.Time, ttl time.Duration) time.Time {
			if row.shape == "cut, then add" {
				return firstShowingIn(read, row.cut, ber).Add(ttl)
			}
			return firstShowingIn(read.Add(ttl), row.cut, ber)
		}
		// replay counts the reads on day, in Berlin as the process's zone
		// and with no zone read, that pass o's value of a time to live ttl.
		replay := func(o models.ClockOffset, ttl time.Duration) (passed, reads int) {
			for from := day; from.Before(day.Add(24 * hour)); from = from.Add(17 * time.Minute) {
				read := from.Add(500 * ms)
				for _, s := range []Span{{From: from, To: from.Add(time.Second), Zone: ber, Own: true}, {From: from, To: from.Add(time.Second), Zone: time.UTC}} {
					reads++
					if ok, _ := CheckClockRead(p, o, InstantLeaf(p, p, val(read, ttl), ms), s, true); ok {
						passed++
					}
				}
			}
			return passed, reads
		}
		for _, z := range []struct {
			name string
			loc  *time.Location
		}{{"Europe/Berlin", ber}, {"no zone", nil}} {
			learn := func(call time.Time) Learned {
				dc := clockedDrifts()
				dc.zone = z.loc
				fcall := call.Add(FullRoundLead)
				w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
				shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
				return ClassifyIn(dc, []string{p}, shape(val(fcall.Add(-30*ms), 2*hour)), shape(val(call.Add(-30*ms), 2*hour)), call, nil, nil, nil, w)[0]
			}
			after := time.Date(2026, 10, 26, 10, 0, 7, 300e6, time.UTC)
			ln := learn(after)
			if ln.Offset == nil {
				t.Fatalf("FALSE RED: %s, %s, learned with %s at %s: %s (%s); want clock + 2h", row.name, row.shape, z.name, after, ln.Kind, ln.Why)
			}
			if right, reads := replay(*ln.Offset, 2*hour); right != reads {
				t.Errorf("FALSE RED: %s, %s, learned with %s at %s: %+v; a replay on %s passed the right value at %d of %d reads", row.name, row.shape, z.name, after, ln.Offset, day.Format("2006-01-02"), right, reads)
			}
			if short, reads := replay(*ln.Offset, hour); short != 0 {
				t.Errorf("FALSE GREEN: %s, %s, learned with %s at %s: %+v; a replay on %s passed a time to live an hour shorter at %d of %d reads", row.name, row.shape, z.name, after, ln.Offset, day.Format("2006-01-02"), short, reads)
			}
			for _, call := range []time.Time{turn.Add(10*time.Minute + 7300*ms), turn.Add(40*time.Minute + 7300*ms)} {
				ln := learn(call)
				if ln.Offset == nil {
					continue
				}
				if short, reads := replay(*ln.Offset, hour); short != 0 {
					t.Errorf("FALSE GREEN: %s, %s, learned with %s at %s, both runs in the second showing: %s %+v; a replay on %s passed a time to live an hour shorter at %d of %d reads", row.name, row.shape, z.name, call, ln.Kind, ln.Offset, day.Format("2006-01-02"), short, reads)
				}
			}
		}
	}
}

// An application that resolves a wall time shown twice to its first
// showing and cuts its clock to the hour, before adding its time to live or
// after, near a turn whose wall time shown twice holds the start of one of
// its zone's wall hours strictly inside it.
//   - Pacific/Chatham's clocks go back from 03:45 +1345 to 02:45 +1245
//     (2026-04-05, 14:00 UTC the day before), and 02:45 to 03:45 holds
//     03:00. A recording at 02:59 in the second showing cuts to 02:00,
//     shown once, and writes the right value; its full round, a lead later,
//     reads 03:00 in the second showing and writes 03:00's first showing,
//     an hour early, so of a time to live of 2h the two show clock + 1h.
//   - Antarctica/Troll's clocks go back from 03:00 +02 to 01:00 +00
//     (2026-10-25, 01:00 UTC), and 01:00 to 03:00 holds 02:00, with 01:00
//     shown twice too. A recording at 01:59 in the second showing writes
//     01:00's first showing and its full round 02:00's, both two hours
//     early, so of a time to live of 3h the two show one offset, clock + 1h.
//
// The learn step refuses an hour's cut near such a turn, naming it, the
// hour it holds and why the runs there stand from the application's
// offset, with the zone known and with no zone known (any zone's such
// turn): without the refusal it learns clock + 1h there, and a replay on
// another day passes a time to live two hours shorter at every read. Away
// from the turn it learns the time to live, and a replay of that passes
// the shorter one only near the cut's own turn. (Berlin's wall time shown
// twice is one whole hour: its hour's cut learns near its turn, its own
// row above.)
func TestAnOffsetCutToAnHourIsRefusedNearATurnWhoseWallTimeShownTwiceHoldsAnHoursStart(t *testing.T) {
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	legs := []struct {
		zone          string
		turn          time.Time
		before, after int
		// held is the instant the hour the turn holds starts at in the
		// second showing; ttl the application's time to live.
		held, ttl time.Duration
		// near is where a refusal for the turn may fall, from the turn;
		// from and to the calls swept.
		near     [2]time.Duration
		from, to time.Duration
		day      time.Time
		want     []string
	}{
		{
			zone: "Pacific/Chatham", turn: time.Date(2026, 4, 4, 14, 0, 0, 0, time.UTC),
			before: 13*3600 + 45*60, after: 12*3600 + 45*60, held: 15 * time.Minute, ttl: 2 * hour,
			near: [2]time.Duration{-5 * hour, 2 * hour}, from: -6 * hour, to: 6 * hour,
			day: time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC),
			want: []string{
				"shows twice as its clocks go back from 03:45 +1345 to 02:45 +1245 on 2026-04-05, which holds the start of the wall hour 03:00",
				"a run in the second showing before 03:00 writes the hour before it, shown once, and so the right value, and one after it 03:00's first showing, an hour early",
				"writes a value an hour early there",
			},
		},
		{
			zone: "Antarctica/Troll", turn: time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
			before: 2 * 3600, after: 0, held: hour, ttl: 3 * hour,
			near: [2]time.Duration{-7 * hour, 2*hour + time.Minute}, from: -10 * hour, to: 6 * hour,
			day: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC),
			want: []string{
				"the 2h Antarctica/Troll shows twice as its clocks go back from 03:00 +02 to 01:00 +00 on 2026-10-25, which holds the start of the wall hour 02:00",
				"a run in the second showing before 02:00 writes 01:00's first showing, shown twice too, and one after it 02:00's, so both are two hours early",
				"writes a value two hours early there",
			},
		},
	}
	for _, leg := range legs {
		zl, err := time.LoadLocation(leg.zone)
		if err != nil {
			t.Fatal(err)
		}
		turn := leg.turn
		if _, b := turn.Add(-time.Nanosecond).In(zl).Zone(); b != leg.before {
			t.Fatalf("%s before %s: offset %ds, want %ds", leg.zone, turn, b, leg.before)
		}
		if _, a := turn.In(zl).Zone(); a != leg.after {
			t.Fatalf("%s at %s: offset %ds, want %ds", leg.zone, turn, a, leg.after)
		}
		if h := turn.Add(leg.held).In(zl); h.Minute() != 0 || h.Second() != 0 {
			t.Fatalf("%s: the hour held starts at %s in the second showing, not on the hour", leg.zone, h)
		}
		for _, shape := range []string{"cut-then-add", "add-then-cut"} {
			val := func(r time.Time, ttl time.Duration) time.Time {
				if shape == "cut-then-add" {
					return firstShowingIn(r, hour, zl).Add(ttl)
				}
				return firstShowingIn(r.Add(ttl), hour, zl)
			}
			// The straddle: the recording at a wall time 61 s to 1 s
			// before the hour held, in the second showing, whose full round
			// reads it or later there; read there by the call itself
			// (cut-then-add) or by the call plus its time to live
			// (add-then-cut).
			straddle := func(call time.Time) bool {
				at := call
				if shape == "add-then-cut" {
					at = call.Add(leg.ttl)
				}
				h := turn.Add(leg.held)
				return !at.Before(h.Add(-61*time.Second)) && !at.After(h.Add(-time.Second))
			}
			// shorter is how many of a day's reads a replay on another day,
			// in the process's zone, passes a time to live two hours shorter at.
			shorter := func(o models.ClockOffset) (int, int) {
				pass, n := 0, 0
				for from := leg.day; from.Before(leg.day.Add(24 * hour)); from = from.Add(7 * time.Minute) {
					n++
					s := Span{From: from, To: from.Add(time.Second), Zone: zl, Own: true}
					if ok, _ := CheckClockRead(p, o, InstantLeaf(p, p, val(from.Add(500*ms), leg.ttl-2*hour), ms), s, true); ok {
						pass++
					}
				}
				return pass, n
			}
			for _, lz := range []*time.Location{zl, nil} {
				name := shape + ", the learn step in " + leg.zone
				if lz == nil {
					name = shape + ", the learn step knowing no zone, an application in " + leg.zone
				}
				learned, refused, straddled := 0, 0, 0
				for call := turn.Add(leg.from - 950*ms); call.Before(turn.Add(leg.to)); call = call.Add(13*time.Second + 17*ms) {
					dc := clockedDrifts()
					dc.zone = lz
					fcall := call.Add(FullRoundLead)
					w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
					leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
					ln := ClassifyIn(dc, []string{p}, leaf(val(fcall.Add(-30*ms), leg.ttl)), leaf(val(call.Add(-30*ms), leg.ttl)), call, nil, nil, nil, w)[0]
					near := !call.Before(turn.Add(leg.near[0])) && call.Before(turn.Add(leg.near[1]))
					if straddle(call) {
						straddled++
					}
					if ln.Kind == KindClockOffset && ln.Offset != nil {
						o := *ln.Offset
						if o.Min > leg.ttl+time.Minute || o.Max < leg.ttl-2*time.Minute || o.Res != hour {
							t.Errorf("FALSE GREEN: %s: learned at %s (local %s), %s from the turn: %s; want clock + %s at the hour, or refused naming the turn", name, call, call.In(zl).Format("15:04:05 MST"), call.Sub(turn), o.String(), models.ShortDuration(leg.ttl))
						} else if straddle(call) {
							t.Errorf("FALSE GREEN: %s: learned at %s (local %s), in the straddle: %s; want refused naming the turn", name, call, call.In(zl).Format("15:04:05 MST"), o.String())
						}
						if near {
							if pass, n := shorter(o); pass > n/10 {
								t.Errorf("FALSE GREEN: %s: learned at %s, %s from the turn, as %s: a replay on %s passed a time to live two hours shorter at %d of %d reads", name, call, call.Sub(turn), o.String(), leg.day.Format("2006-01-02"), pass, n)
							}
						}
						learned++
						continue
					}
					if !strings.Contains(ln.Why, "clocks go back") {
						if straddle(call) {
							t.Errorf("%s: at %s (local %s), in the straddle: %s (%s); want refused naming the turn", name, call, call.In(zl).Format("15:04:05 MST"), ln.Kind, ln.Why)
						}
						continue
					}
					refused++
					if !near {
						t.Errorf("FALSE RED: %s: refused for a turn at %s, %s from %s's: %s", name, call, call.Sub(turn), leg.zone, ln.Why)
					}
					for _, want := range leg.want {
						if !strings.Contains(ln.Why, want) {
							t.Errorf("%s: refused at %s: %q; want it to name %q", name, call, ln.Why, want)
						}
					}
					if lz != nil && !strings.Contains(ln.Why, leg.zone+" shows twice") {
						t.Errorf("%s: refused at %s: %q; want it to name %s", name, call, ln.Why, leg.zone)
					}
					if lz == nil && !strings.Contains(ln.Why, "keploy knows no zone of the application's here") {
						t.Errorf("%s: refused at %s: %q; want it to say keploy knows no zone", name, call, ln.Why)
					}
				}
				if learned < 20 || refused < 5 || straddled < 4 {
					t.Errorf("%s: learned %d, refused for the turn %d, calls in the straddle %d; want 20, 5 and 4 or more", name, learned, refused, straddled)
				}
			}
		}
	}
}

// A read plus an offset whose stored zone keploy cannot read (a name this
// keploy's zone files lack), or learned as no one zone (AppZoneApart), cut
// to the second: its replay, where keploy reads no zone of the
// application's process, knows no zone of the application's, so it reads
// every zone's turns (FallBackAnywhere). In the hour Europe/Berlin shows
// twice (2026-10-25), an application there that resolves the wall time to
// its first showing writes clock + 2h an hour early, so a time to live an
// hour longer stands where the right one would: the replay refuses it
// there, naming a turn and that keploy knows no zone. Kept as a known
// zone, the stored name would leave the check in the agent's zone (UTC),
// which never turns, and the longer one passes at every read. On another
// day the right value passes.
func TestAnOffsetWhoseStoredZoneIsUnreadIsCheckedAgainstEveryZonesTurns(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	val := func(r time.Time, ttl time.Duration) time.Time { return firstShowingIn(r, time.Second, ber).Add(ttl) }
	dc := clockedDrifts()
	dc.zone = ber
	call := time.Date(2026, 10, 26, 10, 0, 7, 300e6, time.UTC)
	fcall := call.Add(FullRoundLead)
	w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
	leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
	ln := ClassifyIn(dc, []string{p}, leaf(val(fcall.Add(-30*ms), 2*hour)), leaf(val(call.Add(-30*ms), 2*hour)), call, nil, nil, nil, w)[0]
	if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.AppZone != "Europe/Berlin" {
		t.Fatalf("learned %s (%s) %+v; want clock + 2h at the second, naming Europe/Berlin", ln.Kind, ln.Why, ln.Offset)
	}
	for _, z := range []string{"Mars/Olympus_Mons", models.AppZoneApart} {
		o := *ln.Offset
		o.AppZone = z
		longer, n := 0, 0
		for from := turn; from.Before(turn.Add(hour)); from = from.Add(37 * time.Second) {
			n++
			s := Span{From: from, To: from.Add(time.Second), Zone: time.UTC}
			ok, why := CheckClockRead(p, o, InstantLeaf(p, p, val(from.Add(500*ms), 3*hour), ms), s, true)
			if ok {
				longer++
				continue
			}
			if !strings.Contains(why, "clocks go back") || !strings.Contains(why, "keploy knows no zone of the application's at this call") {
				t.Errorf("stored zone %q, read at %s in the hour Berlin shows twice: refused %q; want it to name a turn and that keploy knows no zone", z, from, why)
			}
		}
		if longer > 0 {
			t.Errorf("FALSE GREEN: stored zone %q, the process's zone unread: in the hour Berlin shows twice, a time to live an hour longer passed at %d of %d reads; want none", z, longer, n)
		}
		far := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
		if ok, why := CheckClockRead(p, o, InstantLeaf(p, p, val(far.Add(500*ms), 2*hour), ms), Span{From: far, To: far.Add(time.Second), Zone: time.UTC}, true); !ok {
			t.Errorf("FALSE RED: stored zone %q, read on %s: the right value refused: %s", z, far, why)
		}
	}
}

// With no zone known at learn or at replay, a read plus an offset cut to
// the second, by an application in America/Santiago that adds two hours
// and then resolves its wall time to its first showing: Santiago's clocks
// go back at 03:00 UTC on 2026-04-05, a turn no other zone's window covers
// the hours before. A replay there reads every zone's turns over the call's
// window and over the values its check admits (the window plus the
// offset): the values a time to live an hour longer writes in the hour
// before the turn stand in the hour shown twice while the call's window
// does not, so a check of the window alone passes it, from 00:00 to 01:00
// UTC. The replay refuses it at every read.
func TestTheUnionOfEveryZonesTurnsReadsTheValuesACheckAdmits(t *testing.T) {
	scl, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	val := func(r time.Time, ttl time.Duration) time.Time { return firstShowingIn(r.Add(ttl), time.Second, scl) }
	dc := clockedDrifts()
	dc.zone = nil
	call := time.Date(2026, 5, 6, 10, 0, 7, 300e6, time.UTC)
	fcall := call.Add(FullRoundLead)
	w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
	leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
	ln := ClassifyIn(dc, []string{p}, leaf(val(fcall.Add(-30*ms), 2*hour)), leaf(val(call.Add(-30*ms), 2*hour)), call, nil, nil, nil, w)[0]
	if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.AppZone != "" {
		t.Fatalf("learned %s (%s) %+v; want clock + 2h at the second, naming no zone", ln.Kind, ln.Why, ln.Offset)
	}
	o := *ln.Offset
	longer, n, before := 0, 0, 0
	for from := time.Date(2026, 4, 4, 20, 0, 0, 0, time.UTC); from.Before(time.Date(2026, 4, 5, 6, 0, 0, 0, time.UTC)); from = from.Add(37 * time.Second) {
		n++
		if !from.Before(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)) && from.Before(time.Date(2026, 4, 5, 1, 0, 0, 0, time.UTC)) {
			before++
		}
		s := Span{From: from, To: from.Add(time.Second), Zone: time.UTC}
		if ok, _ := CheckClockRead(p, o, InstantLeaf(p, p, val(from.Add(500*ms), 3*hour), ms), s, true); ok {
			longer++
			if longer <= 3 {
				t.Errorf("FALSE GREEN: no zone known, read at %s near America/Santiago's turn: a time to live an hour longer passed", from)
			}
		}
	}
	if before < 90 {
		t.Fatalf("%d reads from 00:00 to 01:00 UTC; want 90 or more", before)
	}
	if longer > 0 {
		t.Errorf("FALSE GREEN: a time to live an hour longer passed at %d of %d reads; want none", longer, n)
	}
	h := time.Date(2026, 4, 5, 0, 30, 0, 0, time.UTC)
	if ok, why := CheckClockRead(p, o, InstantLeaf(p, p, val(h.Add(500*ms), 3*hour), ms), Span{From: h, To: h.Add(time.Second), Zone: time.UTC}, true); ok || !strings.Contains(why, "keploy knows no zone of the application's at this call") || !strings.Contains(why, "clocks go back") {
		t.Errorf("no zone known, read at %s: passed %v (%s); want refused, naming a turn and that keploy knows no zone", h, ok, why)
	}
}

// With no zone known at learn or at replay, a read plus an offset cut to
// the second, by an application in Antarctica/Troll that cuts its clock and
// then adds two hours, resolving its wall time to its first showing:
// Troll's clocks go back from 03:00 +02 to 01:00 +00 at 01:00 UTC on
// 2026-10-25, and in the two hours it shows twice the application writes a
// value two hours early. From 02:00 to 03:00 UTC no other zone's window
// covers the call (the European Union's, at the same instant, goes back an
// hour), and Troll's turn is one whose wall time shown twice holds an
// hour's start (models.AcrossHour), which the union of every zone's turns
// keeps beside the others for a replay at any cut. A time to live two hours
// longer stands there where the right one would: the replay refuses it at
// every read, naming Troll's turn and that keploy knows no zone. On another
// day the right value passes.
func TestAReplayKnowingNoZoneRefusesAValueInTrollsSecondHourShownTwice(t *testing.T) {
	troll, err := time.LoadLocation("Antarctica/Troll")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	hour, ms := time.Hour, time.Millisecond
	val := func(r time.Time, ttl time.Duration) time.Time { return firstShowingIn(r, time.Second, troll).Add(ttl) }
	dc := clockedDrifts()
	dc.zone = nil
	call := time.Date(2026, 11, 5, 10, 0, 7, 300e6, time.UTC)
	fcall := call.Add(FullRoundLead)
	w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
	leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
	ln := ClassifyIn(dc, []string{p}, leaf(val(fcall.Add(-30*ms), 2*hour)), leaf(val(call.Add(-30*ms), 2*hour)), call, nil, nil, nil, w)[0]
	if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.AppZone != "" || ln.Offset.Res != time.Second {
		t.Fatalf("learned %s (%s) %+v; want clock + 2h at the second, naming no zone", ln.Kind, ln.Why, ln.Offset)
	}
	o := *ln.Offset
	from, to := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	longer, n := 0, 0
	for at := from; at.Before(to); at = at.Add(37 * time.Second) {
		n++
		s := Span{From: at, To: at.Add(time.Second), Zone: time.UTC}
		ok, why := CheckClockRead(p, o, InstantLeaf(p, p, val(at.Add(500*ms), 4*hour), ms), s, true)
		if ok {
			longer++
			if longer <= 3 {
				t.Errorf("FALSE GREEN: no zone known, read at %s in the second hour Antarctica/Troll shows twice: a time to live two hours longer passed", at)
			}
			continue
		}
		for _, want := range []string{"Antarctica/Troll shows twice as its clocks go back from 03:00 +02 to 01:00 +00 on 2026-10-25", "keploy knows no zone of the application's at this call"} {
			if !strings.Contains(why, want) {
				t.Errorf("no zone known, read at %s: refused %q; want it to name %q", at, why, want)
			}
		}
	}
	if n < 90 {
		t.Fatalf("%d reads from 02:00 to 03:00 UTC; want 90 or more", n)
	}
	if longer > 0 {
		t.Errorf("FALSE GREEN: a time to live two hours longer passed at %d of %d reads; want none", longer, n)
	}
	far := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	if ok, why := CheckClockRead(p, o, InstantLeaf(p, p, val(far.Add(500*ms), 2*hour), ms), Span{From: far, To: far.Add(time.Second), Zone: time.UTC}, true); !ok {
		t.Errorf("FALSE RED: no zone known, read on %s: the right value refused: %s", far, why)
	}
}
