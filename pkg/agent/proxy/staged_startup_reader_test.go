package proxy

import (
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
