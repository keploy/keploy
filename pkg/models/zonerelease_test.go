package models

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every zone file of Go's own zoneinfo.zip (the toolchain's: go env
// GOROOT), the zone data built into keploy, is of a release zoneReleases
// holds: ZoneReading.OfARelease reads it as its name's. So keploy's digest
// of a file (ZoneRulesDigest, run by this Go) is the one
// tools/zonereleases wrote of the release, and the table is no older than
// keploy's own zone data: a Go whose zone data is a later release than
// ZoneReleasesLast fails here until `go generate ./pkg/models/` writes the
// table again.
func TestEveryZoneFileOfGosZoneDataIsOfAReleaseKeployKnows(t *testing.T) {
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	zp := filepath.Join(strings.TrimSpace(string(root)), "lib", "time", "zoneinfo.zip")
	r, err := zip.OpenReader(zp)
	if err != nil {
		t.Fatalf("Go's own zone data (%s): %v", zp, err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	n := 0
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocationFromTZData(f.Name, b)
		if err != nil {
			t.Fatalf("%s in %s: %v", f.Name, zp, err)
		}
		n++
		if !ReadZoneReleases(loc).OfARelease(f.Name) {
			t.Errorf("Go's %s is of no release of tzdata %s to %s keploy knows: run go generate ./pkg/models/", f.Name, ZoneReleasesFirst, ZoneReleasesLast)
		}
	}
	if n < 500 {
		t.Fatalf("Go's own zone data holds %d zone files; want the whole of a release", n)
	}
}

// A zone file's digest is of its offsets, not its form: a slim Europe/Berlin
// (zic -b slim, Go's zoneinfo.zip) and a fat one (a distribution's) read one
// offset at every instant and have one digest, which is a release's of
// Europe/Berlin. A file whose offsets no release gives the name is of none
// (Berlin's rules with its first summer time of 1980 a week late), but of
// a source that gives them (a release's tzdata.zi that moved it so: Of).
// Releases names every name a release reads a file's offsets for:
// Asia/Kolkata's for Asia/Kolkata and its link Asia/Calcutta, none for a
// file no release holds.
func TestAZoneFilesDigestIsOfItsOffsetsAndNamesTheReleasesOfThem(t *testing.T) {
	host := func(name string) *time.Location {
		b, err := os.ReadFile("/usr/share/zoneinfo/" + name)
		if err != nil {
			t.Fatalf("the host's zone file of %s: %v", name, err)
		}
		loc, err := time.LoadLocationFromTZData(name, b)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	slim, err := time.LoadLocationFromTZData("Europe/Berlin", slimZoneData(t, "Europe/Berlin"))
	if err != nil {
		t.Fatal(err)
	}
	berlin := host("Europe/Berlin")
	if ZoneRulesDigest(slim, zoneDigestTo) != ZoneRulesDigest(berlin, zoneDigestTo) {
		t.Errorf("a slim and a fat Europe/Berlin: two digests; want one")
	}
	if !ReadZoneReleases(slim).OfARelease("Europe/Berlin") || !ReadZoneReleases(berlin).OfARelease("Europe/Berlin") {
		t.Errorf("a slim or a fat Europe/Berlin read as no release's of Europe/Berlin")
	}
	if ReadZoneReleases(berlin).OfARelease("Asia/Kolkata") {
		t.Errorf("Europe/Berlin's file read as a release's of Asia/Kolkata")
	}
	// Berlin's offsets with the change of 1980-04-06T01:00Z a week late.
	changes := zoneChangesIn(berlin, zoneDigestFrom, zoneDigestTo)
	offs := []int{3600}
	for _, c := range changes {
		_, o := c.In(berlin).Zone()
		offs = append(offs, o)
	}
	if !changes[0].Equal(time.Date(1980, 4, 6, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("precondition: Berlin's first change from 1970 is at %s", changes[0])
	}
	changes[0] = time.Date(1980, 4, 13, 1, 0, 0, 0, time.UTC)
	moved, err := time.LoadLocationFromTZData("Europe/Berlin", tzifV1(offs, changes))
	if err != nil {
		t.Fatal(err)
	}
	if ReadZoneReleases(moved).OfARelease("Europe/Berlin") {
		t.Errorf("Berlin's rules, its first summer time of 1980 a week late, read as a release's of Europe/Berlin")
	}
	src := testSource(t)
	text := strings.Replace(string(src.Extract("Europe/Berlin")), "1 DE CE%sT 1980\n", "1 DE CE%sT 1980\n1 - CET 1980 Ap 13 1u\n", 1)
	movedSrc, err := ParseZoneSource([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if !ReadZoneReleases(moved).Of(movedSrc, "Europe/Berlin") || ReadZoneReleases(moved).Of(src, "Europe/Berlin") || ReadZoneReleases(moved).Of(nil, "Europe/Berlin") {
		t.Errorf("Berlin's rules, its first summer time of 1980 a week late: read as the source's that moved it %v, as %s's %v; want the first alone",
			ReadZoneReleases(moved).Of(movedSrc, "Europe/Berlin"), ZoneReleasesLast, ReadZoneReleases(moved).Of(src, "Europe/Berlin"))
	}
	if names := ReadZoneReleases(host("Asia/Kolkata")).Releases(); !slices.Contains(names, "Asia/Kolkata") || !slices.Contains(names, "Asia/Calcutta") || !slices.IsSorted(names) {
		t.Errorf("the names of Asia/Kolkata's offsets: %v; want Asia/Kolkata and Asia/Calcutta among them, sorted", names)
	}
	if names := ReadZoneReleases(moved).Releases(); len(names) != 0 {
		t.Errorf("the names of offsets no release gives: %v; want none", names)
	}
	if !strings.HasPrefix(ZoneReleasesFirst, "20") || ZoneReleasesLast < ZoneReleasesFirst {
		t.Errorf("the table's releases: %q to %q", ZoneReleasesFirst, ZoneReleasesLast)
	}
}

// removedNames are the names IANA dropped from its zone data from
// ZoneReleasesFirst on, by the release that dropped them (its NEWS).
var removedNames = map[string]string{
	"Canada/East-Saskatchewan": "2017c",
	"US/Pacific-New":           "2020b",
}

// ianaName is a name of IANA's zone data: each part letters, digits, "_",
// "-" and "+", never starting with "-" or "." (theory.html's rules, and
// GMT+0 and Etc/GMT-14's).
var ianaName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z][A-Za-z0-9_+-]*)*$`)

// Every name the table holds is a zone or link name of IANA's zone data:
// one of ZoneReleasesLast's, as its own tzdata.zi lists them (IANA's
// Makefile writes it from the release's Zone and Link lines), or one IANA
// dropped since ZoneReleasesFirst (removedNames); never a file zic left
// behind (".zic" and six letters, as zic 2021c to 2022e left beside a link
// it made), which a refusal would name as a zone. Every name of the release
// is in the table, with the digest of its file of that release.
func TestEveryNameOfTheTableIsAZoneOrLinkOfIANAsData(t *testing.T) {
	src := testSource(t)
	for name := range zoneReleases {
		newest, all, ok := zoneReleasesOf(name)
		switch {
		case !ianaName.MatchString(name):
			t.Errorf("%q: no name of IANA's zone data", name)
		case !ok || len(all) == 0:
			t.Errorf("%s: no digest", name)
		case src.Holds(name) != (newest != 0):
			t.Errorf("%s: of %s's names %v, its digest there %x", name, ZoneReleasesLast, src.Holds(name), newest)
		case !src.Holds(name) && removedNames[name] == "":
			t.Errorf("%s: neither a name of %s's tzdata.zi nor one IANA dropped", name, ZoneReleasesLast)
		}
	}
	for _, name := range src.Names() {
		if _, ok := zoneReleases[name]; !ok {
			t.Errorf("%s: a name of %s's tzdata.zi the table lacks", name, ZoneReleasesLast)
		}
	}
	for name, rel := range removedNames {
		if _, ok := zoneReleases[name]; !ok || src.Holds(name) {
			t.Errorf("%s, which IANA dropped in %s: in the table %v, in %s %v", name, rel, ok, ZoneReleasesLast, src.Holds(name))
		}
	}
}
