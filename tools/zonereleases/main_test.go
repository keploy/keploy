package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// testSource is a release's source of one zone and two links.
const testSource = `# version 2099a
Z Test/Zone 1 - CET
L Test/Zone Test/Link
L Test/Link Test/Link2
`

// writeFiles writes each file (by its path under dir) of files.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A build's zone files are its release's Zone and Link names, and no
// other file but zic's temporary ones (".zic" and six letters), which zic
// 2021c to 2022e left beside a link: such a file is never a name of the
// table. A file of no name, a name of no file, and a zone.tab name of no
// zone each fail the build, saying which.
func TestABuildsFilesAreItsReleasesZoneAndLinkNames(t *testing.T) {
	src, err := models.ParseZoneSource([]byte(testSource))
	if err != nil {
		t.Fatal(err)
	}
	whole := map[string]string{"Test/Zone": "z", "Test/Link": "z", "Test/Link2": "z"}
	dir := t.TempDir()
	writeFiles(t, dir, whole)
	writeFiles(t, dir, map[string]string{"Test/.zicXlvzRq": "z", ".zic53ALo6": "z"})
	files, err := filesOf(dir, src, []string{"Test/Zone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files["Test/Link2"] == nil {
		t.Errorf("the files of a build of three names and two of zic's temporary files: %v; want the three names'", files)
	}
	for what, row := range map[string]struct {
		files map[string]string
		tabs  []string
		says  string
	}{
		"a file of no name":                {map[string]string{"Test/Other": "z"}, nil, `"Test/Other"`},
		"a dot file that is none of zic's": {map[string]string{"Test/.hidden": "z"}, nil, `"Test/.hidden"`},
		"a zone.tab name of no zone":       {nil, []string{"Test/Tabbed"}, "Test/Tabbed (zone.tab)"},
	} {
		d := t.TempDir()
		writeFiles(t, d, whole)
		writeFiles(t, d, row.files)
		if _, err := filesOf(d, src, row.tabs); err == nil || !strings.Contains(err.Error(), row.says) {
			t.Errorf("%s: %v; want an error saying %s", what, err, row.says)
		}
	}
	d := t.TempDir()
	writeFiles(t, d, map[string]string{"Test/Zone": "z", "Test/Link": "z"})
	if _, err := filesOf(d, src, nil); err == nil || !strings.Contains(err.Error(), `"Test/Link2"`) {
		t.Errorf("a name of no file: %v; want an error naming it", err)
	}
}

// A tarball must hold the sha256 its pin gives; one of no pin is pinned.
// The pins are written sorted, one per line, and read back the same.
func TestATarballMustHoldItsPin(t *testing.T) {
	b := []byte("a tarball")
	sum := sha256.Sum256(b)
	sums := map[string]string{"tzdata2099a.tar.gz": strings.Repeat("0", 64)}
	if err := pin("tzdata2099a.tar.gz", b, sums); err == nil || !strings.Contains(err.Error(), "pinned at") {
		t.Errorf("a tarball of another sha256 than its pin: %v; want refused", err)
	}
	if err := pin("tzcode2099a.tar.gz", b, sums); err != nil || sums["tzcode2099a.tar.gz"] != hex.EncodeToString(sum[:]) {
		t.Errorf("a tarball of no pin: %v, pinned %q; want pinned at its sha256", err, sums["tzcode2099a.tar.gz"])
	}
	p := filepath.Join(t.TempDir(), "sums")
	if err := writeSums(p, sums); err != nil {
		t.Fatal(err)
	}
	got, err := readSums(p)
	if err != nil || len(got) != 2 || got["tzdata2099a.tar.gz"] != sums["tzdata2099a.tar.gz"] {
		t.Errorf("the pins read back: %v %v; want %v", got, err, sums)
	}
	if text, _ := os.ReadFile(p); !strings.HasPrefix(string(text), sums["tzcode2099a.tar.gz"]+"  tzcode2099a.tar.gz\n") {
		t.Errorf("the pins file %q; want sorted lines of \"<sha256>  <tarball>\"", text)
	}
	if err := os.WriteFile(p, []byte("not a pin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSums(p); err == nil {
		t.Errorf("a pins file of a line of no pin: read; want an error")
	}
}

// tgz is a gzipped tar of files, by name.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A release's tarballs must name it: each one's version file, or, before
// 2016h, the tzcode one's NEWS and the tzdata one's Makefile (2016g's
// names none).
func TestAReleasesTarballsMustNameIt(t *testing.T) {
	for what, row := range map[string]struct {
		code, data map[string]string
		ok         bool
	}{
		"version files of the release": {map[string]string{"version": "2099a\n"}, map[string]string{"version": "2099a\n"}, true},
		"version files of another":     {map[string]string{"version": "2099a\n"}, map[string]string{"version": "2099b\n"}, false},
		"NEWS and a Makefile, before version files": {map[string]string{"NEWS": "News\n\nRelease 2099a - 2099-01-01 00:00:00 +0000\n"},
			map[string]string{"Makefile": "VERSION=\t2099a\n"}, true},
		"a Makefile naming no release": {map[string]string{"NEWS": "Release 2099a - 2099-01-01 00:00:00 +0000\n"}, map[string]string{"Makefile": "VERSION=\tunknown\n"}, true},
		"NEWS naming another release first": {map[string]string{"NEWS": "Release 2098z - 2098-12-01 00:00:00 +0000\n"},
			map[string]string{"Makefile": "VERSION=\t2099a\n"}, false},
	} {
		work := t.TempDir()
		writeFiles(t, work, map[string]string{"tzcode2099a.tar.gz": string(tgz(t, row.code)), "tzdata2099a.tar.gz": string(tgz(t, row.data))})
		if err := checkRelease(work, "2099a"); (err == nil) != row.ok {
			t.Errorf("%s: %v; want ok %v", what, err, row.ok)
		}
	}
}

// IANA's listing gives the releases from 2014a on, oldest first, up to
// -last (which it must list).
func TestTheReleasesAreIANAsFromTheFirstToTheLast(t *testing.T) {
	listing := []byte(`<a href="tzdata2013i.tar.gz">x</a> <a href="tzdata2014a.tar.gz">x</a> <a href="tzdata2026f.tar.gz">x</a> tzdata2026f.tar.gz.asc <a href="tzdata2014b.tar.gz">x</a>`)
	if rs, err := releasesIn(listing, ""); err != nil || strings.Join(rs, " ") != "2014a 2014b 2026f" {
		t.Errorf("the releases: %v %v; want 2014a 2014b 2026f", rs, err)
	}
	if rs, err := releasesIn(listing, "2014b"); err != nil || strings.Join(rs, " ") != "2014a 2014b" {
		t.Errorf("the releases up to 2014b: %v %v; want 2014a 2014b", rs, err)
	}
	if _, err := releasesIn(listing, "2015a"); err == nil {
		t.Errorf("a last release the listing does not name: read; want an error")
	}
}

// The table's text depends on nothing but its names and digests: built
// in another order, it is the same bytes, gofmt's, its names sorted, each
// with its newest release's digest first.
func TestTheTableIsTheSameBytesHoweverItIsBuilt(t *testing.T) {
	build := func(order []string) []byte {
		tb := &table{first: "2014a", last: "2099a", all: map[string]map[uint64]bool{}, newest: map[string]uint64{}}
		for _, n := range order {
			tb.all[n] = map[uint64]bool{3: true, 1: true, 2: true}
			tb.newest[n] = 2
		}
		b, err := tb.render()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := build([]string{"B/Zone", "A/Zone", "C"}), build([]string{"C", "A/Zone", "B/Zone"})
	if !bytes.Equal(a, b) {
		t.Errorf("one table built in two orders: two texts\n%s\n%s", a, b)
	}
	if !strings.Contains(string(a), "\t\"A/Zone\": {0x0000000000000002, 0x0000000000000001, 0x0000000000000002, 0x0000000000000003},\n\t\"B/Zone\"") {
		t.Errorf("the table's text: %s; want each name sorted, its newest digest, then its digests sorted", a)
	}
}
