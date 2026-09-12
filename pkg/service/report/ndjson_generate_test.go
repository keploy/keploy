package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type fakeReportDB struct {
	runIDs  []string
	reports map[string]*models.TestReport
}

func (f *fakeReportDB) GetAllTestRunIDs(context.Context) ([]string, error) { return f.runIDs, nil }
func (f *fakeReportDB) GetReport(_ context.Context, _ string, testSetID string) (*models.TestReport, error) {
	return f.reports[testSetID], nil
}

type fakeTestDB struct{ testSets []string }

func (f *fakeTestDB) GetReportTestSets(context.Context, string) ([]string, error) {
	return f.testSets, nil
}

func newFakeReportFixture() (*fakeReportDB, *fakeTestDB) {
	return &fakeReportDB{
		runIDs: []string{"test-run-0", "test-run-1"},
		reports: map[string]*models.TestReport{
			"test-set-0": {
				TestSet: "test-set-0",
				Total:   2,
				Tests: []models.TestResult{
					{
						Kind: models.HTTP, TestCaseID: "test-1", Status: models.TestStatusFailed,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.DependencyMissing},
						},
						Result: models.Result{
							DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true, DepsConsumed: 5,
						},
					},
					{Kind: models.HTTP, TestCaseID: "test-2", Status: models.TestStatusPassed},
				},
			},
		},
	}, &fakeTestDB{testSets: []string{"test-set-0"}}
}

func runGenerateReport(t *testing.T, cfg *config.Config) string {
	t.Helper()
	rdb, tdb := newFakeReportFixture()
	r := New(zap.NewNop(), cfg, rdb, tdb)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.GenerateReport(context.Background()); err != nil {
		t.Fatalf("GenerateReport: %v", err)
	}
	return buf.String()
}

// --format json emits NDJSON over the persisted report, and does so even when
// the global --json flag is also set: the explicit format wins over the
// whole-map blob.
func TestGenerateReport_FormatJSONEmitsNDJSON(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{name: "format json alone", cfg: &config.Config{Report: config.Report{Format: "json"}}},
		{name: "format json beats the global --json blob", cfg: &config.Config{
			JSONOutput: true, Report: config.Report{Format: "json"},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runGenerateReport(t, tc.cfg)
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected 2 NDJSON lines, got %d:\n%s", len(lines), out)
			}

			var first testVerdict
			if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
				t.Fatalf("line 0 is not JSON: %v (%q)", err, lines[0])
			}
			if first.SchemaVersion != EffectsSchemaVersion {
				t.Errorf("schema_version = %q, want %q", first.SchemaVersion, EffectsSchemaVersion)
			}
			// getLatestTestRunID picks the highest-numbered run.
			if first.TestRunID != "test-run-1" {
				t.Errorf("test_run_id = %q, want test-run-1", first.TestRunID)
			}
			if first.TestSetID != "test-set-0" || first.TestCaseID != "test-1" {
				t.Errorf("unexpected identity: %+v", first)
			}
			if len(first.Effects) != 1 || first.Effects[0].Matched {
				t.Errorf("expected one failed effect, got %+v", first.Effects)
			}
			if len(first.FailureCategories) != 1 || first.FailureCategories[0] != string(models.DependencyMissing) {
				t.Errorf("failure_categories = %v", first.FailureCategories)
			}

			var second testVerdict
			if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
				t.Fatalf("line 1 is not JSON: %v (%q)", err, lines[1])
			}
			if second.TestCaseID != "test-2" || len(second.Effects) != 0 {
				t.Errorf("unexpected second verdict: %+v", second)
			}
			if !strings.Contains(lines[1], `"effects":[]`) {
				t.Errorf("effects must serialize as [] for a test with no dependencies: %s", lines[1])
			}
		})
	}
}

// --format json filters to the selected test cases, like the other formats.
func TestGenerateReport_FormatJSONHonoursTestCaseFilter(t *testing.T) {
	out := runGenerateReport(t, &config.Config{Report: config.Report{
		Format:      "json",
		TestCaseIDs: []string{"test-2"},
	}})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"test_case_id":"test-2"`) {
		t.Errorf("wrong test emitted: %s", lines[0])
	}
}

// The machine formats project a whole run; --report-path addresses a single
// test-set file with no run identity, so the combination is refused.
func TestGenerateReport_MachineFormatsRejectReportPath(t *testing.T) {
	for _, format := range []string{"junit", "json"} {
		t.Run(format, func(t *testing.T) {
			rdb, tdb := newFakeReportFixture()
			r := New(zap.NewNop(), &config.Config{Report: config.Report{
				Format:     format,
				ReportPath: "/tmp/does-not-matter.yaml",
			}}, rdb, tdb)
			err := r.GenerateReport(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "--format "+format) ||
				!strings.Contains(err.Error(), "--report-path") {
				t.Errorf("unhelpful error: %v", err)
			}
		})
	}
}

// The default text format must not be perturbed by any of this: with the
// global --json flag it still emits the whole-report BLOB, not NDJSON.
//
// The blob now goes through r.out like every other branch. It used to be
// written straight to os.Stdout, which meant this assertion was vacuous (it
// checked a buffer that branch never touched) and every `go test` run of this
// package dumped multiple KB of JSON into the test log.
func TestGenerateReport_TextFormatStillUsesTheGlobalJSONBlob(t *testing.T) {
	out := runGenerateReport(t, &config.Config{JSONOutput: true, Report: config.Report{Format: "text"}})
	if out == "" {
		t.Fatal("the global --json branch wrote nothing to r.out")
	}
	if strings.Contains(out, `"schema_version"`) {
		t.Errorf("text format took the NDJSON branch:\n%s", out)
	}
	// One JSON document keyed by test-set name, not one object per test.
	var blob map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &blob); err != nil {
		t.Fatalf("the --json blob is not a single JSON document: %v\n%s", err, out)
	}
	if _, ok := blob["test-set-0"]; !ok {
		t.Errorf("the blob is not keyed by test set: %v", out)
	}
}

// dependencies_checked has to survive the whole GenerateReport path, not just
// the projection unit test: it is the field an agent reads before trusting an
// empty `effects`.
func TestGenerateReport_FormatJSONCarriesDependenciesChecked(t *testing.T) {
	out := runGenerateReport(t, &config.Config{Report: config.Report{Format: "json"}})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines, got %d:\n%s", len(lines), out)
	}
	want := map[string]bool{"test-1": true, "test-2": false}
	for i, line := range lines {
		var v testVerdict
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if v.DependenciesChecked != want[v.TestCaseID] {
			t.Errorf("%s: dependencies_checked = %v, want %v — an agent cannot tell "+
				"'checked and clean' from 'never checked' without it",
				v.TestCaseID, v.DependenciesChecked, want[v.TestCaseID])
		}
		if v.TestCaseID == "test-1" && v.DependenciesConsumed != 5 {
			t.Errorf("test-1: dependencies_consumed = %d, want 5 — the count is the only "+
				"trace of the dependencies the test DID exercise", v.DependenciesConsumed)
		}
	}
}
