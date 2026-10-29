package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// RunTestSet-level tests for which tests' consumption a mapping write may take:
// only a test whose replay passed. They write and read a real mappings.yaml
// through mapdb, the store `keploy record` writes and `keploy test` reads, so
// what they assert is the file the next replay loads.

// mongoAt is a per-test Mongo mock as the agent reports it consumed, recorded
// ms milliseconds after attr0 (see pinWindows: test-i's window is
// [i s, i s + 100 ms]).
func mongoAt(name string, ms int) models.MockState {
	return models.MockState{
		Name: name, Kind: models.Mongo, Usage: models.Deleted,
		Lifetime:         models.LifetimePerTest,
		ReqTimestampMock: models.FormatMockTimestamp(attrAt(ms)),
		ResTimestampMock: models.FormatMockTimestamp(attrAt(ms + 1)),
	}
}

func mongoEntries(names ...string) []models.MockEntry {
	out := make([]models.MockEntry, 0, len(names))
	for _, n := range names {
		out = append(out, models.MockEntry{Name: n, Kind: string(models.Mongo)})
	}
	return out
}

// mappingFile is the harness's set with a real mappings.yaml. recorded, when
// not nil, is written first, as `keploy record` writes it (UpsertBatch).
func mappingFile(t *testing.T, h *prRun, recorded map[string][]models.MockEntry) *mapdb.MappingDb {
	t.Helper()
	db := mapdb.New(zap.NewNop(), h.replayer.config.Path, "")
	if recorded != nil {
		if err := db.UpsertBatch(context.Background(), "test-set-0", recorded); err != nil {
			t.Fatalf("writing the recorded mappings.yaml: %v", err)
		}
	}
	h.replayer.mappingDB = db
	return db
}

// mappedOnDisk is each test's mapping as mappings.yaml holds it after the run,
// by mock name in file order; nil when there is no file.
func mappedOnDisk(t *testing.T, db *mapdb.MappingDb) map[string][]string {
	t.Helper()
	exists, err := db.Exists(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("checking for mappings.yaml: %v", err)
	}
	if !exists {
		return nil
	}
	byTest, _, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("reading mappings.yaml back: %v", err)
	}
	out := make(map[string][]string, len(byTest))
	for id, entries := range byTest {
		for _, e := range entries {
			out[id] = append(out[id], e.Name)
		}
	}
	return out
}

// consumedByWindow answers each GetConsumedMocks call with what the test whose
// window the agent was last told consumed (none for the set's setup drain).
func consumedByWindow(h *prRun, byTest map[string][]models.MockState) func() []models.MockState {
	byStart := map[string]string{}
	for _, tc := range h.cases {
		byStart[tc.HTTPReq.Timestamp.String()] = tc.Name
	}
	return func() []models.MockState {
		h.instr.mu.Lock()
		after := h.instr.windowAfter
		h.instr.mu.Unlock()
		return byTest[byStart[after.String()]]
	}
}

func statusOf(t *testing.T, h *prRun, test string) models.TestStatus {
	t.Helper()
	for _, r := range h.report.results {
		if r.TestCaseID == test {
			return r.Status
		}
	}
	t.Fatalf("no result for %s", test)
	return ""
}

// The parse-server incident (keploy/enterprise#2835), at the level the mapping
// is written. POST /schemas/CovClass makes the app send find, insert, find on
// one connection; record mapped all three to the test. A wrong tie-break served
// the second find's answer (mock-110) first, the app answered 400 without
// sending the rest, and the test did not pass. Auto-replay runs with
// --update-test-mapping, so the test's mapping became [mock-110]: every later
// mapping-based replay could serve it only that mock, and it could never pass
// again, whatever matcher replayed it.
//
// A test that did not pass keeps the mapping record wrote for it, whichever
// verdict it got: OBSOLETE as the sandbox auto-replay judges it (a wrong answer
// with recorded mocks left unconsumed), FAILED under --strict-failure, as the
// `keploy record` auto-replay judges it. A passing test's consumption still
// refreshes its own (the control: a writer that never writes would pass the
// rest).
func TestRunTestSetKeepsAFailedTestsRecordedMapping(t *testing.T) {
	for _, tc := range []struct {
		name          string
		strictFailure bool
		want          models.TestStatus
	}{
		{name: "sandbox auto-replay", want: models.TestStatusObsolete},
		{name: "--strict-failure", strictFailure: true, want: models.TestStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			// What enterprise's sandbox auto-replay sets (cmd/enterprise/cli/auto_replay.go).
			h.replayer.config.Test.UpdateTestMapping = true
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.PreserveFailedMocks = true
			h.replayer.config.Test.StrictFailure = tc.strictFailure
			db := mappingFile(t, h, map[string][]models.MockEntry{
				"test-1": mongoEntries("mock-108", "mock-109", "mock-110"),
				"test-2": mongoEntries("mock-111"),
			})
			h.replayer.hookImpl = prHooks{
				wrongBody: map[string]bool{"test-1": true},
				consumedFor: consumedByWindow(h, map[string][]models.MockState{
					"test-1": {mongoAt("mock-110", 1032)},
					"test-2": {mongoAt("mock-111", 2010), mongoAt("mock-112", 2020)},
				}),
			}
			_ = h.run(t)

			if got := statusOf(t, h, "test-1"); got != tc.want {
				t.Fatalf("precondition: test-1 should be %s, got %s", tc.want, got)
			}
			onDisk := mappedOnDisk(t, db)
			if want := []string{"mock-108", "mock-109", "mock-110"}; !reflect.DeepEqual(onDisk["test-1"], want) {
				t.Fatalf("test-1 is %s and mappings.yaml now maps it to %v; want its recorded mapping %v, unchanged", tc.want, onDisk["test-1"], want)
			}
			if want := []string{"mock-111", "mock-112"}; !reflect.DeepEqual(onDisk["test-2"], want) {
				t.Fatalf("test-2 passed and mappings.yaml maps it to %v; want what it consumed, %v", onDisk["test-2"], want)
			}
		})
	}
}

// Every verdict other than a pass keeps the recorded mapping, on both write
// paths (--update-test-mapping into a file, and the default flag creating one).
// Each case's test-1 consumed something other than its recorded mapping; test-2
// passes and is the control.
func TestRunTestSetOnlyAPassingTestsConsumptionIsItsMapping(t *testing.T) {
	recorded := map[string][]models.MockEntry{
		"test-1": mongoEntries("mock-a", "mock-b"),
		"test-2": mongoEntries("mock-c"),
	}
	for _, tc := range []struct {
		name string
		// consumed1 is what test-1 consumed.
		consumed1 []models.MockState
		// wrongBody fails test-1's response.
		wrongBody     bool
		strictFailure bool
		// miss1 is a call test-1 made that no mock answered.
		miss1 *models.UnmatchedCall
		// proven turns KEPLOY_PROVEN on, under which a miss fails its test.
		proven bool
		// disableMapping is --disable-mapping: each test is served its
		// recorded window, whatever its mapping.
		disableMapping bool
		// recorded is the file before the run; nil: there is none.
		recorded      map[string][]models.MockEntry
		updateMapping bool
		// want is test-1's mapping on disk after the run (nil: no entry).
		want       []string
		wantStatus models.TestStatus
	}{
		{
			// Consumed every recorded mock, and one more, and answered wrong.
			name:      "failed: response did not match",
			consumed1: []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)}, wrongBody: true,
			recorded: recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusFailed,
		},
		{
			// Left a recorded mock unconsumed and answered wrong.
			name:      "obsolete: response did not match and a recorded mock went unconsumed",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)}, wrongBody: true,
			recorded: recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusObsolete,
		},
		{
			name:      "failed: the same, under --strict-failure",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)}, wrongBody: true, strictFailure: true,
			recorded: recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusFailed,
		},
		{
			// The app swallowed the unanswered call's error and still answered
			// as recorded, having consumed every recorded mock (a
			// fire-and-forget publish no mock was recorded for): its verdict,
			// and its report, is a pass (the proven model's is not).
			name:      "passed with a call no mock answered",
			consumed1: []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)},
			miss1:     &models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "publish audit-event"},
			recorded:  recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b", "mock-z"}, wantStatus: models.TestStatusPassed,
		},
		{
			// #2835 with the error swallowed: the first read was served the
			// second's answer (mock-z), the second read went unanswered, and
			// the first read's mock-a was left over. A pass, whose mapping
			// written from that would lose mock-a for good.
			name:      "passed with a call no mock answered and a recorded mock left unconsumed",
			consumed1: []models.MockState{mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)},
			miss1:     &models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find _SCHEMA"},
			recorded:  recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusPassed,
		},
		{
			// A DNS miss is no miss here, as nowhere else (resolution order
			// is not deterministic): a pass that left mock-a unconsumed.
			name:      "passed with a DNS query no mock answered and a recorded mock left unconsumed",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)},
			miss1:     &models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A mongo.internal."},
			recorded:  recorded, updateMapping: true,
			want: []string{"mock-b"}, wantStatus: models.TestStatusPassed,
		},
		{
			// The same, creating the file: a test the report passes is mapped.
			name:      "passed with a call no mock answered, default flag, no file",
			consumed1: []models.MockState{mongoAt("mock-a", 1010)},
			miss1:     &models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "publish audit-event"},
			want:      []string{"mock-a"}, wantStatus: models.TestStatusPassed,
		},
		{
			// The same miss under the proven switch fails the test, though its
			// answer matched and it consumed every recorded mock (so the
			// switch's unconsumed-mock rule does not fail it). Under the
			// switch only record writes mappings.yaml: the file is
			// left as recorded, the passing test-2's entry too.
			name:      "failed under KEPLOY_PROVEN: a call no mock answered, response matched",
			consumed1: []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)},
			miss1:     &models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "publish audit-event"},
			proven:    true,
			recorded:  recorded, updateMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusFailed,
		},
		{
			// The same with no file: under the switch none is created.
			name:      "failed under KEPLOY_PROVEN: a call no mock answered, default flag, no file",
			consumed1: []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-z", 1030)},
			miss1:     &models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "publish audit-event"},
			proven:    true,
			want:      nil, wantStatus: models.TestStatusFailed,
		},
		{
			name:      "passed: the control",
			consumed1: []models.MockState{mongoAt("mock-a", 1010)},
			recorded:  recorded, updateMapping: true,
			want: []string{"mock-a"}, wantStatus: models.TestStatusPassed,
		},
		{
			// No entry: the agent serves the test its recorded window
			// (models.MergeStartupMockNames), and goes on doing so.
			name:      "failed, with no entry in the file",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)}, wrongBody: true,
			recorded: map[string][]models.MockEntry{"test-2": mongoEntries("mock-c")}, updateMapping: true,
			want: nil, wantStatus: models.TestStatusFailed,
		},
		{
			name:      "failed, --update-test-mapping, no file",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)}, wrongBody: true,
			updateMapping: true,
			want:          nil, wantStatus: models.TestStatusFailed,
		},
		{
			// The default flag never rewrites a file's entries: the control for
			// the write path, not for the rule.
			name:      "failed, default flag, file exists",
			consumed1: []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)}, wrongBody: true,
			recorded: recorded,
			want:     []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusFailed,
		},
		{
			// The file is created, with the passing test-2 only: test-1 stays on
			// its recorded window. Holding the create would leave test-2
			// unmapped as well.
			name:      "failed, default flag, no file",
			consumed1: []models.MockState{mongoAt("mock-b", 1020)}, wrongBody: true,
			want: nil, wantStatus: models.TestStatusFailed,
		},
		{
			// The remap the hints suggest for a stale mapping: each test is
			// served its recorded window, and one that passes is mapped from
			// what it consumed.
			name:      "--disable-mapping --update-test-mapping: passed",
			consumed1: []models.MockState{mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)},
			recorded:  recorded, updateMapping: true, disableMapping: true,
			want: []string{"mock-b", "mock-z"}, wantStatus: models.TestStatusPassed,
		},
		{
			name:      "--disable-mapping --update-test-mapping: failed",
			consumed1: []models.MockState{mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)}, wrongBody: true,
			recorded: recorded, updateMapping: true, disableMapping: true,
			want: []string{"mock-a", "mock-b"}, wantStatus: models.TestStatusFailed,
		},
		{
			name:       "passed, default flag, no file: the control",
			consumed1:  []models.MockState{mongoAt("mock-a", 1010)},
			want:       []string{"mock-a"},
			wantStatus: models.TestStatusPassed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.proven)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.replayer.config.Test.StrictFailure = tc.strictFailure
			h.replayer.config.DisableMapping = tc.disableMapping
			db := mappingFile(t, h, tc.recorded)
			h.replayer.hookImpl = prHooks{
				wrongBody: map[string]bool{"test-1": tc.wrongBody},
				consumedFor: consumedByWindow(h, map[string][]models.MockState{
					"test-1": tc.consumed1,
					"test-2": {mongoAt("mock-c", 2010)},
				}),
			}
			if tc.miss1 != nil {
				h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
					h.instr.mu.Lock()
					after := h.instr.windowAfter
					h.instr.mu.Unlock()
					if after.Equal(h.cases[0].HTTPReq.Timestamp) {
						return []models.UnmatchedCall{*tc.miss1}, nil
					}
					return nil, nil
				}
			}
			_ = h.run(t)

			if got := statusOf(t, h, "test-1"); got != tc.wantStatus {
				t.Fatalf("precondition: test-1 should be %s, got %s", tc.wantStatus, got)
			}
			if got := statusOf(t, h, "test-2"); got != models.TestStatusPassed {
				t.Fatalf("precondition: test-2 should pass, got %s", got)
			}
			onDisk := mappedOnDisk(t, db)
			if tc.proven {
				// Under the switch only record writes mappings.yaml.
				var want map[string][]string
				if tc.recorded != nil {
					want = map[string][]string{}
					for id, entries := range tc.recorded {
						for _, e := range entries {
							want[id] = append(want[id], e.Name)
						}
					}
				}
				if !reflect.DeepEqual(onDisk, want) {
					t.Fatalf("under KEPLOY_PROVEN the replay changed mappings.yaml: %v, want it as recorded, %v", onDisk, want)
				}
				return
			}
			if onDisk == nil {
				t.Fatal("no mappings.yaml after the run; want one that maps at least the passing test-2")
			}
			if !reflect.DeepEqual(onDisk["test-1"], tc.want) {
				t.Fatalf("test-1 is %s and mappings.yaml maps it to %v; want %v", tc.wantStatus, onDisk["test-1"], tc.want)
			}
			if want := []string{"mock-c"}; !reflect.DeepEqual(onDisk["test-2"], want) {
				t.Fatalf("test-2 passed and mappings.yaml maps it to %v; want %v", onDisk["test-2"], want)
			}
		})
	}
}

// The verdict that decides is the final one, as the BeforeTestResult hook
// leaves it: the report's. One exception: a wrong answer that left a recorded
// mock unconsumed (the OBSOLETE shape) is not a pass whatever the hook sets,
// as --strict-failure only relabels it FAILED, which the auto-noise hook may
// pass when the differences are values only.
func TestRunTestSetMapsByTheVerdictTheHookLeft(t *testing.T) {
	// covered consumes all of test-1's recorded mapping and one more mock; the
	// default consumes mock-b alone, leaving mock-a unconsumed.
	covered := []models.MockState{mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)}
	for _, tc := range []struct {
		name      string
		wrongBody bool
		strict    bool
		consumed1 []models.MockState
		loopSaid  models.TestStatus
		hookSets  models.TestStatus
		want      []string
	}{
		// The auto-noise hook flips FAILED rows only: --strict-failure makes
		// these wrong answers FAILED rather than OBSOLETE.
		{name: "a failure the hook passed is a pass", wrongBody: true, strict: true, consumed1: covered, loopSaid: models.TestStatusFailed, hookSets: models.TestStatusPassed, want: []string{"mock-a", "mock-b", "mock-z"}},
		{name: "a failure the hook passed that left a recorded mock unconsumed is not a pass", wrongBody: true, strict: true, loopSaid: models.TestStatusFailed, hookSets: models.TestStatusPassed, want: []string{"mock-a", "mock-b"}},
		{name: "an obsolete test the hook passed is not a pass", wrongBody: true, loopSaid: models.TestStatusObsolete, hookSets: models.TestStatusPassed, want: []string{"mock-a", "mock-b"}},
		{name: "a pass the hook failed is a failure", loopSaid: models.TestStatusPassed, hookSets: models.TestStatusFailed, want: []string{"mock-a", "mock-b"}},
		{name: "a test the hook quarantined is not a pass", loopSaid: models.TestStatusPassed, hookSets: models.TestStatusIgnored, want: []string{"mock-a", "mock-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.config.Test.UpdateTestMapping = true
			db := mappingFile(t, h, map[string][]models.MockEntry{
				"test-1": mongoEntries("mock-a", "mock-b"),
				"test-2": mongoEntries("mock-c"),
			})
			h.replayer.config.Test.StrictFailure = tc.strict
			consumed1 := tc.consumed1
			if consumed1 == nil {
				consumed1 = []models.MockState{mongoAt("mock-b", 1020)}
			}
			// saw is test-1's verdict as the hook was handed it.
			var saw models.TestStatus
			setVerdict := setStatus("test-1", tc.hookSets)
			h.replayer.hookImpl = verdictHooks{
				prHooks: prHooks{
					wrongBody: map[string]bool{"test-1": tc.wrongBody},
					consumedFor: consumedByWindow(h, map[string][]models.MockState{
						"test-1": consumed1,
						"test-2": {mongoAt("mock-c", 2010)},
					}),
				},
				set: func(results []models.TestResult) {
					for _, r := range results {
						if r.TestCaseID == "test-1" {
							saw = r.Status
						}
					}
					setVerdict(results)
				},
			}
			_ = h.run(t)

			if saw != tc.loopSaid {
				t.Fatalf("precondition: the hook was handed test-1 %s, want %s", saw, tc.loopSaid)
			}

			if got := mappedOnDisk(t, db)["test-1"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the hook left test-1 %s and mappings.yaml maps it to %v; want %v", tc.hookSets, got, tc.want)
			}
		})
	}
}

// A failed test's entry is the recorded one field for field (name, kind and
// timestamps), not a list of the same names rebuilt from what it consumed.
func TestRunTestSetLeavesAFailedTestsEntryAsRecorded(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.UpdateTestMapping = true
	h.replayer.config.Test.StrictFailure = true
	recordedEntry := []models.MockEntry{
		{Name: "mock-a", Kind: string(models.Mongo), Timestamp: 11, ReqTimestampMock: "2026-09-30T09:00:01.01Z", ResTimestampMock: "2026-09-30T09:00:01.011Z"},
		{Name: "mock-b", Kind: string(models.Mongo), Timestamp: 12},
	}
	db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": recordedEntry, "test-2": mongoEntries("mock-c")})
	h.replayer.hookImpl = prHooks{
		wrongBody: map[string]bool{"test-1": true},
		consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"test-1": {mongoAt("mock-b", 1020)},
			"test-2": {mongoAt("mock-c", 2010)},
		}),
	}
	_ = h.run(t)

	byTest, _, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(byTest["test-1"], recordedEntry) {
		t.Fatalf("test-1 failed and its entry is now %+v; want it as recorded, %+v", byTest["test-1"], recordedEntry)
	}
	raw, err := os.ReadFile(filepath.Join(h.replayer.config.Path, "test-set-0", "mappings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mock-a") {
		t.Fatalf("mappings.yaml lost test-1's mock-a:\n%s", raw)
	}
}

// A streamed test is mapped in the deferred pass, by the same write: one that
// fails keeps its recorded mapping too, and so does one a hook passes after it
// answered wrong with a recorded mock left unconsumed (the OBSOLETE shape).
func TestRunTestSetKeepsAFailedStreamedTestsRecordedMapping(t *testing.T) {
	for _, hookPasses := range []bool{false, true} {
		name := "failed"
		if hookPasses {
			name = "the hook passed it"
		}
		t.Run(name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.streaming("test-2")
			h.replayer.config.Test.UpdateTestMapping = true
			h.replayer.config.Test.StrictFailure = true
			db := mappingFile(t, h, map[string][]models.MockEntry{
				"test-1": mongoEntries("mock-a"),
				"test-2": mongoEntries("mock-b", "mock-c"),
			})
			hooks := prHooks{
				wrongBody: map[string]bool{"test-2": true},
				consumedFor: consumedByWindow(h, map[string][]models.MockState{
					"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-x", 1020)},
					"test-2": {mongoAt("mock-c", 2020)},
				}),
			}
			h.replayer.hookImpl = hooks
			// handed is the streamed test-2's verdict as the hook was handed it.
			var handed models.TestStatus
			if hookPasses {
				pass := setStatus("test-2", models.TestStatusPassed)
				h.replayer.hookImpl = verdictHooks{prHooks: hooks, set: func(results []models.TestResult) {
					for _, r := range results {
						if r.TestCaseID == "test-2" {
							handed = r.Status
						}
					}
					pass(results)
				}}
			}
			_ = h.run(t)

			if got := statusOf(t, h, "test-2"); got != models.TestStatusFailed {
				t.Fatalf("precondition: the streamed test-2 should fail, got %s", got)
			}
			if hookPasses && handed != models.TestStatusFailed {
				t.Fatalf("precondition: the hook should be handed the streamed test-2 FAILED, got %q", handed)
			}
			onDisk := mappedOnDisk(t, db)
			if want := []string{"mock-b", "mock-c"}; !reflect.DeepEqual(onDisk["test-2"], want) {
				t.Fatalf("the streamed test-2 answered wrong and mappings.yaml maps it to %v; want its recorded %v", onDisk["test-2"], want)
			}
			if want := []string{"mock-a", "mock-x"}; !reflect.DeepEqual(onDisk["test-1"], want) {
				t.Fatalf("test-1 passed and mappings.yaml maps it to %v; want what it consumed, %v", onDisk["test-1"], want)
			}
		})
	}
}

// A streamed test that passed with a call no mock answered and a recorded mock
// of its mapping left unconsumed (a wrong serve the app swallowed) keeps its
// recorded mapping, as in the main loop. Without the miss it is mapped from
// what it consumed: the control.
func TestRunTestSetKeepsTheMappingOfAStreamedPassThatSwallowedAMiss(t *testing.T) {
	for _, missed := range []bool{true, false} {
		name := "a call no mock answered"
		if !missed {
			name = "every call answered: the control"
		}
		t.Run(name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.streaming("test-2")
			h.replayer.config.Test.UpdateTestMapping = true
			db := mappingFile(t, h, map[string][]models.MockEntry{
				"test-1": mongoEntries("mock-a"),
				"test-2": mongoEntries("mock-b", "mock-c"),
			})
			// The agent's read drains what it hands out: the stream reads the
			// window twice, and the second read gets nothing new.
			byWindow := consumedByWindow(h, map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)},
				"test-2": {mongoAt("mock-c", 2020), mongoAt("mock-x", 2030)},
			})
			var drainMu sync.Mutex
			drained := map[string]bool{}
			h.replayer.hookImpl = prHooks{
				consumedFor: func() []models.MockState {
					out := byWindow()
					drainMu.Lock()
					defer drainMu.Unlock()
					if len(out) == 0 || drained[out[0].Name] {
						return nil
					}
					drained[out[0].Name] = true
					return out
				},
			}
			if missed {
				h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
					h.instr.mu.Lock()
					after := h.instr.windowAfter
					h.instr.mu.Unlock()
					if after.Equal(h.cases[1].HTTPReq.Timestamp) {
						return []models.UnmatchedCall{{Protocol: "MongoDB", ActualSummary: "find _SCHEMA"}}, nil
					}
					return nil, nil
				}
			}
			_ = h.run(t)

			if got := statusOf(t, h, "test-2"); got != models.TestStatusPassed {
				t.Fatalf("precondition: the streamed test-2 should pass, got %s", got)
			}
			if missed {
				var reported bool
				for _, r := range h.report.results {
					reported = reported || (r.TestCaseID == "test-2" && dataPlaneMisses(r.FailureInfo.UnmatchedCalls) > 0)
				}
				if !reported {
					t.Fatal("precondition: test-2's row should report the call no mock answered")
				}
			}
			want := []string{"mock-c", "mock-x"}
			if missed {
				want = []string{"mock-b", "mock-c"}
			}
			if got := mappedOnDisk(t, db)["test-2"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("the streamed test-2 (missed=%v) is mapped to %v; want %v", missed, got, want)
			}
		})
	}
}

// Under KEPLOY_PROVEN a test whose unanswered calls could not all be read has
// no verdict (its set is INTERNAL_ERR, O-1): whatever its row says, its
// consumption is not a pass's. Under the switch the replay writes no mapping
// at all, --update-test-mapping or not: the passing test-2 keeps its
// recorded entry too.
func TestRunTestSetUnderTheProvenSwitchAnUnreadMissKeepsTheRecordedMapping(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.UpdateTestMapping = true
	db := mappingFile(t, h, map[string][]models.MockEntry{
		"test-1": mongoEntries("mock-a", "mock-b"),
		"test-2": mongoEntries("mock-c"),
	})
	// test-1 consumes every recorded mock (so the switch's unconsumed-mock
	// rule passes it) and one more, which a write from its consumption would
	// add to its mapping.
	h.replayer.hookImpl = prHooks{
		consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-e", 1030)},
			"test-2": {mongoAt("mock-c", 2010), mongoAt("mock-d", 2020)},
		}),
	}
	h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
		h.instr.mu.Lock()
		after := h.instr.windowAfter
		h.instr.mu.Unlock()
		if after.Equal(h.cases[0].HTTPReq.Timestamp) {
			return nil, errors.New("the agent's miss channel overflowed")
		}
		return nil, nil
	}
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("precondition: under the switch an unread miss leaves the set unjudged (INTERNAL_ERR), got %q", status)
	}
	// What the exception is for: a row that says PASSED and has no verdict.
	for _, r := range h.report.results {
		if r.TestCaseID == "test-1" && (r.Status != models.TestStatusPassed || !r.FailureInfo.UnmatchedCallsUnread) {
			t.Fatalf("precondition: test-1's row should say PASSED with its misses unread, got %s unread=%v", r.Status, r.FailureInfo.UnmatchedCallsUnread)
		}
	}

	onDisk := mappedOnDisk(t, db)
	if want := []string{"mock-a", "mock-b"}; !reflect.DeepEqual(onDisk["test-1"], want) {
		t.Fatalf("test-1's misses could not be read and mappings.yaml maps it to %v; want its recorded %v", onDisk["test-1"], want)
	}
	if want := []string{"mock-c"}; !reflect.DeepEqual(onDisk["test-2"], want) {
		t.Fatalf("test-2 passed and mappings.yaml maps it to %v; under KEPLOY_PROVEN want its recorded %v", onDisk["test-2"], want)
	}
}

// consumedFailsFor is prHooks whose GetConsumedMocks fails while the agent's
// window is the named test's.
type consumedFailsFor struct {
	prHooks
	h    *prRun
	test string
}

func (f consumedFailsFor) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	f.h.instr.mu.Lock()
	after := f.h.instr.windowAfter
	f.h.instr.mu.Unlock()
	for _, tc := range f.h.cases {
		if tc.Name == f.test && after.Equal(tc.HTTPReq.Timestamp) {
			return nil, errors.New("the agent did not answer the consumed-mocks read")
		}
	}
	return f.prHooks.GetConsumedMocks(ctx)
}

// A test whose consumption could not be read has no consumption to write, with
// the switch on or off, whatever its verdict: the entry the run holds for it is
// only what the window attribution gave it from another test's read (a
// carry-over send of its window that the other test consumed), and writing
// that would cut its recorded mapping down to it. A streamed test is read in
// the deferred pass, and is held the same way. The other test passes and is
// the control.
func TestRunTestSetKeepsTheMappingOfATestWhoseConsumptionWasNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unread is the test whose consumption read fails; other is the one
		// that consumes the carry-over send of unread's window.
		unread, other string
		streamed      bool
		consumed      map[string][]models.MockState
	}{
		{
			name: "test-1, its send consumed by test-2", unread: "test-1", other: "test-2",
			consumed: map[string][]models.MockState{
				"test-2": {mongoAt("mock-c", 2010), consumedAt("send-1", 1050, models.Deleted, true)},
			},
		},
		{
			name: "the streamed test-2, its send consumed by test-1", unread: "test-2", other: "test-1", streamed: true,
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-c", 1010), consumedAt("send-2", 2050, models.Deleted, true)},
			},
		},
	} {
		for _, on := range []bool{false, true} {
			t.Run(tc.name+"/"+switchName(on), func(t *testing.T) {
				setSwitch(t, on)
				h := newPartialRunHarness(t, 2, 0)
				pinWindows(h)
				if tc.streamed {
					h.streaming(tc.unread)
				}
				h.replayer.config.Test.UpdateTestMapping = true
				db := mappingFile(t, h, map[string][]models.MockEntry{
					tc.unread: mongoEntries("mock-a", "mock-b"),
					tc.other:  mongoEntries("mock-c"),
				})
				h.replayer.hookImpl = consumedFailsFor{
					prHooks: prHooks{consumedFor: consumedByWindow(h, tc.consumed)},
					h:       h,
					test:    tc.unread,
				}
				_ = h.run(t)

				if !on {
					if got := statusOf(t, h, tc.unread); got != models.TestStatusPassed {
						t.Fatalf("precondition: with the switch off %s should pass, got %s", tc.unread, got)
					}
				}
				onDisk := mappedOnDisk(t, db)
				if want := []string{"mock-a", "mock-b"}; !reflect.DeepEqual(onDisk[tc.unread], want) {
					t.Fatalf("what %s consumed could not be read and mappings.yaml maps it to %v; want its recorded %v", tc.unread, onDisk[tc.unread], want)
				}
				if want := []string{"mock-c"}; !reflect.DeepEqual(onDisk[tc.other], want) {
					t.Fatalf("%s passed and mappings.yaml maps it to %v; want what it consumed in its own window, %v", tc.other, onDisk[tc.other], want)
				}
			})
		}
	}
}

func TestPassingTests(t *testing.T) {
	row := func(id string, s models.TestStatus) models.TestResult {
		return models.TestResult{TestCaseID: id, Status: s}
	}
	missed := row("passed-with-a-miss", models.TestStatusPassed)
	missed.FailureInfo.UnmatchedCalls = []models.UnmatchedCall{{Protocol: "MongoDB"}}
	unread := row("unread", models.TestStatusPassed)
	unread.FailureInfo.UnmatchedCallsUnread = true

	got := passingTests([]models.TestResult{
		row("passed", models.TestStatusPassed),
		missed,
		unread,
		row("passed-consumption-unread", models.TestStatusPassed),
		row("failed", models.TestStatusFailed),
		row("obsolete", models.TestStatusObsolete),
		row("pending", models.TestStatusPending),
		row("running", models.TestStatusRunning),
		row("no-status", ""),
		row("ignored", models.TestStatusIgnored),
		// Two rows: every one must pass, in either order.
		row("passed-then-failed", models.TestStatusPassed),
		row("passed-then-failed", models.TestStatusFailed),
		row("failed-then-passed", models.TestStatusFailed),
		row("failed-then-passed", models.TestStatusPassed),
		row("passed-twice", models.TestStatusPassed),
		row("passed-twice", models.TestStatusPassed),
		row("ignored-and-passed", models.TestStatusIgnored),
		row("ignored-and-passed", models.TestStatusPassed),
		row("passed-obsolete-shaped", models.TestStatusPassed),
	}, []string{"passed-consumption-unread", "failed", "not-in-results"}, []string{"passed-obsolete-shaped"})

	want := map[string]bool{"passed": true, "passed-with-a-miss": true, "passed-twice": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("passing = %v, want %v", got, want)
	}
	if got := passingTests(nil, nil); len(got) != 0 {
		t.Fatalf("no results: %v, want nothing passing", got)
	}
}

func TestPassedMissShaped(t *testing.T) {
	row := func(id string, s models.TestStatus, protocols ...string) models.TestResult {
		r := models.TestResult{TestCaseID: id, Status: s}
		for _, p := range protocols {
			r.FailureInfo.UnmatchedCalls = append(r.FailureInfo.UnmatchedCalls, models.UnmatchedCall{Protocol: p})
		}
		return r
	}
	got := passedMissShaped([]models.TestResult{
		row("left-missed", models.TestStatusPassed, "MongoDB"),
		row("left-missed", models.TestStatusPassed, "MongoDB"),
		row("left-dns-only", models.TestStatusPassed, "DNS", "dns"),
		row("left-no-miss", models.TestStatusPassed),
		row("missed-not-left", models.TestStatusPassed, "MySQL"),
		row("left-missed-in-a-later-row", models.TestStatusPassed),
		row("left-missed-in-a-later-row", models.TestStatusPassed, "Postgres"),
	}, []string{"left-missed", "left-dns-only", "left-no-miss", "left-missed-in-a-later-row", "left-no-row"})
	if want := []string{"left-missed", "left-missed-in-a-later-row"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("passedMissShaped = %v, want %v", got, want)
	}
	if got := passedMissShaped([]models.TestResult{row("a", models.TestStatusPassed, "MongoDB")}, nil); got != nil {
		t.Fatalf("nothing left unconsumed: %v, want none", got)
	}
}

func TestPassingEntriesOf(t *testing.T) {
	doc := &models.Mapping{
		Version: "v", Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{
			{ID: "a", Mocks: mongoEntries("m1")},
			{ID: "b", Mocks: mongoEntries("m2")},
			{ID: "c", Mocks: mongoEntries("m3")},
		},
		Startup: mongoEntries("boot"),
	}
	out, kept := passingEntriesOf(doc, map[string]bool{"a": true, "c": true, "not-in-doc": true})
	if len(out.TestCases) != 2 || out.TestCases[0].ID != "a" || out.TestCases[1].ID != "c" {
		t.Fatalf("written entries = %+v, want a and c, in order", out.TestCases)
	}
	if !reflect.DeepEqual(kept, []string{"b"}) {
		t.Fatalf("kept = %v, want [b]", kept)
	}
	if out.TestSetID != "test-set-0" || out.Kind != models.MappingKind || !reflect.DeepEqual(out.Startup, doc.Startup) {
		t.Fatalf("the document's header or startup section changed: %+v", out)
	}
	// The write changes its entries (keepConnFailureMappings appends to them):
	// the run's own record of what was consumed must not change with them.
	out.TestCases[0].Mocks[0].Name = "renamed"
	out.TestCases[0].Mocks = append(out.TestCases[0].Mocks, models.MockEntry{Name: "cf"})
	if len(doc.TestCases) != 3 || len(doc.TestCases[0].Mocks) != 1 || doc.TestCases[0].Mocks[0].Name != "m1" {
		t.Fatalf("passingEntriesOf changed its input: %+v", doc.TestCases)
	}
	if out, kept := passingEntriesOf(nil, nil); out != nil || kept != nil {
		t.Fatalf("nil document: %v, %v", out, kept)
	}
}

// A run where no test passed has no test to map, and vouches for nothing it
// consumed at boot either. With no file yet it must not create one, under
// either flag: a file that maps no test, existing, stops every later default
// run from creating the one that does. It says why, naming the tests. Into a
// file that exists it leaves the startup section as it is. The control, where
// test-2 passes, creates the file with test-2 and the section; a run where no
// test consumed a mock creates nothing either, quietly.
func TestRunTestSetCreatesNoFileThatMapsNoTest(t *testing.T) {
	boot := models.MockState{Name: "mock-boot", Kind: models.Mongo, Usage: models.Updated, Lifetime: models.LifetimeSession}
	for _, tc := range []struct {
		name          string
		wrongBody     map[string]bool
		consumeNone   bool
		updateMapping bool
		fileExists    bool
		// wantMapped is the file's tests after the run; nil: no file.
		wantMapped  map[string][]string
		wantStartup string
		wantWarn    bool
		// wantReason is in the warning's reason; empty: the tests are named.
		wantReason string
		// simErr: every request gets no answer.
		simErr bool
		// wantUntouched: the existing file is not rewritten at all.
		wantUntouched bool
	}{
		{name: "every test failed, default flag", wrongBody: map[string]bool{"test-1": true, "test-2": true}, wantWarn: true},
		{name: "every test failed, --update-test-mapping", wrongBody: map[string]bool{"test-1": true, "test-2": true}, updateMapping: true, wantWarn: true},
		{
			name: "every test failed, --update-test-mapping, file exists", wrongBody: map[string]bool{"test-1": true, "test-2": true},
			updateMapping: true, fileExists: true,
			wantMapped:  map[string][]string{"test-1": {"mock-a", "mock-b"}, "test-2": {"mock-old-c"}},
			wantStartup: "mock-old-boot", wantUntouched: true,
		},
		{
			name: "test-2 passed: the control", wrongBody: map[string]bool{"test-1": true},
			wantMapped: map[string][]string{"test-2": {"mock-c"}}, wantStartup: "mock-boot",
		},
		{name: "every test passed and none consumed a mock", consumeNone: true},
		{
			// The no-answer hold, and its reason, still come first: a lane's
			// hint sends its reader to that line.
			name: "every request got no answer, default flag", simErr: true, consumeNone: true,
			wantWarn: true, wantReason: "a test got no answer",
		},
		{
			name: "every test failed and none consumed a mock, --update-test-mapping", consumeNone: true,
			wrongBody: map[string]bool{"test-1": true, "test-2": true}, updateMapping: true,
			wantWarn: true, wantReason: "no test passed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			db := mappingFile(t, h, nil)
			if tc.fileExists {
				if err := db.Insert(context.Background(), &models.Mapping{
					Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
					TestCases: []models.MappedTestCase{
						{ID: "test-1", Mocks: mongoEntries("mock-a", "mock-b")},
						{ID: "test-2", Mocks: mongoEntries("mock-old-c")},
					},
					Startup: mongoEntries("mock-old-boot"),
				}); err != nil {
					t.Fatal(err)
				}
			}
			consumed := map[string][]models.MockState{
				"":       {boot}, // the set's setup drain, before any test's window
				"test-1": {mongoAt("mock-a", 1010)},
				"test-2": {mongoAt("mock-c", 2010)},
			}
			if tc.consumeNone {
				consumed = map[string][]models.MockState{"": {boot}}
			}
			mappingsPath := filepath.Join(h.replayer.config.Path, "test-set-0", "mappings.yaml")
			var before []byte
			if tc.fileExists {
				var err error
				if before, err = os.ReadFile(mappingsPath); err != nil {
					t.Fatal(err)
				}
				// A rewrite that changes nothing would still be a rewrite:
				// mark the file so one shows.
				before = append([]byte("# written before the run\n"), before...)
				if err := os.WriteFile(mappingsPath, before, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			hooks := prHooks{wrongBody: tc.wrongBody, consumedFor: consumedByWindow(h, consumed)}
			if tc.simErr {
				timeout := prClientTimeoutErr(t)
				hooks.simErr = map[string]error{"test-1": timeout, "test-2": timeout}
			}
			h.replayer.hookImpl = hooks
			_ = h.run(t)

			onDisk := mappedOnDisk(t, db)
			if !reflect.DeepEqual(onDisk, tc.wantMapped) {
				t.Fatalf("mappings.yaml maps %v; want %v (nil: no file)", onDisk, tc.wantMapped)
			}
			if tc.wantUntouched {
				after, err := os.ReadFile(mappingsPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatalf("a run with nothing to write rewrote mappings.yaml:\n%s\nwas:\n%s", after, before)
				}
			}
			if tc.wantStartup != "" {
				startup, err := db.GetStartup(context.Background(), "test-set-0")
				if err != nil || len(startup) != 1 || startup[0].Name != tc.wantStartup {
					t.Fatalf("startup section = %+v (err %v), want %s", startup, err, tc.wantStartup)
				}
			}
			held := logs.FilterMessage("not writing mappings.yaml").All()
			if !tc.wantWarn {
				if len(held) != 0 {
					t.Fatalf("want no \"not writing mappings.yaml\" warning, got %+v", held)
				}
				return
			}
			if len(held) != 1 {
				t.Fatalf("want one \"not writing mappings.yaml\" warning, got %+v", held)
			}
			if tc.wantReason != "" {
				if reason := fmt.Sprint(held[0].ContextMap()["reason"]); !strings.Contains(reason, tc.wantReason) {
					t.Fatalf("the warning's reason is %q; want it to say %q", reason, tc.wantReason)
				}
			} else if !strings.Contains(fmt.Sprint(held[0].ContextMap()["testsNotCountedAsPassing"]), "test-1") {
				t.Fatalf("want the warning to name the tests that did not pass, got %+v", held[0].ContextMap())
			}
		})
	}
}

// retryHooks answers a test's first n simulated requests as recorded and every
// later one wrong, so it passes the first RetryPassing cycles and fails the
// next.
type retryHooks struct {
	prHooks
	mu       *sync.Mutex
	calls    map[string]int
	passFor  map[string]int
	wrongAll map[string]bool
}

func (h retryHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	h.mu.Lock()
	h.calls[tc.Name]++
	wrong := h.wrongAll[tc.Name]
	if n, ok := h.passFor[tc.Name]; ok && h.calls[tc.Name] > n {
		wrong = true
	}
	h.mu.Unlock()
	resp, err := h.prHooks.SimulateRequest(ctx, tc, testSetID)
	if r, ok := resp.(*models.HTTPResp); ok && wrong {
		r.Body = `{"ok":false}`
	}
	return resp, err
}

// With RetryPassing a test passes one cycle and fails a later one: its last
// verdict is the failure, and the consumption the cycles appended for it is
// not its mapping. test-2 fails the first cycle (which is what makes the
// passing tests run again); test-3 passes every cycle and is the control.
func TestRunTestSetRetryPassingKeepsTheMappingOfATestThatFailedALaterCycle(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	h.replayer.config.RetryPassing = true
	h.replayer.config.Test.UpdateTestMapping = true
	db := mappingFile(t, h, map[string][]models.MockEntry{
		"test-1": mongoEntries("mock-a", "mock-b"),
		"test-2": mongoEntries("mock-c"),
		"test-3": mongoEntries("mock-d"),
	})
	h.replayer.hookImpl = retryHooks{
		prHooks: prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-b", 1020), mongoAt("mock-z", 1030)},
			"test-2": {mongoAt("mock-c", 2010), mongoAt("mock-y", 2020)},
			"test-3": {mongoAt("mock-d", 3010), mongoAt("mock-x", 3020)},
		})},
		mu:       &sync.Mutex{},
		calls:    map[string]int{},
		passFor:  map[string]int{"test-1": 1},
		wrongAll: map[string]bool{"test-2": true},
	}
	_ = h.run(t)

	for test, want := range map[string]models.TestStatus{"test-1": models.TestStatusFailed, "test-2": models.TestStatusFailed, "test-3": models.TestStatusPassed} {
		for _, r := range h.report.results {
			if r.TestCaseID == test && r.Status != want {
				t.Fatalf("precondition: %s has a %s row, want every row %s", test, r.Status, want)
			}
		}
	}
	if n := h.replayer.hookImpl.(retryHooks).calls["test-1"]; n < 2 {
		t.Fatalf("precondition: test-1 ran %d time(s); want a second RetryPassing cycle", n)
	}
	onDisk := mappedOnDisk(t, db)
	if want := []string{"mock-a", "mock-b"}; !reflect.DeepEqual(onDisk["test-1"], want) {
		t.Fatalf("test-1 failed its second cycle and mappings.yaml maps it to %v; want its recorded %v", onDisk["test-1"], want)
	}
	if want := []string{"mock-c"}; !reflect.DeepEqual(onDisk["test-2"], want) {
		t.Fatalf("test-2 failed and mappings.yaml maps it to %v; want its recorded %v", onDisk["test-2"], want)
	}
	if got := onDisk["test-3"]; len(got) < 2 || got[0] != "mock-d" || got[1] != "mock-x" {
		t.Fatalf("test-3 passed every cycle and mappings.yaml maps it to %v; want what it consumed (mock-d, mock-x)", got)
	}
}

// keploy test names itself the source of the test entries it writes, and
// leaves the file's source alone when it writes the startup section only.
func TestMarkWrittenByKeployTestOnlyWithTestEntries(t *testing.T) {
	entries := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-1"}}}
	markWrittenByKeployTest(entries)
	if entries.Source != models.MappingSourceKeployTest {
		t.Fatalf("test entries: source %q", entries.Source)
	}
	startupOnly := &models.Mapping{Startup: []models.MockEntry{{Name: "boot"}}}
	markWrittenByKeployTest(startupOnly)
	if startupOnly.Source != "" {
		t.Fatalf("startup only: source %q, want the file's kept", startupOnly.Source)
	}
	markWrittenByKeployTest(nil)
}

// entriesOnDisk is mappedOnDisk with an entry that names no mock kept, as an
// empty list; nil when there is no file.
func entriesOnDisk(t *testing.T, db *mapdb.MappingDb) map[string][]string {
	t.Helper()
	if exists, err := db.Exists(context.Background(), "test-set-0"); err != nil || !exists {
		if err != nil {
			t.Fatalf("checking for mappings.yaml: %v", err)
		}
		return nil
	}
	byTest, _, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("reading mappings.yaml back: %v", err)
	}
	out := make(map[string][]string, len(byTest))
	for id, entries := range byTest {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name)
		}
		out[id] = names
	}
	return out
}

// A passing test that consumed no mock at all gets an entry with none, in the
// file the run writes, so that a test with no entry is only one the run gave
// no verdict on (a replay that downloads only the mapped mocks skips those).
// Before, it got no entry, which kept its recorded one or, with none, left it
// unmapped for good. A failed test, a test whose consumed mocks all went to
// other windows, and every test of a run that read no consumption (not
// instrumented) get no new entry.
//
// --update-test-mapping (auto-replay) creates the file for those entries
// though a test not counted as passing consumed mocks and no passing test
// consumed one: the set then has a mapping, and the test not counted as
// passing has no entry (a replay that skips a test with no entry reports it
// "skipped: no mapping entry"). The default flag still creates no file that
// maps no mock: it would stop every later default run from creating one.
func TestRunTestSetGivesAPassingTestThatConsumedNothingAnEmptyEntry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tests           int // 2 when 0
		updateMapping   bool
		fileExists      bool
		notInstrumented bool
		wrongBody       map[string]bool
		test1None       bool // test-1 consumed nothing; else mock-a
		test2Consumed   []models.MockState
		// want is each test's entry after the run; nil: no file.
		want map[string][]string
	}{
		{
			name: "--update-test-mapping, no file, test-1 failed after consuming a mock", tests: 3, updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			want:      map[string][]string{"test-2": {}, "test-3": {}},
		},
		{
			name: "--update-test-mapping, no file, test-1 failed consuming nothing", updateMapping: true,
			wrongBody: map[string]bool{"test-1": true}, test1None: true,
			want: map[string][]string{"test-2": {}},
		},
		{
			name:      "default flag, no file, test-1 failed after consuming a mock: no file",
			wrongBody: map[string]bool{"test-1": true},
		},
		{
			name: "--update-test-mapping, file exists", updateMapping: true, fileExists: true,
			want: map[string][]string{"test-1": {"mock-a"}, "test-2": {}},
		},
		{
			name: "--update-test-mapping, no file", updateMapping: true,
			want: map[string][]string{"test-1": {"mock-a"}, "test-2": {}},
		},
		{
			name: "default flag, no file",
			want: map[string][]string{"test-1": {"mock-a"}, "test-2": {}},
		},
		{
			name: "test-2 failed: no entry", updateMapping: true, wrongBody: map[string]bool{"test-2": true},
			want: map[string][]string{"test-1": {"mock-a"}},
		},
		{
			// It consumed a mock, recorded in no window of its own: the window
			// filter drops it, and the test keeps its recorded entry.
			name: "test-2 consumed only another window's mock", updateMapping: true, fileExists: true,
			test2Consumed: []models.MockState{mongoAt("mock-x", 1500)},
			want:          map[string][]string{"test-1": {"mock-a"}, "test-2": {"mock-old-c"}},
		},
		{
			name: "not instrumented: no consumption read, the file stays", updateMapping: true, fileExists: true, notInstrumented: true,
			want: map[string][]string{"test-1": {"mock-a", "mock-b"}, "test-2": {"mock-old-c"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.tests
			if n == 0 {
				n = 2
			}
			h := newPartialRunHarness(t, n, 0)
			pinWindows(h)
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.replayer.instrument = !tc.notInstrumented
			db := mappingFile(t, h, nil)
			if tc.fileExists {
				if err := db.Insert(context.Background(), &models.Mapping{
					Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
					TestCases: []models.MappedTestCase{
						{ID: "test-1", Mocks: mongoEntries("mock-a", "mock-b")},
						{ID: "test-2", Mocks: mongoEntries("mock-old-c")},
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			test1 := []models.MockState{mongoAt("mock-a", 1010)}
			if tc.test1None {
				test1 = nil
			}
			consumed := map[string][]models.MockState{
				"test-1": test1,
				"test-2": tc.test2Consumed,
			}
			h.replayer.hookImpl = prHooks{wrongBody: tc.wrongBody, consumedFor: consumedByWindow(h, consumed)}
			_ = h.run(t)

			for test, failed := range tc.wrongBody {
				if want := models.TestStatusFailed; failed && statusOf(t, h, test) != want {
					t.Fatalf("precondition: %s is %s, want %s", test, statusOf(t, h, test), want)
				}
			}
			if got := entriesOnDisk(t, db); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mappings.yaml holds %v; want %v (nil: no file)", got, tc.want)
			}
		})
	}
}

// A new mappings.yaml gives every passing test an entry, or is not created:
// a passing test with no entry in it is "no verdict" to a replay that skips a
// test with no entry, which would skip a test that passed, run after run. A
// passing test gets no entry when keploy could not tell what it consumed: each
// mock it consumed fell in no test's window, or in another test's, or the run
// read no consumption. With no file the set keeps its whole recording. Under
// either flag; the default flag's own hold, where a new file would map no mock,
// comes first, with its reason as before. (A file that exists is written as
// before: TestRunTestSetGivesAPassingTestThatConsumedNothingAnEmptyEntry.)
//
// A passing test whose drain caught only startup traffic (mock-boot, asked
// for before the set's first test, or mock-fboot, a flagged mock released
// before it) consumed nothing but what the same file's startup section lists,
// and gets an empty entry: the section serves it. A test that consumed a mock
// of another test's window still holds the file.
//
// Under the default flag a run that mapped no mock creates no file (its
// passing tests' empty entries map none). With nothing mapped at all (no
// startup traffic either) it says nothing, as before the empty entries,
// whatever else the run held for: a test that got no answer, a subset run.
// With only startup traffic mapped, the hold for a new file that would map no
// mock is logged at Debug, as before. A hold that names the tests not counted
// as passing names them whenever no mock is mapped, an empty entry or not.
func TestRunTestSetCreatesNoFileThatLeavesAPassingTestWithNoEntry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tests           int // 2 when 0
		updateMapping   bool
		notInstrumented bool
		// existsFails: the store cannot tell whether the file exists.
		existsFails bool
		wrongBody   map[string]bool
		// noAnswer: these tests' requests get no answer.
		noAnswer []string
		// selected: only these tests run (a subset run); all when empty.
		selected []string
		consumed map[string][]models.MockState
		// wantFile is each test's entry after the run; nil: no file.
		wantFile map[string][]string
		// wantStartup is the startup section of the file written.
		wantStartup []string
		// wantNamed is the warning's passing tests with no entry; empty: the
		// warning does not name any.
		wantNamed  string
		wantReason string
		// wantNotCounted is the warning's tests not counted as passing; empty:
		// the warning does not name any.
		wantNotCounted string
		// wantQuiet is the reason of a hold logged at Debug, with no warning.
		wantQuiet string
	}{
		{
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 passed consuming only a mock of no window", updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-x", 1500)}},
			wantNamed: "[test-2]", wantReason: "could not tell what a passing test consumed", wantNotCounted: "[test-1]",
		},
		{
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 passed consuming only a mock of test-1's window", updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-y", 1050)}},
			wantNamed: "[test-2]", wantReason: "could not tell what a passing test consumed", wantNotCounted: "[test-1]",
		},
		{
			// test-2's empty entry maps no mock: test-1 is named as not
			// counted as passing, as with no entry at all.
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 consumed nothing, test-3 only a mock of no window", tests: 3, updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-3": {mongoAt("mock-x", 2500)}},
			wantNamed: "[test-3]", wantReason: "could not tell what a passing test consumed", wantNotCounted: "[test-1]",
		},
		{
			// test-2's entry maps mock-c, so the warning does not name test-1
			// as not counted as passing: it names those only while no mock is
			// mapped.
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 passed consuming mock-c, test-3 only a mock of no window", tests: 3, updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-c", 2010)}, "test-3": {mongoAt("mock-x", 2500)},
			},
			wantNamed: "[test-3]", wantReason: "could not tell what a passing test consumed",
		},
		{
			name: "--update-test-mapping, every test passed, each consuming only a mock of no window", updateMapping: true,
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-x", 1500)}, "test-2": {mongoAt("mock-z", 2500)}},
			wantNamed: "[test-1 test-2]", wantReason: "could not tell what a passing test consumed",
		},
		{
			name: "--update-test-mapping, not instrumented: no consumption read", updateMapping: true, notInstrumented: true,
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}},
			wantNamed: "[test-1 test-2]", wantReason: "could not tell what a passing test consumed",
		},
		{
			name: "default flag, both passed, test-2 consumed only a mock of no window", consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-x", 1500)},
			},
			wantNamed: "[test-2]", wantReason: "could not tell what a passing test consumed",
		},
		{
			// The default flag's hold for a new file that would map no mock
			// comes first, with its reason, as before.
			name:       "default flag, test-1 failed after consuming a mock, test-2 passed consuming only a mock of no window",
			wrongBody:  map[string]bool{"test-1": true},
			consumed:   map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-x", 1500)}},
			wantReason: "no test that consumed a mock counts as passing", wantNotCounted: "[test-1]",
		},
		{
			// test-2's empty entry maps no mock: the same hold, and it still
			// names test-1, whose consumption the file would leave out.
			name:       "default flag, test-1 failed after consuming a mock, test-2 passed consuming nothing",
			wrongBody:  map[string]bool{"test-1": true},
			consumed:   map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}},
			wantReason: "no test that consumed a mock counts as passing", wantNotCounted: "[test-1]",
		},
		{
			name: "--update-test-mapping, both passed, test-1 consumed mock-a, test-2 consumed only a mock of test-1's window", updateMapping: true,
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-y", 1050)}},
			wantNamed: "[test-2]", wantReason: "could not tell what a passing test consumed",
		},
		{
			// Whether a file exists cannot be read: it is taken as absent, and
			// no file is written over one that may be there.
			name: "--update-test-mapping, the store cannot tell whether the file exists, test-1 failed after consuming a mock, test-2 passed consuming only a mock of no window", updateMapping: true,
			existsFails: true,
			wrongBody:   map[string]bool{"test-1": true},
			consumed:    map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-x", 1500)}},
			wantNamed:   "[test-2]", wantReason: "could not tell what a passing test consumed", wantNotCounted: "[test-1]",
		},
		// The default flag, nothing mapped: no file, and no hold logged.
		{
			name:     "default flag, both passed, test-1 consumed nothing, test-2 consumed only a mock of no window",
			consumed: map[string][]models.MockState{"test-2": {mongoAt("mock-x", 1500)}},
		},
		{
			name: "default flag, both passed, neither consumed a mock",
		},
		{
			name:      "default flag, test-1 failed consuming nothing, test-2 passed consuming nothing",
			wrongBody: map[string]bool{"test-1": true},
		},
		{
			name:     "default flag, test-1 got no answer, test-2 passed consuming nothing",
			noAnswer: []string{"test-1"},
		},
		{
			name:     "default flag, a subset run of test-1 alone, which passed consuming nothing",
			selected: []string{"test-1"},
		},
		{
			// Only startup traffic mapped: the hold, at Debug, as before.
			name:      "default flag, both passed, each test's drain caught only startup traffic",
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-boot", 500)}, "test-2": {mongoAt("mock-boot2", 600)}},
			wantQuiet: "no test consumed a mock",
		},
		{
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 passed, its drain caught only startup traffic", updateMapping: true,
			wrongBody:   map[string]bool{"test-1": true},
			consumed:    map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-boot", 500)}},
			wantFile:    map[string][]string{"test-2": {}},
			wantStartup: []string{"mock-boot"},
		},
		{
			name:        "default flag, both passed, test-1 consumed mock-a, test-2's drain caught only startup traffic",
			consumed:    map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-boot", 500)}},
			wantFile:    map[string][]string{"test-1": {"mock-a"}, "test-2": {}},
			wantStartup: []string{"mock-boot"},
		},
		{
			// A flagged mock released before the first test is the startup
			// section's, whichever drain caught it.
			name: "--update-test-mapping, test-1 failed after consuming a mock, test-2 passed, its drain caught only a flagged mock released before the first test", updateMapping: true,
			wrongBody:   map[string]bool{"test-1": true},
			consumed:    map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {carriedAt("mock-fboot", 500)}},
			wantFile:    map[string][]string{"test-2": {}},
			wantStartup: []string{"mock-fboot"},
		},
		{
			name:        "default flag, both passed, test-1 consumed mock-a, test-2's drain caught only a flagged mock released before the first test",
			consumed:    map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {carriedAt("mock-fboot", 500)}},
			wantFile:    map[string][]string{"test-1": {"mock-a"}, "test-2": {}},
			wantStartup: []string{"mock-fboot"},
		},
		{
			name: "control: --update-test-mapping, test-1 failed after consuming a mock, test-2 consumed nothing", updateMapping: true,
			wrongBody: map[string]bool{"test-1": true},
			consumed:  map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}},
			wantFile:  map[string][]string{"test-2": {}},
		},
		{
			name: "control: default flag, both passed in their own windows", consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-c", 2010)},
			},
			wantFile: map[string][]string{"test-1": {"mock-a"}, "test-2": {"mock-c"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.tests
			if n == 0 {
				n = 2
			}
			core, logs := observer.New(zap.DebugLevel)
			h := newPartialRunHarness(t, n, 0)
			pinWindows(h)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.replayer.instrument = !tc.notInstrumented
			if len(tc.selected) > 0 {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": tc.selected}
			}
			db := mappingFile(t, h, nil)
			if tc.existsFails {
				h.replayer.mappingDB = existsFailsDB{db}
			}
			hooks := prHooks{wrongBody: tc.wrongBody, consumedFor: consumedByWindow(h, tc.consumed)}
			if len(tc.noAnswer) > 0 {
				timeout := prClientTimeoutErr(t)
				hooks.simErr = map[string]error{}
				for _, id := range tc.noAnswer {
					hooks.simErr[id] = timeout
				}
			}
			h.replayer.hookImpl = hooks
			_ = h.run(t)

			for i := 1; i <= n; i++ {
				id := fmt.Sprintf("test-%d", i)
				if len(tc.selected) > 0 && !slices.Contains(tc.selected, id) {
					for _, r := range h.report.results {
						if r.TestCaseID == id {
							t.Fatalf("precondition: %s, not selected, has a result row (%s)", id, r.Status)
						}
					}
					continue
				}
				want := models.TestStatusPassed
				if tc.wrongBody[id] || slices.Contains(tc.noAnswer, id) {
					want = models.TestStatusFailed
				}
				if got := statusOf(t, h, id); got != want {
					t.Fatalf("precondition: %s is %s, want %s", id, got, want)
				}
			}
			if got := entriesOnDisk(t, db); !reflect.DeepEqual(got, tc.wantFile) {
				t.Fatalf("mappings.yaml: %s; want %s", fileState(got), fileState(tc.wantFile))
			}
			if tc.wantFile != nil {
				startup, err := db.GetStartup(context.Background(), "test-set-0")
				if err != nil {
					t.Fatalf("reading the startup section back: %v", err)
				}
				got := []string{}
				for _, e := range startup {
					got = append(got, e.Name)
				}
				want := tc.wantStartup
				if want == nil {
					want = []string{}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("the startup section lists %v; want %v", got, want)
				}
			}
			var held, quiet []observer.LoggedEntry
			for _, e := range logs.FilterMessage("not writing mappings.yaml").All() {
				if e.Level >= zap.WarnLevel {
					held = append(held, e)
				} else {
					quiet = append(quiet, e)
				}
			}
			if tc.wantQuiet == "" && len(quiet) != 0 {
				t.Fatalf("want no hold logged at Debug, got %+v", quiet)
			}
			if tc.wantQuiet != "" {
				if len(quiet) != 1 {
					t.Fatalf("want one hold logged at Debug, saying %q, got %+v", tc.wantQuiet, quiet)
				}
				if reason := fmt.Sprint(quiet[0].ContextMap()["reason"]); !strings.Contains(reason, tc.wantQuiet) {
					t.Fatalf("the quiet hold's reason is %q; want it to say %q", reason, tc.wantQuiet)
				}
			}
			if tc.wantReason == "" {
				if len(held) != 0 {
					t.Fatalf("want no \"not writing mappings.yaml\" warning, got %+v", held)
				}
				return
			}
			if len(held) != 1 {
				t.Fatalf("want one \"not writing mappings.yaml\" warning, got %+v", held)
			}
			ctx := held[0].ContextMap()
			if reason := fmt.Sprint(ctx["reason"]); !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("the warning's reason is %q; want it to say %q", reason, tc.wantReason)
			}
			notCounted, ok := ctx["testsNotCountedAsPassing"]
			switch {
			case tc.wantNotCounted == "" && ok:
				t.Fatalf("the warning names tests not counted as passing %v; want none named", notCounted)
			case tc.wantNotCounted != "" && fmt.Sprint(notCounted) != tc.wantNotCounted:
				t.Fatalf("the warning names tests not counted as passing %v; want %s", notCounted, tc.wantNotCounted)
			}
			named, ok := ctx["passingTestsWithNoEntry"]
			if tc.wantNamed == "" {
				if ok {
					t.Fatalf("the warning names passing tests with no entry %v; want none named", named)
				}
				return
			}
			if got := fmt.Sprint(named); got != tc.wantNamed {
				t.Fatalf("the warning names passing tests with no entry %s; want %s", got, tc.wantNamed)
			}
		})
	}
}

// existsFailsDB is a mapping store that cannot tell whether a file exists.
type existsFailsDB struct{ *mapdb.MappingDb }

func (existsFailsDB) Exists(context.Context, string) (bool, error) {
	return false, errors.New("stat mappings.yaml: input/output error")
}

// fileState is entriesOnDisk's result as a failure message says it.
func fileState(m map[string][]string) string {
	if m == nil {
		return "no file"
	}
	return fmt.Sprintf("a file holding %v", m)
}

func TestPassingWithNoEntry(t *testing.T) {
	results := []models.TestResult{
		{TestCaseID: "c", Status: models.TestStatusPassed},
		{TestCaseID: "a", Status: models.TestStatusPassed},
		{TestCaseID: "b", Status: models.TestStatusPassed},
		{TestCaseID: "a", Status: models.TestStatusPassed},
		{TestCaseID: "d", Status: models.TestStatusFailed},
		{TestCaseID: "e", Status: models.TestStatusPassed},
	}
	passing := map[string]bool{"a": true, "b": true, "c": true, "e": true}
	// b has an entry with a mock, e an empty one; d did not pass; a is named
	// once, though it has two rows.
	doc := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "b", Mocks: mongoEntries("m1")}, {ID: "e"}}}
	if got := passingWithNoEntry(doc, results, passing); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("passingWithNoEntry = %v, want [a c]", got)
	}
	if got := passingWithNoEntry(nil, results, passing); !reflect.DeepEqual(got, []string{"a", "b", "c", "e"}) {
		t.Fatalf("no document: %v, want [a b c e]", got)
	}
	if got := passingWithNoEntry(doc, results, nil); got != nil {
		t.Fatalf("no passing test: %v, want none", got)
	}
}

func TestAddConsumedNothingEntries(t *testing.T) {
	results := []models.TestResult{
		{TestCaseID: "c", Status: models.TestStatusPassed},
		{TestCaseID: "a", Status: models.TestStatusPassed},
		{TestCaseID: "b", Status: models.TestStatusPassed},
		{TestCaseID: "a", Status: models.TestStatusPassed},
		{TestCaseID: "d", Status: models.TestStatusFailed},
		{TestCaseID: "e", Status: models.TestStatusPassed},
	}
	passing := map[string]bool{"a": true, "b": true, "c": true, "e": true}
	none := map[string]struct{}{"a": {}, "b": {}, "c": {}, "d": {}}
	doc := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "b", Mocks: mongoEntries("m1")}}}
	addConsumedNothingEntries(doc, results, passing, none)
	var ids []string
	for _, tc := range doc.TestCases {
		ids = append(ids, tc.ID)
		if tc.ID != "b" && len(tc.Mocks) != 0 {
			t.Fatalf("entry %s names mocks %+v; want none", tc.ID, tc.Mocks)
		}
	}
	// b keeps its own entry; d did not pass; e's consumption was not read
	// empty; a is added once, though it has two rows.
	if !reflect.DeepEqual(ids, []string{"b", "c", "a"}) {
		t.Fatalf("entries = %v, want [b c a]", ids)
	}
	if !mapsAMock(doc) {
		t.Fatal("mapsAMock: the document maps m1 to b")
	}
	empty := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "x"}}}
	addConsumedNothingEntries(empty, results, passing, nil)
	if len(empty.TestCases) != 1 || mapsAMock(empty) || mapsAMock(nil) {
		t.Fatalf("no consumption read must add nothing, and an entry with no mock maps none: %+v", empty.TestCases)
	}
	addConsumedNothingEntries(nil, results, passing, none) // must not panic
}
