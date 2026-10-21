package report

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// fileWithContent is a test result as a report file written with the switch
// on (or by an older keploy) holds it: its call carries the application's
// values.
func fileWithContent() models.TestResult {
	return models.TestResult{FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{
		Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT * FROM users WHERE email = 'kp-file-literal'", ClosestMock: "mock-1",
		MatchPhase: models.MatchPhaseBody, Diff: "actual: SELECT 'kp-file-diff'",
		FieldDiffs: []models.MockFieldDiff{
			{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-file-rec", Actual: "kp-file-live"},
			{Path: "body.gone", Kind: models.DiffKindMissingInLive, Expected: "kp-file-gone"},
		},
		ClosestMockReq: "kp-file-mock-req", ReceivedReq: "kp-file-live-req",
	}}}}
}

// keploy report reads a report file it did not write: by default it prints
// the call and the fields that differ, and none of the values the file holds.
func TestKeployReportPrintsNoContentFromAFileByDefault(t *testing.T) {
	var sb strings.Builder
	renderUnmatchedCalls(&sb, fileWithContent())
	out := sb.String()
	for _, c := range []string{"kp-file-literal", "kp-file-diff", "kp-file-rec", "kp-file-live", "kp-file-gone", "kp-file-mock-req", "kp-file-live-req"} {
		if strings.Contains(out, c) {
			t.Errorf("keploy report printed %q:\n%s", c, out)
		}
	}
	for _, want := range []string{"COM_QUERY SELECT * FROM users WHERE email = ?", "body.email: value changed", "body.gone: absent in live call", models.ContentWithheldNote} {
		if !strings.Contains(out, want) {
			t.Errorf("keploy report does not print %q:\n%s", want, out)
		}
	}
}

// With the switch on it prints the file as it did.
func TestKeployReportPrintsAFilesContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	var sb strings.Builder
	renderUnmatchedCalls(&sb, fileWithContent())
	out := sb.String()
	for _, want := range []string{"kp-file-literal", `body.email: recorded "kp-file-rec", live "kp-file-live"`, `body.gone: recorded "kp-file-gone", absent in live call`} {
		if !strings.Contains(out, want) {
			t.Errorf("with the switch on keploy report does not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, models.ContentWithheldNote) {
		t.Errorf("with the switch on keploy report withholds:\n%s", out)
	}
}

// Every format of keploy report reads the report file through one door
// (getReport): by default none of them prints or writes a value the file
// holds, a test's call or the set's, and each still names the call.
func TestEveryReportFormatReadsAFileWithoutItsContentByDefault(t *testing.T) {
	file := func() *fakeReportDB {
		f := fileWithContent()
		f.Kind, f.TestCaseID, f.Status = models.HTTP, "test-1", models.TestStatusFailed
		return &fakeReportDB{runIDs: []string{"test-run-0"}, reports: map[string]*models.TestReport{"test-set-0": {
			TestSet: "test-set-0", Status: string(models.TestSetStatusFailed), Total: 1, Failure: 1, Tests: []models.TestResult{f},
			UnclaimedCalls: []models.UnmatchedCall{{Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT 'kp-file-unclaimed'", Kind: models.MissOutsideTest}},
		}}}
	}
	canaries := []string{"kp-file-literal", "kp-file-diff", "kp-file-rec", "kp-file-live", "kp-file-gone", "kp-file-mock-req", "kp-file-live-req", "kp-file-unclaimed"}
	// names is what each format says of the calls: the text and the blob
	// the test's call, JUnit the set's (the line for calls after the last
	// test), NDJSON neither (it carries no calls), only the test.
	formats := []struct {
		name  string
		cfg   *config.Config
		names string
	}{
		{"text", &config.Config{}, "users WHERE email = ?"},
		{"--json", &config.Config{JSONOutput: true}, "users WHERE email = ?"},
		{"--format json", &config.Config{Report: config.Report{Format: config.ReportFormatJSON}}, `"test_case_id":"test-1"`},
		{"--format junit", &config.Config{Report: config.Report{Format: config.ReportFormatJUnit}}, "MySQL COM_QUERY SELECT ?"},
	}
	run := func(t *testing.T, cfg *config.Config) string {
		t.Helper()
		r := New(zap.NewNop(), cfg, file(), &fakeTestDB{testSets: []string{"test-set-0"}})
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Errorf("GenerateReport: %v", err)
		}
		_ = r.out.Flush()
		return buf.String()
	}
	for _, f := range formats {
		t.Run(f.name, func(t *testing.T) {
			out := run(t, f.cfg)
			for _, c := range canaries {
				if strings.Contains(out, c) {
					t.Errorf("%s printed %q:\n%s", f.name, c, out)
				}
			}
			if !strings.Contains(out, f.names) {
				t.Errorf("%s does not say %q:\n%s", f.name, f.names, out)
			}
		})
	}
	// With the switch on the file is read as written: the blob carries it.
	t.Cleanup(logcontent.Set(true))
	if out := run(t, &config.Config{JSONOutput: true}); !strings.Contains(out, "kp-file-rec") || !strings.Contains(out, "kp-file-unclaimed") {
		t.Errorf("with the switch on --json lost the file's content:\n%s", out)
	}
}

// A report file written without content is printed as such when keploy
// report is opted in: each field by its path and kind, and the note, never a
// recorded or live value that reads as empty.
func TestKeployReportSaysAWithheldFileIsWithheld(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	var sb strings.Builder
	renderUnmatchedCalls(&sb, models.TestResult{FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{
		Protocol: "HTTP", ActualSummary: "POST /api/users", ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody, ContentWithheld: true,
		FieldDiffs: []models.MockFieldDiff{{Path: "body.email", Kind: models.DiffKindValueChanged}, {Path: "body.gone", Kind: models.DiffKindMissingInLive}},
	}}}})
	out := sb.String()
	for _, want := range []string{"body.email: value changed", "body.gone: absent in live call", models.ContentWithheldNote} {
		if !strings.Contains(out, want) {
			t.Errorf("keploy report does not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `recorded ""`) || strings.Contains(out, `live ""`) {
		t.Errorf("keploy report prints a withheld value as empty:\n%s", out)
	}
}

// A file named by --report-path is read by the same rule as a run's, in
// every format keploy report prints one in (text, --json, --test-case),
// whether keploy reads it as a report or, as an older file, by its tests
// alone: by default none of them prints a value the file holds, and each
// still names the call.
func TestKeployReportReadsAReportPathWithoutItsContentByDefault(t *testing.T) {
	f := fileWithContent()
	f.Kind, f.TestCaseID, f.Status = models.HTTP, "test-1", models.TestStatusFailed
	canonical, err := yaml.Marshal(models.TestReport{Name: "test-run-0", TestSet: "test-set-0", Status: string(models.TestSetStatusFailed), Total: 1, Failure: 1, Tests: []models.TestResult{f}})
	if err != nil {
		t.Fatal(err)
	}
	tests, err := yaml.Marshal(struct {
		Tests []models.TestResult `yaml:"tests"`
	}{[]models.TestResult{f}})
	if err != nil {
		t.Fatal(err)
	}
	// A name keploy cannot read as a report's: the file is read by its
	// tests alone (parseAndProcessLegacyReportFormat).
	legacy := append([]byte("name: {not: a name}\n"), tests...)
	dir := t.TempDir()
	files := map[string]string{"report": filepath.Join(dir, "test-set-0-report.yaml"), "legacy": filepath.Join(dir, "legacy-report.yaml")}
	if err := os.WriteFile(files["report"], canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files["legacy"], legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	canaries := []string{"kp-file-literal", "kp-file-diff", "kp-file-rec", "kp-file-live", "kp-file-gone", "kp-file-mock-req", "kp-file-live-req"}
	run := func(t *testing.T, path string, cfg config.Config) string {
		t.Helper()
		cfg.Report.ReportPath = path
		r := New(zap.NewNop(), &cfg, &fakeReportDB{}, &fakeTestDB{})
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Errorf("GenerateReport: %v", err)
		}
		_ = r.out.Flush()
		return buf.String()
	}
	for _, file := range []string{"report", "legacy"} {
		for _, f := range []struct {
			name string
			cfg  config.Config
		}{
			{"text", config.Config{}},
			{"--json", config.Config{JSONOutput: true}},
			{"--test-case", config.Config{Report: config.Report{TestCaseIDs: []string{"test-1"}}}},
		} {
			t.Run(file+" "+f.name, func(t *testing.T) {
				out := run(t, files[file], f.cfg)
				for _, c := range canaries {
					if strings.Contains(out, c) {
						t.Errorf("%s printed %q:\n%s", f.name, c, out)
					}
				}
				if !strings.Contains(out, "users WHERE email = ?") {
					t.Errorf("%s does not name the call:\n%s", f.name, out)
				}
			})
		}
	}
	// With the switch on the file is read as written.
	t.Cleanup(logcontent.Set(true))
	for _, file := range []string{"report", "legacy"} {
		if out := run(t, files[file], config.Config{JSONOutput: true}); !strings.Contains(out, "kp-file-rec") || !strings.Contains(out, "kp-file-live-req") {
			t.Errorf("with the switch on --json of the %s file lost its content:\n%s", file, out)
		}
	}
}
