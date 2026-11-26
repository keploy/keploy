package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMockManager_SetFilteredMocksWithWindowRetainsUnfilteredAndHitIdx(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	sessHTTP := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "sess-http",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: base.Add(-10 * time.Second),
			ResTimestampMock: base.Add(-10*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true},
	}
	sessDNS := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "sess-dns",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: base.Add(-9 * time.Second),
			ResTimestampMock: base.Add(-9*time.Second + time.Millisecond),
			DNSReq:           &models.DNSReq{Name: "api.internal.", Qtype: 1, Qclass: 1},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true},
	}
	pt1 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "pt-1",
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: base.Add(1 * time.Second),
			ResTimestampMock: base.Add(1*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	pt2 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "pt-2",
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: base.Add(11 * time.Second),
			ResTimestampMock: base.Add(11*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}

	// Initial staging publishes the unfiltered tier.
	mm.SetMocksWithWindow(nil, []*models.Mock{sessHTTP, sessDNS}, models.BaseTime, base.Add(time.Hour))
	if !mm.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() = false after initial staging, want true")
	}
	mm.treesMu.RLock()
	origUnfTree := mm.unfiltered
	origUnfByKind := mm.unfilteredByKind
	mm.treesMu.RUnlock()

	// Bump hit count on sess-http before per-test retained staging.
	mm.MarkMockAsUsed(*sessHTTP)

	// Test 1: SetFilteredMocksWithWindow keeps exact same unfiltered tree pointer.
	if !mm.SetFilteredMocksWithWindow([]*models.Mock{pt1}, base, base.Add(5*time.Second)) {
		t.Fatal("SetFilteredMocksWithWindow returned false, want true")
	}
	mm.treesMu.RLock()
	sameTree := mm.unfiltered == origUnfTree && len(mm.unfilteredByKind) == len(origUnfByKind)
	mm.treesMu.RUnlock()
	if !sameTree {
		t.Fatal("SetFilteredMocksWithWindow rebuilt unfiltered tree instead of retaining it")
	}

	// Verify GetStatelessMocks works for both DNS (indexed) and HTTP (per-kind scan).
	if _, gotDNS := mm.GetStatelessMocks(models.DNS, "api.internal"); len(gotDNS) != 1 || gotDNS[0].Name != "sess-dns" {
		t.Fatalf("GetStatelessMocks(DNS) = %v, want [sess-dns]", gotDNS)
	}
	if _, gotHTTP := mm.GetStatelessMocks(models.HTTP, "sess-http"); len(gotHTTP) != 1 || gotHTTP[0].Name != "sess-http" {
		t.Fatalf("GetStatelessMocks(HTTP) = %v, want [sess-http]", gotHTTP)
	}

	// Test 2: pt-1 must be evicted from hitIdx, pt-2 added, and sess-http count preserved.
	if !mm.SetFilteredMocksWithWindow([]*models.Mock{pt2}, base.Add(10*time.Second), base.Add(15*time.Second)) {
		t.Fatal("SetFilteredMocksWithWindow (test 2) returned false, want true")
	}
	mm.hitMu.RLock()
	sessHits := mm.hitIdx["sess-http"].Load()
	_, hasPT1 := mm.hitIdx["pt-1"]
	_, hasPT2 := mm.hitIdx["pt-2"]
	mm.hitMu.RUnlock()
	if sessHits != 1 {
		t.Fatalf("sess-http hit count = %d, want 1 (preserved across retained staging)", sessHits)
	}
	if hasPT1 || !hasPT2 {
		t.Fatalf("hitIdx per-test update wrong: hasPT1=%v hasPT2=%v, want false true", hasPT1, hasPT2)
	}

	// Non-retained staging with pt-1 must seed prevPerTestHitNames so the very
	// next retained staging evicts pt-1 from hitIdx.
	mm.SetMocksWithWindow([]*models.Mock{pt1.ShallowCopy()}, []*models.Mock{sessHTTP.ShallowCopy(), sessDNS.ShallowCopy()}, base, base.Add(5*time.Second))
	if !mm.SetFilteredMocksWithWindow([]*models.Mock{pt2.ShallowCopy()}, base.Add(10*time.Second), base.Add(15*time.Second)) {
		t.Fatal("SetFilteredMocksWithWindow after non-retained stage returned false, want true")
	}
	mm.hitMu.RLock()
	_, hasPT1After := mm.hitIdx["pt-1"]
	_, hasSessAfter := mm.hitIdx["sess-http"]
	mm.hitMu.RUnlock()
	if hasPT1After || !hasSessAfter {
		t.Fatalf("post-non-retained eviction wrong: hasPT1=%v hasSess=%v, want false true", hasPT1After, hasSessAfter)
	}

	// DeleteUnFilteredMock invalidates retention.
	unf, _ := mm.GetUnFilteredMocks()
	if len(unf) != 2 {
		t.Fatalf("unfiltered count = %d, want 2", len(unf))
	}
	if !mm.DeleteUnFilteredMock(*unf[0]) {
		t.Fatal("DeleteUnFilteredMock failed")
	}
	if mm.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() = true after DeleteUnFilteredMock, want false")
	}

	// TOCTOU check: calling SetFilteredMocksWithWindow or OpenTestFilteredWithWindow
	// after DeleteUnFilteredMock must decline (return false) WITHOUT wiping the
	// remaining unfiltered mock, clearing unfilteredDeleted, or binding variables.
	if mm.SetFilteredMocksWithWindow([]*models.Mock{pt1.ShallowCopy()}, base, base.Add(5*time.Second)) {
		t.Fatal("SetFilteredMocksWithWindow succeeded after DeleteUnFilteredMock, want false")
	}
	if mm.OpenTestFilteredWithWindow([]*models.Mock{pt1.ShallowCopy()}, base, base.Add(5*time.Second), []models.VariableBinding{{Name: "k", Value: "v"}}) {
		t.Fatal("OpenTestFilteredWithWindow succeeded after DeleteUnFilteredMock, want false")
	}
	if mm.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() flipped true after declined retained staging")
	}
	if rem, _ := mm.GetUnFilteredMocks(); len(rem) != 1 {
		t.Fatalf("declined retained staging wiped unfiltered pool: got %d, want 1 remaining", len(rem))
	}
	if bound := mm.BoundVariables(); len(bound) != 0 {
		t.Fatalf("declined OpenTestFilteredWithWindow mutated variables: got %v, want empty", bound)
	}

	// ResetForReplaySession also clears retention, and a racing
	// SetFilteredMocksWithWindow must decline without wiping m.unfiltered.
	mm.SetMocksWithWindow(nil, []*models.Mock{sessHTTP.ShallowCopy()}, models.BaseTime, base.Add(time.Hour))
	if !mm.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() = false after re-staging, want true")
	}
	mm.ResetForReplaySession()
	if mm.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() = true after ResetForReplaySession, want false")
	}
	if mm.SetFilteredMocksWithWindow([]*models.Mock{pt1.ShallowCopy()}, base, base.Add(5*time.Second)) {
		t.Fatal("SetFilteredMocksWithWindow succeeded after ResetForReplaySession, want false")
	}
	if rem, _ := mm.GetUnFilteredMocks(); len(rem) != 1 {
		t.Fatalf("declined retained staging after ResetForReplaySession wiped unfiltered pool: got %d, want 1", len(rem))
	}
}

// TestProxy_RetainingDeclinesAtomicallyWithWorkerManagers verifies that when
// multi-worker instance managers are registered in p.instances.mgrs,
// Proxy.CanRetainUnfiltered() returns false and both
// SetFilteredMocksWithWindow and OpenTestFilteredWithWindow return
// agent.ErrUnfilteredNotRetained with zero state mutation on the run's
// MockManager.
func TestProxy_RetainingDeclinesAtomicallyWithWorkerManagers(t *testing.T) {
	logger := zap.NewNop()
	mm := NewMockManager(nil, nil, logger)
	defer mm.Close()
	wm := NewMockManager(nil, nil, logger)
	defer wm.Close()

	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	sess := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "sess-1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: base.Add(-10 * time.Second),
			ResTimestampMock: base.Add(-10*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true},
	}
	origPT := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "pt-orig",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
			ReqTimestampMock: base.Add(time.Second),
			ResTimestampMock: base.Add(time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	newPT := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "pt-new",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
			ReqTimestampMock: base.Add(20 * time.Second),
			ResTimestampMock: base.Add(20*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}

	origStart, origEnd := base, base.Add(5*time.Second)
	mm.SetMocksWithWindow([]*models.Mock{origPT.ShallowCopy()}, []*models.Mock{sess.ShallowCopy()}, origStart, origEnd)
	wm.SetMocksWithWindow([]*models.Mock{origPT.ShallowCopy()}, []*models.Mock{sess.ShallowCopy()}, origStart, origEnd)

	p := &Proxy{
		logger:      logger,
		mockManager: mm,
	}
	p.instances.mgrs = map[uint32]*MockManager{1: wm}

	if p.CanRetainUnfiltered() {
		t.Fatal("CanRetainUnfiltered() = true with worker managers registered, want false")
	}

	ctx := context.Background()
	newStart, newEnd := base.Add(15*time.Second), base.Add(25*time.Second)
	if err := p.SetFilteredMocksWithWindow(ctx, []*models.Mock{newPT.ShallowCopy()}, newStart, newEnd); !errors.Is(err, agent.ErrUnfilteredNotRetained) {
		t.Fatalf("SetFilteredMocksWithWindow err = %v, want ErrUnfilteredNotRetained", err)
	}
	if err := p.OpenTestFilteredWithWindow(ctx, []*models.Mock{newPT.ShallowCopy()}, newStart, newEnd, []models.VariableBinding{{Name: "k", Value: "v"}}); !errors.Is(err, agent.ErrUnfilteredNotRetained) {
		t.Fatalf("OpenTestFilteredWithWindow err = %v, want ErrUnfilteredNotRetained", err)
	}

	// Verify ZERO state mutation on mm (filtered mocks, unfiltered mocks, window, variables).
	flt, _ := mm.GetFilteredMocks()
	if len(flt) != 1 || flt[0].Name != "pt-orig" {
		t.Fatalf("mm filtered mutated on declined retaining stage: got %v, want [pt-orig]", flt)
	}
	unf, _ := mm.GetUnFilteredMocks()
	if len(unf) != 1 || unf[0].Name != "sess-1" {
		t.Fatalf("mm unfiltered mutated on declined retaining stage: got %v, want [sess-1]", unf)
	}
	mm.windowMu.RLock()
	gotStart, gotEnd := mm.windowStart, mm.windowEnd
	mm.windowMu.RUnlock()
	if !gotStart.Equal(origStart) || !gotEnd.Equal(origEnd) {
		t.Fatalf("mm window mutated on declined retaining stage: got [%v,%v], want [%v,%v]", gotStart, gotEnd, origStart, origEnd)
	}
	if bound := mm.BoundVariables(); len(bound) != 0 {
		t.Fatalf("mm variables mutated on declined OpenTestFilteredWithWindow: got %v, want empty", bound)
	}
}
