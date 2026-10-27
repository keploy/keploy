package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
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

const quarantineReason = "flaky/replay_only: failed the replay after record with no keploy evidence"

// A case keploy skipped for a reason carries it in the <skipped> message, the
// attribute CI dashboards show for a skipped case, and in the body.
func TestBuildJUnitSuites_QuarantinedCaseSaysWhy(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:   2,
			Ignored: 2,
			Tests: []models.TestResult{
				{TestCaseID: "tc-quarantined", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
				{TestCaseID: "tc-ignored", Status: models.TestStatusIgnored},
			},
		},
	}
	suite := buildJUnitSuites(reports).Suites[0]
	if suite.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", suite.Skipped)
	}
	q := suite.Cases[0].Skipped
	if q == nil {
		t.Fatal("the quarantined case has no <skipped> element")
	}
	if want := "quarantined by keploy (" + quarantineReason + ")"; q.Message != want {
		t.Fatalf("<skipped message> = %q, want %q", q.Message, want)
	}
	if q.Text != quarantineReason {
		t.Fatalf("<skipped> body = %q, want the reason", q.Text)
	}
	// A case skipped without a reason keeps the historical literal and no body.
	if p := suite.Cases[1].Skipped; p == nil || p.Message != "ignored test case" || p.Text != "" {
		t.Fatalf("a case skipped without a reason changed: %+v", p)
	}

	// And it survives as XML a consumer reads back.
	data, err := xml.Marshal(buildJUnitSuites(reports))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed junitTestSuites
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("the JUnit document does not parse back: %v", err)
	}
	if got := parsed.Suites[0].Cases[0].Skipped.Message; !strings.HasPrefix(got, "quarantined by keploy (") {
		t.Fatalf("round trip: message = %q", got)
	}
}

// The text report prints the reason under an IGNORED test's status line.
func TestPrintTests_QuarantinedTestSaysWhy(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printTests(context.Background(), []models.TestResult{
		{TestCaseID: "q", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
		{TestCaseID: "i", Name: "test-set-0", Status: models.TestStatusIgnored},
	})
	if err != nil {
		t.Fatalf("printTests: %v", err)
	}
	out := buf.String()
	want := "Testcase \"q\" (test-set-0) IGNORED ⏭️ ()\n  quarantined by keploy (" + quarantineReason + ")\n"
	if !strings.Contains(out, want) {
		t.Fatalf("the quarantined test does not say why:\n%s", out)
	}
	if strings.Count(out, "quarantined by keploy") != 1 {
		t.Fatalf("a test skipped without a reason grew a quarantine line:\n%s", out)
	}
}

// The message of a quarantined case is one line and bounded, whatever the
// reason: a multi-line reason is put on one line, and a long one is cut at a
// character's start, marked. The body keeps the whole reason.
func TestQuarantinedSkipMessageIsOneBoundedLine(t *testing.T) {
	// "manual: x" is 9 bytes, so the 200th byte falls inside an "é".
	long := models.SkipReasonText(models.ManualSkipClass, "x"+strings.Repeat("é", 150)+"\nsecond line", "")
	s := ignoredSkip(models.TestResult{Status: models.TestStatusIgnored, SkipReason: long})
	if strings.ContainsAny(s.Message, "\r\n") {
		t.Fatalf("message has a line break: %q", s.Message)
	}
	if !strings.HasPrefix(s.Message, "quarantined by keploy (manual: ") || !strings.HasSuffix(s.Message, "…)") {
		t.Fatalf("message %q; want the quarantine form, cut and marked", s.Message)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s.Message, "quarantined by keploy ("), "…)")
	if len(inner) > maxSkipMessageReason || !utf8.ValidString(inner) {
		t.Fatalf("the reason in the message is %d bytes (valid UTF-8: %v); want at most %d, cut at a character", len(inner), utf8.ValidString(inner), maxSkipMessageReason)
	}
	if s.Text != long {
		t.Fatalf("the body lost the reason: %q", s.Text)
	}
	short := ignoredSkip(models.TestResult{Status: models.TestStatusIgnored, SkipReason: "flaky/flipped:\n order"})
	if short.Message != "quarantined by keploy (flaky/flipped: order)" {
		t.Fatalf("message %q; want the reason on one line, uncut", short.Message)
	}
}

// A legacy-format report counts an IGNORED test as skipped under the switch,
// not as passed; with the switch off, as passed, as before.
func TestProcessLegacySummaryCountsIgnored(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "p", Status: models.TestStatusPassed},
		{TestCaseID: "q", Status: models.TestStatusIgnored, SkipReason: quarantineReason},
	}
	ansiWas := models.IsAnsiDisabled
	models.IsAnsiDisabled = true
	t.Cleanup(func() { models.IsAnsiDisabled = ansiWas })
	for _, on := range []bool{false, true} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.processLegacySummary(tests); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		wantPassed, wantIgnored := "Total test passed: 2", false
		if on {
			wantPassed, wantIgnored = "Total test passed: 1", true
		}
		if !strings.Contains(out, wantPassed) || strings.Contains(out, "Total test ignored: 1") != wantIgnored {
			t.Fatalf("switch %v: summary\n%s\nwant %q and an ignored line %v", on, out, wantPassed, wantIgnored)
		}
	}
}

// The text report's quarantine line is one line, whatever the reason.
func TestPrintTests_AMultiLineReasonIsOneLine(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	err := r.printTests(context.Background(), []models.TestResult{
		{TestCaseID: "q", Name: "test-set-0", Status: models.TestStatusIgnored, SkipReason: "manual: waits on\na fix"},
	})
	if err != nil {
		t.Fatalf("printTests: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "  quarantined by keploy (manual: waits on a fix)\n") {
		t.Fatalf("the reason is not on one line:\n%s", out)
	}
}

// quarantineFixture is a run whose test-set-1 keploy skipped whole, one test
// for a (multi-line) reason and one with none, next to a set that ran.
func quarantineFixture(withReason bool) (*fakeReportDB, *fakeTestDB) {
	reason := ""
	if withReason {
		reason = models.SkipReasonText("flaky", "replay_only", "failed the replay\nafter record")
	}
	return &fakeReportDB{
		runIDs: []string{"test-run-0"},
		reports: map[string]*models.TestReport{
			"test-set-0": {TestSet: "test-set-0", Total: 1, Success: 1, Tests: []models.TestResult{
				{Kind: models.HTTP, TestCaseID: "test-1", Status: models.TestStatusPassed},
			}},
			"test-set-1": {TestSet: "test-set-1", Status: string(models.TestSetStatusIgnored), Total: 2, Ignored: 2, Tests: []models.TestResult{
				{Kind: models.HTTP, TestCaseID: "test-q", Status: models.TestStatusIgnored, SkipReason: reason},
				{Kind: models.HTTP, TestCaseID: "test-i", Status: models.TestStatusIgnored},
			}},
		},
	}, &fakeTestDB{testSets: []string{"test-set-0", "test-set-1"}}
}

// `keploy report --summary` lists the tests keploy quarantined, with their
// reasons, and counts them; a report with no reason prints what it printed
// before.
func TestGenerateReport_SummaryListsTheQuarantinedTests(t *testing.T) {
	for _, withReason := range []bool{false, true} {
		rdb, tdb := quarantineFixture(withReason)
		r := New(zap.NewNop(), &config.Config{Report: config.Report{Summary: true}}, rdb, tdb)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Fatalf("GenerateReport: %v", err)
		}
		out := buf.String()
		listed := strings.Contains(out, "\nQUARANTINED TEST CASES:\n\t- test-set-1/test-q: flaky/replay_only: failed the replay after record\n")
		counted := strings.Contains(out, "\tTotal test quarantined: 1\n")
		if listed != withReason || counted != withReason || strings.Contains(out, "test-i:") {
			t.Fatalf("with a reason %v: listed %v, counted %v; want both %v, and no test without a reason\n%s", withReason, listed, counted, withReason, out)
		}
	}
}

// The same from a report file (`keploy report --summary --report-path`).
func TestGenerateReportFromFile_SummaryListsTheQuarantinedTests(t *testing.T) {
	rdb, _ := quarantineFixture(true)
	rep := rdb.reports["test-set-1"]
	rep.Name = "test-run-0"
	data, err := yaml.Marshal(rep)
	if err != nil {
		t.Fatal(err)
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
	if out := buf.String(); !strings.Contains(out, "QUARANTINED TEST CASES:") || !strings.Contains(out, "test-q: flaky/replay_only: failed the replay after record") {
		t.Fatalf("the file's summary does not list the quarantined test:\n%s", out)
	}
}
