package manager

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// fullOutChanManager returns a manager whose outChan is a tiny-capacity,
// never-drained channel already filled to capacity, so the very next
// sendToOutChanOwned exhausts the send budget and takes the capacity-drop
// branch. Each forced drop costs a real sendBudget (200ms) of wall-clock, so
// tests that need MANY distinct drops should drive recordDroppedTC directly
// instead of routing through the send path.
func fullOutChanManager(t *testing.T) *SyncMockManager {
	t.Helper()
	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	// Fill the single slot so every subsequent send hits the drop branch.
	ch <- &models.Mock{}
	return mgr
}

func newMock() *models.Mock {
	return &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
}

// TestCapacityDropRecordsOwningTCPrecisely proves the core contract: a
// capacity-dropped mock records its OWNER by exact name, so record.go can
// suppress precisely that TC — never a concurrent peer, and never an
// anonymous (owner="") send.
func TestCapacityDropRecordsOwningTCPrecisely(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	// A single owned send that overflows past the budget.
	mgr.sendToOutChanOwned(newMock(), "test-7")

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected DropCount()==1 after a forced capacity drop, got %d", got)
	}
	if !mgr.WasMockDroppedForTC("test-7") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-7\")==true; the owning TC must be recorded for suppression")
	}
	if mgr.WasMockDroppedForTC("test-8") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-8\")==false; a concurrent peer TC must NOT be over-suppressed")
	}
	if mgr.WasMockDroppedForTC("") {
		t.Fatalf("expected WasMockDroppedForTC(\"\")==false; the empty owner must never be recorded")
	}
	if got := mgr.DroppedTCCount(); got != 1 {
		t.Fatalf("expected DroppedTCCount()==1, got %d", got)
	}
}

// TestCapacityDropOwnerEmptyRecordsNothing pins that an anonymous send
// (owner="") that is capacity-dropped bumps the raw drop counter but records
// NO TC name — session/connection/startup/anonymous carve-outs must never
// suppress a TC.
func TestCapacityDropOwnerEmptyRecordsNothing(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	mgr.sendToOutChanOwned(newMock(), "")

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected DropCount()==1, got %d", got)
	}
	if got := mgr.DroppedTCCount(); got != 0 {
		t.Fatalf("expected DroppedTCCount()==0 for an owner=\"\" drop, got %d", got)
	}
}

// TestCapacityDropOnClosedChannelRecordsOwner covers the second drop site: a
// send to an already-closed outChan is a genuine, undeliverable drop and must
// record the owner (else the TC reaches replay mock-less).
func TestCapacityDropOnClosedChannelRecordsOwner(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.CloseOutChan() // outChanClosed = true

	mgr.sendToOutChanOwned(newMock(), "test-closed")

	if !mgr.WasMockDroppedForTC("test-closed") {
		t.Fatalf("a send to a closed outChan must record the owning TC for suppression")
	}
	// The closed-channel path is an early return, not the budget branch, so
	// DropCount() (budget drops only) stays 0 — the owner ledger is the
	// signal here. Pin that contract explicitly: a closed-channel drop must
	// NOT touch the send-budget drop counter.
	if got := mgr.DropCount(); got != 0 {
		t.Fatalf("closed-channel drop is an early return, not the budget branch: expected DropCount()==0, got %d", got)
	}
	if mgr.WasMockDroppedForTC("test-other") {
		t.Fatalf("closed-channel drop must not record an unrelated TC")
	}
}

// TestCapacityDropCountBoundedFIFO inserts more than maxDroppedTCNames
// distinct owners via forced drops and asserts the set is count-bounded FIFO:
// exactly maxDroppedTCNames retained, the oldest evicted, the newest kept.
// Drives recordDroppedTC directly to avoid paying maxDroppedTCNames*sendBudget
// of wall-clock — the eviction logic under test lives entirely in
// recordDroppedTC and the forced-drop path is already covered above.
func TestCapacityDropCountBoundedFIFO(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{}

	const extra = 64
	total := maxDroppedTCNames + extra
	for i := 0; i < total; i++ {
		mgr.recordDroppedTC(fmt.Sprintf("tc-%d", i))
	}

	if got := mgr.DroppedTCCount(); got != maxDroppedTCNames {
		t.Fatalf("expected DroppedTCCount()==%d (count-bounded), got %d", maxDroppedTCNames, got)
	}

	// The oldest `extra` names must have been evicted...
	for i := 0; i < extra; i++ {
		name := fmt.Sprintf("tc-%d", i)
		if mgr.WasMockDroppedForTC(name) {
			t.Fatalf("expected oldest owner %q to be evicted under the count cap", name)
		}
	}
	// ...and the newest maxDroppedTCNames retained.
	for i := extra; i < total; i++ {
		name := fmt.Sprintf("tc-%d", i)
		if !mgr.WasMockDroppedForTC(name) {
			t.Fatalf("expected newest owner %q to be retained under the count cap", name)
		}
	}
}

// TestRecordDroppedTCIdempotent pins that recording the same owner twice does
// not grow the set or the FIFO order (dedup), so a TC that loses several mocks
// counts once.
func TestRecordDroppedTCIdempotent(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{}
	mgr.recordDroppedTC("dup")
	mgr.recordDroppedTC("dup")
	mgr.recordDroppedTC("dup")

	if got := mgr.DroppedTCCount(); got != 1 {
		t.Fatalf("expected DroppedTCCount()==1 for a repeated owner, got %d", got)
	}
	mgr.droppedMu.Lock()
	orderLen := len(mgr.droppedTCOrder)
	mgr.droppedMu.Unlock()
	if orderLen != 1 {
		t.Fatalf("expected droppedTCOrder len==1 for a repeated owner, got %d", orderLen)
	}
}

// TestCapacityDropConcurrentNoRaceNoDeadlock drives N goroutines that force
// capacity drops with distinct owners into a full, undrained outChan, while a
// second set of goroutines concurrently reads WasMockDroppedForTC. Run under
// -race it must stay clean and must not deadlock (the leaf-lock ordering:
// droppedMu is only ever taken while holding outChanMu.RLock, and takes no
// other lock).
func TestCapacityDropConcurrentNoRaceNoDeadlock(t *testing.T) {
	t.Parallel()

	// A never-drained, capacity-1 channel: after it is primed full, every
	// send falls to the budget branch and drops. Use a short-lived manager
	// with the real sendBudget — keep N small so total wall-clock is bounded
	// (the sends run concurrently, so ~sendBudget total, not N*sendBudget).
	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	ch <- &models.Mock{} // prime full

	const writers = 16
	const readers = 8
	var writerWG, readerWG sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < writers; i++ {
		writerWG.Add(1)
		go func(id int) {
			defer writerWG.Done()
			mgr.sendToOutChanOwned(newMock(), fmt.Sprintf("owner-%d", id))
		}(i)
	}

	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func(id int) {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = mgr.WasMockDroppedForTC(fmt.Sprintf("owner-%d", id))
					_ = mgr.DroppedTCCount()
				}
			}
		}(i)
	}

	// Join writers first (each takes one bounded-block drop, all concurrent
	// so ~sendBudget total), then stop and join the readers.
	writerWG.Wait()
	close(stop)
	readerWG.Wait()

	if got := mgr.DroppedTCCount(); got != writers {
		t.Fatalf("expected %d distinct owners recorded, got %d", writers, got)
	}
	for i := 0; i < writers; i++ {
		if !mgr.WasMockDroppedForTC(fmt.Sprintf("owner-%d", i)) {
			t.Fatalf("owner-%d was not recorded after concurrent forced drops", i)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end owner-threading through the three send loops.
//
// The tests above drive sendToOutChanOwned / recordDroppedTC directly. The
// tests below close the remaining gap: that ResolveRange, FlushOwnedWindows,
// and DeleteMocksStrictlyBefore each thread the CORRECT owner into that send
// path — a per-test mock's real test name on its owned branch, and "" on the
// session/connection/startup carve-outs. A future edit that mis-tags a branch
// (owned → "" orphans the TC; carve-out → testName over-suppresses) currently
// passes every test; these fail on it. Each is mutation-verified in the task
// report by flipping the branch's owner tag and observing red→revert→green.
// ---------------------------------------------------------------------------

// TestResolveRangeInWindowDropAttributesOwner exercises the ResolveRange
// in-window owned branch end-to-end: a per-test mock whose ReqTimestampMock
// lands inside [start,end] is matched (keep==true, outChan bound), tagged with
// the resolving test's name, then capacity-dropped on a full outChan. The
// owning TC must be recorded so record.go suppresses exactly it. Guards the
// `owner: testName` tag at the ResolveRange in-window append — flip it to
// owner:"" and this test goes red.
func TestResolveRangeInWindowDropAttributesOwner(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	now := time.Now()
	start := now.Add(-2 * time.Second)
	end := now
	// Per-test mock (zero Lifetime == LifetimePerTest, not startup) whose
	// timestamp is strictly inside the window so the direct [start,end] match
	// claims it on the owned branch.
	inWindow := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(-time.Second)},
	}
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, inWindow)
	mgr.mu.Unlock()

	mgr.ResolveRange(start, end, "test-42", true, false)

	// The mock was genuinely sent and dropped on the budget branch...
	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the in-window mock to be sent and capacity-dropped (DropCount()==1), got %d", got)
	}
	// ...and its owning TC recorded for precise suppression.
	if !mgr.WasMockDroppedForTC("test-42") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-42\")==true; the in-window owned mock dropped, so its TC must be suppressed")
	}
	if mgr.WasMockDroppedForTC("test-99") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-99\")==false; a non-owning TC must never be over-suppressed")
	}
	if got := mgr.DroppedTCCount(); got != 1 {
		t.Fatalf("expected exactly one distinct owner recorded, got %d", got)
	}
}

// TestResolveRangeSessionCarveOutRecordsNothing exercises the ResolveRange
// lifetime carve-out: a session-scoped mock is reusable across every test and
// must NEVER suppress a TC even when it is itself capacity-dropped. It is
// tagged owner="" on the carve-out branch, so a drop bumps DropCount but
// records no TC. Guards `ownedMock{mock: mock}` (no owner) at the lifetime
// carve-out append — tag it with testName and DroppedTCCount()==0 goes to 1.
func TestResolveRangeSessionCarveOutRecordsNothing(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	now := time.Now()
	start := now.Add(-2 * time.Second)
	end := now
	// Session-lifetime mock, timestamp in-window (irrelevant — the lifetime
	// carve-out fires before any window match). Seeded directly so the
	// Lifetime we set survives (bypassing AddMock's DeriveLifetime).
	sessionMock := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(-time.Second)},
	}
	sessionMock.TestModeInfo.Lifetime = models.LifetimeSession
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, sessionMock)
	mgr.mu.Unlock()

	mgr.ResolveRange(start, end, "test-42", true, false)

	// It WAS sent and dropped (proves the carve-out reached the send path)...
	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the session mock to be flushed and capacity-dropped (DropCount()==1), got %d", got)
	}
	// ...yet no TC was recorded — a session mock must never suppress a test.
	if got := mgr.DroppedTCCount(); got != 0 {
		t.Fatalf("expected DroppedTCCount()==0 for a dropped session mock; a session/connection carve-out must never suppress a TC, got %d", got)
	}
	if mgr.WasMockDroppedForTC("test-42") {
		t.Fatalf("a dropped session mock must not suppress test-42")
	}
}

// TestFlushOwnedWindowsDropAttributesOwner exercises the FlushOwnedWindows
// owner-window branch end-to-end. A prior ResolveRange registers test-7's
// window in recentWindows (the real, documented registration path); a late
// mock whose timestamp falls inside it is then flushed on the ticker path,
// tagged with test-7, and capacity-dropped on a full outChan. The owning TC
// must be recorded. Guards `owner: w.testName` at the FlushOwnedWindows
// ownerWindow append — flip it to owner:"" and this test goes red.
func TestFlushOwnedWindowsDropAttributesOwner(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	now := time.Now()
	winStart := now.Add(-2 * time.Second)
	winEnd := now
	// Register test-7's resolved window via a real ResolveRange with an empty
	// buffer: it appends {winStart,winEnd,"test-7"} to recentWindows and sends
	// nothing (no budget paid here).
	mgr.ResolveRange(winStart, winEnd, "test-7", true, false)

	// A late per-test mock (not session, not startup) whose ReqTimestampMock
	// falls inside test-7's now-resolved window — only the ownerWindow branch
	// can claim it, so the test tightly guards that branch's owner tag.
	lateMock := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(-time.Second)},
	}
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, lateMock)
	mgr.mu.Unlock()

	mgr.FlushOwnedWindows()

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the late owner-window mock to be flushed and capacity-dropped (DropCount()==1), got %d", got)
	}
	if !mgr.WasMockDroppedForTC("test-7") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-7\")==true; a late mock flushed into test-7's window then dropped must suppress test-7")
	}
	if got := mgr.DroppedTCCount(); got != 1 {
		t.Fatalf("expected exactly one distinct owner recorded, got %d", got)
	}
}

// TestFlushOwnedWindowsStartupCarveOutRecordsNothing is the negative control
// for FlushOwnedWindows: a startup mock owns no test, rides the startup-rescue
// branch tagged owner="", and must record nothing even when capacity-dropped.
// Guards against the startup rescue being mis-tagged with a real test name.
func TestFlushOwnedWindowsStartupCarveOutRecordsNothing(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	// A startup mock owns no resolved window (recentWindows is empty), so it
	// falls to the startup-rescue branch. Timestamp is arbitrary.
	startupMock := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: time.Now()},
	}
	startupMock.TestModeInfo.IsStartup = true
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, startupMock)
	mgr.mu.Unlock()

	mgr.FlushOwnedWindows()

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the startup mock to be flushed and capacity-dropped (DropCount()==1), got %d", got)
	}
	if got := mgr.DroppedTCCount(); got != 0 {
		t.Fatalf("expected DroppedTCCount()==0 for a dropped startup mock; the startup carve-out must never suppress a TC, got %d", got)
	}
}

// TestDeleteMocksStrictlyBeforeDropAttributesOwner exercises the
// DeleteMocksStrictlyBefore owner-window RESCUE branch end-to-end. A prior
// ResolveRange registers test-3's window; a late kept mock whose timestamp is
// before the cleanup horizon AND inside test-3's window is rescued (not
// deleted as duplicate debris), tagged with test-3, and capacity-dropped on a
// full outChan. The owning TC must be recorded. Guards `owner: w.testName` at
// the DeleteMocksStrictlyBefore rescue append — flip it to owner:"" → red.
func TestDeleteMocksStrictlyBeforeDropAttributesOwner(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	now := time.Now()
	winStart := now.Add(-3 * time.Second)
	winEnd := now.Add(-1 * time.Second)
	// Register test-3's resolved window (empty buffer → no send).
	mgr.ResolveRange(winStart, winEnd, "test-3", true, false)

	// Late kept mock: before the cleanup horizon (now) AND inside test-3's
	// window, not session, not startup — only the ownerWindow rescue can claim
	// it, so this tightly guards that branch's owner tag.
	lateMock := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(-2 * time.Second)},
	}
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, lateMock)
	mgr.mu.Unlock()

	mgr.DeleteMocksStrictlyBefore(now)

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the rescued owner-window mock to be flushed and capacity-dropped (DropCount()==1), got %d", got)
	}
	if !mgr.WasMockDroppedForTC("test-3") {
		t.Fatalf("expected WasMockDroppedForTC(\"test-3\")==true; a late kept mock rescued into test-3's window then dropped must suppress test-3")
	}
	if got := mgr.DroppedTCCount(); got != 1 {
		t.Fatalf("expected exactly one distinct owner recorded, got %d", got)
	}
}

// TestDeleteMocksStrictlyBeforeSessionCarveOutRecordsNothing is the negative
// control for DeleteMocksStrictlyBefore: a session mock before the horizon is
// flushed on the lifetime carve-out tagged owner="", and must record nothing
// when capacity-dropped. Guards against that carve-out being mis-tagged with a
// real test name.
func TestDeleteMocksStrictlyBeforeSessionCarveOutRecordsNothing(t *testing.T) {
	t.Parallel()

	mgr := fullOutChanManager(t)

	now := time.Now()
	// Session mock before the cleanup horizon → lifetime carve-out flush,
	// owner "".
	sessionMock := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(-2 * time.Second)},
	}
	sessionMock.TestModeInfo.Lifetime = models.LifetimeConnection
	sessionMock.Spec.Metadata = map[string]string{"connID": "c-1"}
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, sessionMock)
	mgr.mu.Unlock()

	mgr.DeleteMocksStrictlyBefore(now)

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected the connection mock to be flushed and capacity-dropped (DropCount()==1), got %d", got)
	}
	if got := mgr.DroppedTCCount(); got != 0 {
		t.Fatalf("expected DroppedTCCount()==0 for a dropped connection mock; a session/connection carve-out must never suppress a TC, got %d", got)
	}
}
