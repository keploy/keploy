package proxy

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func stagedStartupTestFixture(t *testing.T, kind models.Kind) (*MockManager, time.Time, time.Time, time.Time, time.Time, time.Time) {
	t.Helper()
	unreg := models.RegisterStagingNeed(kind, models.StagingNeed{
		Use: models.StagingWithStartup,
		Match: func(m *models.Mock) bool {
			return m != nil && m.Spec.Metadata["type"] == "consume"
		},
	})
	t.Cleanup(unreg)

	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)

	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	w1Start, w1End := t0.Add(10*time.Second), t0.Add(12*time.Second)
	w2Start, w2End := t0.Add(20*time.Second), t0.Add(22*time.Second)
	mm.SeedStartupCutoff(w1Start)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-1", Start: w1Start, End: w1End},
		{TestCase: "test-2", Start: w2Start, End: w2End},
	})
	return mm, t0, w1Start, w1End, w2Start, w2End
}

func makeStagedStartupMock(name string, kind models.Kind, typ string, ts time.Time, lt models.Lifetime) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": typ},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Microsecond),
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        lt,
			LifetimeDerived: true,
		},
	}
}

func assertStagedStartupNames(t *testing.T, label string, db integrations.StagedStartupReader, kind models.Kind, want []string) {
	t.Helper()
	got, err := db.GetStagedStartupMocksByKind(kind)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", label, err)
	}
	if len(got) != len(want) {
		names := make([]string, len(got))
		for i, m := range got {
			names[i] = m.Name
		}
		t.Fatalf("%s: got %v, want %v", label, names, want)
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("%s: got[%d]=%q, want %q", label, i, got[i].Name, w)
		}
	}
}

func TestMockManager_StagedStartupReaderRetainsAcrossWindowRefilesAndResetsAcrossSets(t *testing.T) {
	const testKind models.Kind = "StagedStartupTestKind"
	const otherKind models.Kind = "StagedStartupOtherKind"

	mm, t0, w1Start, w1End, w2Start, w2End := stagedStartupTestFixture(t, testKind)

	in1 := makeStagedStartupMock("consume-in-1", testKind, "consume", w1Start.Add(100*time.Millisecond), models.LifetimePerTest)
	gap12 := makeStagedStartupMock("consume-gap-1-2", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	in2 := makeStagedStartupMock("consume-in-2", testKind, "consume", w2Start.Add(100*time.Millisecond), models.LifetimePerTest)
	gapAfter2 := makeStagedStartupMock("consume-gap-after-2", testKind, "consume", w2End.Add(2*time.Second), models.LifetimePerTest)
	pub := makeStagedStartupMock("publish-1", testKind, "publish", w1Start.Add(200*time.Millisecond), models.LifetimePerTest)
	cfg := makeStagedStartupMock("session-cfg", testKind, "consume", t0, models.LifetimeSession)
	other := makeStagedStartupMock("other-kind", otherKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)

	// Initial BaseTime staging populates stagedStartupByKind with ONLY
	// models.IsStagedAtStaging mocks of testKind.
	mm.SetMocksWithWindow(
		[]*models.Mock{in1, gap12, in2, gapAfter2, pub, other},
		[]*models.Mock{cfg},
		models.BaseTime,
		time.Now(),
	)

	wantAll := []string{"consume-in-1", "consume-gap-1-2", "consume-in-2", "consume-gap-after-2"}
	assertStagedStartupNames(t, "after initial staging", mm, testKind, wantAll)
	assertStagedStartupNames(t, "unregistered kind", mm, otherKind, nil)

	// Mutating the returned slice must not corrupt MockManager's copy.
	snap, err := mm.GetStagedStartupMocksByKind(testKind)
	if err != nil || len(snap) != len(wantAll) {
		t.Fatalf("snapshot failed: len=%d err=%v", len(snap), err)
	}
	snap[0] = nil
	assertStagedStartupNames(t, "after caller mutated returned slice", mm, testKind, wantAll)

	// Refile to test 1 and then test 2: GetStartupMocksByKind drops post-cutoff
	// mocks, while GetStagedStartupMocksByKind retains ALL declared mocks
	// (including gap pushes!) for lazy sessions that connect after test 1/2.
	mm.SetMocksWithWindow([]*models.Mock{in1}, []*models.Mock{cfg}, w1Start, w1End)
	assertStagedStartupNames(t, "after test 1 refile", mm, testKind, wantAll)

	mm.SetMocksWithWindow([]*models.Mock{in2}, []*models.Mock{cfg}, w2Start, w2End)
	assertStagedStartupNames(t, "after test 2 refile", mm, testKind, wantAll)

	// Verify startup tree itself really dropped the gap mocks.
	startupAfterT2, err := mm.GetStartupMocksByKind(testKind)
	if err != nil {
		t.Fatalf("GetStartupMocksByKind: %v", err)
	}
	for _, m := range startupAfterT2 {
		if m.Name == "consume-gap-1-2" || m.Name == "consume-gap-after-2" {
			t.Fatalf("unexpected gap mock %q in startup tree after test 2 refile", m.Name)
		}
	}

	// Verify wrappers (scopedMockDb and heldScope) delegate StagedStartupReader.
	scoped := &scopedMockDb{MockMemDb: mm}
	assertStagedStartupNames(t, "scopedMockDb after test 2", scoped, testKind, wantAll)

	held := &heldScope{roundScope: roundScope{
		MockManager: mm,
		g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
	}}
	assertStagedStartupNames(t, "heldScope after test 2", held, testKind, wantAll)

	// ResetForReplaySession clears stagedStartupByKind so a window-only staging
	// in a new replay session sees zero stale mocks from the previous set.
	mm.ResetForReplaySession()
	assertStagedStartupNames(t, "after ResetForReplaySession", mm, testKind, nil)
	mm.SetMocksWithWindow([]*models.Mock{in1}, nil, w1Start, w1End)
	assertStagedStartupNames(t, "window-only staging after reset", mm, testKind, nil)

	// Next set's initial staging replaces cleanly with the new set's mocks.
	set2Gap := makeStagedStartupMock("set2-gap", testKind, "consume", w1End.Add(3*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{set2Gap}, nil, models.BaseTime, time.Now())
	assertStagedStartupNames(t, "after set 2 initial staging", mm, testKind, []string{"set2-gap"})
}

func TestMockManager_StagedStartupReaderMidSetRestageWithNoDeclaredMocksClears(t *testing.T) {
	const testKind models.Kind = "StagedStartupRestageKind"

	mm, _, w1Start, w1End, w2Start, _ := stagedStartupTestFixture(t, testKind)

	gap1 := makeStagedStartupMock("a-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	in2 := makeStagedStartupMock("a-in2", testKind, "consume", w2Start.Add(100*time.Millisecond), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{gap1, in2}, nil, models.BaseTime, time.Now())
	assertStagedStartupNames(t, "initial BaseTime staging", mm, testKind, []string{"a-gap", "a-in2"})

	// A mid-set BaseTime restage (no ResetForReplaySession first, such as
	// RearmAppStart or replacement agent re-registration) that stages no
	// declared StagingWithStartup mocks must clear the previous staging's mocks.
	pubOnly := makeStagedStartupMock("b-pub", testKind, "publish", w1Start.Add(200*time.Millisecond), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{pubOnly}, nil, models.BaseTime, time.Now())
	assertStagedStartupNames(t, "mid-set BaseTime restage with no declared mock", mm, testKind, nil)

	// A subsequent BaseTime staging installs only its own declared mocks.
	gapC := makeStagedStartupMock("c-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{gapC}, nil, models.BaseTime, time.Now())
	assertStagedStartupNames(t, "subsequent BaseTime restage", mm, testKind, []string{"c-gap"})
}

func TestMockManager_StagedStartupReaderHeldScopeEmptyAfterStagingMoves(t *testing.T) {
	const testKind models.Kind = "StagedStartupHeldKind"

	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	gapA := makeStagedStartupMock("setA-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{gapA}, nil, models.BaseTime, time.Now())

	held := &heldScope{roundScope: roundScope{
		MockManager: mm,
		g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
	}}
	assertStagedStartupNames(t, "heldScope before staging moves", held, testKind, []string{"setA-gap"})

	// Move to the next test set's staging: the old held call's store must not
	// read the new set's staged startup mocks.
	mm.ResetForReplaySession()
	gapB := makeStagedStartupMock("setB-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{gapB}, nil, models.BaseTime, time.Now())

	assertStagedStartupNames(t, "heldScope after staging moved", held, testKind, nil)
	assertStagedStartupNames(t, "manager sees new staging", mm, testKind, []string{"setB-gap"})
}

func TestMockManager_StagedStartupReaderDeduplicatesAcrossThreeTiers(t *testing.T) {
	const testKind models.Kind = "StagedStartupDedupKind"

	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	shared := makeStagedStartupMock("dup-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindowThreeTier(
		[]*models.Mock{shared},
		[]*models.Mock{shared},
		[]*models.Mock{shared},
		models.BaseTime,
		time.Now(),
	)
	assertStagedStartupNames(t, "same pointer across filtered/unfiltered/explicitStartup", mm, testKind, []string{"dup-gap"})
}

func TestMockManager_StagedStartupReaderScopedMockDbAppliesWorkerFilter(t *testing.T) {
	const testKind models.Kind = "StagedStartupScopedKind"

	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	mine := makeStagedStartupMock("worker-mine", testKind, "consume", w1End.Add(1*time.Second), models.LifetimePerTest)
	other := makeStagedStartupMock("worker-other", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	shared := makeStagedStartupMock("shared-unmapped", testKind, "consume", w1End.Add(3*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{mine, other, shared}, nil, models.BaseTime, time.Now())

	scoped := workerScoped(t, mm, []string{"worker-mine"}, []string{"worker-mine", "worker-other"})
	assertStagedStartupNames(t, "worker-scoped view hides other worker's mock", scoped, testKind, []string{"worker-mine", "shared-unmapped"})

	// Consuming this worker's mock through the scoped view hides it from
	// subsequent scoped reads while the bare manager still retains it.
	if ok := scoped.DeleteFilteredMock(*mine); !ok {
		t.Fatalf("DeleteFilteredMock(worker-mine) returned false")
	}
	assertStagedStartupNames(t, "worker-scoped view hides consumed mock", scoped, testKind, []string{"shared-unmapped"})
	assertStagedStartupNames(t, "bare manager still retains declared mocks", mm, testKind, []string{"worker-mine", "worker-other", "shared-unmapped"})
}

func TestMockManager_StagedStartupReaderRetainsExplicitStartupAndUnfiltered(t *testing.T) {
	t.Run("explicitStartup_only", func(t *testing.T) {
		const testKind models.Kind = "StagedStartupExplicitKind"
		mm, _, w1Start, w1End, _, _ := stagedStartupTestFixture(t, testKind)

		onlyStartup := makeStagedStartupMock("explicit-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
		mm.SetMocksWithWindowThreeTier(nil, nil, []*models.Mock{onlyStartup}, models.BaseTime, time.Now())
		assertStagedStartupNames(t, "after initial three-tier staging", mm, testKind, []string{"explicit-gap"})

		mm.SetMocksWithWindow(nil, nil, w1Start, w1End)
		assertStagedStartupNames(t, "retained after window refile", mm, testKind, []string{"explicit-gap"})
	})

	t.Run("unfiltered_only", func(t *testing.T) {
		const testKind models.Kind = "StagedStartupUnfilteredKind"
		mm, _, w1Start, w1End, _, _ := stagedStartupTestFixture(t, testKind)

		onlyUnfiltered := makeStagedStartupMock("unfiltered-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
		mm.SetMocksWithWindowThreeTier(nil, []*models.Mock{onlyUnfiltered}, nil, models.BaseTime, time.Now())
		assertStagedStartupNames(t, "after initial three-tier staging", mm, testKind, []string{"unfiltered-gap"})

		mm.SetMocksWithWindow(nil, nil, w1Start, w1End)
		assertStagedStartupNames(t, "retained after window refile", mm, testKind, []string{"unfiltered-gap"})
	})
}

func TestMockManager_StagedStartupReaderGenerationTracksSetChanges(t *testing.T) {
	const testKind models.Kind = "StagedStartupGenKind"
	mm, _, w1Start, w1End, w2Start, w2End := stagedStartupTestFixture(t, testKind)

	g0 := mm.StagedStartupGeneration()
	in1 := makeStagedStartupMock("gen-in1", testKind, "consume", w1Start.Add(100*time.Millisecond), models.LifetimePerTest)
	gap := makeStagedStartupMock("gen-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)

	// BaseTime staging advances generation.
	mm.SetMocksWithWindow([]*models.Mock{in1, gap}, nil, models.BaseTime, time.Now())
	g1 := mm.StagedStartupGeneration()
	if g1 <= g0 {
		t.Fatalf("BaseTime staging did not advance generation: before=%d after=%d", g0, g1)
	}

	// Per-test window refiles keep generation constant.
	mm.SetMocksWithWindow([]*models.Mock{in1}, nil, w1Start, w1End)
	mm.SetMocksWithWindow(nil, nil, w2Start, w2End)
	if got := mm.StagedStartupGeneration(); got != g1 {
		t.Fatalf("window refiles changed generation: got %d, want %d", got, g1)
	}

	// SetScopeMocks(open=true) bumps StagingEpoch but does NOT replace
	// stagedStartupByKind, so StagedStartupGeneration stays constant.
	epochBeforeScope := mm.StagingEpoch()
	mm.SetScopeMocks([]*models.Mock{in1}, nil, true)
	if mm.StagingEpoch() == epochBeforeScope {
		t.Fatalf("expected SetScopeMocks(open=true) to advance StagingEpoch")
	}
	if got := mm.StagedStartupGeneration(); got != g1 {
		t.Fatalf("SetScopeMocks(open=true) changed generation: got %d, want %d", got, g1)
	}

	// SetScopeMocks(open=false) restages as BaseTime and advances generation.
	mm.SetScopeMocks([]*models.Mock{in1, gap}, nil, false)
	g2 := mm.StagedStartupGeneration()
	if g2 <= g1 {
		t.Fatalf("SetScopeMocks(open=false) did not advance generation: before=%d after=%d", g1, g2)
	}

	// Wrappers delegate generation; heldScope returns 0 once staging moves.
	scoped := &scopedMockDb{MockMemDb: mm}
	if got := scoped.StagedStartupGeneration(); got != g2 {
		t.Fatalf("scopedMockDb generation = %d, want %d", got, g2)
	}
	held := &heldScope{roundScope: roundScope{
		MockManager: mm,
		g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
	}}
	if got := held.StagedStartupGeneration(); got != g2 {
		t.Fatalf("heldScope generation before move = %d, want %d", got, g2)
	}

	// ResetForReplaySession clears stagedStartupByKind and advances generation
	// even though StagingEpoch itself does not advance.
	epochBeforeReset := mm.StagingEpoch()
	mm.ResetForReplaySession()
	g3 := mm.StagedStartupGeneration()
	if g3 <= g2 {
		t.Fatalf("ResetForReplaySession did not advance generation: before=%d after=%d (StagingEpoch %d->%d)", g2, g3, epochBeforeReset, mm.StagingEpoch())
	}

	// BaseTime staging with no declared mocks still advances generation.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	g4 := mm.StagedStartupGeneration()
	if g4 <= g3 {
		t.Fatalf("empty BaseTime restage did not advance generation: before=%d after=%d", g3, g4)
	}
	if got := held.StagedStartupGeneration(); got != integrations.RestagedGeneration || integrations.KeptGeneration(got) || got < g4 {
		t.Fatalf("heldScope generation after staging moved = %d, want RestagedGeneration (%d, non-keepable and >= %d)", got, integrations.RestagedGeneration, g4)
	}
}

func TestMockManager_StagedStartupReaderGenerationScopedWorkerViewChanges(t *testing.T) {
	const testKind models.Kind = "StagedStartupScopedGenKind"
	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	a := makeStagedStartupMock("a-1", testKind, "consume", w1End.Add(time.Second), models.LifetimePerTest)
	b := makeStagedStartupMock("b-1", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest)
	sh := makeStagedStartupMock("sh-1", testKind, "consume", w1End.Add(3*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{a, b, sh}, nil, models.BaseTime, time.Now())

	sample := func(db integrations.StagedStartupReader) (uint64, []string) {
		g := db.StagedStartupGeneration()
		got, err := db.GetStagedStartupMocksByKind(testKind)
		if err != nil {
			t.Fatalf("GetStagedStartupMocksByKind: %v", err)
		}
		names := make([]string, len(got))
		for i, m := range got {
			names[i] = m.Name
		}
		return g, names
	}

	self := uint32(os.Getpid())
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a-1", "b-1"})
	p.SetWorkerScope(self, []string{"a-1"})
	conn, ok := p.scopedFor(self, mm).(*scopedMockDb)
	if !ok {
		t.Fatal("expected *scopedMockDb from Proxy.scopedFor")
	}

	// Switching the worker to test 2 changes the visible set ([a-1 sh-1] -> [b-1 sh-1])
	// and must advance StagedStartupGeneration so a cache never serves stale pushes.
	g1, n1 := sample(conn)
	p.ClearWorkerScope(self)
	p.SetWorkerScope(self, []string{"b-1"})
	g2, n2 := sample(conn)
	if slices.Equal(n1, n2) {
		t.Fatalf("expected worker switch to change scoped view, both were %v", n1)
	}
	if g2 == g1 {
		t.Fatalf("worker switch changed scoped view %v -> %v without advancing StagedStartupGeneration (%d)", n1, n2, g1)
	}

	// Updating the mapped universe hides previously shared mocks and must also advance generation.
	p.SetMappedUniverse([]string{"a-1", "b-1", "sh-1"})
	g3, n3 := sample(conn)
	if slices.Equal(n2, n3) {
		t.Fatalf("expected mapped universe change to alter scoped view, both were %v", n2)
	}
	if g3 == g2 {
		t.Fatalf("mapped universe change altered scoped view %v -> %v without advancing StagedStartupGeneration (%d)", n2, n3, g2)
	}
}

func TestMockManager_StagedStartupReaderGenerationHeldViewGoneAndMoved(t *testing.T) {
	const testKind models.Kind = "StagedStartupHeldGenKind"
	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	mk := func(pfx string) []*models.Mock {
		return []*models.Mock{makeStagedStartupMock(pfx+"-gap", testKind, "consume", w1End.Add(time.Second), models.LifetimePerTest)}
	}
	mm.SetMocksWithWindow(mk("a"), nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow(mk("b"), nil, models.BaseTime, time.Now())

	t.Run("viewGone_moves_to_nonkeepable_sentinel", func(t *testing.T) {
		h := &heldScope{roundScope: roundScope{
			MockManager: mm,
			g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
		}}
		h.diagnosing = true
		h.heldIn = mm.exactView()
		g1 := h.StagedStartupGeneration()
		m1, err := h.GetStagedStartupMocksByKind(testKind)
		if err != nil || len(m1) != 1 {
			t.Fatalf("in-force held read: len=%d err=%v", len(m1), err)
		}
		h.heldIn = &exactView{}
		if !h.viewGone() {
			t.Fatal("expected viewGone() to be true after replacing heldIn")
		}
		g2 := h.StagedStartupGeneration()
		m2, err := h.GetStagedStartupMocksByKind(testKind)
		if err != nil || len(m2) != 0 {
			t.Fatalf("viewGone held read: len=%d err=%v, want 0", len(m2), err)
		}
		if g2 == g1 || integrations.KeptGeneration(g2) {
			t.Fatalf("viewGone StagedStartupGeneration = %d (before=%d, KeptGeneration=%v), want distinct non-keepable sentinel", g2, g1, integrations.KeptGeneration(g2))
		}
	})

	t.Run("moved_is_monotonic_nonkeepable_sentinel", func(t *testing.T) {
		h := &heldScope{roundScope: roundScope{
			MockManager: mm,
			g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
		}}
		before := h.StagedStartupGeneration()
		mm.SetScopeMocks(mk("c"), nil, true) // epoch moves while retained set stays
		after := h.StagedStartupGeneration()
		if after < before {
			t.Fatalf("held StagedStartupGeneration regressed %d -> %d after move", before, after)
		}
		if integrations.KeptGeneration(after) || after != integrations.RestagedGeneration {
			t.Fatalf("moved held StagedStartupGeneration = %d, want RestagedGeneration (%d)", after, integrations.RestagedGeneration)
		}
	})
}

func TestMockManager_StagedStartupReaderGenerationHeldSampleBeforeCheckRace(t *testing.T) {
	const testKind models.Kind = "StagedStartupHeldRaceKind"
	mm, _, _, w1End, _, _ := stagedStartupTestFixture(t, testKind)

	mk := func() []*models.Mock {
		return []*models.Mock{makeStagedStartupMock("x-gap", testKind, "consume", w1End.Add(time.Second), models.LifetimePerTest)}
	}
	mm.SetMocksWithWindow(mk(), nil, models.BaseTime, time.Now())
	offset := int64(mm.StagedStartupGeneration()) - int64(mm.StagingEpoch())
	mm.SetMocksWithWindow(mk(), nil, models.BaseTime, time.Now())
	if int64(mm.StagedStartupGeneration())-int64(mm.StagingEpoch()) != offset {
		t.Skip("generation and epoch do not move 1:1 on BaseTime staging")
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			mm.SetMocksWithWindow(mk(), nil, models.BaseTime, time.Now())
		}
	}()

	var hits atomic.Int64
	deadline := time.Now().Add(500 * time.Millisecond)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				e := mm.StagingEpoch()
				h := &heldScope{roundScope: roundScope{
					MockManager: mm,
					g:           roundGuard{at: integrations.RoundAt{Epoch: e}, staging: true},
				}}
				g := h.StagedStartupGeneration()
				if integrations.KeptGeneration(g) && int64(g)-offset > int64(e) {
					hits.Add(1)
				}
			}
		}()
	}
	time.Sleep(550 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if hits.Load() > 0 {
		t.Fatalf("heldScope reported a later staging's keepable generation %d times", hits.Load())
	}
}

func TestMockManager_StagedStartupReaderConcurrentReadsAndStaging(t *testing.T) {
	const testKind models.Kind = "StagedStartupConcurrentKind"
	mm, t0, w1Start, w1End, w2Start, w2End := stagedStartupTestFixture(t, testKind)

	var stop atomic.Bool
	var reads atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scoped := &scopedMockDb{MockMemDb: mm}
			for !stop.Load() {
				held := &heldScope{roundScope: roundScope{
					MockManager: mm,
					g:           roundGuard{at: integrations.RoundAt{Epoch: mm.StagingEpoch()}, staging: true},
				}}
				for _, db := range []integrations.StagedStartupReader{mm, scoped, held} {
					_ = db.StagedStartupGeneration()
					got, err := db.GetStagedStartupMocksByKind(testKind)
					if err != nil {
						t.Errorf("GetStagedStartupMocksByKind: %v", err)
						return
					}
					prefix := ""
					for i, m := range got {
						if m == nil {
							t.Errorf("nil mock in staged startup snapshot")
							return
						}
						_ = m.Spec.Metadata["type"]
						idx := strings.IndexByte(m.Name, '-')
						if idx <= 0 {
							t.Errorf("unexpected mock name %q", m.Name)
							return
						}
						p := m.Name[:idx]
						if prefix == "" {
							prefix = p
						} else if p != prefix {
							t.Errorf("single read mixed two stagings: %q vs %q", prefix, p)
							return
						}
						got[i] = nil
					}
					reads.Add(1)
				}
			}
		}()
	}

	for i := 0; i < 200 && !t.Failed(); i++ {
		pfx := fmt.Sprintf("s%d", i)
		mks := func() []*models.Mock {
			return []*models.Mock{
				makeStagedStartupMock(pfx+"-boot", testKind, "consume", t0.Add(time.Second), models.LifetimePerTest),
				makeStagedStartupMock(pfx+"-in1", testKind, "consume", w1Start.Add(100*time.Millisecond), models.LifetimePerTest),
				makeStagedStartupMock(pfx+"-gap", testKind, "consume", w1End.Add(2*time.Second), models.LifetimePerTest),
				makeStagedStartupMock(pfx+"-pub", testKind, "publish", w1Start.Add(200*time.Millisecond), models.LifetimePerTest),
				makeStagedStartupMock(pfx+"-in2", testKind, "consume", w2Start.Add(100*time.Millisecond), models.LifetimePerTest),
			}
		}
		switch i % 4 {
		case 0:
			mm.ResetForReplaySession()
		case 1:
			mm.ResetForReplaySession()
			mm.ArmSetFence()
		}
		mm.SetMocksWithWindow(mks(), nil, models.BaseTime, time.Now())
		all := mks()
		mm.SetMocksWithWindow(all[1:2], nil, w1Start, w1End)
		mm.SetMocksWithWindow(all[4:5], nil, w2Start, w2End)
		if i%5 == 0 {
			mm.SetScopeMocks(mks()[1:2], nil, true)
			mm.SetScopeMocks(mks(), nil, false)
		}
	}
	stop.Store(true)
	wg.Wait()
	if reads.Load() == 0 {
		t.Fatal("expected concurrent readers to complete at least one read")
	}
}
