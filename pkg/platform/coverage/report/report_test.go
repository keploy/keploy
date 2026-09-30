package report

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Every fixture under testdata/ is a report a real runner wrote, not one typed
// by hand: go test -coverprofile, node --test --experimental-test-coverage
// --test-reporter=lcov, pytest --cov-report=xml / --cov-report=lcov, and the
// jacoco-maven-plugin. Each exercises the same three functions (two covered,
// one never called), so the expected counts are each runner's own verdict.
func TestParseFile_RealRunnerReports(t *testing.T) {
	cases := []struct {
		file           string
		covered, total int64
		unit, format   string
	}{
		// go test printed "coverage: 37.5% of statements".
		{"go.coverprofile", 3, 8, "statements", "go"},
		{"node.lcov", 6, 13, "lines", "lcov"},
		{"pytest-cov.xml", 6, 11, "lines", "cobertura"},
		{"pytest-cov.lcov", 6, 11, "lines", "lcov"},
		{"jacoco.xml", 3, 8, "lines", "jacoco"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			s, err := ParseFile(filepath.Join("testdata", c.file))
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if s.Covered != c.covered || s.Total != c.total || s.Unit != c.unit || s.Format != c.format {
				t.Fatalf("got %d/%d %s (%s), want %d/%d %s (%s)",
					s.Covered, s.Total, s.Unit, s.Format, c.covered, c.total, c.unit, c.format)
			}
		})
	}
}

// -coverpkg across several test binaries writes each block once per binary.
// Summing them double-counts every statement; go tool cover merges by position.
func TestParse_GoProfileMergesRepeatedBlocks(t *testing.T) {
	profile := "mode: set\n" +
		"ex/a.go:1.1,2.2 3 0\n" +
		"ex/a.go:3.1,4.2 2 1\n" +
		"mode: set\n" + // a second binary's profile, concatenated
		"ex/a.go:1.1,2.2 3 1\n" +
		"ex/a.go:3.1,4.2 2 0\n"
	s, err := Parse(strings.NewReader(profile))
	if err != nil {
		t.Fatal(err)
	}
	if s.Covered != 5 || s.Total != 5 {
		t.Fatalf("got %d/%d, want 5/5: a block covered by either binary is covered", s.Covered, s.Total)
	}
}

// Per-worker LCOV files concatenated list one source several times. Each line
// is one line, however many workers ran it.
func TestParse_LCOVMergesRepeatedSources(t *testing.T) {
	lcov := "SF:a.js\nDA:1,1\nDA:2,0\nDA:3,0\nend_of_record\n" +
		"SF:a.js\nDA:1,0\nDA:2,4\nDA:3,0\nend_of_record\n"
	s, err := Parse(strings.NewReader(lcov))
	if err != nil {
		t.Fatal(err)
	}
	if s.Covered != 2 || s.Total != 3 {
		t.Fatalf("got %d/%d, want 2/3", s.Covered, s.Total)
	}
}

// A record with only summary lines (some reporters omit DA) still counts.
func TestParse_LCOVSummaryOnly(t *testing.T) {
	s, err := Parse(strings.NewReader("TN:\nSF:a.js\nLF:10\nLH:4\nend_of_record\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Covered != 4 || s.Total != 10 {
		t.Fatalf("got %d/%d, want 4/10", s.Covered, s.Total)
	}
}

// line-rate alone is a percentage with no denominator. The package reports
// "N of M" or nothing, so it refuses rather than invent the totals.
func TestParse_CoberturaWithoutTotalsIsRefused(t *testing.T) {
	_, err := Parse(strings.NewReader(`<?xml version="1.0"?><coverage line-rate="0.8"></coverage>`))
	if err == nil {
		t.Fatal("a Cobertura report with no totals parsed; want an error")
	}
}

func TestParse_UnknownFormat(t *testing.T) {
	for _, body := range []string{"", "hello\n", `{"total":{}}`, `<?xml version="1.0"?><html></html>`} {
		if _, err := Parse(strings.NewReader(body)); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", body)
		}
	}
}

// The report-level LINE counter is the LAST in the file; package- and
// class-level LINE counters come first and must not be taken for it.
func TestParse_JaCoCoTakesTheReportLevelCounter(t *testing.T) {
	x := `<?xml version="1.0"?><!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd">` +
		`<report name="r"><sessioninfo id="s" start="1" dump="2"/>` +
		`<package name="p"><class name="c"><counter type="LINE" missed="9" covered="1"/></class>` +
		`<counter type="LINE" missed="9" covered="1"/></package>` +
		`<counter type="INSTRUCTION" missed="1" covered="1"/><counter type="LINE" missed="20" covered="80"/></report>`
	s, err := Parse(strings.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	if s.Covered != 80 || s.Total != 100 {
		t.Fatalf("got %d/%d, want 80/100", s.Covered, s.Total)
	}
}

func TestFloor1(t *testing.T) {
	for in, want := range map[float64]string{79.96: "79.9", 80: "80.0", 37.5: "37.5", 0: "0.0", 100: "100.0", 66.6666: "66.6"} {
		if got := Floor1(in); got != want {
			t.Errorf("Floor1(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestFromCommand(t *testing.T) {
	cases := map[string][]string{
		"go test ./... -coverprofile=coverage.out":             {"coverage.out"},
		"go test -coverprofile cov.txt ./...":                  {"cov.txt"},
		`go test -coverprofile="out dir/c.out" ./...`:          {"out dir/c.out"},
		"pytest --cov=app --cov-report=xml":                    {"coverage.xml"},
		"pytest --cov=app --cov-report xml:build/cov.xml":      {"build/cov.xml"},
		"pytest --cov=app --cov-report=lcov --cov-report=term": {"coverage.lcov"},
		"npm test": nil,
	}
	for cmd, want := range cases {
		if got := FromCommand(cmd); !reflect.DeepEqual(got, want) {
			t.Errorf("FromCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// A report from an earlier run describes a different run. Quoting it would
// attach someone else's number to this replay, so only a file this run created
// or changed counts -- including one an earlier run wrote a moment ago, which
// a timestamp window let through.
func TestFind_OnlyReportsThisRunWrote(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The previous run's report, written an instant before this run starts.
	write("coverage.xml", "<coverage/>")
	before := Snapshot(dir, "", "pytest -q")
	if got, why := Find(dir, "", "pytest -q", before); got != "" || why == "" {
		t.Fatalf("the previous run's report was credited to this one: %q", got)
	}

	// This run writes a report: it counts.
	write("coverage/lcov.info", "SF:a\nDA:1,1\nend_of_record\n")
	if got, _ := Find(dir, "", "npm test", before); got != filepath.Join(dir, "coverage/lcov.info") {
		t.Fatalf("got %q, want the report this run wrote", got)
	}

	// Rewriting an existing report counts, even within the same second: the
	// size changed.
	write("coverage.xml", "<coverage lines-valid=\"1\" lines-covered=\"1\"/>")
	if got, _ := Find(dir, "", "pytest -q", before); got == "" {
		t.Fatal("a report this run rewrote was not found")
	}

	// The command's own -coverprofile outranks a default the run also wrote.
	before = Snapshot(dir, "", "go test -coverprofile=named.out ./...")
	write("named.out", "mode: set\n")
	write("coverage.out", "mode: set\n")
	if got, _ := Find(dir, "", "go test -coverprofile=named.out ./...", before); got != filepath.Join(dir, "named.out") {
		t.Fatalf("got %q, want the report the command names", got)
	}

	// An explicit path is final: untouched means no report, with the reason.
	write("explicit.xml", "<coverage/>")
	before = Snapshot(dir, "explicit.xml", "")
	if got, why := Find(dir, "explicit.xml", "", before); got != "" || !strings.Contains(why, "not updated by this run") {
		t.Fatalf("got %q (%q), want no report because the run did not touch it", got, why)
	}
	if got, why := Find(dir, "missing.xml", "", Snapshot(dir, "missing.xml", "")); got != "" || !strings.Contains(why, "not written") {
		t.Fatalf("got %q (%q), want no report because it does not exist", got, why)
	}
}
