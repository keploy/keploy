package replay

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A wrong answer is demoted to OBSOLETE, with the proven switch off, when the
// dependency calls the test made diverged from its recorded mapping: on the
// regular path when it left a recorded mock of its mapping unconsumed, on the
// streaming path when it consumed a mock outside the mapping. OBSOLETE does
// not fail a set, so such a set used to read PASSED although a test answered
// wrong (false-pass row 0). The test now has no verdict (MappingStale), and
// its set is not judged (INTERNAL_ERR), with the stale mapping as its reason
// and its remedy. A wrong answer the demotion does not take stays FAILED and
// fails its set, and a right answer is PASSED whatever its mocks did.
func TestADemotedWrongAnswerLeavesItsTestAndSetNotJudged(t *testing.T) {
	for _, c := range []struct {
		name      string
		streaming bool
		entry     []string // test-2's recorded mapping
		consumed  []string // what test-2 consumed
		wrong     bool
		wantTest  models.TestStatus
		wantStale bool
		wantSet   models.TestSetStatus
		notJudged bool
	}{
		{"regular, a mock of its mapping left unconsumed", false, []string{"own-2", "own-2b"}, []string{"own-2"}, true,
			models.TestStatusObsolete, true, models.TestSetStatusInternalErr, true},
		{"regular, a mock outside its mapping consumed", false, []string{"own-2"}, []string{"own-2", "extra-x"}, true,
			models.TestStatusFailed, false, models.TestSetStatusFailed, false},
		{"regular, its mapping consumed exactly", false, []string{"own-2"}, []string{"own-2"}, true,
			models.TestStatusFailed, false, models.TestSetStatusFailed, false},
		{"streaming, a mock outside its mapping consumed", true, []string{"own-2"}, []string{"own-2", "extra-x"}, true,
			models.TestStatusObsolete, true, models.TestSetStatusInternalErr, true},
		{"streaming, a mock of its mapping left unconsumed", true, []string{"own-2", "own-2b"}, []string{"own-2"}, true,
			models.TestStatusFailed, false, models.TestSetStatusFailed, false},
		{"streaming, its mapping consumed exactly", true, []string{"own-2"}, []string{"own-2"}, true,
			models.TestStatusFailed, false, models.TestSetStatusFailed, false},
		{"regular, a right answer with a mock of its mapping left unconsumed", false, []string{"own-2", "own-2b"}, []string{"own-2"}, false,
			models.TestStatusPassed, false, models.TestSetStatusPassed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			if c.streaming {
				h.streaming("test-2")
			}
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{
				"test-1": mongoEntries("own-1"),
				"test-2": mongoEntries(c.entry...),
			}
			consumed := make([]models.MockState, 0, len(c.consumed))
			for i, n := range c.consumed {
				consumed = append(consumed, mongoAt(n, 2050+i))
			}
			hooks := prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
				"test-1": {mongoAt("own-1", 1050)}, "test-2": consumed,
			})}
			if c.wrong {
				hooks.wrongBody = map[string]bool{"test-2": true}
			}
			h.replayer.hookImpl = hooks

			if status := h.run(t); status != c.wantSet {
				t.Fatalf("the set is %s; want %s", status, c.wantSet)
			}
			if got := h.replayer.notJudged("test-set-0"); got != c.notJudged {
				t.Fatalf("the set's not-judged mark is %v; want %v", got, c.notJudged)
			}
			// A demoted wrong answer has no verdict whatever its set's status,
			// for the run's exit code to read (Replayer.stoppedAfterNoVerdict).
			if got, want := fmt.Sprint(h.replayer.noVerdicts()), map[bool]string{true: "map[test-set-0:[test-2]]", false: "map[]"}[c.wantStale]; got != want {
				t.Fatalf("the run's marks of tests with no verdict are %s; want %s", got, want)
			}
			var row models.TestResult
			for _, r := range h.report.results {
				if r.TestCaseID == "test-2" {
					row = r
				}
			}
			if row.Status != c.wantTest {
				t.Fatalf("test-2 is %s; want %s", row.Status, c.wantTest)
			}
			if stale := row.FailureInfo.MappingStale != ""; stale != c.wantStale {
				t.Fatalf("test-2's MappingStale = %q; want set=%v", row.FailureInfo.MappingStale, c.wantStale)
			}
			if unknown := len(row.VerdictUnknown()) > 0; unknown != c.wantStale {
				t.Fatalf("test-2's verdict unknown = %v (%v); want %v", unknown, row.VerdictUnknown(), c.wantStale)
			}
			if c.notJudged {
				h.report.mu.Lock()
				rep := h.report.report
				h.report.mu.Unlock()
				if rep == nil || !strings.Contains(rep.FailureReason, "stale mapping") || !strings.Contains(rep.FailureReason, "--disable-mapping --update-test-mapping") {
					t.Fatalf("the not-judged set's report does not give the stale mapping and its remedy: %+v", rep)
				}
			}
		})
	}
}

// A set with a verdict of its own keeps it when one of its tests is a wrong
// answer demoted to OBSOLETE: a FAILED test failed whatever another's mapping
// says, and a set that stopped part-way says so first. Its reason names the
// tests not judged, after its own result, and does not send its user to
// re-record first. A set not judged only for a stale mapping carries no
// application logs: the application is not at fault.
func TestAStaleMappingLeavesTheSetsOwnVerdict(t *testing.T) {
	type row struct {
		entry, consumed []string
		wrong           bool
	}
	for _, c := range []struct {
		name          string
		tests         []row
		failUpdateNth int
		wantSet       models.TestSetStatus
		notJudged     bool
		reason        []string
	}{
		{
			name: "another test failed",
			tests: []row{
				{[]string{"own-1"}, []string{"own-1"}, false},
				{[]string{"own-2", "own-2b"}, []string{"own-2"}, true}, // demoted
				{[]string{"own-3"}, []string{"own-3"}, true},           // fails
			},
			wantSet: models.TestSetStatusFailed,
			reason:  []string{"1 test(s) (test-2) were not judged", "deal with the set's own result first"},
		},
		{
			name: "the set stopped part-way",
			tests: []row{
				{[]string{"own-1"}, []string{"own-1"}, false},
				{[]string{"own-2", "own-2b"}, []string{"own-2"}, true}, // demoted
				{[]string{"own-3"}, []string{"own-3"}, false},
				{[]string{"own-4"}, []string{"own-4"}, false},
			},
			failUpdateNth: 4,
			wantSet:       models.TestSetStatusFaultUserApp,
			reason:        []string{partialRunReason, "1 test(s) (test-2) were not judged"},
		},
		{
			name: "only the stale mapping",
			tests: []row{
				{[]string{"own-1"}, []string{"own-1"}, false},
				{[]string{"own-2", "own-2b"}, []string{"own-2"}, true}, // demoted
			},
			wantSet:   models.TestSetStatusInternalErr,
			notJudged: true,
			reason:    []string{"not judged: 1 test(s) (test-2)"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, len(c.tests), c.failUpdateNth)
			pinWindows(h)
			h.instr.recentAppLogs = "APP-LOG-LINE"
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{}
			byTest := map[string][]models.MockState{}
			hooks := prHooks{wrongBody: map[string]bool{}}
			for i, r := range c.tests {
				id := fmt.Sprintf("test-%d", i+1)
				h.mappings.onDisk[id] = mongoEntries(r.entry...)
				for k, n := range r.consumed {
					byTest[id] = append(byTest[id], mongoAt(n, 1000*(i+1)+50+k))
				}
				if r.wrong {
					hooks.wrongBody[id] = true
				}
			}
			hooks.consumedFor = consumedByWindow(h, byTest)
			h.replayer.hookImpl = hooks

			if status := h.run(t); status != c.wantSet {
				t.Fatalf("the set is %s; want %s", status, c.wantSet)
			}
			if got := h.replayer.notJudged("test-set-0"); got != c.notJudged {
				t.Fatalf("the set's not-judged mark is %v; want %v", got, c.notJudged)
			}
			h.report.mu.Lock()
			rep := h.report.report
			h.report.mu.Unlock()
			if rep == nil {
				t.Fatal("no report")
			}
			for _, want := range c.reason {
				if !strings.Contains(rep.FailureReason, want) {
					t.Fatalf("the report's reason %q lacks %q", rep.FailureReason, want)
				}
			}
			if c.notJudged && rep.AppLogs != "" {
				t.Fatalf("a set not judged only for a stale mapping carries application logs %q", rep.AppLogs)
			}
			for _, r := range h.report.results {
				if r.TestCaseID == "test-2" && (r.Status != models.TestStatusObsolete || r.FailureInfo.MappingStale == "") {
					t.Fatalf("test-2 is %s with MappingStale %q; want OBSOLETE and not judged", r.Status, r.FailureInfo.MappingStale)
				}
			}
		})
	}
}
