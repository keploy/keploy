package models

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testSource is testdata/tzdata.zi, the tzdata.zi of ZoneReleasesLast as
// its Makefile writes it (tools/zonereleases -zi): IANA's own text of the
// release, which a distribution installs beside its zone files.
func testSource(t *testing.T) *ZoneSource {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "tzdata.zi"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := ParseZoneSource(b)
	if err != nil {
		t.Fatalf("testdata/tzdata.zi: %v", err)
	}
	return src
}

// Every zone and link of a release's tzdata.zi is read from it with the
// offsets zic compiled it to: the table's digest of each name in
// ZoneReleasesLast's main data, of the files zic wrote of that release
// (tools/zonereleases, which checks every release so), is one of the
// source's of the name. So a root whose zone data is a release keploy
// does not know is read in its own tzdata.zi as zic built its files.
func TestASourceReadsEveryZoneOfItsReleaseAsZicCompiledIt(t *testing.T) {
	src := testSource(t)
	if v := src.Version(); v != ZoneReleasesLast {
		t.Fatalf("testdata/tzdata.zi is of release %q; want ZoneReleasesLast's %s (go generate ./pkg/models/ writes both)", v, ZoneReleasesLast)
	}
	names := src.Names()
	if len(names) < 590 {
		t.Fatalf("testdata/tzdata.zi names %d zones and links; want a whole release", len(names))
	}
	for _, n := range names {
		newest, _, ok := zoneReleasesOf(n)
		if !ok || newest == 0 {
			t.Errorf("%s: no digest of ZoneReleasesLast's file of it in the table", n)
			continue
		}
		if ds, _ := src.Digests(n); !slices.Contains(ds, newest) {
			t.Errorf("%s: the source reads it as %x; zic's file of it reads %x", n, ds, newest)
		}
	}
}

// A zone's extract (its Rule, Zone and Link lines alone) reads it as the
// whole source does: what the keploy CLI hands a docker-mode agent.
func TestAZonesExtractReadsItAsItsWholeSourceDoes(t *testing.T) {
	src := testSource(t)
	for _, n := range src.Names() {
		e := src.Extract(n)
		one, err := ParseZoneSource(e)
		if err != nil {
			t.Errorf("%s's extract: %v\n%s", n, err, e)
			continue
		}
		got, _ := one.Digests(n)
		want, _ := src.Digests(n)
		if !slices.Equal(got, want) || one.Version() != src.Version() {
			t.Errorf("%s's extract reads %x of release %q; its source %x of %q", n, got, one.Version(), want, src.Version())
		}
	}
	if src.Extract("Nowhere/Nothing") != nil {
		t.Errorf("the extract of a name the source has no zone of: some; want none")
	}
}

// formsZone is a zone written with every form zic reads a rule's day and
// time in: the last Sunday, a weekday on or after a day and on or before
// one, 24:00, and times of standard (s), universal (u) and wall clock; an
// UNTIL of universal time and one of a year alone. formsOffsets are its
// offsets from 1970, worked out by hand: UTC+01:00, then from 1990-01-01
// UTC+02:00 with summer time, then UTC+03:00 from the end of 1992.
const formsZone = `# A zone of every form.
Rule	Tst	1990	only	-	Mar	lastSun	2:00s	1:00	D
Rule	Tst	1990	only	-	Oct	Sun>=8	2:00	0	S
Rule	Tst	1991	1992	-	Apr	Sat<=7	24:00	1:00	D
Rule	Tst	1991	1992	-	Sep	lastSun	1:00u	0	S
Zone	Test/Forms	1:00	-	CET	1990 Jan  1 0:00u
		2:00	Tst	EE%sT	1993
		3:00	-	+03
`

// formsZoneZi is formsZone as zishrink writes tzdata.zi, with two links.
const formsZoneZi = `# version 2099a
R Tst 1990 o - Mar lastSu 2s 1 D
R Tst 1990 o - O Su>=8 2 0 S
R Tst 1991 1992 - Ap Sa<=7 24 1 D
R Tst 1991 1992 - S lastSu 1u 0 S
Z Test/Forms 1 - CET 1990 Ja 1 0u
2 Tst EE%sT 1993
3 - +03
L Test/Forms Test/Alias
L Test/Alias Test/Alias2
`

func formsOffsets() ([]int, []time.Time) {
	u := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }
	return []int{3600, 7200, 10800, 7200, 10800, 7200, 10800, 7200, 10800},
		[]time.Time{u(1990, 1, 1, 0), u(1990, 3, 25, 0), u(1990, 10, 13, 23), u(1991, 4, 6, 22), u(1991, 9, 29, 1), u(1992, 4, 4, 22), u(1992, 9, 27, 1), u(1992, 12, 31, 22)}
}

// digestOf is ZoneRulesDigest of a version 1 zone file of offs at at.
func digestOf(t *testing.T, offs []int, at []time.Time) uint64 {
	t.Helper()
	loc, err := time.LoadLocationFromTZData("Test", tzifV1(offs, at))
	if err != nil {
		t.Fatal(err)
	}
	return ZoneRulesDigest(loc, zoneDigestTo)
}

// A source is read as zic reads it: each form of a rule's day and time,
// in IANA's long form and in tzdata.zi's short one, and a link to a link
// as the zone it ends at.
func TestASourcesRulesAreReadInEveryFormZicReads(t *testing.T) {
	offs, at := formsOffsets()
	want := digestOf(t, offs, at)
	for what, text := range map[string]string{"the long form": formsZone, "tzdata.zi's form": formsZoneZi} {
		src, err := ParseZoneSource([]byte(text))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, n := range []string{"Test/Forms", "Test/Alias", "Test/Alias2"} {
			if n != "Test/Forms" && what == "the long form" {
				continue
			}
			if ds, ok := src.Digests(n); !ok || !slices.Equal(ds, []uint64{want}) {
				t.Errorf("%s, %s: %x %v; want the offsets worked out by hand, %x", what, n, ds, ok, want)
			}
		}
		if got := src.Holding(want); !slices.Contains(got, "Test/Forms") {
			t.Errorf("%s: the names of the zone's offsets %v; want Test/Forms among them", what, got)
		}
	}
	src, err := ParseZoneSource([]byte(formsZoneZi))
	if err != nil {
		t.Fatal(err)
	}
	if v := src.Version(); v != "2099a" {
		t.Errorf("the release a source names: %q; want 2099a", v)
	}
	if !slices.Equal(src.Names(), []string{"Test/Alias", "Test/Alias2", "Test/Forms"}) || !src.Holds("Test/Alias2") || src.Holds("Test/None") {
		t.Errorf("the source's names %v; want its zone and its two links", src.Names())
	}
	// A Link replaces a Zone of its name, as zic writes the links last.
	src, err = ParseZoneSource([]byte(formsZoneZi + "Z Test/Other 5 - +05\nL Test/Forms Test/Other\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ds, _ := src.Digests("Test/Other"); !slices.Equal(ds, []uint64{want}) {
		t.Errorf("a zone a later link replaces: %x; want the link's target's %x", ds, want)
	}
}

// insideZone begins a line inside its rules' summer time with no rule of
// its own before it (as Africa/Windhoek of tzdata 2014a to 2016j does): zic
// writes UTC+01:00 at the line's start, which a fat file lists, while a
// slim file's footer rule takes over there and reads summer time until the
// next April's change.
const insideZone = `R Nm 1994 ma - S Su>=1 2 1 S
R Nm 1995 ma - Ap Su>=1 2 0 -
Z Test/Inside 2 - CAT 1994 Mar 21
1 Nm WA%sT
`

// firstSunday is the first Sunday of y's month m.
func firstSunday(y int, m time.Month) time.Time {
	d := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	for d.Weekday() != time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// A zone whose fat and slim files read apart has a digest of each: the fat
// file's (every change listed), then the slim file's (its footer from its
// last change on). A zone of no such line has one.
func TestAZoneWhoseSlimAndFatFilesReadApartHasTheDigestOfEach(t *testing.T) {
	src, err := ParseZoneSource([]byte(insideZone))
	if err != nil {
		t.Fatal(err)
	}
	// Summer time from the first Sunday of September at 02:00 (UTC+01:00),
	// to the first Sunday of April at 02:00 (UTC+02:00).
	var yearly []time.Time
	var offs []int
	for y := 1995; y <= 2037; y++ {
		yearly = append(yearly, firstSunday(y, time.April), firstSunday(y, time.September).Add(time.Hour))
		offs = append(offs, 3600, 7200)
	}
	sep94 := firstSunday(1994, time.September).Add(time.Hour)
	fat := digestOf(t, append([]int{7200, 3600, 7200}, offs...), append([]time.Time{time.Date(1994, 3, 20, 22, 0, 0, 0, time.UTC), sep94}, yearly...))
	slim := digestOf(t, append([]int{7200, 3600, 7200}, offs...), append([]time.Time{firstSunday(1994, time.April), sep94}, yearly...))
	if ds, _ := src.Digests("Test/Inside"); !slices.Equal(ds, []uint64{fat, slim}) {
		t.Errorf("a zone whose slim file reads summer time where its fat one reads standard: %x; want the fat file's %x, then the slim file's %x", ds, fat, slim)
	}
	forms, err := ParseZoneSource([]byte(formsZone))
	if err != nil {
		t.Fatal(err)
	}
	if ds, _ := forms.Digests("Test/Forms"); len(ds) != 1 {
		t.Errorf("a zone whose files read alike: %d digests; want one", len(ds))
	}
}

// A zone keeps summer time all year where its last line's latest rule is a
// summer one with no standard rule of the "max" year beside it (2022b's
// backzone Africa/Freetown), or its own SAVE is summer time; not where it
// runs two rules to the "max" year, or ends in standard time.
func TestAZoneKeepsSummerTimeAllYearWhereItsLatestRuleIsSummers(t *testing.T) {
	src, err := ParseZoneSource([]byte(insideZone + formsZoneZi + `R SL 1933 1938 - Mar 31 24 0 -01
R SL 1933 1939 - Au 31 24 0:20 -0040
Z Test/Summer -1 SL %s 1939 S 5
0 SL GMT/+01
Z Test/Fixed 1 1 +02
`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"Test/Summer": true, "Test/Fixed": true, "Test/Inside": false, "Test/Forms": false, "Test/Alias": false, "Test/None": false} {
		if got := src.SummerAllYear(name); got != want {
			t.Errorf("%s keeps summer time all year: %v; want %v", name, got, want)
		}
	}
}

// A source zic would not compile is none: a line of no kind, a zone named
// twice, a link to no zone, a month or a day of none, a zone whose
// continuation line is missing, rules of no name nor time.
func TestASourceZicWouldNotCompileIsRefused(t *testing.T) {
	for what, text := range map[string]string{
		"a line of no kind":     "Q x y z\n",
		"a zone named twice":    "Z A 1 - X\nZ A 2 - Y\n",
		"a link to no zone":     "L Nowhere A\n",
		"a month of none":       "R T 1990 o - Smarch 1 0 1 D\nZ A 1 T X\n",
		"a day of none":         "R T 1990 o - Ja Funday>=1 0 1 D\nZ A 1 T X\n",
		"a 30 February":         "R T 1990 o - F 30 0 1 D\nZ A 1 T X\n",
		"no continuation line":  "Z A 1 - X 1990\n",
		"rules of no name":      "Z A 1 Nope X\n",
		"an odd quotation mark": "Z \"A 1 - X\n",
	} {
		if _, err := ParseZoneSource([]byte(text)); err == nil {
			t.Errorf("%s: parsed; want an error", what)
		}
	}
	if _, err := ParseZoneSource([]byte(strings.ReplaceAll(formsZone, "Rule", "Leap\t1990 Jan 1 0:00:00 + S\nRule"))); err != nil {
		t.Errorf("a source with the leapseconds file's lines: %v; want them skipped", err)
	}
}
