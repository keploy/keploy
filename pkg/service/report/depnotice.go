package report

import (
	"fmt"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// The COMPACT, BOUNDED, DEDUPLICATED dependency block for `keploy report`.
//
// WHY IT IS GROUPED BY DEPENDENCY AND NOT BY TEST. The population this block
// describes is dominated by one shape: a single outgoing call the app stopped
// making (or, per the caveat below, makes after flushing its response) shows
// up as the SAME missing dependency on every test that used it. Rendering one
// four-line block per test turned a 16-line all-green `keploy report` into
// 1,217 lines on a 300-test suite, every block repeating one identical
// sentence, and put a <system-out> on all 300 JUnit cases. Grouping collapses
// that to a single line naming the dependency and how many tests it affected,
// which is the sentence a human or an agent actually acts on.
//
// WHY IT IS CAPPED TWICE. Grouping alone is not a bound: a suite where every
// test loses a DIFFERENT dependency has as many groups as tests. So the number
// of groups named is capped (depNoticeGroupCap) and the tests listed under
// each group are capped (depNoticeTestSampleSize), with the remainder carried
// as counts. Nothing is silently dropped — the totals are always exact, and
// the full per-test rows stay in the report file and in `--format json`, which
// is where an exhaustive consumer should be reading them from anyway.
//
// WHAT IT DELIBERATELY DOES NOT DO: offer a name/glob suppression list. The
// documented false-positive class (a call made after the response is written)
// is a TIMING artifact, not a dependency a user wants permanently unwatched;
// globbing it away restores exactly the silent green this slice exists to
// close, and would do so for the real regression on the same dependency too.
// One summary line per dependency is the tuning-down.
const (
	// depNoticeGroupCap bounds how many DISTINCT dependency rows are named.
	depNoticeGroupCap = 10
	// depNoticeTestSampleSize bounds how many test IDs are listed per
	// dependency here, and how many dependency lines depNonFailureBody puts
	// in a JUnit <system-out> / <skipped> body. Mirrors depWarnSampleSize on
	// the replayer's own end-of-test-set Warn, so every sampled surface uses
	// one number: whatever a reader is willing to skim in a summary is the
	// same whether it is test ids or dependency names.
	depNoticeTestSampleSize = 5
)

// depNoticeGroup is one missing dependency and the tests that lost it.
type depNoticeGroup struct {
	Name  string
	Tests []string
}

// depNoticeTestLabel is the identity a test is listed under. TestCaseID is the
// id every other `keploy report` surface prints; Name is the fallback for
// results persisted without one.
func depNoticeTestLabel(t models.TestResult) string {
	if t.TestCaseID != "" {
		return t.TestCaseID
	}
	return t.Name
}

// groupDepNotices inverts test -> missing dependencies into dependency ->
// tests, ordered by blast radius (most tests first) and then by name, so the
// dependency a user should look at first is printed first and two runs over
// the same data produce identical output.
func groupDepNotices(tests []models.TestResult) []depNoticeGroup {
	byDep := map[string][]string{}
	for _, t := range tests {
		label := depNoticeTestLabel(t)
		for _, name := range models.MissingDepNames(t.Result.DepResult) {
			byDep[name] = append(byDep[name], label)
		}
	}

	groups := make([]depNoticeGroup, 0, len(byDep))
	for name, labels := range byDep {
		sort.Strings(labels)
		groups = append(groups, depNoticeGroup{Name: name, Tests: labels})
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Tests) != len(groups[j].Tests) {
			return len(groups[i].Tests) > len(groups[j].Tests)
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// renderDepNoticeSummary renders the block.
//
// notices is the folded-in population (every admitted result that is NOT
// getting a full diff); admitted is the whole set the run admitted, FAILED
// tests included. BOTH counts are stated because the run has two different
// true answers to "how many tests lost a dependency" and they must not read as
// a contradiction: `keploy report --summary` counts every status
// (report.depNoticeCounts), while this block only folds in the tests that were
// not failed for it. A run with 3 PASSED and 2 FAILED tests all losing the same
// dependency otherwise printed "3" here and "5" under --summary, and an agent
// diffing the two invocations got two numbers for one question.
//
// Returns "" when no folded-in test lost a dependency, which is what keeps
// `keploy report` byte-identical to pre-slice-4 for every report that carries
// no missing row.
func renderDepNoticeSummary(notices, admitted []models.TestResult) string {
	groups := groupDepNotices(notices)
	if len(groups) == 0 {
		return ""
	}

	// Counted, not len(): the caller hands over every result in each
	// population, and a clean test among them must not be reported as having
	// lost something.
	//
	// total >= affected holds by construction and is NOT re-checked here:
	// notices is the second return of partitionDepNotices over admitted, so it
	// is a subset of it, and countWithMissingDeps is monotonic over subsets. A
	// defensive `if total < affected { total = affected }` clamp was
	// unreachable from the only call site — it could not be tested and would
	// have quietly papered over a caller that had genuinely paired the wrong
	// two populations. If a future caller can pass a narrower admitted set,
	// fix the pairing there.
	affected := countWithMissingDeps(notices)
	total := countWithMissingDeps(admitted)

	var sb strings.Builder
	sb.WriteString("\n=== DEPENDENCIES NOT EXERCISED ===\n")
	if total == affected {
		fmt.Fprintf(&sb, "%d test(s) did not exercise a recorded outgoing call and were NOT failed for it.\n", affected)
	} else {
		fmt.Fprintf(&sb, "%d test(s) did not exercise a recorded outgoing call; %d of them were NOT failed for it and are summarised below.\n",
			total, affected)
	}
	fmt.Fprintf(&sb, "(%s)\n", models.DepNoticeHint)
	// The caveat belongs on the surface a human reads, not only in the flag
	// help: consumed mocks are drained the moment the response comes back, so
	// a call made after that point is attributed to the next test. Without
	// this sentence the block reads as a regression report for a benign
	// fire-and-forget write.
	sb.WriteString("A call your app makes AFTER writing its response is attributed to the NEXT test, not this one.\n")

	shown := groups
	if len(shown) > depNoticeGroupCap {
		shown = shown[:depNoticeGroupCap]
	}
	for _, g := range shown {
		fmt.Fprintf(&sb, "  %s %s\n", models.DepNoticePrefix, g.Name)
		fmt.Fprintf(&sb, "      %d test(s): %s\n", len(g.Tests), sampleList(g.Tests, depNoticeTestSampleSize))
	}
	if rest := len(groups) - len(shown); rest > 0 {
		fmt.Fprintf(&sb, "  ...and %d more dependenc%s not exercised - see the report file or `keploy report --format json`\n",
			rest, plural(rest, "y", "ies"))
	}
	sb.WriteString("--------------------------------------------------------------------\n")
	return sb.String()
}

// countWithMissingDeps counts the results carrying at least one failed
// dependency assertion.
func countWithMissingDeps(tests []models.TestResult) int {
	n := 0
	for _, t := range tests {
		if t.Result.HasMissingDeps() {
			n++
		}
	}
	return n
}

// sampleList joins at most n entries and reports how many were elided.
func sampleList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s ...and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
