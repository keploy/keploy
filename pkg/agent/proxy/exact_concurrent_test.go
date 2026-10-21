package proxy

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Shared recordings (ServeExactTestConcurrent, keploy record's full round):
// a test that ran beside others at record time is shown, after its own test
// mocks, the recordings made while they were open together. Each answers at
// most once in the staging, for whichever of those tests makes its call
// first, and none is required of the test.

// A shared recording is read after every one of the test's own, whatever the
// recorded order: a parser that takes the first recording that matches
// (Postgres v3's Pass 1, in its cohort's input order) takes the test's own
// over an equal shared one. Each copy says it is shared.
func TestASharedRecordingIsReadAfterTheTestsOwn(t *testing.T) {
	shared := exactMock("shared", 0, models.LifetimePerTest)
	own := exactMock("own", time.Second, models.LifetimePerTest)
	later := exactMock("own-later", 2*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{shared, own, later}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("T", []*models.Mock{own, later}, ExactConcurrent{Cands: []*models.Mock{shared, own}})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "own", "own-later", "shared")
	for _, m := range pt {
		if want := m.Name == "shared"; m.TestModeInfo.Concurrent != want {
			t.Fatalf("%s: Concurrent = %v, want %v (own named twice stays the test's own)", m.Name, m.TestModeInfo.Concurrent, want)
		}
	}
}

// A shared recording one test answered with is shown to no other test, and a
// consume of it there is refused; the test that answered with it does not
// get it again. Its consume is flagged consumed and shared (MockState.Concurrent),
// and ClaimantOf names its test.
func TestASharedRecordingAnswersOnceForWhicheverTestCallsFirst(t *testing.T) {
	c1 := exactMock("c1", 0, models.LifetimePerTest)
	c2 := exactMock("c2", time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{c1, c2}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("A", nil, ExactConcurrent{Cands: []*models.Mock{c1, c2}})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "A's view", pt, "c1", "c2")
	if !mm.DeleteFilteredMock(*pt[1]) {
		t.Fatal("A's call of c2 was refused")
	}
	if test, ok := mm.ClaimantOf("c2"); !ok || test != "A" {
		t.Fatalf("ClaimantOf(c2) = %q %v; want A", test, ok)
	}
	states := mm.GetConsumedMocks()
	if len(states) != 1 || states[0].Name != "c2" || !states[0].Concurrent {
		t.Fatalf("consumed %+v; want c2, flagged shared", states)
	}
	stale := pt[1]
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("B", nil, ExactConcurrent{Cands: []*models.Mock{c1, c2}})
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "B's view", pt, "c1")
	equalNames(t, "B's taken", mm.ExactTakenCandidates(models.HTTP), "c2")
	byName, _ := mm.GetFilteredMocksByKind(models.HTTP)
	equalNames(t, "B's view by kind", byName, "c1")
	// B's own copy of c2, as a parser that built it by name would hold it.
	c2B := c2.ShallowCopy()
	if mm.DeleteFilteredMock(*c2B) {
		t.Fatal("B answered with the recording A answered with")
	}
	if mm.DeleteFilteredMock(*stale) {
		t.Fatal("a copy A's view served answered in B's")
	}
	if !mm.DeleteFilteredMock(*pt[0]) {
		t.Fatal("B's call of c1 was refused")
	}
	if test, _ := mm.ClaimantOf("c1"); test != "B" {
		t.Fatalf("ClaimantOf(c1) = %q; want B", test)
	}
}

// A test opened again (a retry) gives back the shared recordings it answered
// with, and only those: another test's stay taken.
func TestAReopenedTestGivesBackOnlyItsOwnClaims(t *testing.T) {
	c1 := exactMock("c1", 0, models.LifetimePerTest)
	c2 := exactMock("c2", time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{c1, c2}, nil)
	mm.ServeExactPool("-", nil)
	cands := ExactConcurrent{Cands: []*models.Mock{c1, c2}}
	mm.ServeExactTestConcurrent("A", nil, cands)
	pt, _ := mm.GetPerTestMocksInWindow()
	mm.DeleteFilteredMock(*pt[0]) // A takes c1
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("B", nil, cands)
	pt, _ = mm.GetPerTestMocksInWindow()
	mm.DeleteFilteredMock(*pt[0]) // B takes c2
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("A", nil, cands)
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "A again", pt, "c1")
	if test, _ := mm.ClaimantOf("c2"); test != "B" {
		t.Fatalf("B's claim on c2 went with A's retry: claimant %q", test)
	}
	if _, ok := mm.ClaimantOf("c1"); ok {
		t.Fatal("A's retry kept its claim on c1")
	}
}

// A shared recording is never required of the test: the outcome lists only
// its own, answered or not, and the mocks of the tests begun hold only its
// own; a call the learn step reads its inputs and what it may not pair with
// for, through ExactConcurrentReader.
func TestASharedRecordingIsNeverRequiredOfTheTest(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	c1 := exactMock("c1", time.Second, models.LifetimePerTest)
	c2 := exactMock("c2", 2*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own, c1, c2}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("T", []*models.Mock{own}, ExactConcurrent{Cands: []*models.Mock{c1, c2}, Inputs: []string{"acme"}, NotLearnable: []string{"c2"}})
	pt, _ := mm.GetPerTestMocksInWindow()
	if !mm.DeleteFilteredMock(*pt[1]) {
		t.Fatal("c1 was refused")
	}
	if !reflect.DeepEqual(mm.ExactTestInputs(), []string{"acme"}) || !mm.ExactTestNotLearnable("c2") || mm.ExactTestNotLearnable("c1") {
		t.Fatalf("inputs %v, not learnable c1=%v c2=%v", mm.ExactTestInputs(), mm.ExactTestNotLearnable("c1"), mm.ExactTestNotLearnable("c2"))
	}
	mm.ServeExactPool("-", nil)
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || len(consumed) != 0 || strings.Join(unconsumed, ",") != "own" {
		t.Fatalf("outcome %q consumed=%v unconsumed=%v; want only own, unconsumed", test, consumed, unconsumed)
	}
	if !mm.StagedOwnerBegun("own") || mm.StagedOwnerBegun("c1") || mm.StagedOwnerBegun("c2") {
		t.Fatal("the tests begun hold a shared recording")
	}
	if mm.ExactTestInputs() != nil || mm.ExactTestNotLearnable("c2") {
		t.Fatal("a pool view says what the ended test's request held")
	}
}

// Without shared recordings ServeExactTest is what it was: no copy is marked
// shared and nothing is claimed.
func TestServeExactTestSharesNothing(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.ServeExactTest("T", []*models.Mock{own})
	pt, _ := mm.GetPerTestMocksInWindow()
	if len(pt) != 1 || pt[0].TestModeInfo.Concurrent {
		t.Fatalf("view %v", exactNames(pt))
	}
	mm.DeleteFilteredMock(*pt[0])
	if _, ok := mm.ClaimantOf("own"); ok {
		t.Fatal("a test's own recording was claimed")
	}
	if states := mm.GetConsumedMocks(); len(states) != 1 || states[0].Concurrent {
		t.Fatalf("consumed %+v", states)
	}
}
