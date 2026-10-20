package replay

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestLearnedByTest(t *testing.T) {
	added := map[string][]string{
		"mock-1": {"body.id (noise)"},
		"mock-2": {"body.key (echo key)", "header.x (noise)"},
		"mock-9": {"url.path.1 (noise)"},
	}
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-3"}}},
		{ID: "test-2", Mocks: []models.MockEntry{{Name: "mock-2"}}},
		{ID: "test-3", Mocks: []models.MockEntry{{Name: "mock-4"}}},
	}}
	got := learnedByTest(added, mapping)
	want := []learnedLine{
		{test: "test-1", fields: []string{"mock-1 body.id (noise)"}},
		{test: "test-2", fields: []string{"mock-2 body.key (echo key)", "mock-2 header.x (noise)"}},
		{fields: []string{"mock-9 url.path.1 (noise)"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if learnedByTest(nil, mapping) != nil {
		t.Fatal("nothing learned says nothing")
	}
}

// P056-h: what record's full round learned is kept only with a set the round
// judged, as keploy record's judging reads a round: PASSED or FAILED, with a
// verdict for every one of its tests. The prune is held for any other. A set
// with no verdict (its agent was started again while it ran: the agent after
// knows none of the earlier places the one before served, so a value that
// flowed from one is learned as noise; a run that stopped part-way, PASSED
// or FAILED so far; a set whose every test is ignored), or one a test of
// which got none (it is ignored, or the run did not select it), is not
// judged by keploy record either, which says nothing was written for it: its
// recording stays as recorded, for a round that can judge it, and CI never
// loads noise no round judged.
func TestRunTestSet_WhatAFullRoundLearnedIsKeptOnlyWithASetItJudged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tests     int // 0: two
		restartAt int // 0: one agent process answers for the whole set
		stopAt    int // 0: every test runs
		wrongBody bool
		ignore    []string
		selected  []string
		status    models.TestSetStatus
		kept      bool
	}{
		{name: "judged PASSED", status: models.TestSetStatusPassed, kept: true},
		{name: "judged FAILED", wrongBody: true, status: models.TestSetStatusFailed, kept: true},
		{name: "not judged: its agent was started again mid-set", restartAt: 2, status: models.TestSetStatusInternalErr},
		{name: "not judged: the run stopped part-way", stopAt: 3},
		{name: "not judged: a FAILED run stopped part-way", tests: 3, wrongBody: true, stopAt: 3, status: models.TestSetStatusFailed},
		{name: "no verdict: every test is ignored", ignore: []string{"test-1", "test-2"}, status: models.TestSetStatusIgnored},
		{name: "not judged: a test of a PASSED set is ignored", ignore: []string{"test-2"}, status: models.TestSetStatusPassed},
		{name: "not judged: a test of a FAILED set is ignored", wrongBody: true, ignore: []string{"test-2"}, status: models.TestSetStatusFailed},
		{name: "not judged: the run selected one of the set's tests", selected: []string{"test-1"}, status: models.TestSetStatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			tests := tc.tests
			if tests == 0 {
				tests = 2
			}
			h := newPartialRunHarness(t, tests, tc.stopAt)
			h.replayer.config.Test.Learn = true
			store := &learnedStore{prMockDB: h.mocks}
			h.replayer.mockDB = store
			// What the learn step learned on a mock the set consumed.
			hooks := prHooks{consumed: []models.MockState{{Name: "mock-1", Kind: models.HTTP, ReqBodyNoise: map[string][]string{"body.id": {}}}}}
			if tc.wrongBody {
				hooks.wrongBody = map[string]bool{"test-1": true}
			}
			h.replayer.hookImpl = hooks
			if tc.restartAt > 0 {
				newOverTheWire(t, h, 0, tc.restartAt)
			}
			if len(tc.ignore) > 0 {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignore}
			}
			if len(tc.selected) > 0 {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": tc.selected}
			}
			status := h.run(t)
			switch {
			case tc.status != "" && status != tc.status:
				t.Fatalf("fixture: status %s, want %s", status, tc.status)
			case tc.status == "" && (status == models.TestSetStatusPassed || status == models.TestSetStatusFailed):
				t.Fatalf("fixture: status %s, want a set with no verdict", status)
			}
			if tc.kept {
				if _, ok := store.persisted["mock-1"]; store.writes != 1 || !ok {
					t.Fatalf("a judged %s set did not keep what its full round learned: %d writes of %v", status, store.writes, store.persisted)
				}
				return
			}
			if store.writes != 0 {
				t.Fatalf("a set with no verdict (%s) kept what its full round learned: %d writes of %v", status, store.writes, store.persisted)
			}
		})
	}
}

// judgedSet reads a set as judged as keploy record's judging does: PASSED or
// FAILED, every test of the set with a row that is a verdict (not IGNORED,
// read and checked whole), and results read back whole.
func TestJudgedSetNeedsAVerdictForEveryTestOfTheSet(t *testing.T) {
	row := func(id string, st models.TestStatus) models.TestResult {
		return models.TestResult{TestCaseID: id, Status: st}
	}
	passed, failed := models.TestSetStatusPassed, models.TestSetStatusFailed
	both := []string{"t1", "t2"}
	unread := row("t2", models.TestStatusPassed)
	unread.FailureInfo.TestMocksUnread = true
	uncapturable := row("t2", models.TestStatusFailed)
	uncapturable.FailureInfo.CannotCapture = models.CaptureLosses{{}}
	for _, tc := range []struct {
		name     string
		status   models.TestSetStatus
		loaded   []string
		results  []models.TestResult
		complete bool
		judged   bool
	}{
		{"every test passed", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusPassed)}, true, true},
		{"failed and obsolete are verdicts", failed, both, []models.TestResult{row("t1", models.TestStatusFailed), row("t2", models.TestStatusObsolete)}, true, true},
		{"an ignored attempt and a passed one", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusIgnored), row("t2", models.TestStatusPassed)}, true, true},
		{"a test ignored", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusIgnored)}, true, false},
		{"a test never reached", failed, both, []models.TestResult{row("t1", models.TestStatusFailed)}, true, false},
		{"a test whose mocks could not be read", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), unread}, true, false},
		{"a test one of whose rows could not be read", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusPassed), unread}, true, false},
		{"a test keploy cannot capture", failed, both, []models.TestResult{row("t1", models.TestStatusPassed), uncapturable}, true, false},
		{"results not read back whole", passed, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusPassed)}, false, false},
		{"not judged", models.TestSetStatusInternalErr, both, []models.TestResult{row("t1", models.TestStatusPassed), row("t2", models.TestStatusPassed)}, true, false},
		{"stopped part-way", models.TestSetStatusFaultUserApp, both, []models.TestResult{row("t1", models.TestStatusPassed)}, true, false},
		{"no test loaded", passed, nil, nil, true, false},
	} {
		if got := judgedSet(tc.status, tc.loaded, tc.results, tc.complete); got != tc.judged {
			t.Errorf("%s: judged %v, want %v", tc.name, got, tc.judged)
		}
	}
	cases := []*models.TestCase{{Name: "t1"}, nil, {Name: "t2"}, {Name: "t3"}}
	if got := testNames(cases); strings.Join(got, ",") != "t1,t2,t3" {
		t.Errorf("the set's tests: %v", got)
	}
}
