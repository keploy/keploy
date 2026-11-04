package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// claimRig is a manager with test-2 staged as the agent stages a test
// (SetMocksWithWindow with the test's recorded window) and open, holding
// recording, and test-2's own copy of it as the per-test pool serves it.
func claimRig(t *testing.T, recording ...*models.Mock) (*MockManager, []*models.Mock) {
	t.Helper()
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2", "test-3"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow(recording, nil, winStart(2), winEnd(2))
	pool, err := mm.GetPerTestMocksInWindow()
	if err != nil || len(pool) == 0 {
		t.Fatalf("test-2's per-test pool %v (%v), want its recordings", pool, err)
	}
	return mm, pool
}

func kept(mm *MockManager, name string) bool {
	k := mm.KeptForRepeats()
	return k != nil && k(name)
}

// The consume that serves the open test's recording first keeps it for the
// test's repeats and leaves it in the per-test pool, counted as used; a
// second claim of it keeps nothing, so only one request that does not
// repeat it is served it.
func TestAClaimKeepsTheOpenTestsRecordingOnceAndLeavesItInThePool(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	var db integrations.MockMemDb = mm
	if claimed, claims := integrations.ClaimForRepeats(db, *pool[0]); !claims || !claimed {
		t.Fatalf("first claim: claimed %v, claims %v; want the store to keep it", claimed, claims)
	}
	if !kept(mm, "poll") {
		t.Fatalf("after the claim the store does not name the recording kept")
	}
	if after, _ := mm.GetPerTestMocksInWindow(); len(after) != 1 {
		t.Fatalf("after the claim the per-test pool holds %d recordings, want the kept one left in it", len(after))
	}
	if used := mm.GetConsumedMocks(); len(used) != 1 || used[0].Name != "poll" {
		t.Errorf("the store reports %v used, want the claimed recording reported once", used)
	}
	if mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("a second claim of a kept recording kept it again: two requests that do not repeat it would both be served it")
	}
}

// Requests beside each other that claim one recording keep it once.
func TestConcurrentClaimsKeepARecordingOnce(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	var won atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if mm.ClaimForRepeats(*pool[0]) {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := won.Load(); got != 1 {
		t.Fatalf("%d of 32 concurrent claims kept the recording, want 1", got)
	}
}

// Every staging drops what was kept: the next test's staging, and the same
// test staged again (a re-run of the set), whose claim keeps it afresh.
func TestAStagingDropsWhatWasKept(t *testing.T) {
	poll := statedPoll("poll", `{"id":"a"}`, 2010, 2050)
	mm, pool := claimRig(t, poll)
	if !mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("claim in test-2 refused")
	}
	mm.SetMocksWithWindow(nil, nil, winStart(3), winEnd(3))
	if mm.KeptForRepeats() != nil {
		t.Fatalf("test-3's staging left test-2's kept recording kept")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{poll}, nil, winStart(2), winEnd(2))
	again, _ := mm.GetPerTestMocksInWindow()
	if len(again) != 1 || !mm.ClaimForRepeats(*again[0]) {
		t.Fatalf("the set staged again: pool %v, claim refused; want its recording kept afresh", again)
	}
}

// A consume that read test-2's recording and lands after test-3 was staged
// keeps nothing: its owner is decided with the keep, under the staging's
// lock, and test-2's recording is not test-3's own.
func TestAClaimThatStraddlesTheNextTestsStagingKeepsNothing(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050), statedPoll("poll-3", `{"id":"c"}`, 3010, 3050))
	var own2 *models.Mock
	for _, m := range pool {
		if m.Name == "poll" {
			own2 = m
		}
	}
	if own2 == nil {
		t.Fatalf("test-2's pool %v holds no poll", pool)
	}
	mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 2010, 2050), statedPoll("poll-3", `{"id":"c"}`, 3010, 3050)}, nil, winStart(3), winEnd(3))
	if mm.ClaimForRepeats(*own2) {
		t.Fatalf("test-2's recording, claimed once test-3 was staged, was kept: a repeat in test-3 would be served it")
	}
	if mm.KeptForRepeats() != nil {
		t.Fatalf("the refused claim left a recording kept")
	}
}

// A copy read before the set was staged again, at a set boundary, is of the
// earlier set: its claim keeps nothing, not even for the moment before the
// store refuses to mark it used (a request beside it would then read the new
// staging's same-named recording as kept, and not be served it), and the new
// staging's same-named recording is still free to be kept.
func TestAClaimOfAnEarlierStagingsCopyKeepsNothing(t *testing.T) {
	poll := statedPoll("poll", `{"id":"a"}`, 2010, 2050)
	mm, pool := claimRig(t, poll)
	// The next set's staging, as the agent starts one.
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 2010, 2050)}, nil, winStart(2), winEnd(2))
	if mm.keepOwned(pool[0]) {
		t.Fatalf("the claim of a copy of the earlier staging kept it before marking it used (its set %d, the store's %d)", pool[0].TestModeInfo.StagedSet, mm.setGen.Load())
	}
	if mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("a copy of the earlier staging was kept (its set %d, the store's %d)", pool[0].TestModeInfo.StagedSet, mm.setGen.Load())
	}
	fresh, _ := mm.GetPerTestMocksInWindow()
	if len(fresh) != 1 || !mm.ClaimForRepeats(*fresh[0]) {
		t.Fatalf("the new staging's recording could not be kept after the stale claim")
	}
}

// With no test open (between tests, after the window closed) nothing is the
// open test's own, so nothing is kept.
func TestNoClaimWhileNoTestIsOpen(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 2010, 2050)}, nil, time.Time{}, time.Time{})
	if mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("a recording was kept with no test open")
	}
}

// A claim the store cannot mark used (an unnamed mock is never marked) keeps
// nothing.
func TestAClaimTheStoreCannotMarkUsedKeepsNothing(t *testing.T) {
	unnamed := statedPoll("", `{"id":"a"}`, 2010, 2050)
	mm, pool := claimRig(t, unnamed)
	if mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("an unnamed recording was claimed")
	}
	if mm.KeptForRepeats() != nil {
		t.Fatalf("the refused claim left the unnamed recording kept")
	}
}

// No staging lands between a claim's owner check and its keep. The next
// test's staging, tried at that point, waits for the claim and then drops
// what it kept, so test-3's same-named recording is free to be kept for
// test-3. Before the claim decided under the staging's lock, the staging
// landed there and the claim kept test-2's recording's name in test-3: the
// same-named recording of test-3 read as kept from its start, so it answered
// only byte-identical repeats and a request only the schema matches missed.
// The hook lands the staging at once when it can (no claim holds the
// staging's lock), so the row is decided by the lock, not by timing.
func TestNoStagingLandsBetweenAClaimsOwnerCheckAndItsKeep(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	stage3 := func() {
		mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 3010, 3050)}, nil, winStart(3), winEnd(3))
	}
	decided, landedInside := 0, false
	staged := make(chan struct{})
	mm.testHookKeepOwnedDecided = func() {
		decided++
		if decided > 1 {
			return
		}
		if mm.swapMu.TryLock() {
			mm.swapMu.Unlock()
			stage3()
			landedInside = true
			close(staged)
			return
		}
		go func() { stage3(); close(staged) }()
	}
	mm.ClaimForRepeats(*pool[0])
	<-staged
	if decided != 1 {
		t.Fatalf("the claim of test-2's own recording reached its keep %d times, want once", decided)
	}
	if landedInside {
		t.Errorf("test-3's staging landed between the claim's owner check and its keep")
	}
	if mm.KeptForRepeats() != nil {
		t.Fatalf("test-3 is staged with test-2's recording kept: test-3's same-named recording would answer only repeats")
	}
	next, err := mm.GetPerTestMocksInWindow()
	if err != nil || len(next) != 1 || !mm.ClaimForRepeats(*next[0]) {
		t.Fatalf("test-3's pool %v (%v): its recording's claim was refused, want it kept for test-3", next, err)
	}
}
