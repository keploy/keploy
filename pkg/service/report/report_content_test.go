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

// fileMongoRequest is a Mongo mock's summary as a report file written with
// the switch on (or by an older keploy) names it: the whole request the
// recorder wrote as the mock's operation, createUser's password in it.
const fileMongoRequest = `MongoDB {"flagBits":0,"sections":["{ SectionSingle msg: {\"createUser\":\"kp-file-mongo-user\",\"pwd\":\"kp-file-mongo-pwd\"} }"],"checksum":0}`

// fileWithContent is a test result as a report file written with the switch
// on (or by an older keploy) holds it: its call carries the application's
// values, and so do the mocks its matched calls and dependency rows name.
func fileWithContent() models.TestResult {
	return models.TestResult{
		FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{
			Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT * FROM users WHERE email = 'kp-file-literal'", ClosestMock: "mock-1",
			MatchPhase: models.MatchPhaseBody, Diff: "actual: SELECT 'kp-file-diff'",
			FieldDiffs: []models.MockFieldDiff{
				{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-file-rec", Actual: "kp-file-live"},
				{Path: "body.gone", Kind: models.DiffKindMissingInLive, Expected: "kp-file-gone"},
			},
			ClosestMockReq: "kp-file-mock-req", ReceivedReq: "kp-file-live-req",
		}}, MatchedCalls: []models.MatchedCall{
			{MockName: "mock-2", Protocol: string(models.HTTP), Summary: "GET http://auth-svc:8080/v1/session?token=kp-file-matched-token"},
			{MockName: "mock-3", Protocol: string(models.Mongo), Summary: fileMongoRequest},
		}},
		Result: models.Result{DepResult: []models.DepResult{{
			Name: models.DepRowName(0, "mongo", fileMongoRequest), Type: "mongo",
			Meta: []models.DepMetaResult{{Key: models.DepKeyPresence, Expected: models.DepPresenceConsumed, Actual: models.DepPresenceMissing}},
		}}},
	}
}

// fileCanaries are every value fileWithContent's test holds.
var fileCanaries = []string{"kp-file-literal", "kp-file-diff", "kp-file-rec", "kp-file-live", "kp-file-gone", "kp-file-mock-req", "kp-file-live-req",
	"kp-file-matched-token", "kp-file-mongo-user", "kp-file-mongo-pwd"}

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
	for _, want := range []string{"COM_QUERY SELECT * FROM users WHERE email = ?", "body.email: value changed", "body.gone: absent in live call",
		models.ContentWithheldNote + " (for a report written without them, set it for the replay)"} {
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
	canaries := append([]string{"kp-file-unclaimed"}, fileCanaries...)
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
	if out := run(t, &config.Config{JSONOutput: true}); !strings.Contains(out, "kp-file-rec") || !strings.Contains(out, "kp-file-unclaimed") ||
		!strings.Contains(out, "kp-file-matched-token") || !strings.Contains(out, "kp-file-mongo-pwd") {
		t.Errorf("with the switch on --json lost the file's content:\n%s", out)
	}
}

// fileWrittenByDefault is a failed test as the replay writes it to its report
// file with KEPLOY_LOG_CONTENT unset: a matched Mongo call and the row of a
// Mongo dependency it did not consume, each named by the mock's command, and
// marked written without content.
const fileWrittenByDefault = `
kind: Http
name: test-1
status: FAILED
test_case_id: test-1
failure_info:
  matched_calls:
    - mock_name: mock-4
      protocol: Mongo
      summary: MongoDB find
      content_withheld: true
result:
  status_code: {normal: true, expected: 200, actual: 200}
  headers_result: []
  body_result: []
  dep_result:
    - name: deps[0] mongo mongo:27017 find (presence)
      type: mongo
      meta:
        - {normal: false, key: presence, expected: consumed, actual: missing}
      content_withheld: true
  deps_checked: true
`

// defaultWrittenMatched and defaultWrittenRow are what fileWrittenByDefault
// names its matched call and its dependency row by.
const (
	defaultWrittenMatched = "MongoDB find"
	defaultWrittenRow     = "deps[0] mongo mongo:27017 find (presence)"
)

// Every format of keploy report, from a run's report and from a file named by
// --report-path (read as a report or by its tests alone), prints a file the
// replay wrote by default as the file names its calls: a Mongo dependency
// row by its command, as its YAML and the NDJSON effects' stable name do.
// What the writer withheld is not withheld again: its command, read as the
// request a Mongo mock records, would name none.
func TestEveryReportFormatPrintsADefaultWrittenFileAsWritten(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	var f models.TestResult
	if err := yaml.Unmarshal([]byte(fileWrittenByDefault), &f); err != nil {
		t.Fatal(err)
	}
	report := func() *models.TestReport {
		return &models.TestReport{Name: "test-run-0", TestSet: "test-set-0", Status: string(models.TestSetStatusFailed), Total: 1, Failure: 1, Tests: []models.TestResult{f}}
	}
	run := func(t *testing.T, db *fakeReportDB, cfg config.Config) string {
		t.Helper()
		r := New(zap.NewNop(), &cfg, db, &fakeTestDB{testSets: []string{"test-set-0"}})
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.GenerateReport(context.Background()); err != nil {
			t.Errorf("GenerateReport: %v", err)
		}
		_ = r.out.Flush()
		return buf.String()
	}
	check := func(t *testing.T, name, out string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not print the file's %q:\n%s", name, want, out)
			}
		}
	}
	for _, c := range []struct {
		name  string
		cfg   config.Config
		wants []string
	}{
		{"text", config.Config{}, []string{defaultWrittenRow}},
		{"--json", config.Config{JSONOutput: true}, []string{defaultWrittenRow, defaultWrittenMatched}},
		{"--format json", config.Config{Report: config.Report{Format: config.ReportFormatJSON}}, []string{defaultWrittenRow}},
		{"--format junit", config.Config{Report: config.Report{Format: config.ReportFormatJUnit}}, []string{defaultWrittenRow}},
		{"--test-case", config.Config{Report: config.Report{TestCaseIDs: []string{"test-1"}}}, []string{defaultWrittenRow}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeReportDB{runIDs: []string{"test-run-0"}, reports: map[string]*models.TestReport{"test-set-0": report()}}
			check(t, c.name, run(t, db, c.cfg), c.wants...)
		})
	}

	canonical, err := yaml.Marshal(report())
	if err != nil {
		t.Fatal(err)
	}
	tests, err := yaml.Marshal(struct {
		Tests []models.TestResult `yaml:"tests"`
	}{[]models.TestResult{f}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string][]byte{"report": canonical, "legacy": append([]byte("name: {not: a name}\n"), tests...)}
	for _, file := range []string{"report", "legacy"} {
		path := filepath.Join(dir, file+"-report.yaml")
		if err := os.WriteFile(path, files[file], 0o600); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name  string
			cfg   config.Config
			wants []string
		}{
			{"text", config.Config{}, []string{defaultWrittenRow}},
			{"--json", config.Config{JSONOutput: true}, []string{defaultWrittenRow, defaultWrittenMatched}},
			{"--test-case", config.Config{Report: config.Report{TestCaseIDs: []string{"test-1"}}}, []string{defaultWrittenRow}},
		} {
			t.Run("--report-path "+file+" "+c.name, func(t *testing.T) {
				c.cfg.Report.ReportPath = path
				check(t, c.name, run(t, &fakeReportDB{}, c.cfg), c.wants...)
			})
		}
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
	// Setting the switch here brings back nothing the file was written
	// without: the note says the replay must run with it.
	for _, want := range []string{"body.email: value changed", "body.gone: absent in live call",
		"values were withheld when this report was written; replay with KEPLOY_LOG_CONTENT=1 to include them"} {
		if !strings.Contains(out, want) {
			t.Errorf("keploy report does not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, models.ContentWithheldNote) {
		t.Errorf("opted in, keploy report says setting the switch shows a file's withheld values:\n%s", out)
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
	canaries := fileCanaries
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
				if f.name == "--json" && (!strings.Contains(out, "GET http://auth-svc:8080/v1/session") || !strings.Contains(out, "MongoDB createUser")) {
					t.Errorf("%s does not name the matched calls and the dependency:\n%s", f.name, out)
				}
			})
		}
	}
	// With the switch on the file is read as written.
	t.Cleanup(logcontent.Set(true))
	for _, file := range []string{"report", "legacy"} {
		if out := run(t, files[file], config.Config{JSONOutput: true}); !strings.Contains(out, "kp-file-rec") || !strings.Contains(out, "kp-file-live-req") ||
			!strings.Contains(out, "kp-file-matched-token") || !strings.Contains(out, "kp-file-mongo-pwd") {
			t.Errorf("with the switch on --json of the %s file lost its content:\n%s", file, out)
		}
	}
}
