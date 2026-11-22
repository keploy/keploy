package proxy

import (
	"fmt"
	"runtime"
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
	if _, keeps := mm.keepOwned(pool[0]); keeps {
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

// A claim the store refuses to mark used takes back its own keep and no
// other. The claim runs a test hook once it kept test-2's recording, before
// the mark (testHookMark), and the row stages the next set there, as the agent
// starts one, and claims the new set's same-named recording for its test-2.
// The store then refuses to mark the earlier set's copy used. A claim that
// takes back its keep by name alone deletes the new set's keep: the new set's
// recording, served once, would serve a request that does not repeat it again,
// and a repeat would not be answered by it. The row is decided by the hook,
// not by timing.
func TestARefusedClaimTakesBackOnlyItsOwnKeep(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	marks := 0
	var fresh []*models.Mock
	var freshClaimed bool
	mm.testHookMark = func() {
		marks++
		if marks > 1 {
			return
		}
		mm.ResetForReplaySession()
		mm.ArmSetFence()
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 2010, 2050)}, nil, winStart(2), winEnd(2))
		fresh, _ = mm.GetPerTestMocksInWindow()
		if len(fresh) == 1 {
			freshClaimed = mm.ClaimForRepeats(*fresh[0])
		}
	}
	// The hook runs inside the claim, on this goroutine, so marks is final
	// when it returns; a claim that skips the hook fails at once.
	if mm.ClaimForRepeats(*pool[0]) {
		t.Fatalf("the earlier set's copy was marked used once the next set was staged (its set %d, the store's %d)", pool[0].TestModeInfo.StagedSet, mm.setGen.Load())
	}
	if marks != 2 {
		t.Fatalf("the claims reached their mark %d times, want twice (the earlier set's claim and the new set's inside it)", marks)
	}
	if len(fresh) != 1 || !freshClaimed {
		t.Fatalf("the new set's pool %v: its recording's claim kept %v, want it kept for its test", fresh, freshClaimed)
	}
	if !kept(mm, "poll") {
		t.Fatalf("the earlier set's refused claim took back the new set's keep of its same-named recording")
	}
	if mm.ClaimForRepeats(*fresh[0]) {
		t.Fatalf("the new set's recording was kept a second time: two requests that do not repeat it would both be served it")
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

// No staging lands from a claim's owner check, at the point that read the
// open test's window and pool, through its keep: the claim holds the
// staging's lock over all of it. The claim runs a test hook at each end, as
// the owner check returns (testHookOwnerRead) and just before the keep
// (testHookKeep), and the row checks the lock at both, in two cases. In the
// first, the next test's staging is queued on the lock from the owner
// check's read. A queued writer lets no new reader in before it, so a claim
// that lets go of the lock there, even to take it again, lets the staging in
// before its keep. In the second, the staging's lock is tried just before
// the keep, and the staging is started there. In both, the staging then
// waits for the claim and drops what it kept, so test-3's same-named
// recording is free to be kept for test-3. A staging that landed inside
// would leave test-2's recording's name kept in test-3: test-3's same-named
// recording would read as kept from its start and answer only
// byte-identical repeats, and a request only the schema matches would miss.
// The row is decided by the lock, not by timing.
func TestNoStagingLandsFromAClaimsOwnerReadToItsKeep(t *testing.T) {
	stage3 := func(mm *MockManager) {
		mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 3010, 3050)}, nil, winStart(3), winEnd(3))
	}
	// lockFree is true when no claim holds the staging's lock: a staging
	// could land at this point.
	lockFree := func(mm *MockManager) bool {
		if !mm.swapMu.TryLock() {
			return false
		}
		mm.swapMu.Unlock()
		return true
	}
	// claim runs test-2's claim with the hooks set, and checks that each ran
	// once, that the staging could land nowhere inside the claim, and that
	// test-3 is staged with nothing kept and its recording free to be kept
	// for it. The hooks run inside the claim, on this goroutine, so their
	// counts are final when it returns; staged closes only after a hook ran,
	// so a claim that skips its hooks fails at once instead of waiting.
	claim := func(t *testing.T, mm *MockManager, pool []*models.Mock, reads, keeps *int, inside *[]string, staged chan struct{}) {
		t.Helper()
		mm.ClaimForRepeats(*pool[0])
		if *reads != 1 || *keeps != 1 {
			t.Fatalf("the claim of test-2's own recording ran its owner check's read %d times and reached its keep %d times, want once each", *reads, *keeps)
		}
		<-staged
		if len(*inside) != 0 {
			t.Errorf("test-3's staging could land inside the claim: %v", *inside)
		}
		if mm.KeptForRepeats() != nil {
			t.Fatalf("test-3 is staged with test-2's recording kept: test-3's same-named recording would answer only repeats")
		}
		next, err := mm.GetPerTestMocksInWindow()
		if err != nil || len(next) != 1 || !mm.ClaimForRepeats(*next[0]) {
			t.Fatalf("test-3's pool %v (%v): its recording's claim was refused, want it kept for test-3", next, err)
		}
	}
	t.Run("a staging queued on the lock from the owner check's read", func(t *testing.T) {
		mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
		reads, keeps := 0, 0
		var inside []string
		var landed atomic.Bool
		staged := make(chan struct{})
		mm.testHookOwnerRead = func() {
			reads++
			if reads > 1 {
				return
			}
			if lockFree(mm) {
				inside = append(inside, "the lock was free at the owner check's read")
			}
			go func() {
				mm.swapMu.Lock()
				landed.Store(true)
				mm.swapMu.Unlock()
				stage3(mm)
				close(staged)
			}()
			// Wait for the staging to queue on the lock (a reader is refused
			// from then on) or to have taken it.
			for !landed.Load() && mm.swapMu.TryRLock() {
				mm.swapMu.RUnlock()
				runtime.Gosched()
			}
		}
		mm.testHookKeep = func() {
			keeps++
			if keeps == 1 && landed.Load() {
				inside = append(inside, "the staging queued at the owner check's read took the lock before the keep")
			}
		}
		claim(t, mm, pool, &reads, &keeps, &inside, staged)
	})
	t.Run("a staging tried just before the keep", func(t *testing.T) {
		mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
		reads, keeps := 0, 0
		var inside []string
		staged := make(chan struct{})
		mm.testHookOwnerRead = func() {
			reads++
			if reads == 1 && lockFree(mm) {
				inside = append(inside, "the lock was free at the owner check's read")
			}
		}
		mm.testHookKeep = func() {
			keeps++
			if keeps > 1 {
				return
			}
			if lockFree(mm) {
				inside = append(inside, "the lock was free just before the keep")
			}
			go func() { stage3(mm); close(staged) }()
		}
		claim(t, mm, pool, &reads, &keeps, &inside, staged)
	})
}

// No set boundary (the next set's staging) lands from a claim's set check, at
// the point that read the set generation, through its keep: the claim holds
// the staging's lock over all of it, so a copy of the previous set's pool read
// before the next set's staging keeps nothing for the new set. The claim runs
// a test hook as its set check returns (testHookSetRead) and just before the
// keep (testHookKeep). At the set check's read the row starts the next set's
// staging. When the lock is free there, the staging lands there. When the
// claim holds it, the staging queues on it and must not have taken it by the
// keep: a queued writer lets no new reader in before it, so a claim that lets
// go of the lock after its set check, even to take it again, lets the staging
// in before its keep. At the keep the row also checks that the copy kept is of
// the store's set. A claim that decides the set before it takes the lock keeps
// the earlier set's copy's name in the new set until the store refuses to mark
// it used, and a request beside it then reads the new set's same-named
// recording as kept, and is not served it. The staging then drops what the
// claim kept, so the new set's recording is free to be kept for its test. The
// row is decided by the lock, not by timing.
func TestNoSetBoundaryLandsFromAClaimsSetCheckToItsKeep(t *testing.T) {
	mm, pool := claimRig(t, statedPoll("poll", `{"id":"a"}`, 2010, 2050))
	// The next set's staging, as the agent starts one.
	nextSet := func() {
		mm.ResetForReplaySession()
		mm.ArmSetFence()
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow([]*models.Mock{statedPoll("poll", `{"id":"a"}`, 2010, 2050)}, nil, winStart(2), winEnd(2))
	}
	set := pool[0].TestModeInfo.StagedSet
	if set == 0 || set != mm.setGen.Load() {
		t.Fatalf("test-2's recording is of set %d, the store's set is %d: want it stamped with the store's set", set, mm.setGen.Load())
	}
	sets, keeps := 0, 0
	var inside []string
	var landed atomic.Bool
	staged := make(chan struct{})
	mm.testHookSetRead = func() {
		sets++
		if sets > 1 {
			return
		}
		if mm.swapMu.TryLock() {
			mm.swapMu.Unlock()
			inside = append(inside, "the lock was free at the set check's read")
			nextSet()
			close(staged)
			return
		}
		go func() {
			mm.swapMu.Lock()
			landed.Store(true)
			mm.swapMu.Unlock()
			nextSet()
			close(staged)
		}()
		// Wait for the staging to queue on the lock (a reader is refused
		// from then on) or to have taken it.
		for !landed.Load() && mm.swapMu.TryRLock() {
			mm.swapMu.RUnlock()
			runtime.Gosched()
		}
	}
	mm.testHookKeep = func() {
		keeps++
		if keeps > 1 {
			return
		}
		if landed.Load() {
			inside = append(inside, "the staging queued at the set check's read took the lock before the keep")
		}
		if now := mm.setGen.Load(); now != set {
			inside = append(inside, fmt.Sprintf("the claim kept set %d's copy in set %d", set, now))
		}
	}
	// The hooks run inside the claim, on this goroutine, so their counts are
	// final when it returns; staged closes only after the set hook ran, so a
	// claim that skips its hooks fails at once instead of waiting.
	mm.ClaimForRepeats(*pool[0])
	if sets != 1 || keeps != 1 {
		t.Fatalf("the claim of test-2's own recording ran its set check's read %d times and reached its keep %d times, want once each", sets, keeps)
	}
	<-staged
	if len(inside) != 0 {
		t.Errorf("the next set's staging could land inside the claim: %v", inside)
	}
	if mm.KeptForRepeats() != nil {
		t.Fatalf("the next set is staged with the previous set's recording kept: its same-named recording would answer only repeats")
	}
	fresh, err := mm.GetPerTestMocksInWindow()
	if err != nil || len(fresh) != 1 || !mm.ClaimForRepeats(*fresh[0]) {
		t.Fatalf("the new set's pool %v (%v): its recording's claim was refused, want it kept for its test", fresh, err)
	}
}
