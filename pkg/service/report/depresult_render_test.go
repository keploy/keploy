package report

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func missingDepRow() models.DepResult {
	return models.DepResult{
		Name: "deps[0] postgres PostgreSQL INSERT (presence)",
		Type: "postgres",
		Meta: []models.DepMetaResult{{
			Normal:   false,
			Key:      models.DepKeyPresence,
			Expected: models.DepPresenceConsumed,
			Actual:   models.DepPresenceMissing,
		}},
	}
}

func consumedDepRow() models.DepResult {
	return models.DepResult{
		Name: "deps[1] http GET api.internal:80/orders (presence)",
		Type: "http",
		Meta: []models.DepMetaResult{{
			Normal:   true,
			Key:      models.DepKeyPresence,
			Expected: models.DepPresenceConsumed,
			Actual:   models.DepPresenceConsumed,
		}},
	}
}

func TestRenderDepResults(t *testing.T) {
	tests := []struct {
		name     string
		deps     []models.DepResult
		wantZero bool
		contains []string
	}{
		{
			name:     "no dependency rows writes zero bytes",
			deps:     nil,
			wantZero: true,
		},
		{
			name: "missing dependency renders expected vs actual",
			deps: []models.DepResult{missingDepRow()},
			contains: []string{
				"=== DEPENDENCY ASSERTIONS ===",
				"MISSING deps[0] postgres PostgreSQL INSERT (presence)",
				`presence: expected "consumed", actual "not consumed"`,
			},
		},
		{
			// Same rule: a matched row on its own is not worth a block.
			name:     "a matched row alone renders nothing",
			deps:     []models.DepResult{consumedDepRow()},
			wantZero: true,
		},
		{
			// The overflow row stands in for the dependencies past the per-test
			// cap; it must render as a MISSING line without restating its own
			// count.
			name: "the missing overflow renders alongside the named rows",
			deps: []models.DepResult{missingDepRow(), models.DepMissingOverflowRow(150)},
			contains: []string{
				"MISSING deps[0] postgres PostgreSQL INSERT (presence)",
				"MISSING deps[*] 150 more not consumed (presence)",
			},
		},
		{
			name: "mixed rows render both shapes",
			deps: []models.DepResult{missingDepRow(), consumedDepRow()},
			contains: []string{
				"MISSING deps[0] postgres PostgreSQL INSERT (presence)",
				"OK      deps[1] http GET api.internal:80/orders (presence) - consumed (presence only)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			renderDepResults(&sb, models.TestResult{Result: models.Result{DepResult: tt.deps}})
			out := sb.String()
			if tt.wantZero {
				if sb.Len() != 0 {
					t.Fatalf("expected no output, got %q", out)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("rendered output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRenderSingleFailedTest_IncludesDependencyBlock(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)

	tests := []struct {
		name       string
		test       models.TestResult
		wantHeader string
		contains   []string
		absent     []string
	}{
		{
			name: "failed test with a missing dependency",
			test: models.TestResult{
				Kind:       models.HTTP,
				Name:       "test-set-0",
				TestCaseID: "test-1",
				Status:     models.TestStatusFailed,
				Result:     models.Result{DepResult: []models.DepResult{missingDepRow()}},
			},
			wantHeader: "Testrun failed for test-set-0/test-1",
			contains:   []string{"=== DEPENDENCY ASSERTIONS ===", "MISSING deps[0] postgres PostgreSQL INSERT (presence)"},
		},
		{
			name: "failed test with no dependencies is unchanged",
			test: models.TestResult{
				Kind:       models.HTTP,
				Name:       "test-set-0",
				TestCaseID: "test-3",
				Status:     models.TestStatusFailed,
			},
			wantHeader: "Testrun failed for test-set-0/test-3",
			absent:     []string{"DEPENDENCY ASSERTIONS"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			if err := r.renderSingleFailedTest(context.Background(), &sb, tt.test); err != nil {
				t.Fatalf("renderSingleFailedTest: %v", err)
			}
			out := sb.String()
			if !strings.Contains(out, tt.wantHeader) {
				t.Errorf("output missing header %q:\n%s", tt.wantHeader, out)
			}
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(out, bad) {
					t.Errorf("output unexpectedly contains %q:\n%s", bad, out)
				}
			}
		})
	}
}

// A test that lost a recorded dependency must reach the renderer WHATEVER its
// status. The flagship silent-green case (response matched, outgoing call
// vanished) lands on PASSED, so a FAILED-or-OBSOLETE-only admission rule left
// it invisible in text and JUnit unless the user opted into a verdict knob.
func TestExtractFailedTestsFromResults_AdmissionIsStatusIndependentForDependencies(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{}, nil, nil)

	withMissing := func(id string, status models.TestStatus) models.TestResult {
		return models.TestResult{TestCaseID: id, Status: status,
			Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}}
	}

	tests := []struct {
		name  string
		input []models.TestResult
		want  []string
	}{
		{
			name: "failed only (historical behaviour for reports with no dependency rows)",
			input: []models.TestResult{
				{TestCaseID: "t1", Status: models.TestStatusFailed},
				{TestCaseID: "t2", Status: models.TestStatusPassed},
				{TestCaseID: "t3", Status: models.TestStatusObsolete},
				{TestCaseID: "t4", Status: models.TestStatusIgnored},
			},
			want: []string{"t1"},
		},
		{
			name:  "obsolete with a missing dependency is admitted",
			input: []models.TestResult{withMissing("t1", models.TestStatusObsolete)},
			want:  []string{"t1"},
		},
		{
			name: "PASSED with a missing dependency is admitted — this is the silent-green case",
			input: []models.TestResult{
				withMissing("t1", models.TestStatusPassed),
				{TestCaseID: "t2", Status: models.TestStatusPassed},
			},
			want: []string{"t1"},
		},
		{
			name:  "IGNORED with a missing dependency is admitted too",
			input: []models.TestResult{withMissing("t1", models.TestStatusIgnored)},
			want:  []string{"t1"},
		},
		{
			name: "a test whose dependencies all held stays out, whatever its status",
			input: []models.TestResult{
				{TestCaseID: "t1", Status: models.TestStatusObsolete,
					Result: models.Result{DepResult: []models.DepResult{consumedDepRow()}}},
				{TestCaseID: "t2", Status: models.TestStatusPassed,
					Result: models.Result{DepsChecked: true, DepsConsumed: 9}},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.extractFailedTestsFromResults(tt.input)
			var ids []string
			for _, g := range got {
				ids = append(ids, g.TestCaseID)
			}
			if len(ids) != len(tt.want) {
				t.Fatalf("got %v, want %v", ids, tt.want)
			}
			for i := range ids {
				if ids[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", ids, tt.want)
				}
			}
		})
	}
}

// --full uses a different renderer (matcher.DiffsPrinter buffered into the
// same builder); the dependency block has to reach it too, in the SAME
// position relative to the response diffs as the compact renderer puts it.
func TestRenderSingleFullBodyFailedTest_IncludesDependencyBlock(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)

	tests := []struct {
		name     string
		test     models.TestResult
		contains []string
		absent   []string
	}{
		{
			name: "missing dependency is rendered",
			test: models.TestResult{
				Kind: models.HTTP, Name: "test-set-0", TestCaseID: "test-1",
				Status: models.TestStatusFailed,
				Result: models.Result{
					StatusCode: models.IntResult{Normal: true},
					DepResult:  []models.DepResult{missingDepRow()},
				},
			},
			contains: []string{"Testrun failed for test-set-0/test-1", "MISSING deps[0] postgres PostgreSQL INSERT (presence)"},
		},
		{
			name: "no dependencies leaves the output untouched",
			test: models.TestResult{
				Kind: models.HTTP, Name: "test-set-0", TestCaseID: "test-2",
				Status: models.TestStatusFailed,
				Result: models.Result{StatusCode: models.IntResult{Normal: true}},
			},
			absent: []string{"DEPENDENCY ASSERTIONS"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			if err := r.renderSingleFullBodyFailedTest(context.Background(), &sb, tt.test); err != nil {
				t.Fatalf("renderSingleFullBodyFailedTest: %v", err)
			}
			out := sb.String()
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(out, bad) {
					t.Errorf("output unexpectedly contains %q:\n%s", bad, out)
				}
			}
		})
	}
}

// The dependency block must sit AFTER the response diffs in both modes. It
// used to land before them in --full (the diff printer buffers and only
// flushes on Render) and after them in compact mode — same data, two positions
// depending on a flag.
func TestDependencyBlockPositionIsTheSameInBothModes(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	test := models.TestResult{
		Kind: models.HTTP, Name: "test-set-0", TestCaseID: "test-1",
		Status: models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{Normal: false, Expected: 200, Actual: 500},
			DepResult:  []models.DepResult{missingDepRow()},
		},
	}

	render := func(t *testing.T, full bool) string {
		t.Helper()
		var sb strings.Builder
		var err error
		if full {
			err = r.renderSingleFullBodyFailedTest(context.Background(), &sb, test)
		} else {
			err = r.renderSingleFailedTest(context.Background(), &sb, test)
		}
		if err != nil {
			t.Fatalf("render(full=%v): %v", full, err)
		}
		return sb.String()
	}

	for _, full := range []bool{false, true} {
		out := render(t, full)
		depAt := strings.Index(out, "=== DEPENDENCY ASSERTIONS ===")
		diffAt := strings.Index(out, "500")
		if depAt < 0 {
			t.Fatalf("full=%v: no dependency block:\n%s", full, out)
		}
		if diffAt < 0 {
			t.Fatalf("full=%v: no response diff to order against:\n%s", full, out)
		}
		if depAt < diffAt {
			t.Errorf("full=%v: the dependency block came BEFORE the response diffs (dep@%d, diff@%d):\n%s",
				full, depAt, diffAt, out)
		}
	}
}

// printTests is the `--test-case` path. A passing test keeps its one-line
// notice and gains exactly one line when it lost a dependency.
func TestPrintTests_PassedTestCarriesTheDependencyNotice(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printTests(context.Background(), []models.TestResult{
		{TestCaseID: "clean", Name: "test-set-0", Status: models.TestStatusPassed, TimeTaken: "1ms"},
		{TestCaseID: "lost-dep", Name: "test-set-0", Status: models.TestStatusPassed, TimeTaken: "2ms",
			Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
	})
	if err != nil {
		t.Fatalf("printTests: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `Testcase "clean" (test-set-0) PASSED ✅ (1ms)`) {
		t.Errorf("the historical pass line changed:\n%s", out)
	}
	if !strings.Contains(out, `Testcase "lost-dep" (test-set-0) PASSED ✅ (2ms)`) {
		t.Errorf("a test that lost a dependency must still be reported as PASSED:\n%s", out)
	}
	if !strings.Contains(out, models.DepNoticePrefix+" deps[0] postgres PostgreSQL INSERT (presence)") {
		t.Errorf("the dependency notice is missing:\n%s", out)
	}
	// The clean test must not have grown anything.
	cleanBlock := out[:strings.Index(out, "lost-dep")]
	if strings.Contains(cleanBlock, models.DepNoticePrefix) {
		t.Errorf("a clean pass grew a notice:\n%s", cleanBlock)
	}
}

// The same one-line path takes EVERY non-FAILED status, and it used to
// hardcode "PASSED ✅" for all of them. `keploy report --test-case <obsolete>`
// therefore announced a pass for a demoted test — and once this slice started
// printing the dependency notice underneath, the two adjacent lines
// contradicted each other on the one surface a user reached deliberately.
func TestPrintTests_PrintsTheActualStatusNotAHardcodedPass(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printTests(context.Background(), []models.TestResult{
		{TestCaseID: "passed", Name: "test-set-0", Status: models.TestStatusPassed, TimeTaken: "1ms"},
		{TestCaseID: "obsolete", Name: "test-set-0", Status: models.TestStatusObsolete, TimeTaken: "5ms"},
		{TestCaseID: "ignored", Name: "test-set-0", Status: models.TestStatusIgnored, TimeTaken: "0ms"},
		{TestCaseID: "obsolete-lost-dep", Name: "test-set-0", Status: models.TestStatusObsolete, TimeTaken: "6ms",
			Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
	})
	if err != nil {
		t.Fatalf("printTests: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		// Byte-identical to pre-slice-4 for the status that actually passed.
		`Testcase "passed" (test-set-0) PASSED ✅ (1ms)`,
		`Testcase "obsolete" (test-set-0) OBSOLETE ⚠️ (5ms)`,
		`Testcase "ignored" (test-set-0) IGNORED ⏭️ (0ms)`,
		`Testcase "obsolete-lost-dep" (test-set-0) OBSOLETE ⚠️ (6ms)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	// The contradiction itself: the notice must never sit under a line
	// claiming the test passed.
	noticeAt := strings.Index(out, models.DepNoticePrefix)
	if noticeAt < 0 {
		t.Fatalf("the obsolete test lost its dependency notice:\n%s", out)
	}
	head := out[:noticeAt]
	lastLineStart := strings.LastIndex(strings.TrimRight(head, "\n"), "\n")
	statusLine := strings.TrimSpace(head[lastLineStart+1:])
	if strings.Contains(statusLine, "PASSED") {
		t.Errorf("the dependency notice is printed directly under a line asserting the test passed: %q", statusLine)
	}
}

func TestTestStatusLabel(t *testing.T) {
	for status, want := range map[models.TestStatus]string{
		models.TestStatusPassed:   "PASSED ✅",
		models.TestStatusFailed:   "FAILED ❌",
		models.TestStatusObsolete: "OBSOLETE ⚠️",
		models.TestStatusIgnored:  "IGNORED ⏭️",
		models.TestStatus("WAT"):  "WAT",
	} {
		if got := testStatusLabel(status); got != want {
			t.Errorf("testStatusLabel(%s) = %q, want %q", status, got, want)
		}
	}
}

// The summary is the only surface a CI operator reads on a green run.
func TestPrintSummary_CountsDependenciesAndCategories(t *testing.T) {
	tests := []struct {
		name     string
		reports  map[string]*models.TestReport
		contains []string
		absent   []string
	}{
		{
			name: "a green run that lost a dependency says so",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 2, Success: 2, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusPassed,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
					{TestCaseID: "t2", Status: models.TestStatusPassed},
				}},
			},
			contains: []string{
				"Dependencies not exercised: 1 test(s)",
				"1 of them PASSED",
				// The claim has to match what the data can support. Consumed
				// mocks are drained when the response comes back, so a call
				// the app makes AFTER writing its response lands in the next
				// test's window: keploy can say it did not observe the call
				// here, not that the app never made it.
				"not observed during the test's own window",
				"--assert-dependencies",
			},
			absent: []string{"FAILURE RISK DISTRIBUTION", "was never made"},
		},
		{
			name: "DEPENDENCY_MISSING reaches the category block even with no risk and no failure",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusObsolete,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.DependencyMissing},
						},
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
				}},
			},
			contains: []string{"FAILURE CATEGORIES:", "DEPENDENCY_MISSING: 1"},
		},
		{
			name: "a run with nothing missing is untouched",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Success: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusPassed},
				}},
			},
			absent: []string{"Dependencies not exercised", "FAILURE CATEGORIES", "FAILURE RISK DISTRIBUTION"},
		},
		{
			// DELIBERATE, PRE-EXISTING-REPORT BEHAVIOUR CHANGE, pinned so it
			// stays deliberate. Categories used to be counted only inside
			// `if Status == FAILED && Risk != ""`. A legacy report whose
			// failure carries APP_CONNECTION_ERROR with no risk level — the
			// shape replay.go's app-connection path produces — printed "No
			// specific categories identified"; it now prints the category. The
			// per-test header already showed it, so the summary was the odd
			// one out, but a scraped block did change.
			name: "a FAILED test with a category and NO risk level is now counted",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Failure: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusFailed,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.AppConnectionError},
						}},
				}},
			},
			contains: []string{"FAILURE CATEGORIES:", "APP_CONNECTION_ERROR: 1"},
			absent:   []string{"No specific categories identified", "Dependencies not exercised"},
		},
		{
			// The risk block is still risk-gated: a category with no risk must
			// not invent a High/Medium/Low distribution.
			name: "counting a risk-less category does not fabricate a risk distribution",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Failure: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusFailed,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.AppConnectionError},
						}},
				}},
			},
			contains: []string{"High Risk: 0", "Medium Risk: 0", "Low Risk: 0"},
		},
		{
			// A PASSED test never grows a failure category (attachDepResults
			// refuses to label one), so nothing it carries can reach the
			// failure taxonomy even though its dependency notice is counted.
			name: "a PASSED test's dependency loss is counted but never becomes a failure category",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Success: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusPassed,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
				}},
			},
			contains: []string{"Dependencies not exercised: 1 test(s)"},
			absent:   []string{"FAILURE CATEGORIES"},
		},
		{
			// BACKWARD COMPATIBILITY, on data this slice never touched. An
			// OBSOLETE test's OTHER categories have never been counted, and
			// counting them would change the numbers under a heading called
			// FAILURE CATEGORIES for reports written long before slice 4:
			// SCHEMA_BROKEN would read 2 next to "Total test failed: 1".
			name: "an OBSOLETE test's non-dependency category does not inflate the count",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 2, Failure: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusFailed,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.SchemaBroken},
						}},
					{TestCaseID: "t2", Status: models.TestStatusObsolete,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.SchemaBroken},
						}},
				}},
			},
			contains: []string{"Total test failed: 1", "SCHEMA_BROKEN: 1"},
			absent:   []string{"SCHEMA_BROKEN: 2"},
		},
		{
			// The other half: a report with NO failures must not grow a
			// FAILURE CATEGORIES block at all just because an obsolete test
			// carries an unrelated category. `keploy report --summary` on an
			// unchanged pre-slice-4 report has to stay byte-identical.
			name: "a zero-failure report grows no category block from an obsolete test",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 2, Success: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusPassed},
					{TestCaseID: "t2", Status: models.TestStatusObsolete,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.SchemaBroken},
						}},
				}},
			},
			contains: []string{"Total test failed: 0"},
			absent:   []string{"FAILURE CATEGORIES", "SCHEMA_BROKEN"},
		},
		{
			// The gate reads the report's own `failure` counter, which can
			// disagree with the statuses in a partially written report. Even
			// then the ONLY thing that may summon the block on a zero-failure
			// run is a dependency finding: `len(categoryCounts) > 0` would
			// print a brand-new three-line section here, on data this slice
			// never touched.
			name: "a report whose failure counter is zero prints no block for a non-dependency category",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusFailed,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.SchemaBroken},
						}},
				}},
			},
			contains: []string{"Total test failed: 0"},
			absent:   []string{"FAILURE CATEGORIES", "SCHEMA_BROKEN"},
		},
		{
			// THE NUMBER ITSELF, over a population where total != passed.
			//
			// Every other row here loses a dependency on PASSED tests only, so
			// widening `if t.Status == models.TestStatusPassed` in
			// depNoticeCounts to count every status left the printed numbers
			// unchanged and the suite green. The claim that mutation breaks is
			// the load-bearing one: "N of them PASSED — the response matched"
			// is false for an OBSOLETE or FAILED test, whose response did NOT
			// match. That is precisely the distinction
			// replay.unexercisedSummary's doc comment says the wording is
			// careful about, and this run has one test of each.
			name: "the PASSED count is the passed subset, not the whole affected population",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 3, Success: 1, Failure: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t-passed", Status: models.TestStatusPassed,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
					{TestCaseID: "t-obsolete", Status: models.TestStatusObsolete,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
					{TestCaseID: "t-failed", Status: models.TestStatusFailed,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
					// A clean PASSED test is in neither number.
					{TestCaseID: "t-clean", Status: models.TestStatusPassed},
				}},
			},
			contains: []string{
				"Dependencies not exercised: 3 test(s)",
				"1 of them PASSED",
			},
			absent: []string{
				// The mutation's output: every affected test counted as passed.
				"3 of them PASSED",
				"2 of them PASSED",
				"4 test(s)",
			},
		},
		{
			// The other direction: when NONE of the affected tests passed, the
			// "N of them PASSED" line must not appear at all. Widening the
			// count would print "2 of them PASSED — the response matched" for
			// two tests whose response did not match.
			name: "no passed test among the affected population prints no PASSED line",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 2, Failure: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t-obsolete", Status: models.TestStatusObsolete,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
					{TestCaseID: "t-failed", Status: models.TestStatusFailed,
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
				}},
			},
			contains: []string{"Dependencies not exercised: 2 test(s)"},
			absent:   []string{"of them PASSED", "the response matched"},
		},
		{
			// ...but a dependency finding on a zero-failure run DOES earn the
			// block. That is the one new trigger, and the reason the print gate
			// is not simply `failed > 0`.
			name: "a zero-failure report with a DEPENDENCY_MISSING obsolete test does print the block",
			reports: map[string]*models.TestReport{
				"test-set-0": {Total: 1, Obsolete: 1, Tests: []models.TestResult{
					{TestCaseID: "t1", Status: models.TestStatusObsolete,
						FailureInfo: models.FailureInfo{
							Category: []models.FailureCategory{models.SchemaBroken, models.DependencyMissing},
						},
						Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true}},
				}},
			},
			contains: []string{"FAILURE CATEGORIES:", "DEPENDENCY_MISSING: 1"},
			// The obsolete test's OTHER category still does not get counted,
			// even once the block is on screen.
			absent: []string{"SCHEMA_BROKEN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
			var buf bytes.Buffer
			r.out = bufio.NewWriter(&buf)
			if err := r.printSummary(tt.reports); err != nil {
				t.Fatalf("printSummary: %v", err)
			}
			_ = r.out.Flush()
			out := buf.String()
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("summary missing %q:\n%s", want, out)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(out, bad) {
					t.Errorf("summary unexpectedly contains %q:\n%s", bad, out)
				}
			}
		})
	}
}
