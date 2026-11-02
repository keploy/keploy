package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// shareFixture is a run whose test-set-1 passed 1 of its 4 tests, record
// having set the other 3 aside, next to a set that compared all it ran.
func shareFixture() (*fakeReportDB, *fakeTestDB) {
	unmocked := models.SkipReasonText("keploy", "unmocked_call", "Pulsar SEND had no recorded answer")
	return &fakeReportDB{
		runIDs: []string{"test-run-0"},
		reports: map[string]*models.TestReport{
			"test-set-0": {TestSet: "test-set-0", Status: string(models.TestSetStatusPassed), Total: 1, Success: 1, Tests: []models.TestResult{
				{Kind: models.HTTP, TestCaseID: "test-1", Status: models.TestStatusPassed},
			}},
			"test-set-1": {TestSet: "test-set-1", Status: string(models.TestSetStatusPassed), Total: 4, Success: 1, Ignored: 3, Tests: []models.TestResult{
				{Kind: models.HTTP, TestCaseID: "test-p", Status: models.TestStatusPassed},
				{Kind: models.HTTP, TestCaseID: "test-q1", Status: models.TestStatusIgnored, SkipReason: unmocked},
				{Kind: models.HTTP, TestCaseID: "test-q2", Status: models.TestStatusIgnored, SkipReason: unmocked},
				{Kind: models.HTTP, TestCaseID: "test-i", Status: models.TestStatusIgnored},
			}},
		},
	}, &fakeTestDB{testSets: []string{"test-set-0", "test-set-1"}}
}

func generate(t *testing.T, cfg *config.Config) string {
	t.Helper()
	rdb, tdb := shareFixture()
	r := New(zap.NewNop(), cfg, rdb, tdb)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.GenerateReport(context.Background()); err != nil {
		t.Fatalf("GenerateReport: %v", err)
	}
	return buf.String()
}

// `keploy report --summary` ends with the run's compared share.
func TestReportSummaryEndsWithTheComparedShare(t *testing.T) {
	out := generate(t, &config.Config{Report: config.Report{Summary: true}})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := "2 of 5 compared; 3 set aside: 2 keploy/unmocked_call, 1 for 1 other reason"
	if last := lines[len(lines)-1]; last != want {
		t.Fatalf("the summary's last line is %q, want %q:\n%s", last, want, out)
	}
}

// A run that set nothing aside ends as it did.
func TestReportSummaryOfARunThatSetNothingAside(t *testing.T) {
	rdb, tdb := shareFixture()
	delete(rdb.reports, "test-set-1")
	tdb.testSets = []string{"test-set-0"}
	r := New(zap.NewNop(), &config.Config{Report: config.Report{Summary: true}}, rdb, tdb)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.GenerateReport(context.Background()); err != nil {
		t.Fatalf("GenerateReport: %v", err)
	}
	if out := buf.String(); !strings.HasSuffix(out, "<=========================================>\n") || strings.Contains(out, "compared;") {
		t.Fatalf("a run that set nothing aside does not end with the summary's separator:\n%s", out)
	}
}

// `keploy report --json` carries each set's share, of the tests it shows:
// --test-case recounts it.
func TestReportJSONCarriesTheShare(t *testing.T) {
	decode := func(out string) map[string]*models.TestReport {
		t.Helper()
		var reports map[string]*models.TestReport
		if err := json.Unmarshal([]byte(out), &reports); err != nil {
			t.Fatalf("--json output does not parse: %v\n%s", err, out)
		}
		return reports
	}
	reports := decode(generate(t, &config.Config{JSONOutput: true}))
	if got := reports["test-set-1"].SetAside.Line(); got != "1 of 4 compared; 3 set aside: 2 keploy/unmocked_call, 1 for 1 other reason" {
		t.Fatalf("test-set-1's share: %q", got)
	}
	if s := reports["test-set-0"].SetAside; s != nil {
		t.Fatalf("test-set-0 set nothing aside and carries %+v", s)
	}
	shown := decode(generate(t, &config.Config{JSONOutput: true, Report: config.Report{TestCaseIDs: []string{"test-p", "test-q1"}}}))
	if got := shown["test-set-1"].SetAside.Line(); got != "1 of 2 compared; 1 set aside: 1 keploy/unmocked_call" {
		t.Fatalf("with --test-case, test-set-1's share is %q, want that of the two tests shown", got)
	}
}

// recountShown counts each status of the rows a report shows.
func TestRecountShown(t *testing.T) {
	rep := &models.TestReport{Total: 9, Success: 9, Failure: 9, Ignored: 9, Obsolete: 9, Tests: []models.TestResult{
		{Status: models.TestStatusPassed}, {Status: models.TestStatusPassed},
		{Status: models.TestStatusFailed}, {Status: models.TestStatusFailed}, {Status: models.TestStatusFailed}, {Status: models.TestStatusFailed},
		{Status: models.TestStatusIgnored},
		{Status: models.TestStatusObsolete}, {Status: models.TestStatusObsolete}, {Status: models.TestStatusObsolete},
		// No count is of a status recountShown does not count: each differs
		// from these five.
		{Status: models.TestStatusRunning}, {Status: models.TestStatusRunning}, {Status: models.TestStatusRunning}, {Status: models.TestStatusRunning}, {Status: models.TestStatusRunning},
	}}
	recountShown(rep)
	if rep.Total != 15 || rep.Success != 2 || rep.Failure != 4 || rep.Ignored != 1 || rep.Obsolete != 3 {
		t.Fatalf("recounted %+v", rep)
	}
}

// A run's share skips a set with no report, as the summary's list does.
func TestReportsShareSkipsAMissingReport(t *testing.T) {
	got := reportsShare(map[string]*models.TestReport{"a": nil, "b": {Total: 2, Success: 1, Ignored: 1}})
	if got.Line() != "1 of 2 compared; 1 set aside: 1 "+models.NoSkipReason {
		t.Fatalf("share %q", got.Line())
	}
}

// A JUnit suite of a set that set tests aside carries its share as the
// keploy.compared property; one that set none aside has no property; with
// --test-case it is of the cases shown.
func TestJUnitSuiteCarriesTheShare(t *testing.T) {
	compared := func(cfg *config.Config) map[string]string {
		t.Helper()
		out := generate(t, cfg)
		var doc junitTestSuites
		if err := xml.Unmarshal([]byte(out[strings.Index(out, "<testsuites"):]), &doc); err != nil {
			t.Fatalf("JUnit does not parse: %v\n%s", err, out)
		}
		got := map[string]string{}
		for _, s := range doc.Suites {
			if s.Properties == nil {
				continue
			}
			for _, p := range s.Properties.Property {
				if p.Name == "keploy.compared" {
					got[s.Name] = p.Value
				}
			}
		}
		return got
	}
	got := compared(&config.Config{Report: config.Report{Format: config.ReportFormatJUnit}})
	want := map[string]string{"test-set-1": "1 of 4 compared; 3 set aside: 2 keploy/unmocked_call, 1 for 1 other reason"}
	if len(got) != 1 || got["test-set-1"] != want["test-set-1"] {
		t.Fatalf("keploy.compared properties %v, want %v", got, want)
	}
	shown := compared(&config.Config{Report: config.Report{Format: config.ReportFormatJUnit, TestCaseIDs: []string{"test-q1"}}})
	if shown["test-set-1"] != "0 of 1 compared; 1 set aside: 1 keploy/unmocked_call" {
		t.Fatalf("with --test-case: %v, want the share of the case shown", shown)
	}
}

// The share is a property next to a capture verification's, not instead.
func TestJUnitShareKeepsTheCaptureProperties(t *testing.T) {
	rep := &models.TestReport{Total: 2, Success: 1, Ignored: 1,
		CaptureVerification: &models.CaptureVerification{Status: "unverified", Reason: "no count"},
		Tests:               []models.TestResult{{TestCaseID: "a", Status: models.TestStatusPassed}, {TestCaseID: "b", Status: models.TestStatusIgnored}}}
	props := buildJUnitSuite("s", rep).Properties.Property
	names := make([]string, 0, len(props))
	for _, p := range props {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "keploy.capture_verification,keploy.capture_verification.reason,keploy.compared" {
		t.Fatalf("properties %v", names)
	}
}
