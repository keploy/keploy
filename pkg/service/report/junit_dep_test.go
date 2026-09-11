package report

import (
	"encoding/xml"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestBuildFailure_IncludesDependencyRows(t *testing.T) {
	tests := []struct {
		name     string
		result   models.TestResult
		contains []string
		absent   []string
	}{
		{
			name: "dependency rows appear next to body rows",
			result: models.TestResult{
				Kind:   models.HTTP,
				Status: models.TestStatusFailed,
				Result: models.Result{
					StatusCode: models.IntResult{Normal: true},
					BodyResult: []models.BodyResult{{Normal: false, Type: models.JSON}},
					DepResult:  []models.DepResult{missingDepRow(), consumedDepRow()},
				},
			},
			contains: []string{
				"body mismatch (JSON)",
				`dependency deps[0] postgres PostgreSQL INSERT (presence) [postgres] presence: expected "consumed", got "not consumed"`,
			},
			// A row that held is not a failure line.
			absent: []string{"deps[1] http"},
		},
		{
			name: "no dependency rows leaves the failure text untouched",
			result: models.TestResult{
				Kind:   models.HTTP,
				Status: models.TestStatusFailed,
				Result: models.Result{
					StatusCode: models.IntResult{Normal: false, Expected: 200, Actual: 500},
				},
			},
			contains: []string{"status: expected 200, got 500"},
			absent:   []string{"dependency"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := buildFailure(tt.result)
			if f == nil {
				t.Fatal("buildFailure returned nil")
			}
			for _, want := range tt.contains {
				if !strings.Contains(f.Text, want) {
					t.Errorf("failure text missing %q:\n%s", want, f.Text)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(f.Text, bad) {
					t.Errorf("failure text unexpectedly contains %q:\n%s", bad, f.Text)
				}
			}
		})
	}
}

// The <skipped message> attribute is what CI dashboards group and display
// skipped cases by, so it must stay the historical literal no matter what the
// test's dependencies did; the detail belongs in the element body.
func TestObsoleteSkipMessageIsAFixedLiteral(t *testing.T) {
	cases := []struct {
		name     string
		result   models.TestResult
		wantBody string
	}{
		{
			name:   "nothing missing: empty body",
			result: models.TestResult{Status: models.TestStatusObsolete},
		},
		{
			name: "every dependency held: empty body",
			result: models.TestResult{Status: models.TestStatusObsolete,
				Result: models.Result{DepResult: []models.DepResult{consumedDepRow()}}},
		},
		{
			name: "missing dependency lands in the body, not the attribute",
			result: models.TestResult{Status: models.TestStatusObsolete,
				Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
			wantBody: `dependency deps[0] postgres PostgreSQL INSERT (presence) [postgres] presence: expected "consumed", got "not consumed"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suite := buildJUnitSuite("test-set-0", &models.TestReport{
				Total: 1, Tests: []models.TestResult{tc.result},
			})
			if len(suite.Cases) != 1 || suite.Cases[0].Skipped == nil {
				t.Fatalf("expected one skipped case, got %+v", suite.Cases)
			}
			skipped := suite.Cases[0].Skipped
			if skipped.Message != "obsolete test case" {
				t.Errorf("skip message attribute changed to %q; it is a consumer-visible grouping key", skipped.Message)
			}
			if skipped.Text != tc.wantBody {
				t.Errorf("skip body = %q, want %q", skipped.Text, tc.wantBody)
			}
		})
	}
}

// The flagship silent-green case: the response still matched, so the test is
// PASSED, but a recorded outgoing call vanished. It must reach a CI consumer
// WITHOUT the status flipping — that flip is what --assert-dependencies is for.
func TestBuildJUnitSuite_PassedWithMissingDependencyGetsSystemOut(t *testing.T) {
	suite := buildJUnitSuite("test-set-0", &models.TestReport{
		Total: 2,
		Tests: []models.TestResult{
			{
				Kind: models.HTTP, TestCaseID: "tc-passed", Status: models.TestStatusPassed,
				Result: models.Result{DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true, DepsConsumed: 3},
			},
			{
				Kind: models.HTTP, TestCaseID: "tc-clean", Status: models.TestStatusPassed,
				Result: models.Result{DepsChecked: true, DepsConsumed: 3},
			},
		},
	})

	if suite.Failures != 0 {
		t.Fatalf("a reported-only dependency must not create a failure: %d", suite.Failures)
	}
	if suite.Skipped != 0 {
		t.Fatalf("a reported-only dependency must not skip the case: %d", suite.Skipped)
	}

	byName := map[string]junitTestCase{}
	for _, c := range suite.Cases {
		byName[c.Name] = c
	}

	lost := byName["tc-passed"]
	if lost.Failure != nil || lost.Skipped != nil {
		t.Errorf("tc-passed must stay a clean pass: %+v", lost)
	}
	if !strings.Contains(lost.SystemOut, "deps[0] postgres PostgreSQL INSERT (presence)") {
		t.Errorf("tc-passed lost its dependency notice; system-out = %q", lost.SystemOut)
	}

	if clean := byName["tc-clean"]; clean.SystemOut != "" {
		t.Errorf("a test whose dependencies all held must stay byte-identical: system-out = %q", clean.SystemOut)
	}

	data, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "<system-out>") {
		t.Errorf("system-out did not survive marshalling:\n%s", out)
	}
	if strings.Contains(out, "<failure") {
		t.Errorf("a reported-only dependency emitted a <failure>:\n%s", out)
	}
}

func TestBuildJUnitSuites_DependencyRowsSurviveMarshalling(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-0": {
			TestSet: "test-set-0",
			Total:   3,
			Tests: []models.TestResult{
				{
					Kind: models.HTTP, TestCaseID: "test-1", Status: models.TestStatusFailed,
					Result: models.Result{
						StatusCode: models.IntResult{Normal: true},
						DepResult:  []models.DepResult{missingDepRow()},
					},
				},
				{
					Kind: models.HTTP, TestCaseID: "test-2", Status: models.TestStatusObsolete,
					Result: models.Result{DepResult: []models.DepResult{missingDepRow()}},
				},
				{
					Kind: models.HTTP, TestCaseID: "test-3", Status: models.TestStatusPassed,
					Result: models.Result{DepResult: []models.DepResult{missingDepRow()}},
				},
			},
		},
	}

	data, err := xml.MarshalIndent(buildJUnitSuites(reports), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"deps[0] postgres PostgreSQL INSERT (presence)",
		`<skipped message="obsolete test case">`,
		`<failure message="test assertion failed"`,
		"<system-out>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("junit xml missing %q:\n%s", want, out)
		}
	}
	// The message attribute must not have grown the dependency detail.
	if strings.Contains(out, `message="obsolete test case: `) {
		t.Errorf("the skipped message attribute grew per-test detail:\n%s", out)
	}
}

// THE ASYMMETRY THE BOUNDING PASS MISSED. The text block is grouped by
// dependency and double-capped, and the persisted YAML is capped at 50 rows per
// test, but the JUnit body hanging off GREEN testcases was one line per failed
// assertion with no cap of its own.
//
// Measured on the slice's own flagship shape — 300 PASSED tests that each lost
// 50 recorded dependencies, i.e. a downstream service removed — the same report
// rendered 3,120 B of text and 1,439 B of summary but 2,160,091 B of JUnit XML,
// all of it on testcases the CI system reports as passing.
func TestBuildJUnitSuite_NonFailureBodyIsCapped(t *testing.T) {
	rows := make([]models.DepResult, 0, 50)
	for i := range 50 {
		rows = append(rows, models.DepResult{
			Name: models.DepRowName(i, models.DepTypePostgres, "db:5432 SELECT"),
			Type: models.DepTypePostgres,
			Meta: []models.DepMetaResult{{
				Normal: false, Key: models.DepKeyPresence,
				Expected: models.DepPresenceConsumed, Actual: models.DepPresenceMissing,
			}},
		})
	}

	suite := buildJUnitSuite("test-set-0", &models.TestReport{
		Total: 2,
		Tests: []models.TestResult{
			{
				Kind: models.HTTP, TestCaseID: "tc-passed", Status: models.TestStatusPassed,
				Result: models.Result{DepResult: rows, DepsChecked: true},
			},
			{
				Kind: models.HTTP, TestCaseID: "tc-obsolete", Status: models.TestStatusObsolete,
				Result: models.Result{DepResult: rows, DepsChecked: true},
			},
		},
	})

	byName := map[string]junitTestCase{}
	for _, c := range suite.Cases {
		byName[c.Name] = c
	}

	bodies := map[string]string{
		"<system-out> on a green case": byName["tc-passed"].SystemOut,
		"<skipped> body on an obsolete case": func() string {
			if s := byName["tc-obsolete"].Skipped; s != nil {
				return s.Text
			}
			return ""
		}(),
	}
	for what, body := range bodies {
		lines := strings.Split(body, "\n")
		// depNoticeTestSampleSize named rows + the "...and N more" line.
		if len(lines) != depNoticeTestSampleSize+1 {
			t.Errorf("%s rendered %d lines for 50 missing dependencies, want %d — an uncapped body "+
				"turns a green 300-test run into a 2 MB JUnit artifact:\n%s",
				what, len(lines), depNoticeTestSampleSize+1, body)
		}
		// The count stays EXACT even though the listing is sampled.
		if !strings.Contains(body, "...and 45 more dependencies not exercised") {
			t.Errorf("%s lost the exact remainder:\n%s", what, body)
		}
		if !strings.Contains(body, "deps[0] postgres db:5432 SELECT (presence)") {
			t.Errorf("%s dropped the first offender:\n%s", what, body)
		}
	}

	// A FAILED test's <failure> body is where a human reads the whole story,
	// so it is deliberately NOT sampled.
	full := buildFailure(models.TestResult{
		Kind: models.HTTP, TestCaseID: "tc-failed", Status: models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{Normal: true, Expected: 200, Actual: 200},
			DepResult:  rows, DepsChecked: true,
		},
	})
	if got := len(strings.Split(full.Text, "\n")); got != 50 {
		t.Errorf("a FAILED test's <failure> body carries %d lines, want all 50", got)
	}
}

// A report with nothing missing must render byte-identical XML: no
// <system-out> element at all, and an OBSOLETE case's <skipped> stays
// self-closing rather than growing an empty body.
func TestBuildJUnitSuite_NonFailureBodyIsEmptyWhenNothingIsMissing(t *testing.T) {
	suites := buildJUnitSuites(map[string]*models.TestReport{"test-set-0": {
		Total: 2,
		Tests: []models.TestResult{
			{Kind: models.HTTP, TestCaseID: "tc-passed", Status: models.TestStatusPassed,
				Result: models.Result{DepsChecked: true, DepsConsumed: 12}},
			{Kind: models.HTTP, TestCaseID: "tc-obsolete", Status: models.TestStatusObsolete,
				Result: models.Result{DepsChecked: true, DepsConsumed: 12}},
		},
	}})
	data, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "system-out") {
		t.Errorf("a nothing-missing report grew a <system-out>:\n%s", data)
	}
	if !strings.Contains(string(data), `<skipped message="obsolete test case">`) &&
		!strings.Contains(string(data), `<skipped message="obsolete test case"></skipped>`) {
		t.Errorf("the obsolete skip element changed:\n%s", data)
	}
}
