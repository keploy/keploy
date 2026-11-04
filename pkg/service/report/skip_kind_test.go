package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const (
	thisRunOnlyReason     = "flaky/flipped: keploy test --judge suggested it"
	untilRerecordedReason = "no mapping entry: auto-replay vouched for no mocks of this test, so the replay is not served any; re-record it"
)

func forEachSwitchMode(t *testing.T, f func(t *testing.T)) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			f(t)
		})
	}
}

// Each kind of skip a test can have, with the words the test's line and
// JUnit say it in, before its reason.
var skipKindLines = []struct {
	kind  models.SkipKind
	words string
}{
	{models.SkipQuarantined, "quarantined by keploy ("},
	{models.SkipThisRunOnly, "skipped by keploy for this run only ("},
	{models.SkipUntilRerecorded, "skipped by keploy until it is re-recorded ("},
}

// A test skipped for this run only (keploy test --judge's rerun skipping a
// test it suggested quarantining) and one skipped until it is re-recorded (a
// cloud replay skipping a test with no mapping entry) each say so under
// their status, with their reason, and neither is called quarantined; a
// quarantine beside them says what it said.
func TestPrintTests_EachSkipIsSaidAsItsKind(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		err := r.printTests(context.Background(), []models.TestResult{
			{TestCaseID: "q", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
			{TestCaseID: "r", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: thisRunOnlyReason, SkipKind: models.SkipThisRunOnly},
			{TestCaseID: "u", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: untilRerecordedReason, SkipKind: models.SkipUntilRerecorded},
		})
		if err != nil {
			t.Fatalf("printTests: %v", err)
		}
		out := buf.String()
		for _, want := range []string{
			"Testcase \"q\" (test-set-0) IGNORED ⏭️ ()\n  quarantined by keploy (" + quarantineReason + ")\n",
			"Testcase \"r\" (test-set-0) IGNORED ⏭️ ()\n  skipped by keploy for this run only (" + thisRunOnlyReason + ")\n",
			"Testcase \"u\" (test-set-0) IGNORED ⏭️ ()\n  skipped by keploy until it is re-recorded (" + untilRerecordedReason + ")\n",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("want %q:\n%s", want, out)
			}
		}
		if strings.Count(out, "quarantined by keploy") != 1 {
			t.Fatalf("want only the quarantine called quarantined:\n%s", out)
		}
	})
}

// JUnit's <skipped message> of each case says its kind of skip, with its
// reason on one line; the body keeps the whole reason.
func TestBuildJUnitSuites_EachSkipIsSaidAsItsKind(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		tests := make([]models.TestResult, 0, len(skipKindLines))
		for _, k := range skipKindLines {
			tests = append(tests, models.TestResult{TestCaseID: "tc-" + string(k.kind), Status: models.TestStatusIgnored, SkipReason: "manual: two\nlines", SkipKind: k.kind})
		}
		cases := buildJUnitSuites(map[string]*models.TestReport{"test-set-1": {Total: len(tests), Ignored: len(tests), Tests: tests}}).Suites[0].Cases
		for i, k := range skipKindLines {
			s := cases[i].Skipped
			if s == nil || s.Message != k.words+"manual: two lines)" || s.Text != "manual: two\nlines" {
				t.Fatalf("kind %q: %+v; want %q and the whole reason", k.kind, s, k.words+"manual: two lines)")
			}
		}
	})
}

// The message of a case of every kind of skip is bounded: a long reason is
// cut at a character's start, marked, as a quarantine's is.
func TestASkipMessageOfEveryKindIsBounded(t *testing.T) {
	long := models.SkipReasonText(models.ManualSkipClass, "x"+strings.Repeat("é", 150), "")
	for _, k := range skipKindLines {
		s := ignoredSkip(models.TestResult{Status: models.TestStatusIgnored, SkipReason: long, SkipKind: k.kind})
		if !strings.HasPrefix(s.Message, k.words+"manual: ") || !strings.HasSuffix(s.Message, "…)") {
			t.Fatalf("kind %q: message %q; want its words, cut and marked", k.kind, s.Message)
		}
		if inner := strings.TrimSuffix(strings.TrimPrefix(s.Message, k.words), "…)"); len(inner) > maxSkipMessageReason || !utf8.ValidString(inner) {
			t.Fatalf("kind %q: the reason in the message is %d bytes; want at most %d, cut at a character", k.kind, len(inner), maxSkipMessageReason)
		}
		if s.Text != long {
			t.Fatalf("kind %q: the body lost the reason: %q", k.kind, s.Text)
		}
	}
}

// skipKindFixture is quarantineFixture with two more tests of test-set-1, one
// skipped for this run only and one until it is re-recorded, and a test of
// test-set-0 that ran though its row carries a skip, which no summary counts.
func skipKindFixture() (*fakeReportDB, *fakeTestDB) {
	rdb, tdb := quarantineFixture(true)
	ran := rdb.reports["test-set-0"]
	ran.Total, ran.Success = 2, 2
	ran.Tests = append(ran.Tests, models.TestResult{Kind: models.HTTP, TestCaseID: "test-p", Status: models.TestStatusPassed, SkipReason: "stale", SkipKind: models.SkipThisRunOnly})
	rep := rdb.reports["test-set-1"]
	rep.Total, rep.Ignored = 4, 4
	rep.Tests = append(rep.Tests,
		models.TestResult{Kind: models.HTTP, TestCaseID: "test-u", Status: models.TestStatusIgnored, SkipReason: untilRerecordedReason, SkipKind: models.SkipUntilRerecorded},
		models.TestResult{Kind: models.HTTP, TestCaseID: "test-r", Status: models.TestStatusIgnored, SkipReason: thisRunOnlyReason, SkipKind: models.SkipThisRunOnly})
	return rdb, tdb
}

// wantSkipKindSummary checks a summary of skipKindFixture's run, whose
// skipped set is listed as set: each kind of skip counted and listed under
// its own words, the quarantine first, and no test listed twice.
func wantSkipKindSummary(t *testing.T, out, set string) {
	t.Helper()
	for _, want := range []string{
		"\tTotal test quarantined: 1\n\tTotal test skipped for this run only: 1\n\tTotal test skipped until re-recorded: 1\n",
		"\nQUARANTINED TEST CASES:\n\t- " + set + "/test-q: flaky/replay_only: failed the replay after record\n" +
			"\nTEST CASES SKIPPED FOR THIS RUN ONLY:\n\t- " + set + "/test-r: " + thisRunOnlyReason + "\n" +
			"\nTEST CASES SKIPPED UNTIL RE-RECORDED:\n\t- " + set + "/test-u: " + untilRerecordedReason + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "test-r:") != 1 || strings.Count(out, "test-u:") != 1 || strings.Contains(out, "test-p:") {
		t.Fatalf("want each skipped test listed once, and the test that ran not at all:\n%s", out)
	}
}

// `keploy report --summary` counts and lists each kind of skip apart.
func TestGenerateReport_SummaryListsEachKindOfSkipApart(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		rdb, tdb := skipKindFixture()
		r := New(zap.NewNop(), &config.Config{Report: config.Report{Summary: true}}, rdb, tdb)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Fatalf("GenerateReport: %v", err)
		}
		wantSkipKindSummary(t, buf.String(), "test-set-1")
	})
}

// The kind is in the report file, so `keploy report --report-path` says the
// same; an empty kind (every quarantine) writes nothing, so a report whose
// skips are all quarantines is the file it was.
func TestGenerateReportFromFile_ASkipKeepsItsKind(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		rdb, _ := skipKindFixture()
		rep := rdb.reports["test-set-1"]
		rep.Name = "test-run-0"
		data, err := yaml.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(data), "skip_kind:"); n != 2 || !strings.Contains(string(data), "skip_kind: this_run_only\n") || !strings.Contains(string(data), "skip_kind: until_rerecorded\n") {
			t.Fatalf("want two skip_kinds in the file, this_run_only's and until_rerecorded's:\n%s", data)
		}
		path := filepath.Join(t.TempDir(), "test-set-1-report.yaml")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		r := New(zap.NewNop(), &config.Config{Report: config.Report{Summary: true}}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.generateReportFromFile(context.Background(), path); err != nil {
			t.Fatalf("generateReportFromFile: %v", err)
		}
		wantSkipKindSummary(t, buf.String(), rep.Name)
	})
}

// A result's JSON (the run report a cloud replay uploads) carries a kind only
// when it has one: a quarantine's, and every other result's, is the JSON it
// was before skips had kinds.
func TestATestResultsJSONCarriesAKindOnlyWhenItHasOne(t *testing.T) {
	for _, c := range []struct {
		row  models.TestResult
		want string
	}{
		{models.TestResult{TestCaseID: "test-q", Status: models.TestStatusIgnored, SkipReason: "why"}, ""},
		{models.TestResult{TestCaseID: "test-r", Status: models.TestStatusIgnored, SkipReason: "why", SkipKind: models.SkipThisRunOnly}, `"skip_kind":"this_run_only"`},
		{models.TestResult{TestCaseID: "test-u", Status: models.TestStatusIgnored, SkipReason: "why", SkipKind: models.SkipUntilRerecorded}, `"skip_kind":"until_rerecorded"`},
	} {
		data, err := json.Marshal(c.row)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(data), `"skip_kind"`); (c.want == "" && got != 0) || (c.want != "" && (got != 1 || !strings.Contains(string(data), c.want))) {
			t.Fatalf("%s: JSON %s; want skip_kind %q", c.row.TestCaseID, data, c.want)
		}
		var back models.TestResult
		if err := json.Unmarshal(data, &back); err != nil || back.SkipKind != c.row.SkipKind {
			t.Fatalf("%s: read back kind %q (%v); want %q", c.row.TestCaseID, back.SkipKind, err, c.row.SkipKind)
		}
	}
}

// quarantineOnlySummary is the summary `keploy report --summary` printed for
// quarantineFixture(true) before skips had kinds, byte for byte.
const quarantineOnlySummary = "<=========================================>\n" +
	" COMPLETE TESTRUN SUMMARY.\n" +
	"\tTotal tests: 3\n" +
	"\tTotal test passed: 1\n" +
	"\tTotal test failed: 0\n" +
	"\tTotal test quarantined: 1\n" +
	"\n" +
	"\tTotal time taken: \"N/A\"\n" +
	"   Test Suite   Total   Passed   Failed   Time Taken   \n" +
	"   test-set-0   1       1        0        N/A          \n" +
	"   test-set-1   2       0        0        N/A          \n" +
	"\n" +
	"FAILED TEST CASES:\n" +
	"\t(none)\n" +
	"\n" +
	"QUARANTINED TEST CASES:\n" +
	"\t- test-set-1/test-q: flaky/replay_only: failed the replay after record\n" +
	"<=========================================>\n"

// A report whose skips are all quarantines prints the summary it printed
// before skips had kinds, byte for byte.
func TestGenerateReport_AQuarantineOnlySummaryIsWhatItWas(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		rdb, tdb := quarantineFixture(true)
		r := New(zap.NewNop(), &config.Config{DisableANSI: true, Report: config.Report{Summary: true}}, rdb, tdb)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Fatalf("GenerateReport: %v", err)
		}
		if got := buf.String(); got != quarantineOnlySummary {
			t.Fatalf("summary\n%q\nwant what it printed before kinds\n%q", got, quarantineOnlySummary)
		}
	})
}

// A report whose skips with a reason are all of one kind that is no
// quarantine counts and lists them under that kind's words, and prints no
// quarantine count or list, nor another kind's.
func TestGenerateReport_SkipsOfOneKindHaveNoOtherKindsLines(t *testing.T) {
	for _, k := range skipKindLines[1:] {
		t.Run(string(k.kind), func(t *testing.T) {
			forEachSwitchMode(t, func(t *testing.T) {
				rdb, tdb := skipKindFixture()
				rep := rdb.reports["test-set-1"]
				for i := range rep.Tests {
					if rep.Tests[i].SkipReason != "" {
						rep.Tests[i].SkipKind = k.kind
					}
				}
				r := New(zap.NewNop(), &config.Config{Report: config.Report{Summary: true}}, rdb, tdb)
				var buf bytes.Buffer
				r.out = bufio.NewWriter(&buf)
				if err := r.GenerateReport(context.Background()); err != nil {
					t.Fatalf("GenerateReport: %v", err)
				}
				out := buf.String()
				total, heading := skipSummaryWords(k.kind)
				want := "\n" + heading + "\n\t- test-set-1/test-q: flaky/replay_only: failed the replay after record\n\t- test-set-1/test-u: " + untilRerecordedReason + "\n\t- test-set-1/test-r: " + thisRunOnlyReason + "\n"
				if !strings.Contains(out, "\t"+total+": 3\n") || !strings.Contains(out, want) {
					t.Fatalf("want the three tests counted and listed under %q:\n%s", heading, out)
				}
				if n := strings.Count(out, "Total test skipped") + strings.Count(out, "Total test quarantined"); n != 1 {
					t.Fatalf("want one count of skipped tests, its kind's:\n%s", out)
				}
				if n := strings.Count(out, "TEST CASES:") + strings.Count(out, "TEST CASES SKIPPED"); n != 2 {
					t.Fatalf("want the failed list and one list of skipped tests, its kind's:\n%s", out)
				}
			})
		})
	}
}

// failingWriter takes the first room bytes written to it and fails every
// write past them, as a full stdout does.
type failingWriter struct{ room int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.room {
		return 0, errors.New("no space left on device")
	}
	w.room -= len(p)
	return len(p), nil
}

// A write of a skipped test's lines that fails is returned at that write,
// naming what it was printing.
func TestASkippedTestsFailedWriteIsReturned(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		newReport := func(room int) *Report {
			r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
			r.out = bufio.NewWriterSize(&failingWriter{room: room}, 16)
			return r
		}
		row := models.TestResult{Kind: models.HTTP, TestCaseID: "test-r", Name: "test-set-1", Status: models.TestStatusIgnored, SkipReason: thisRunOnlyReason, SkipKind: models.SkipThisRunOnly}
		if err := newReport(0).printTests(context.Background(), []models.TestResult{row}); err == nil || !strings.Contains(err.Error(), "why a test was skipped") {
			t.Fatalf("printTests: %v; want the failed write of the skip's line", err)
		}
		rdb, _ := skipKindFixture()
		if err := newReport(0).printSummary(rdb.reports); err == nil || !strings.Contains(err.Error(), "count of skipped tests") {
			t.Fatalf("printSummary: %v; want the failed write of the count of skipped tests", err)
		}
		// Room for all of a quarantine-only summary but its list, which is
		// the write that fails.
		rdb, _ = quarantineFixture(true)
		if err := newReport(strings.Index(quarantineOnlySummary, "\nQUARANTINED TEST CASES:")).printSummary(rdb.reports); err == nil || !strings.Contains(err.Error(), "the skipped tests") {
			t.Fatalf("printSummary: %v; want the failed write of the list of skipped tests", err)
		}
	})
}

// A kind keploy does not know is said as a quarantine, as a skip of no kind
// is: in the test's line, in JUnit and in the summary.
func TestAnUnknownSkipKindIsSaidAsBefore(t *testing.T) {
	forEachSwitchMode(t, func(t *testing.T) {
		row := models.TestResult{Kind: models.HTTP, TestCaseID: "test-u", Name: "test-set-1", Status: models.TestStatusIgnored, SkipReason: "why u", SkipKind: models.SkipKind("a_later_kind")}
		r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.printTests(context.Background(), []models.TestResult{row}); err != nil {
			t.Fatalf("printTests: %v", err)
		}
		if out := buf.String(); !strings.Contains(out, "  quarantined by keploy (why u)\n") {
			t.Fatalf("the test's line:\n%s", out)
		}
		if s := ignoredSkip(row); s.Message != "quarantined by keploy (why u)" {
			t.Fatalf("JUnit message %q", s.Message)
		}
		if skipped := skippedTests(map[string]*models.TestReport{"test-set-1": {Tests: []models.TestResult{row}}}); len(skipped) != 1 || len(skipped[models.SkipQuarantined]) != 1 {
			t.Fatalf("summary: %v; want it quarantined", skipped)
		}
	})
}
