package manager

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// pendingRevokesSnapshot returns a copy of m.pendingRevokes under the leaf
// lock, so a test can assert on the queue without racing the drain path.
func pendingRevokesSnapshot(m *SyncMockManager) []string {
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	out := make([]string, len(m.pendingRevokes))
	copy(out, m.pendingRevokes)
	return out
}

// containsName reports whether the comma-joined revoked_tests payload carries
// the exact TC name (split-and-membership, not a substring test — so "test-1"
// is not spuriously matched by "test-10").
func containsName(payload, name string) bool {
	for _, n := range strings.Split(payload, ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}

// TestRevokeDisabledQueuesAndSendsNothing is case (a): with revokeCapable=false
// (an older CLI, or the default), recordDroppedTC must queue NOTHING and
// drainPendingRevokes must emit NOTHING — the version-skew guard for the
// deferred-orphan revoke protocol.
func TestRevokeDisabledQueuesAndSendsNothing(t *testing.T) {
	t.Parallel()

	// A working, drainable outChan so a stray send would be observable.
	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	// revokeCapable left at its zero value (false) — do NOT call SetRevokeCapable.

	mgr.recordDroppedTC("test-1")

	if got := pendingRevokesSnapshot(mgr); len(got) != 0 {
		t.Fatalf("revokeCapable=false: recordDroppedTC must queue nothing, got pendingRevokes=%v", got)
	}
	// The drop itself is still recorded for stream-time suppression — only the
	// LIVE revoke emission is gated. Sanity-check that the suppression ledger is
	// unaffected by the capability flag.
	if !mgr.WasMockDroppedForTC("test-1") {
		t.Fatalf("revokeCapable=false must NOT disable the suppression ledger; test-1 should still be recorded")
	}

	mgr.drainPendingRevokes()

	if got := len(ch); got != 0 {
		t.Fatalf("revokeCapable=false: drainPendingRevokes must send nothing, got %d frame(s) on outChan", got)
	}
}

// TestRevokeEnabledDeliversOneFrame is case (b): with revokeCapable=true, a
// drop queues the owner, and drainPendingRevokes into a working outChan
// delivers exactly ONE RevokedTests control frame whose
// Metadata["revoked_tests"] carries the name, and empties pendingRevokes.
func TestRevokeEnabledDeliversOneFrame(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetRevokeCapable(true)

	mgr.recordDroppedTC("test-5")

	if got := pendingRevokesSnapshot(mgr); len(got) != 1 || got[0] != "test-5" {
		t.Fatalf("revokeCapable=true: a drop must queue the owner, got pendingRevokes=%v", got)
	}

	mgr.drainPendingRevokes()

	// Exactly one frame delivered.
	if got := len(ch); got != 1 {
		t.Fatalf("expected exactly ONE revoke frame delivered, got %d", got)
	}
	frame := <-ch
	if frame.Kind != models.RevokedTests {
		t.Fatalf("expected a Kind=%q control frame, got %q", models.RevokedTests, frame.Kind)
	}
	if frame.Spec.Metadata == nil {
		t.Fatalf("revoke frame must carry Spec.Metadata")
	}
	if payload := frame.Spec.Metadata["revoked_tests"]; !containsName(payload, "test-5") {
		t.Fatalf("revoke frame Metadata[\"revoked_tests\"]=%q must contain \"test-5\"", payload)
	}

	// The queue must be empty after a successful drain.
	if got := pendingRevokesSnapshot(mgr); len(got) != 0 {
		t.Fatalf("drainPendingRevokes must empty pendingRevokes on delivery, got %v", got)
	}
}

// TestRevokeBatchesMultipleNamesInOneFrame pins that a batch of queued names is
// carried in a SINGLE comma-joined frame (one frame per drain, not one per
// name) — the wire-efficiency contract the comma-join relies on.
func TestRevokeBatchesMultipleNamesInOneFrame(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetRevokeCapable(true)

	names := []string{"test-1", "test-2", "test-3"}
	for _, n := range names {
		mgr.recordDroppedTC(n)
	}

	mgr.drainPendingRevokes()

	if got := len(ch); got != 1 {
		t.Fatalf("expected ONE batched revoke frame for %d names, got %d frame(s)", len(names), got)
	}
	frame := <-ch
	for _, n := range names {
		if payload := frame.Spec.Metadata["revoked_tests"]; !containsName(payload, n) {
			t.Fatalf("batched revoke frame Metadata[\"revoked_tests\"]=%q missing %q", payload, n)
		}
	}
}

// TestRevokeUndeliverableReQueues is case (c): when trySendControlFrame cannot
// deliver (a full outChan here), the batch must be RE-QUEUED, not lost, so a
// later tick can retry while the stream is still open.
func TestRevokeUndeliverableReQueues(t *testing.T) {
	t.Parallel()

	// Capacity-1 channel primed full so the non-blocking control send fails.
	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetRevokeCapable(true)
	ch <- &models.Mock{} // prime full → trySendControlFrame takes the default (drop) branch

	mgr.recordDroppedTC("test-9")
	mgr.drainPendingRevokes()

	// The name must survive for the next tick.
	if got := pendingRevokesSnapshot(mgr); len(got) != 1 || got[0] != "test-9" {
		t.Fatalf("undeliverable revoke must be re-queued, got pendingRevokes=%v", got)
	}

	// Drain the primed slot, then a second drain must now deliver the frame.
	<-ch
	mgr.drainPendingRevokes()
	if got := len(ch); got != 1 {
		t.Fatalf("after the outChan drained, the re-queued revoke must deliver on the next drain, got %d", got)
	}
	frame := <-ch
	if frame.Kind != models.RevokedTests || !containsName(frame.Spec.Metadata["revoked_tests"], "test-9") {
		t.Fatalf("re-delivered frame wrong: kind=%q payload=%q", frame.Kind, frame.Spec.Metadata["revoked_tests"])
	}
	if got := pendingRevokesSnapshot(mgr); len(got) != 0 {
		t.Fatalf("pendingRevokes must be empty after the retried delivery, got %v", got)
	}
}

// TestRevokeConcurrentDrainCloseNoDeadlock is case (d): it drives drops through
// the REAL send path so a writer goroutine actually holds outChanMu.RLock and
// THEN takes droppedMu (inside recordDroppedTC) — the exact ordering the drain
// must never invert. A saturated, never-drained capacity-1 outChan forces every
// sendToOutChanOwned to the send-budget drop; drainPendingRevokes (snapshot
// under droppedMu, RELEASE, then trySendControlFrame's outChanMu.RLock) and a
// racing CloseOutChan (outChanMu.Lock) run concurrently. Under -race this must
// stay clean and must NOT deadlock — had someone held droppedMu across the send,
// the outChanMu.RLock→droppedMu vs droppedMu→outChanMu.RLock cycle would form
// here (writer-preferring RWMutex + a pending CloseOutChan writer).
func TestRevokeConcurrentDrainCloseNoDeadlock(t *testing.T) {
	t.Parallel()

	// Capacity-1, primed full, NEVER drained: every sendToOutChanOwned exhausts
	// its send budget and drops, so the writer holds outChanMu.RLock across the
	// select and then takes droppedMu inside recordDroppedTC.
	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetRevokeCapable(true)
	ch <- &models.Mock{} // prime full

	const writers = 16
	const drainers = 8
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Real send path: outChanMu.RLock held across the select, then
			// recordDroppedTC (droppedMu) on the budget drop.
			mgr.sendToOutChanOwned(&models.Mock{}, fmt.Sprintf("owner-%d", id))
		}(i)
	}
	for i := 0; i < drainers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				mgr.drainPendingRevokes()
			}
		}()
	}
	// One closer racing the drains — CloseOutChan runs FlushOwnedWindows then
	// seals the channel under outChanMu.Lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr.CloseOutChan()
	}()

	// Fail loudly on a deadlock rather than hanging the whole suite.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent sendToOutChanOwned + drainPendingRevokes + CloseOutChan deadlocked")
	}

	// A final post-close drain must be safe (trySendControlFrame sees the
	// closed channel and re-queues; no send-on-closed panic).
	mgr.drainPendingRevokes()
}
