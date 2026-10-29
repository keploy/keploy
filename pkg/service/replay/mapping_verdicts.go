package replay

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

// Which tests' consumption a mapping write may take.
//
// A replay records what each test consumed (carryOverAttribution.mapConsumed)
// and, with --update-test-mapping (enterprise's auto-replay sets it), or when
// the set has no mappings.yaml yet, writes that as the test's mapping. A
// mapping-based replay then serves the test only the mocks its mapping names.
// So a mapping written from a replay that went wrong is the wrong replay made
// permanent: in keploy/enterprise#2835 a matcher served the second of two
// identical reads first, the test failed, its mapping became the one mock it
// had consumed, and no later replay, with a fixed matcher or not, could serve
// it the other two.
//
// The rule: only a PASSING test's consumption is its mapping (see below for
// the flagged mocks of its window that others consumed). Every other test
// keeps what record gave it: its entry's mocks in mappings.yaml as they are,
// or, with no entry, none, so the agent goes on serving it the mocks of its
// recorded window (models.MergeStartupMockNames hands it no names, and
// Agent.loadPerTestMocks loads its window), the mocks exactly as record wrote
// them. A list of the window's mocks computed here instead would be this
// replay's approximation of record's binding (which also settles concurrent
// windows and late mocks), and would switch the test to mapping mode on it.
// The same holds for a mappings.yaml the run creates (no file yet, the default
// flag): it has no entry for a test that did not pass.
//
// A passing test's entry is what the window attribution gives it, not only
// what it consumed itself: a flagged mock (a carry-over send, a pushed
// message) recorded in its window is its own wherever it was consumed, so one
// that a failing test consumed can be in a passing test's entry. The startup
// section, which a run where some test passed writes, takes a flagged mock
// from before every window and the app's startup traffic (bootBand), whichever
// test's drain caught it, a failing one's included.
// That is where record put the mock, not the failing test's replay.
//
// This is the mapping only. The mocks themselves are the prune's: with
// --remove-unused-mocks and PreserveFailedMocks off, the prune still deletes
// a failed test's mocks that no passing test consumed (the startup band and
// carry-over keeps aside), as it did before, and its kept entry then names
// mocks that are gone. A test the hook passes as noisy is kept whole:
// retainNoisyTestCaseMocks keeps what it consumed, which is what its entry is
// written from.
//
// What counts as passing is decided per result row, on the verdicts as the
// report finally holds them: read at the write, after the BeforeTestResult
// hook and (under KEPLOY_PROVEN) the evidence floor that holds a test with
// evidence against it FAILED whatever the hook set. The hook may set verdicts:
// enterprise's auto-noise hook passes a failure whose only difference was
// noise, and that test's consumption is then a passing test's, with one
// exception (the OBSOLETE shape, below). A hook judges a pass on the row:
// GetNoisyTestCaseNames is not one (k8s-proxy's also names failed tests it
// keeps for the prune), so a test a hook only names as noisy, its row left
// FAILED, keeps its recorded mapping.
//
// The rule keeps "PASSED in the report" and "its consumption is written" the
// same wherever it can, as what reads the report as a pass (enterprise's
// suite linker) reads the mapping for that test's mocks. The exceptions below
// that hold back a PASSED row (misses that could not be read, under the
// switch; a consumption that could not be read; the OBSOLETE shape) leave a
// file that exists as it is; only a run that creates the file can leave such a
// test without an entry.
//
// A passing test that consumed no mock of its own gets an entry with none
// (addConsumedNothingEntries), so a test with no entry is one the run gave no
// verdict on.
//
//   - PASSED: passing. A pass that left a recorded mock unconsumed is a pass
//     when its answer matched (it stays PASSED only while --assert-dependencies
//     and the proven switch are both off; either fails it): its consumption
//     drops that mock from its mapping, which is what refreshing a mapping is
//     for.
//   - PASSED with a call no recorded mock answered (a miss the app swallowed,
//     still answering as recorded): passing with the switch off, as its
//     verdict and its report say, when it consumed every recorded mock of its
//     mapping. Most such calls have no recorded mock at all (a fire-and-forget
//     publish the app ignores), and holding its mapping back would leave a
//     test the report passes without one. The proven model fails it
//     (keploy#4748: a miss fails its test), and as FAILED it keeps its
//     mapping. "A FAILED test whose response matched but that had a miss" is
//     that test.
//   - PASSED with such a miss and a recorded mock of its mapping left
//     unconsumed: not passing (passedMissShaped). That is a wrong serve the
//     app swallowed: the #2835 tie-break serving the second read's answer
//     first, the first read's mock then left over and the second read
//     unanswered. Written, its mapping would drop the mock the miss should
//     have matched, and a replay with the matcher fixed would fail on the
//     cut-down mapping and keep it. Its entry stays as it is, which the test
//     passed on. Like the OBSOLETE shape it needs a mapping in force, so a run
//     that creates the file maps it.
//   - PASSED whose misses could not all be read (UnmatchedCallsUnread, set only
//     under KEPLOY_PROVEN): not passing. Under the switch that row has no
//     verdict whatever its status says, and its set is not judged
//     (INTERNAL_ERR).
//   - PASSED whose consumption could not be read (the read of what it consumed
//     failed, in any cycle, switch on or off): not passing for the mapping.
//     What it consumed is not known, and the entry the run holds for it is
//     only part of it: what was read before a read failed (a streamed test's
//     pre-stream mocks) and what the window attribution gave it from other
//     tests' reads. Under the switch such a test has no verdict either
//     (consumedUnread).
//   - FAILED: not passing, whatever failed it: the response, a template value,
//     --strict-failure, --assert-dependencies or a strict schema-noise reject
//     (the last two fail a test whose answer matched), or, under the proven
//     model, a miss or an unconsumed test mock (a FAILED test whose response
//     matched included).
//   - OBSOLETE: not passing. The answer was wrong and a recorded mock went
//     unconsumed: the very shape of a wrong-mock serve.
//   - That OBSOLETE shape under another verdict: not passing, whatever the
//     final row says. --strict-failure (or a template failure, or a miss)
//     labels it FAILED instead, and a hook may then pass it: enterprise's
//     auto-noise hook passes a failure whose differences are values only,
//     which is also what a wrong mock's answer can look like, and it does not
//     look at what the test consumed. Whether the mapping is kept must not
//     turn on that label. A hook-passed test whose consumption covered its
//     recorded mapping is a pass. (The streaming pass also counts a wrong
//     answer that consumed a mock outside its mapping, which is what OBSOLETE
//     means there.) The shape can only show while a mapping is in force for
//     the test: in a run with no mappings.yaml yet (the first auto-replay of a
//     sandbox recording, where its mappings are created), with
//     --disable-mapping (the remap the hints suggest), or for a test with no
//     entry, there is no recorded mock to leave unconsumed, so a value-only
//     wrong serve that a hook passes is mapped there, as a wrong serve that
//     answered as recorded always was. And it sees only the mocks the per-test
//     assertion takes part in (eligibleExpectedEntries): session-tier mocks,
//     such as untagged HTTP, Postgres and MySQL egress, never show as left
//     unconsumed, so for them the shape does not show either.
//   - IGNORED: not passing. A test the user ignores never runs, and one a hook
//     quarantined after it ran was not judged a pass.
//   - PENDING, RUNNING, or anything else: not passing. No final verdict.
//   - A test with no result row at all: not passing.
//   - A test with more than one row (a RetryPassing cycle's failure written
//     beside an earlier cycle's PASSED row): passing only if every row is.

// passingTests is every test whose consumption may be its mapping: each of its
// result rows is a pass (passedWithAVerdict), and it is in none of held, the
// tests the run itself knows not to count whatever their rows say (RunTestSet
// passes those whose consumption could not be read, the OBSOLETE-shaped, and
// passedMissShaped's).
// A test with no row is not in it.
func passingTests(results []models.TestResult, held ...[]string) map[string]bool {
	passing := make(map[string]bool, len(results))
	for _, r := range results {
		prev, seen := passing[r.TestCaseID]
		passing[r.TestCaseID] = (prev || !seen) && passedWithAVerdict(r)
	}
	for _, ids := range held {
		for _, id := range ids {
			delete(passing, id)
		}
	}
	for id, ok := range passing {
		if !ok {
			delete(passing, id)
		}
	}
	return passing
}

// passedMissShaped names the tests among left, those that left a recorded mock
// of their mapping unconsumed, whose result rows report a data-plane call no
// mock answered (DNS is left out, as everywhere a miss is judged): a wrong
// serve the app swallowed, whatever verdict the rows give it.
func passedMissShaped(results []models.TestResult, left []string) []string {
	if len(left) == 0 {
		return nil
	}
	isLeft := make(map[string]bool, len(left))
	for _, id := range left {
		isLeft[id] = true
	}
	var out []string
	for _, r := range results {
		if isLeft[r.TestCaseID] && dataPlaneMisses(r.FailureInfo.UnmatchedCalls) > 0 {
			out = appendOnce(out, r.TestCaseID)
		}
	}
	return out
}

// passedWithAVerdict reports whether one result row is a pass whose
// consumption may be its mapping: PASSED, and not a row the proven switch
// leaves without a verdict.
func passedWithAVerdict(r models.TestResult) bool {
	return r.Status == models.TestStatusPassed && !r.FailureInfo.UnmatchedCallsUnread && !r.FailureInfo.CaptureUnread
}

// leftRecordedMockUnconsumed reports whether a test left a mock of its recorded
// mapping unconsumed, on the main loop's terms (mockSetMismatch): the mapped
// entries the per-test assertion takes part in (eligibleExpectedEntries)
// against what it consumed, less what the assertion leaves out. With the switch
// on or off; the streaming pass asks it, as its own mock-set check asks the
// other way round (a consumed mock outside the mapping).
func leftRecordedMockUnconsumed(expected []models.MockEntry, consumed []models.MockState, kinds map[string]models.Kind, excluded map[string]bool) bool {
	got := make(map[string]bool, len(consumed))
	for _, m := range consumed {
		if !consumedOutsideAssertion(m) {
			got[m.Name] = true
		}
	}
	for _, m := range eligibleExpectedEntries(expected, kinds, excluded) {
		if !got[m.Name] {
			return true
		}
	}
	return false
}

// markWrittenByKeployTest names keploy test as the source of doc's test
// entries (models.MappingSourceKeployTest), when it has any: entries a replay
// consumed, not what each test recorded, which under the proven switch do not
// say what a test owns (provenMappings). A write of the startup section alone
// keeps the file's source.
func markWrittenByKeployTest(doc *models.Mapping) {
	if doc != nil && len(doc.TestCases) > 0 {
		doc.Source = models.MappingSourceKeployTest
	}
}

// addConsumedNothingEntries gives every passing test that has no entry in doc
// an entry with no mock: it passed, and consumed no mock but the startup
// section's, which the same file lists. none names the tests whose consumption
// the run read and found empty of any other
// (carryOverAttribution.consumedNothing); a test outside it gets nothing, and
// none does when the run read no test's consumption (none nil: not
// instrumented, so the agent was never asked). The entries are added in the
// order of results; the write sorts them by test (mapdb).
//
// Without this "no entry" meant two things: the test needs no mock, or the run
// gave no verdict on it (it failed, was held, or got none). A replay that
// downloads only the mocks a mapping names, and skips a test with no entry,
// could not tell them apart, and would skip for good every test that needs no
// mock: its next auto-replay gives it no entry again. With it, no entry means
// only "no verdict". A local replay serves an empty entry as it serves no
// entry (models.MergeStartupMockNames hands it no names: its recorded window).
//
// The holds that keep a run from creating a file that maps no mock decide on
// mapsAMock, and an empty entry maps none. A new file is created only when
// every passing test has an entry (passingWithNoEntry), an empty one counting.
func addConsumedNothingEntries(doc *models.Mapping, results []models.TestResult, passing map[string]bool, none map[string]struct{}) {
	if doc == nil || len(none) == 0 {
		return
	}
	has := make(map[string]bool, len(doc.TestCases))
	for _, tc := range doc.TestCases {
		has[tc.ID] = true
	}
	for _, r := range results {
		id := r.TestCaseID
		if _, ok := none[id]; !ok || has[id] || !passing[id] {
			continue
		}
		has[id] = true
		doc.TestCases = append(doc.TestCases, models.MappedTestCase{ID: id})
	}
}

// passingWithNoEntry names, once each and sorted, the passing tests doc holds
// no entry for: tests the run could not tell what they
// consumed (each mock they consumed fell in no test's window or in another
// test's, or the run read no consumption). A file created from doc would leave
// such a test with no entry, which a replay that skips a test with no entry
// reads as "no verdict" and skips, though it passed: RunTestSet creates no
// file then.
func passingWithNoEntry(doc *models.Mapping, results []models.TestResult, passing map[string]bool) []string {
	seen := make(map[string]bool, len(results))
	if doc != nil {
		for _, tc := range doc.TestCases {
			seen[tc.ID] = true
		}
	}
	var out []string
	for _, r := range results {
		id := r.TestCaseID
		if !passing[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// mapsAMock reports whether doc maps at least one mock to a test.
func mapsAMock(doc *models.Mapping) bool {
	if doc == nil {
		return false
	}
	for _, tc := range doc.TestCases {
		if len(tc.Mocks) > 0 {
			return true
		}
	}
	return false
}

// passingEntriesOf is the mapping document a run may write: doc with the entries
// of the passing tests only, and the startup section as it is. kept names the
// tests whose consumption entry was left out, in doc order, so that
// mappings.yaml keeps their recorded mocks (mapdb.Insert replaces only the
// entries a document holds). doc is not changed.
func passingEntriesOf(doc *models.Mapping, passing map[string]bool) (out *models.Mapping, kept []string) {
	if doc == nil {
		return nil, nil
	}
	cp := *doc
	cp.TestCases = make([]models.MappedTestCase, 0, len(doc.TestCases))
	for _, tc := range doc.TestCases {
		if !passing[tc.ID] {
			kept = append(kept, tc.ID)
			continue
		}
		tc.Mocks = append([]models.MockEntry(nil), tc.Mocks...)
		cp.TestCases = append(cp.TestCases, tc)
	}
	return &cp, kept
}
