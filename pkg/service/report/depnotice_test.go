package report

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// namedMissingRow builds a MISSING row with an arbitrary name, so a test can
// control how many DISTINCT dependencies a population carries.
func namedMissingRow(name string) models.DepResult {
	return models.DepResult{
		Name: name,
		Type: "http",
		Meta: []models.DepMetaResult{{
			Normal:   false,
			Key:      models.DepKeyPresence,
			Expected: models.DepPresenceConsumed,
			Actual:   models.DepPresenceMissing,
		}},
	}
}

func passedWithDeps(id string, rows ...models.DepResult) models.TestResult {
	return models.TestResult{
		TestCaseID: id, Name: "test-set-0", Status: models.TestStatusPassed,
		Result: models.Result{DepResult: rows, DepsChecked: true},
	}
}

// passedChecked is a test whose dependency assertion RAN and found nothing
// missing: no rows at all, just the two scalars. That is the shape every
// healthy test in an instrument+mapping run persists.
func passedChecked(id string, consumed int) models.TestResult {
	return models.TestResult{
		TestCaseID: id, Name: "test-set-0", Status: models.TestStatusPassed,
		Result: models.Result{DepsChecked: true, DepsConsumed: consumed},
	}
}

func TestGroupDepNotices(t *testing.T) {
	tests := []struct {
		name  string
		input []models.TestResult
		want  []depNoticeGroup
	}{
		{
			name: "nothing missing groups to nothing",
			input: []models.TestResult{
				passedWithDeps("t1"),
				passedChecked("t2", 4),
			},
			want: nil,
		},
		{
			name: "one dependency lost by three tests is ONE group",
			input: []models.TestResult{
				passedWithDeps("t1", namedMissingRow("deps[4] http POST analytics/events (presence)")),
				passedWithDeps("t2", namedMissingRow("deps[4] http POST analytics/events (presence)")),
				passedWithDeps("t3", namedMissingRow("deps[4] http POST analytics/events (presence)")),
			},
			want: []depNoticeGroup{{
				Name:  "deps[4] http POST analytics/events (presence)",
				Tests: []string{"t1", "t2", "t3"},
			}},
		},
		{
			name: "groups are ordered by blast radius, then by name",
			input: []models.TestResult{
				passedWithDeps("t1", namedMissingRow("deps[0] a")),
				passedWithDeps("t2", namedMissingRow("deps[1] b")),
				passedWithDeps("t3", namedMissingRow("deps[1] b")),
				passedWithDeps("t4", namedMissingRow("deps[2] c")),
			},
			want: []depNoticeGroup{
				{Name: "deps[1] b", Tests: []string{"t2", "t3"}},
				{Name: "deps[0] a", Tests: []string{"t1"}},
				{Name: "deps[2] c", Tests: []string{"t4"}},
			},
		},
		{
			name: "a test that lost two dependencies appears in both groups",
			input: []models.TestResult{
				passedWithDeps("t1", namedMissingRow("deps[0] a"), namedMissingRow("deps[1] b")),
			},
			want: []depNoticeGroup{
				{Name: "deps[0] a", Tests: []string{"t1"}},
				{Name: "deps[1] b", Tests: []string{"t1"}},
			},
		},
		{
			name:  "a result with no TestCaseID falls back to its name",
			input: []models.TestResult{{Name: "only-a-name", Status: models.TestStatusPassed, Result: models.Result{DepResult: []models.DepResult{namedMissingRow("deps[0] a")}}}},
			want:  []depNoticeGroup{{Name: "deps[0] a", Tests: []string{"only-a-name"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupDepNotices(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d groups %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].Name != tt.want[i].Name {
					t.Fatalf("group %d: got name %q, want %q (full: %v)", i, got[i].Name, tt.want[i].Name, got)
				}
				if strings.Join(got[i].Tests, ",") != strings.Join(tt.want[i].Tests, ",") {
					t.Fatalf("group %q: got tests %v, want %v", got[i].Name, got[i].Tests, tt.want[i].Tests)
				}
			}
		})
	}
}

// THE MEASURED REGRESSION. A 300-test all-PASSED run in which every test lost
// the SAME dependency (the shape an app that writes an audit/analytics call
// after flushing its response produces) rendered 1,217 lines of `keploy
// report` where the pre-slice build rendered 16, every block repeating one
// identical sentence.
func TestRenderDepNoticeSummary_IsBoundedAndDeduplicated(t *testing.T) {
	const shared = "deps[4] http POST analytics.internal:9000/events (presence)"
	population := make([]models.TestResult, 0, 300)
	for i := range 300 {
		population = append(population, passedWithDeps(fmt.Sprintf("tc-%03d", i), namedMissingRow(shared)))
	}

	out := renderDepNoticeSummary(population, population)
	lines := strings.Count(out, "\n")

	// One block, not 300. The exact budget: header rule, the count sentence,
	// the hint, the caveat, one group line, one sample line, the trailing
	// rule, plus the leading blank line.
	if lines > 12 {
		t.Fatalf("300 tests sharing one dependency rendered %d lines; the block must stay bounded:\n%s", lines, out)
	}
	if strings.Count(out, models.DepNoticePrefix) != 1 {
		t.Fatalf("the notice is repeated per test instead of once per dependency:\n%s", out)
	}
	for _, want := range []string{
		"300 test(s) did not exercise a recorded outgoing call",
		models.DepNoticePrefix + " " + shared,
		"300 test(s): tc-000, tc-001, tc-002, tc-003, tc-004 ...and 295 more",
		models.DepNoticeHint,
		"attributed to the NEXT test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block missing %q:\n%s", want, out)
		}
	}
}

// The other unbounded axis: one distinct dependency per test.
func TestRenderDepNoticeSummary_GroupCountIsCapped(t *testing.T) {
	var population []models.TestResult
	for i := range 40 {
		population = append(population, passedWithDeps(fmt.Sprintf("tc-%02d", i),
			namedMissingRow(fmt.Sprintf("deps[%d] http GET svc-%02d/x (presence)", i, i))))
	}

	out := renderDepNoticeSummary(population, population)
	if got := strings.Count(out, models.DepNoticePrefix); got != depNoticeGroupCap {
		t.Fatalf("named %d dependencies, want the cap of %d:\n%s", got, depNoticeGroupCap, out)
	}
	if !strings.Contains(out, "...and 30 more dependencies not exercised") {
		t.Errorf("the elided dependencies are not accounted for:\n%s", out)
	}
	// Totals stay exact even though the listing is capped.
	if !strings.Contains(out, "40 test(s) did not exercise a recorded outgoing call") {
		t.Errorf("the total is wrong or missing:\n%s", out)
	}
}

// The caller hands over every non-FAILED admitted result, so the count has to
// come from the rows, not from the slice length.
func TestRenderDepNoticeSummary_CountsOnlyTheAffectedTests(t *testing.T) {
	population := []models.TestResult{
		passedWithDeps("lost", namedMissingRow("deps[0] a")),
		passedChecked("clean", 2),
		passedWithDeps("no-rows-at-all"),
	}
	out := renderDepNoticeSummary(population, population)
	if !strings.Contains(out, "1 test(s) did not exercise a recorded outgoing call") {
		t.Fatalf("the count includes tests that lost nothing:\n%s", out)
	}
}

func TestRenderDepNoticeSummary_EmptyForACleanPopulation(t *testing.T) {
	if got := renderDepNoticeSummary(nil, nil); got != "" {
		t.Fatalf("a nil population rendered %q", got)
	}
	clean := []models.TestResult{passedChecked("t1", 3)}
	if got := renderDepNoticeSummary(clean, clean); got != "" {
		t.Fatalf("a population that lost nothing rendered %q", got)
	}
}

// Only a genuinely FAILED test gets the diff apparatus. OBSOLETE reaching the
// full renderer meant 100% of obsolete tests grew a failure block on upgrade,
// where before this slice they rendered nothing at all in `keploy report`.
func TestPartitionDepNotices(t *testing.T) {
	withDep := func(id string, status models.TestStatus) models.TestResult {
		return models.TestResult{TestCaseID: id, Status: status,
			Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}}
	}
	diffs, notices := partitionDepNotices([]models.TestResult{
		withDep("failed", models.TestStatusFailed),
		withDep("obsolete", models.TestStatusObsolete),
		withDep("passed", models.TestStatusPassed),
		withDep("ignored", models.TestStatusIgnored),
	})

	ids := func(in []models.TestResult) []string {
		var out []string
		for _, t := range in {
			out = append(out, t.TestCaseID)
		}
		return out
	}
	if got := strings.Join(ids(diffs), ","); got != "failed" {
		t.Fatalf("diff apparatus went to %q, want only the FAILED test", got)
	}
	if got := strings.Join(ids(notices), ","); got != "obsolete,passed,ignored" {
		t.Fatalf("notice population was %q, want every non-FAILED status", got)
	}

	// THE SUBSET INVARIANT renderDepNoticeSummary relies on instead of a
	// runtime clamp. It pairs (notices, admitted) and prints "N of them" over
	// the two counts; because notices is a partition half of admitted,
	// countWithMissingDeps(notices) <= countWithMissingDeps(admitted) always
	// holds and the block can never claim a subset larger than its own total.
	// A defensive `if total < affected { total = affected }` there was
	// unreachable and untestable, so the invariant is asserted at the place
	// that actually establishes it.
	if len(diffs)+len(notices) != 4 {
		t.Fatalf("partition lost or duplicated results: %d diffs + %d notices, want 4 in total",
			len(diffs), len(notices))
	}
	admitted := map[string]bool{"failed": true, "obsolete": true, "passed": true, "ignored": true}
	for _, id := range ids(notices) {
		if !admitted[id] {
			t.Fatalf("notice %q is not in the admitted set; renderDepNoticeSummary's counts stop being "+
				"a subset of its own total", id)
		}
	}
}

// End to end through the real `keploy report` renderer: the FAILED test keeps
// its full block, and 300 notice-only tests collapse into one bounded block.
//
// Run in BOTH modes. The compact path's writeDepNoticeSummary call was pinned;
// the identical call in the --full branch was not, so `keploy report --full`
// could silently lose the "=== DEPENDENCIES NOT EXERCISED ===" block — the only
// text surface for the flagship silent-green case — with the package green.
func TestPrintFailedTestReports_NoticesCollapseIntoOneBlock(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			r := New(zap.NewNop(), &config.Config{
				DisableANSI: true,
				Report:      config.Report{ShowFullBody: full},
			}, nil, nil)
			var buf bytes.Buffer
			r.out = bufio.NewWriter(&buf)

			admitted := []models.TestResult{{
				Kind: models.HTTP, Name: "test-set-0", TestCaseID: "really-failed",
				Status: models.TestStatusFailed,
				Result: models.Result{
					StatusCode: models.IntResult{Normal: false, Expected: 200, Actual: 500},
					DepResult:  []models.DepResult{missingDepRow()},
				},
			}}
			for i := range 300 {
				admitted = append(admitted, passedWithDeps(fmt.Sprintf("tc-%03d", i), missingDepRow()))
			}

			if err := r.printFailedTestReports(context.Background(), admitted); err != nil {
				t.Fatalf("printFailedTestReports: %v", err)
			}
			out := buf.String()

			if strings.Count(out, "Testrun failed for") != 1 {
				t.Fatalf("expected exactly one failure block, got %d:\n%s", strings.Count(out, "Testrun failed for"), out)
			}
			if got := strings.Count(out, "=== DEPENDENCY ASSERTIONS ==="); got != 1 {
				t.Fatalf("the full dependency block was rendered %d times; only the FAILED test gets it", got)
			}
			if got := strings.Count(out, models.DepNoticePrefix); got != 1 {
				t.Fatalf("the compact notice appeared %d times; 300 tests sharing one dependency must collapse to one line", got)
			}
			if !strings.Contains(out, "=== DEPENDENCIES NOT EXERCISED ===") {
				t.Fatalf("the run-level dependency block is missing — it is the only text surface for "+
					"a test that PASSED while losing a recorded outgoing call:\n%s", out)
			}
			// The counts must reconcile with `--summary`, which counts every
			// status: 301 tests lost the dependency, 300 of them were not
			// failed for it and are what this block folds in.
			if !strings.Contains(out, "301 test(s) did not exercise a recorded outgoing call") ||
				!strings.Contains(out, "300 of them were NOT failed for it") {
				t.Errorf("the notice-only population is not accounted for:\n%s", out)
			}
		})
	}
}

// The same --full gap for the ONE-test case, where the block is the only line
// that mentions the dependency at all.
func TestPrintFailedTestReports_FullBodyKeepsTheRunLevelBlock(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{
		DisableANSI: true,
		Report:      config.Report{ShowFullBody: true},
	}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printFailedTestReports(context.Background(), []models.TestResult{
		passedWithDeps("tc-passed", missingDepRow()),
	})
	if err != nil {
		t.Fatalf("printFailedTestReports: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "=== DEPENDENCIES NOT EXERCISED ===") {
		t.Fatalf("`keploy report --full` lost the dependency block for a PASSED test that stopped "+
			"making a recorded outgoing call:\n%s", out)
	}
	if !strings.Contains(out, models.DepNoticePrefix+" deps[0] postgres PostgreSQL INSERT (presence)") {
		t.Errorf("the dependency is not named:\n%s", out)
	}
}

// An OBSOLETE test must NOT gain the failure apparatus it never had before
// this slice.
func TestPrintFailedTestReports_ObsoleteGetsTheCompactNotice(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.printFailedTestReports(context.Background(), []models.TestResult{{
		Kind: models.HTTP, Name: "test-set-0", TestCaseID: "tc-obsolete",
		Status: models.TestStatusObsolete,
		Result: models.Result{DepResult: []models.DepResult{missingDepRow()}},
	}})
	if err != nil {
		t.Fatalf("printFailedTestReports: %v", err)
	}
	out := buf.String()

	for _, bad := range []string{
		"Testrun failed for", "Testrun obsolete for",
		"=== CHANGES IN STATUS AND HEADERS ===",
		"=== CHANGES WITHIN THE RESPONSE BODY ===",
		"=== DEPENDENCY ASSERTIONS ===",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("an OBSOLETE test rendered the failure apparatus (%q):\n%s", bad, out)
		}
	}
	if !strings.Contains(out, models.DepNoticePrefix+" "+missingDepRow().Name) {
		t.Errorf("the obsolete test's lost dependency is invisible:\n%s", out)
	}
}

// THE TWO-NUMBERS-FOR-ONE-QUESTION BUG. `keploy report --summary` counts every
// status (report.depNoticeCounts) while this block only folds in the tests that
// were not failed for it, so a run with 3 PASSED and 2 FAILED tests all losing
// the same dependency printed "3" here and "5" under --summary. Both are true;
// neither said which population it was describing.
func TestRenderDepNoticeSummary_StatesWhichPopulationItDescribes(t *testing.T) {
	const shared = "deps[4] http POST analytics.internal:9000/events (presence)"
	failed := func(id string) models.TestResult {
		return models.TestResult{
			TestCaseID: id, Name: "test-set-0", Status: models.TestStatusFailed,
			Result: models.Result{DepResult: []models.DepResult{namedMissingRow(shared)}, DepsChecked: true},
		}
	}

	notices := []models.TestResult{
		passedWithDeps("p1", namedMissingRow(shared)),
		passedWithDeps("p2", namedMissingRow(shared)),
		passedWithDeps("p3", namedMissingRow(shared)),
	}
	admitted := append(append([]models.TestResult{}, notices...), failed("f1"), failed("f2"))

	out := renderDepNoticeSummary(notices, admitted)

	// The total agrees with what --summary prints for the same run...
	total, _ := depNoticeCounts(map[string]*models.TestReport{"test-set-0": {Tests: admitted}})
	if total != 5 {
		t.Fatalf("fixture is wrong: depNoticeCounts says %d", total)
	}
	if !strings.Contains(out, "5 test(s) did not exercise a recorded outgoing call") {
		t.Errorf("the block does not reconcile with `--summary`'s count of %d:\n%s", total, out)
	}
	// ...and the subset this block actually folds in is named.
	if !strings.Contains(out, "3 of them were NOT failed for it and are summarised below") {
		t.Errorf("the block does not say which subset it is describing:\n%s", out)
	}

	// When the two populations agree, the sentence stays the short historical
	// one rather than restating the same number twice.
	if got := renderDepNoticeSummary(notices, notices); !strings.Contains(got,
		"3 test(s) did not exercise a recorded outgoing call and were NOT failed for it.") {
		t.Errorf("an all-notices run should keep the single-number sentence:\n%s", got)
	}
}
