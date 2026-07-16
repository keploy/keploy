package supervisor

import (
	"context"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// TestEmitMockIncompleteRecordsOrphanWindow is the root-cause repro for the
// go-memory-load-mongo no_mocks flake. When a parser marks its mock incomplete
// (reassembly overflow, a decode error on the post-pressure realignment tail,
// per-conn cap, short write) the NEXT emitMockCore drops the mock SILENTLY —
// and before this fix it told NO ONE, so the test case that owns the operation
// still streamed to the CLI and reached replay mock-less (match_phase=no_mocks).
// The fix records the dropped mock's wire window as an orphan window so
// record.go suppresses the owning TC. This drives the real EmitMock path.
func TestEmitMockIncompleteRecordsOrphanWindow(t *testing.T) {
	t.Parallel()
	mgr := &syncMock.SyncMockManager{}
	ch := make(chan *models.Mock, 1)
	base := time.Now()
	reqTs := base.Add(10 * time.Millisecond)
	resTs := base.Add(30 * time.Millisecond)
	sess := &Session{
		Mocks:  ch,
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
		Mgr:    mgr,
	}

	// Precondition (the bug): with no orphan window recorded, a TC whose HTTP
	// window spans the operation is NOT suppressible — it would reach replay.
	if ok, _ := mgr.WasMockOrphanedInWindow(base, base.Add(50*time.Millisecond)); ok {
		t.Fatalf("no orphan window should exist before any incomplete drop")
	}

	// A mock is voided by the incomplete flag; its wire window is [reqTs, resTs].
	sess.MarkMockIncomplete("request decode error")
	dropped := &models.Mock{
		Name: "find-customers",
		Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: resTs},
	}
	if err := sess.EmitMock(dropped); err != nil {
		t.Fatalf("EmitMock returned err: %v", err)
	}

	// The mock itself must NOT have leaked to the sink (still dropped).
	select {
	case got := <-ch:
		t.Fatalf("incomplete mock leaked to channel: %v", got.Name)
	default:
	}

	// The fix: the owning TC (HTTP window overlapping [reqTs, resTs]) is now
	// suppressible via the recorded orphan window.
	if ok, cnt := mgr.WasMockOrphanedInWindow(base, base.Add(50*time.Millisecond)); !ok || cnt != 1 {
		t.Fatalf("incomplete drop must record an orphan window over the mock's wire window; got ok=%v cnt=%d", ok, cnt)
	}
	// A concurrent TC that does NOT overlap the voided op is not suppressed.
	if ok, _ := mgr.WasMockOrphanedInWindow(base.Add(100*time.Millisecond), base.Add(200*time.Millisecond)); ok {
		t.Fatalf("a TC window not overlapping the voided op must not be suppressed")
	}
}

// TestEmitMockIncompleteSkipsSessionLifetimeMock pins the guard that a voided
// SESSION/CONNECTION mock (a reusable mongo handshake/heartbeat, served from
// the session tier at replay regardless of the drop) does NOT record an orphan
// window — recording it would only risk over-suppressing a healthy per-test TC
// that overlaps its window, since a stale mockIncomplete flag can void a
// reusable mock that belongs to no test at all.
func TestEmitMockIncompleteSkipsSessionLifetimeMock(t *testing.T) {
	t.Parallel()
	for _, lt := range []models.Lifetime{models.LifetimeSession, models.LifetimeConnection} {
		mgr := &syncMock.SyncMockManager{}
		sess := &Session{
			Mocks:  make(chan *models.Mock, 1),
			Ctx:    context.Background(),
			Logger: zaptest.NewLogger(t),
			Mgr:    mgr,
		}
		sess.MarkMockIncomplete("memory_pressure")
		reusable := &models.Mock{
			Name: "hello-heartbeat",
			Spec: models.MockSpec{
				ReqTimestampMock: time.Now(),
				ResTimestampMock: time.Now().Add(time.Millisecond),
			},
			TestModeInfo: models.TestModeInfo{Lifetime: lt},
		}
		if err := sess.EmitMock(reusable); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
		if got := mgr.OrphanRangeCount(); got != 0 {
			t.Fatalf("a voided %v mock must NOT record an orphan window; count=%d", lt, got)
		}
	}
}

// TestEmitMockHealthyRecordsNoOrphanWindow is the control: a normal emit (no
// incomplete flag) must NOT record an orphan window, so healthy TCs are never
// spuriously suppressed.
func TestEmitMockHealthyRecordsNoOrphanWindow(t *testing.T) {
	t.Parallel()
	mgr := &syncMock.SyncMockManager{}
	ch := make(chan *models.Mock, 1)
	sess := &Session{
		Mocks:  ch,
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
		Mgr:    mgr,
	}
	// RouteMocksViaSyncMock is false → direct-channel emit; the mock delivers
	// and NO orphan window is recorded.
	m := &models.Mock{Name: "ok", Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	if err := sess.EmitMock(m); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if got := mgr.OrphanRangeCount(); got != 0 {
		t.Fatalf("healthy emit must not record an orphan window; count=%d", got)
	}
	select {
	case got := <-ch:
		if got.Name != "ok" {
			t.Fatalf("wrong mock delivered: %v", got.Name)
		}
	default:
		t.Fatalf("healthy mock was not delivered")
	}
}

// TestRecordOrphanWindowPublicPath covers the no-mock-built case: a parser that
// voids an operation BEFORE any mock is emitted (reassembly overflow, decode
// error) reports the operation's own wire window via Session.RecordOrphanWindow,
// making that TC suppressible even though emitMockCore never ran for it. Also
// pins the nil-session and zero-start guards are no-ops.
func TestRecordOrphanWindowPublicPath(t *testing.T) {
	t.Parallel()
	mgr := &syncMock.SyncMockManager{}
	sess := &Session{Ctx: context.Background(), Mgr: mgr}

	opStart := time.Now()
	opEnd := opStart.Add(5 * time.Millisecond)
	sess.RecordOrphanWindow(opStart, opEnd)

	if ok, _ := mgr.WasMockOrphanedInWindow(opStart.Add(-time.Millisecond), opEnd.Add(time.Millisecond)); !ok {
		t.Fatalf("RecordOrphanWindow must make the failing op's TC suppressible")
	}

	// Guards: nil session and zero-start are no-ops (must not panic or record).
	var nilSess *Session
	nilSess.RecordOrphanWindow(opStart, opEnd)
	sess.RecordOrphanWindow(time.Time{}, opEnd)
	if got := mgr.OrphanRangeCount(); got != 1 {
		t.Fatalf("guards must be no-ops; expected exactly 1 window, got %d", got)
	}
}
