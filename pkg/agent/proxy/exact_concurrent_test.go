package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
	mm.ServeExactTestConcurrent("T", []*models.Mock{own, later}, []*models.Mock{shared, own})
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
	mm.ServeExactTestConcurrent("A", nil, []*models.Mock{c1, c2})
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
	mm.ServeExactTestConcurrent("B", nil, []*models.Mock{c1, c2})
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
	cands := []*models.Mock{c1, c2}
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
// own.
func TestASharedRecordingIsNeverRequiredOfTheTest(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	c1 := exactMock("c1", time.Second, models.LifetimePerTest)
	c2 := exactMock("c2", 2*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own, c1, c2}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("T", []*models.Mock{own}, []*models.Mock{c1, c2})
	pt, _ := mm.GetPerTestMocksInWindow()
	if !mm.DeleteFilteredMock(*pt[1]) {
		t.Fatal("c1 was refused")
	}
	mm.ServeExactPool("-", nil)
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || len(consumed) != 0 || strings.Join(unconsumed, ",") != "own" {
		t.Fatalf("outcome %q consumed=%v unconsumed=%v; want only own, unconsumed", test, consumed, unconsumed)
	}
	if !mm.StagedOwnerBegun("own") || mm.StagedOwnerBegun("c1") || mm.StagedOwnerBegun("c2") {
		t.Fatal("the tests begun hold a shared recording")
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

// The agent's proxy hands the manager what the agent was sent, both ways it
// opens a test (Proxy.ServeExactTestConcurrent, OpenExactTestConcurrent): the
// test's own, then its shared recordings, each copy saying it is shared, which
// is what a parser's learn step reads to never learn against one. A proxy that
// dropped them would serve the test less than its plan, or, dropping only the
// mark, have its shared recordings learned against as its own.
func TestTheProxyServesATestTheRecordingsItShares(t *testing.T) {
	for _, open := range []struct {
		name string
		do   func(p *Proxy, owned, cands []*models.Mock) error
	}{
		{"serve", func(p *Proxy, owned, cands []*models.Mock) error {
			return p.ServeExactTestConcurrent("T", owned, cands)
		}},
		{"open", func(p *Proxy, owned, cands []*models.Mock) error {
			return p.OpenExactTestConcurrent("T", owned, cands, nil)
		}},
	} {
		t.Run(open.name, func(t *testing.T) {
			own := exactMock("own", 0, models.LifetimePerTest)
			c1 := exactMock("c1", time.Second, models.LifetimePerTest)
			c2 := exactMock("c2", 2*time.Second, models.LifetimePerTest)
			mm := stagedManager(t, []*models.Mock{own, c1, c2}, nil)
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
			p.setMockManager(mm)
			mm.ServeExactPool("-", nil)
			if err := open.do(p, []*models.Mock{own}, []*models.Mock{c1, c2}); err != nil {
				t.Fatal(err)
			}
			pt, _ := mm.GetPerTestMocksInWindow()
			equalNames(t, "T's view", pt, "own", "c1", "c2")
			for _, m := range pt {
				if want := m.Name != "own"; m.TestModeInfo.Concurrent != want {
					t.Fatalf("%s: shared = %v, want %v", m.Name, m.TestModeInfo.Concurrent, want)
				}
			}
			if test, _, _, ok := mm.ExactTestOutcome(); !ok || test != "T" {
				t.Fatalf("the open test is %q (%v); want T", test, ok)
			}
		})
	}
}

// A miss the learn step refused among the recordings a test shares with the
// tests recorded beside it reaches the replay with what the parser knows of
// them (models.MockMismatchReport.Concurrent): its reason's case, the recording
// it names and the places, the words the judging's reason is made of. Without
// it the reason would say only that keploy could not tell who made the call.
func TestAMissCarriesWhatTheLearnStepKnowsOfTheSharedRecordings(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	p.setMockManager(mm)
	mm.ServeExactTest("T", nil)
	p.BeginTestErrorCapture()
	note := &models.ConcurrentNote{Class: []string{"mock-b"}, Case: models.ConcurrentCaseNotOwn, Places: []string{"bind.$2"}}
	p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
		Protocol: "PostgreSQL", ActualSummary: "INSERT", ClosestMock: "mock-b", Reason: models.MissConcurrentAmbiguous, Concurrent: note}))
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("want the one miss, got %+v (%v)", got, err)
	}
	if got[0].Reason != models.MissConcurrentAmbiguous || got[0].Concurrent == nil || got[0].Concurrent.Case != models.ConcurrentCaseNotOwn || strings.Join(got[0].Concurrent.Class, ",") != "mock-b" {
		t.Fatalf("filed %+v; want concurrent_ambiguous with its note", got[0])
	}
}

// The claims are one staging's: a set staged again (its mock names repeat in
// the next set, or the same set's next run) starts with every shared recording
// unclaimed. Without it a recording another set's test answered with would be
// hidden from this set's tests, and their calls refused.
func TestClaimsAreTheStagingsOwn(t *testing.T) {
	c1 := exactMock("c1", 0, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{c1}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("A", nil, []*models.Mock{c1})
	pt, _ := mm.GetPerTestMocksInWindow()
	if !mm.DeleteFilteredMock(*pt[0]) {
		t.Fatal("A's call of c1 was refused")
	}
	mm.SetMocksWithWindow([]*models.Mock{c1}, nil, models.BaseTime, time.Now())
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("B", nil, []*models.Mock{c1})
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "B's view in the next staging", pt, "c1")
	if _, ok := mm.ClaimantOf("c1"); ok {
		t.Fatal("a claim outlived its staging")
	}
	if !mm.DeleteFilteredMock(*pt[0]) {
		t.Fatal("B's call of c1 was refused for A's claim of the staging before")
	}
	if who, _ := mm.ClaimantOf("c1"); who != "B" {
		t.Fatalf("ClaimantOf(c1) = %q; want B", who)
	}
}
