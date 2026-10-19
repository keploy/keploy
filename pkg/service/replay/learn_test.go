package replay

import (
	"reflect"
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
// judged (PASSED or FAILED), as the prune is held for one it did not. A set
// with no verdict (its agent was started again while it ran: the agent after
// knows none of the earlier places the one before served, so a value that
// flowed from one is learned as noise; a run that stopped part-way; a set
// whose every test is ignored) is not judged by keploy record either, which says nothing was written for it: its
// recording stays as recorded, for a round that can judge it, and CI never
// loads noise no round judged.
func TestRunTestSet_WhatAFullRoundLearnedIsKeptOnlyWithASetItJudged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		restartAt int // 0: one agent process answers for the whole set
		stopAt    int // 0: every test runs
		wrongBody bool
		ignoreAll bool
		status    models.TestSetStatus
		kept      bool
	}{
		{name: "judged PASSED", status: models.TestSetStatusPassed, kept: true},
		{name: "judged FAILED", wrongBody: true, status: models.TestSetStatusFailed, kept: true},
		{name: "not judged: its agent was started again mid-set", restartAt: 2, status: models.TestSetStatusInternalErr},
		{name: "not judged: the run stopped part-way", stopAt: 3},
		{name: "no verdict: every test is ignored", ignoreAll: true, status: models.TestSetStatusIgnored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, tc.stopAt)
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
			if tc.ignoreAll {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2"}}
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
