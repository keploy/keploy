package replay

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// s12MappingFile is the harness's set with a mappings.yaml keploy record
// wrote: test-1 owns mock-a, the worker scope holds worker (mock-boot at the
// set's start when none is given), and the set carries the marks recordedWith
// (models.Mapping.RecordedWith).
func s12MappingFile(t *testing.T, h *prRun, recordedWith []string, worker ...models.MockEntry) {
	t.Helper()
	db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": {{Name: "mock-a", Kind: string(models.HTTP),
		ReqTimestampMock: models.FormatMockTimestamp(attrAt(1010))}}})
	if len(worker) == 0 {
		worker = []models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}}
	}
	if err := db.UpsertWorker(context.Background(), "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	for _, f := range recordedWith {
		if err := db.MarkRecordedWith(context.Background(), "test-set-0", f); err != nil {
			t.Fatal(err)
		}
	}
}

// stagedExact is the set's staging call (ExactServing's), and whether there
// was one.
func stagedExact(h *prRun) (models.ExactServing, bool) {
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Stage {
			return *p.Exact, true
		}
	}
	return models.ExactServing{}, false
}

// stagedBackground is the rules the set's staging named (ExactServing's).
func stagedBackground(h *prRun) (string, bool) {
	ex, ok := stagedExact(h)
	return ex.Background, ok
}

// enableS12 registers, for the test, a func that enables S12's serving as on
// says; nil registers none.
func enableS12(t *testing.T, on *bool) {
	t.Helper()
	if on == nil {
		return
	}
	enabled := *on
	t.Cleanup(RegisterS12Serving(func() bool { return enabled }))
}

// A set an S12 record marked (recordedWith: s12-routing), replayed while a
// registered func enables S12's serving, is staged under it, and its end does
// not require its worker scope answered. Replayed with no func registered, or
// one that says no, or unmarked, or marked only with marks this keploy does
// not know, it keeps today's rules: its staging names none, and a
// worker-scope call never made fails it (audit B3, C2).
func TestUnderTheSwitchOnlyAMarkedAndEnabledSetDropsTheWorkerScopesEnd(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recordedWith []string
		enabled      *bool
		rules        string
		want         models.TestSetStatus
	}{
		{"marked and enabled", []string{models.RecordedWithS12Routing}, new(true), models.RecordedWithS12Routing, models.TestSetStatusPassed},
		{"marked among others and enabled", []string{"later-feature", models.RecordedWithS12Routing}, new(true), models.RecordedWithS12Routing, models.TestSetStatusPassed},
		{"marked, none registered", []string{models.RecordedWithS12Routing}, nil, "", models.TestSetStatusFailed},
		{"marked, registered off", []string{models.RecordedWithS12Routing}, new(false), "", models.TestSetStatusFailed},
		{"enabled, not marked", nil, new(true), "", models.TestSetStatusFailed},
		{"enabled, an unknown mark only", []string{"s13-something"}, new(true), "", models.TestSetStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			enableS12(t, tc.enabled)
			h := newPartialRunHarness(t, 1, 0)
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
			s12MappingFile(t, h, tc.recordedWith)
			// The application never makes the boot call.
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
			if got := h.run(t); got != tc.want {
				t.Fatalf("set %s, want %s", got, tc.want)
			}
			if rules, ok := stagedBackground(h); !ok || rules != tc.rules {
				t.Fatalf("the staging named background rules %q (staged %v); want %q", rules, ok, tc.rules)
			}
			// Under S12's serving the boot call never made is said, not
			// required.
			said := logs.FilterMessageSnippet("background recordings of this test set that its end had not read answered").All()
			if tc.rules == "" && len(said) != 0 {
				t.Fatalf("today's rules said %d unanswered-background lines", len(said))
			}
			if tc.rules != "" {
				if len(said) != 1 {
					t.Fatalf("S12's serving said %d unanswered-background lines; want one", len(said))
				}
				if mocks, _ := said[0].ContextMap()["mocks"].([]interface{}); len(mocks) != 1 || mocks[0] != "mock-boot" {
					t.Fatalf("the line names %v; want mock-boot", said[0].ContextMap()["mocks"])
				}
			}
		})
	}
}

// An agent that does not say it serves the staged set by S12's rules (one
// that predates them, or runs without the switch) serves it by today's, so
// the replay keeps today's check: the set's end requires its worker scope,
// and a boot call never made fails the set, though the set is marked and S12's
// serving enabled.
func TestUnderTheSwitchAnAgentThatServesTodaysRulesKeepsTheWorkerScopesEnd(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	enableS12(t, new(true))
	h := newPartialRunHarness(t, 1, 0)
	pinWindows(h)
	h.replayer.config.Command = "./app"
	h.instr.oldAgent = true
	h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
	s12MappingFile(t, h, []string{models.RecordedWithS12Routing})
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
	if got := h.run(t); got != models.TestSetStatusFailed {
		t.Fatalf("set %s; want FAILED: the agent served today's rules, so the boot call is required", got)
	}
	if rules, ok := stagedBackground(h); !ok || rules != models.RecordedWithS12Routing {
		t.Fatalf("the staging named %q (staged %v); want S12's asked for", rules, ok)
	}
	if h.replayer.exact.background {
		t.Fatal("the plan kept S12's serving though the agent did not confirm it")
	}
}

// A recording record's judging moved to the worker scope on a drifted pairing
// (models.MockEntry.Drift), whose owner this run runs, is the owner's own
// again: the owner's quarantine no longer applies (its test edited, or
// verdicts.yaml lost its entry), so the set is staged without it in the worker
// scope and the owner opens with it, as recorded (audit B4).
// With S12's serving enabled or not. An owner the run skips (quarantined),
// an exact move's owner, and an owner the set no longer has keep the move.
func TestUnderTheSwitchADriftMoveGoesBackToAnOwnerThatRuns(t *testing.T) {
	drift := models.MockEntry{Name: "mock-6", Kind: string(models.HTTP), Owner: "test-1", Drift: true,
		ReqTimestampMock: models.FormatMockTimestamp(attrAt(1020)), Reason: "mock-6 moved from test-1 to background"}
	exact := drift
	exact.Drift = false
	gone := drift
	gone.Owner = "test-9"
	for _, tc := range []struct {
		name    string
		moved   models.MockEntry
		ignored bool
		refused bool
		enabled *bool
		back    bool
	}{
		{"its quarantine gone, S12 enabled", drift, false, false, new(true), true},
		{"its quarantine gone, S12 not enabled", drift, false, false, nil, true},
		{"quarantined", drift, true, false, new(true), false},
		{"refused by the template check (fails unsent)", drift, false, true, new(true), false},
		{"an exact move", exact, false, false, new(true), false},
		{"an owner the set no longer has", gone, false, false, new(true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			enableS12(t, tc.enabled)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			if tc.ignored {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1"}}
			}
			if tc.refused {
				// A capture in a request is refused when the set loads.
				h.replayer.testSetConf = &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_1"}}}
				h.cases[0].HTTPReq.Header = map[string]string{"Content-Type": "application/json"}
				h.cases[0].HTTPReq.Body = `{"id":"{{capture string .id}}"}`
			}
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-6", 1020)}
			s12MappingFile(t, h, []string{models.RecordedWithS12Routing}, models.MockEntry{Name: "mock-boot", Kind: string(models.HTTP)}, tc.moved)
			consumed := map[string][]models.MockState{"": {mongoAt("mock-boot", 500), mongoAt("mock-6", 1020)}, "test-1": {mongoAt("mock-a", 1010)}}
			if tc.back {
				consumed = map[string][]models.MockState{"": {mongoAt("mock-boot", 500)}, "test-1": {mongoAt("mock-a", 1010), mongoAt("mock-6", 1020)}}
			}
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}
			h.run(t)
			ex, ok := stagedExact(h)
			if !ok {
				t.Fatal("the set was not staged")
			}
			var worker []string
			for _, e := range ex.Worker {
				worker = append(worker, e.Name)
			}
			inWorker := slices.Contains(worker, "mock-6")
			opened := exactOpened(h)
			if tc.back {
				if inWorker {
					t.Fatalf("the worker scope staged is %v; mock-6 went back to test-1", worker)
				}
				if !slices.Contains(opened, "test-1:mock-a,mock-6") {
					t.Fatalf("the tests opened %v; want test-1 opened with mock-6 after mock-a, as recorded", opened)
				}
				if h.replayer.exact.returned["mock-6"] != "test-1" {
					t.Fatalf("the plan says %v was given back; want mock-6 to test-1", h.replayer.exact.returned)
				}
				return
			}
			if !inWorker {
				t.Fatalf("the worker scope staged is %v; want mock-6 kept there (returned %v)", worker, h.replayer.exact.returned)
			}
			for _, o := range opened {
				if strings.HasPrefix(o, "test-1:") && strings.Contains(o, "mock-6") {
					t.Fatalf("test-1 opened with mock-6: %v", opened)
				}
			}
			if len(h.replayer.exact.returned) != 0 {
				t.Fatalf("the plan gave back %v", h.replayer.exact.returned)
			}
		})
	}
}

// A set whose mappings.yaml lists no test's mocks has what each test owns, and
// its worker scope, derived from its test windows: a drift move the file's
// worker scope holds is not read, so nothing is given back, and the set stays
// owned by its windows.
func TestUnderTheSwitchAWindowOwnedSetGivesNoMoveBack(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 1, 0)
	pinWindows(h)
	h.replayer.config.Command = "./app"
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-6", 1020)}
	db := mappingFile(t, h, nil)
	if err := db.UpsertWorker(context.Background(), "test-set-0", []models.MockEntry{{Name: "mock-6", Kind: string(models.HTTP), Owner: "test-1", Drift: true}}); err != nil {
		t.Fatal(err)
	}
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-6", 1020)}})}
	h.run(t)
	if len(h.replayer.exact.returned) != 0 {
		t.Fatalf("a window-owned set gave back %v", h.replayer.exact.returned)
	}
	if opened := exactOpened(h); !slices.Contains(opened, "test-1:mock-a,mock-6") {
		t.Fatalf("the tests opened %v; want test-1 owning what its window held", opened)
	}
}

// returnStaleMoves gives back only a drift move whose owner runs and that the
// set has, in recorded order among the owner's entries, as recorded (no
// place, line or tie); it leaves its inputs as they are, and hands them back
// untouched when nothing goes back.
func TestReturnStaleMovesGivesBackOnlyARunningOwnersDriftMoves(t *testing.T) {
	at := func(ms int) string { return models.FormatMockTimestamp(attrAt(ms)) }
	mappings := map[string][]models.MockEntry{
		"runs":    {{Name: "a", ReqTimestampMock: at(10)}, {Name: "c", ReqTimestampMock: at(30)}},
		"skipped": {{Name: "s", ReqTimestampMock: at(40)}},
	}
	worker := []models.MockEntry{
		{Name: "boot"},
		{Name: "b", Owner: "runs", Drift: true, Place: "runs", Reason: "moved", ReqTimestampMock: at(20)},
		{Name: "x", Owner: "runs", ReqTimestampMock: at(25)},                 // exact: stays
		{Name: "q", Owner: "skipped", Drift: true, ReqTimestampMock: at(45)}, // its owner skipped: stays
		{Name: "g", Owner: "gone", Drift: true, ReqTimestampMock: at(50)},    // its owner gone: stays
	}
	cases := []*models.TestCase{{Name: "runs"}, {Name: "skipped"}}
	runs := func(name string) bool { return name != "skipped" }
	gotM, gotW, returned := returnStaleMoves(mappings, worker, cases, runs)
	if !reflect.DeepEqual(returned, map[string]string{"b": "runs"}) {
		t.Fatalf("returned %v; want b to runs", returned)
	}
	var names []string
	for _, e := range gotM["runs"] {
		names = append(names, e.Name)
		if e.Name == "b" && (e.Owner != "" || e.Drift || e.Place != "" || e.Reason != "") {
			t.Fatalf("b went back with its move's marks: %+v", e)
		}
	}
	if !reflect.DeepEqual(names, []string{"a", "b", "c"}) {
		t.Fatalf("runs owns %v; want a, b, c in recorded order", names)
	}
	names = nil
	for _, e := range gotW {
		names = append(names, e.Name)
	}
	if !reflect.DeepEqual(names, []string{"boot", "x", "q", "g"}) {
		t.Fatalf("the worker scope is %v; want boot, x, q, g", names)
	}
	if len(mappings["runs"]) != 2 || len(worker) != 5 || worker[1].Owner != "runs" {
		t.Fatal("returnStaleMoves changed its inputs")
	}
	m2, w2, none := returnStaleMoves(mappings, worker, cases, func(string) bool { return false })
	if none != nil || len(w2) != len(worker) || &w2[0] != &worker[0] || len(m2) != len(mappings) {
		t.Fatalf("with no owner running it gave back %v, or copied its inputs", none)
	}
}

// A miss whose closest recording went back to its owner names the owner and
// the remedy: in the set's failure reason (returnedWhy) and for a test's call
// (returnedMissed); nothing for any other miss, or a plan that gave none back.
func TestAMissOfAReturnedRecordingNamesItsOwner(t *testing.T) {
	p := &exactPlan{returned: map[string]string{"mock-6": "post-url-1"}}
	calls := []models.UnmatchedCall{{ClosestMock: "mock-6", ActualSummary: "insert keploy.events"}, {ClosestMock: "mock-9"}, {ClosestMock: "mock-6"}}
	why := p.returnedWhy(calls)
	for _, want := range []string{"mock-6 is post-url-1's own again", "post-url-1's quarantine (keploy/unseparable_write)", "record the test set again"} {
		if !strings.Contains(why, want) {
			t.Fatalf("returnedWhy = %q; want it to say %q", why, want)
		}
	}
	if strings.Count(why, "mock-6 is") != 1 {
		t.Fatalf("returnedWhy names mock-6 more than once: %q", why)
	}
	results := []models.TestResult{
		{TestCaseID: "P", FailureInfo: models.FailureInfo{UnmatchedCalls: calls[:2]}},
		{TestCaseID: "post-url-1", FailureInfo: models.FailureInfo{UnmatchedCalls: calls[:1]}},
	}
	if got := p.returnedMissed(results); !reflect.DeepEqual(got, []string{"P: insert keploy.events (mock-6, given back to post-url-1)"}) {
		t.Fatalf("returnedMissed = %v", got)
	}
	// A test with a row per cycle is named once, from its last row.
	cycles := append([]models.TestResult{{TestCaseID: "P", FailureInfo: models.FailureInfo{UnmatchedCalls: calls[:1]}}}, results...)
	if got := p.returnedMissed(cycles); !reflect.DeepEqual(got, []string{"P: insert keploy.events (mock-6, given back to post-url-1)"}) {
		t.Fatalf("returnedMissed over two cycles = %v; want P named once", got)
	}
	var none *exactPlan
	if none.returnedWhy(calls) != "" || none.returnedMissed(results) != nil || (&exactPlan{}).returnedWhy(calls) != "" {
		t.Fatal("a plan that gave nothing back explains a miss")
	}
}

// lastResultOfEachTest keeps each test's last row, in first-seen order.
func TestLastResultOfEachTestKeepsTheLastRow(t *testing.T) {
	rows := []models.TestResult{{TestCaseID: "a", Name: "a-1"}, {TestCaseID: "b", Name: "b-1"}, {TestCaseID: "a", Name: "a-2"}}
	got := lastResultOfEachTest(rows)
	if len(got) != 2 || got[0].Name != "a-2" || got[1].Name != "b-1" {
		t.Fatalf("lastResultOfEachTest = %+v; want a's last row, then b's", got)
	}
}

// Under S12's serving the set's end requires no worker-scope recording
// answered; under today's it does.
func TestUnansweredWorkerIsNoneUnderS12sRules(t *testing.T) {
	p := &exactPlan{worker: []models.MockEntry{{Name: "w"}}, bootSeen: true}
	if got := p.unansweredWorker(map[string]models.MockState{}, true); len(got) != 1 {
		t.Fatalf("today's rules require %v; want w", got)
	}
	p.background = true
	if got := p.unansweredWorker(map[string]models.MockState{}, true); got != nil {
		t.Fatalf("S12's rules require %v; want none", got)
	}
}
