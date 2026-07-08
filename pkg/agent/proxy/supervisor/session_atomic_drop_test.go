package supervisor

import (
	"context"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// TestEmitMock_IncompleteDropRecordsLedger is the write-half regression test for
// the atomic mock-drop / TC-drop fix (#4336). It proves that when emitMockCore
// abandons an outgoing mock because the session was marked incomplete — the
// exact path taken when the relay tee gates a mongo response chunk under memory
// pressure (DropMemoryPressure -> MarkMockIncomplete) — the mock's request
// timestamp is recorded in the sync-mock dropped-mock ledger, so the owning HTTP
// test case is later suppressed instead of orphaned at replay.
//
// Without the RecordDroppedMock call in emitMockCore, TCHasDroppedMock returns
// false here and the owning TC would be persisted with a missing mock (replay
// no_mocks / candidates:0). That is the load-bearing assertion.
func TestEmitMock_IncompleteDropRecordsLedger(t *testing.T) {
	t.Parallel()

	mgr := syncMock.New(zaptest.NewLogger(t))
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)

	var pendingCleared int
	mocksCh := make(chan *models.Mock, 1)
	sess := &Session{
		Mocks:            mocksCh,
		Ctx:              context.Background(),
		Logger:           zaptest.NewLogger(t),
		Mgr:              mgr,
		OnPendingCleared: func() { pendingCleared++ },
	}

	reqTs := time.Now()
	sess.MarkMockIncomplete("memory_pressure")
	if err := sess.EmitMock(&models.Mock{
		Name: "mongo-find-large",
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: reqTs},
	}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}

	// The mock must have been dropped (not forwarded to the direct channel).
	select {
	case m := <-mocksCh:
		t.Fatalf("incomplete mock must be dropped, but was emitted: %+v", m)
	default:
	}
	// And its drop must be recorded in the ledger so the owning TC is suppressed.
	if c := mgr.DroppedMockCount(); c != 1 {
		t.Fatalf("expected the incomplete-drop to be recorded, ledger count=%d", c)
	}
	if !mgr.TCHasDroppedMock(reqTs.Add(-time.Second), reqTs.Add(time.Second)) {
		t.Fatalf("expected the owning HTTP window to see the dropped mock")
	}
	if pendingCleared != 1 {
		t.Fatalf("OnPendingCleared calls on drop path = %d, want 1", pendingCleared)
	}
}

// TestEmitMock_SessionLifetimeDropNotRecorded is the precision-refinement
// regression test (#4336) at the emitMockCore write site: when a dropped mock is
// SESSION-lifetime (mongo handshake/heartbeat — tag "config"), its request
// timestamp must NOT enter the dropped-mock ledger, so a test case whose window
// happens to overlap it is NOT spuriously suppressed. Session mocks are reusable
// and not owned by any single TC window; only per-test drops orphan a specific
// test case. The mock is still dropped; only the ledger entry is skipped.
func TestEmitMock_SessionLifetimeDropNotRecorded(t *testing.T) {
	t.Parallel()

	mgr := syncMock.New(zaptest.NewLogger(t))
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)

	sess := &Session{
		Mocks:  make(chan *models.Mock, 1),
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
		Mgr:    mgr,
	}

	reqTs := time.Now()
	sess.MarkMockIncomplete("memory_pressure")
	if err := sess.EmitMock(&models.Mock{
		Name: "mongo-handshake",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: reqTs, ResTimestampMock: reqTs,
			Metadata: map[string]string{"type": "config"}, // -> LifetimeSession
		},
	}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}

	if c := mgr.DroppedMockCount(); c != 0 {
		t.Fatalf("a dropped session-lifetime mock must NOT enter the ledger, count=%d", c)
	}
	if mgr.TCHasDroppedMock(reqTs.Add(-time.Second), reqTs.Add(time.Second)) {
		t.Fatalf("a TC window overlapping a dropped session mock must NOT be suppressed")
	}
}

// TestEmitMock_IncompleteDropZeroTsIgnored proves a dropped mock with no request
// timestamp is NOT recorded (it can't be attributed to any TC window, so
// recording it could only cause spurious suppression). The drop itself still
// happens; only the ledger entry is skipped.
func TestEmitMock_IncompleteDropZeroTsIgnored(t *testing.T) {
	t.Parallel()

	mgr := syncMock.New(zaptest.NewLogger(t))
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)

	sess := &Session{
		Mocks:  make(chan *models.Mock, 1),
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
		Mgr:    mgr,
	}

	sess.MarkMockIncomplete("write_error")
	if err := sess.EmitMock(&models.Mock{Name: "no-ts"}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if c := mgr.DroppedMockCount(); c != 0 {
		t.Fatalf("expected zero-timestamp drop to be ignored by the ledger, count=%d", c)
	}
}
