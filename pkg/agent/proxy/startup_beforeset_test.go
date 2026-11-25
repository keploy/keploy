package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// runProvenModes exercises fn with KEPLOY_PROVEN explicitly off and on.
// pkg/agent/proxy/switch_main_test.go clears inherited KEPLOY_PROVEN in
// TestMain, so subtests must set provenswitch.Env explicitly to cover both
// modes independently.
func runProvenModes(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, tc := range []struct {
		name string
		env  string
	}{
		{name: "proven=off", env: ""},
		{name: "proven=on", env: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.env)
			fn(t)
		})
	}
}

// TestBeginTestErrorCaptureFlushesQueuedBootMissIntoBeforeSet verifies that a
// boot miss already queued on errChannel before BeginTestErrorCapture runs is
// flushed into the pre-window queue and attributed to BeforeSet (not Test #1)
// without requiring the caller to drain manually.
func TestBeginTestErrorCaptureFlushesQueuedBootMissIntoBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		// Repeat across multiple iterations to exercise the drain-vs-Begin
		// ordering reliably.
		for i := 0; i < 64; i++ {
			if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
				t.Fatal(err)
			}
			if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
				t.Fatal(err)
			}
			bootMiss := mockMiss("createIndexes on eventsV2")
			bootMiss.MismatchReport.Protocol = "MongoDB"
			p.SendError(bootMiss)
			p.BeginTestErrorCapture()

			got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1"))
			if err != nil {
				t.Fatalf("iter %d: GetMockErrors: %v", i, err)
			}
			if len(got) != 1 {
				t.Fatalf("iter %d: got %d misses, want 1", i, len(got))
			}
			if !got[0].BeforeSet || got[0].Kind != models.MissOutsideTest {
				t.Fatalf("iter %d: boot miss queued before BeginTestErrorCapture leaked into test-1: %+v", i, got[0])
			}
		}
	})
}

// TestLateBootMissMarkedBeforeFirstTestAttributedToBeforeSet verifies that
// under exact serving, a startup call whose round was marked before Test #1's
// view was installed, but whose miss is decided and delivered after
// BeginTestErrorCapture and Test #1 opened, is still attributed to BeforeSet
// rather than Test #1. A miss marked inside Test #1 stays Test #1's, and a
// miss marked between Test #1 and Test #2 stays Between (not BeforeSet).
func TestLateBootMissMarkedBeforeFirstTestAttributedToBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		// Install exact startup pool view (no test open).
		mm.ServeExactPool("", nil)

		// Boot call arrives before Test #1 opens and marks its round during startup.
		bootRound := mm.MarkCallRound()
		if !bootRound.Read() || bootRound.InTest() || !bootRound.BeforeFirstTest() {
			t.Fatalf("expected startup mark (Read=true, InTest=false, BeforeFirstTest=true), got %+v", bootRound)
		}

		// Test #1 opens its capture window and installs its exact test view before
		// the slow boot call finishes matching and files its miss.
		p.BeginTestErrorCapture()
		mm.OpenExactTest("test-1", nil, nil)

		// Boot call's miss is now decided and delivered while Test #1 is open.
		bootReport := &models.MockMismatchReport{
			Protocol:      "MongoDB",
			ActualSummary: "listIndexes on test_case_audits",
			MatchPhase:    models.MatchPhaseExhausted,
		}
		integrations.StampMark(bootReport, bootRound)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, bootReport), 0, 0, nil)

		// Test #1's own call arrives inside Test #1 and misses.
		t1Round := mm.MarkCallRound()
		if !t1Round.InTest() || t1Round.BeforeFirstTest() {
			t.Fatalf("expected test-1 mark (InTest=true, BeforeFirstTest=false), got %+v", t1Round)
		}
		t1Report := &models.MockMismatchReport{
			Protocol:      "MongoDB",
			ActualSummary: "find on orders",
			MatchPhase:    models.MatchPhaseSchema,
		}
		integrations.StampMark(t1Report, t1Round)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, t1Report), 0, 0, nil)

		got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-1): %v", err)
		}
		bySummary := map[string]models.UnmatchedCall{}
		for _, c := range got {
			bySummary[c.ActualSummary] = c
		}
		bootCall, ok := bySummary["listIndexes on test_case_audits"]
		if !ok {
			t.Fatalf("boot miss missing from test-1 read: %+v", got)
		}
		if !bootCall.BeforeSet || bootCall.Kind != models.MissOutsideTest {
			t.Fatalf("late boot miss marked before test-1 must be BeforeSet+outside_test, got %+v", bootCall)
		}
		t1Call, ok := bySummary["find on orders"]
		if !ok {
			t.Fatalf("test-1 miss missing from test-1 read: %+v", got)
		}
		if t1Call.BeforeSet {
			t.Fatalf("test-1's own miss must NOT be BeforeSet, got %+v", t1Call)
		}

		// Verify a miss marked between test-1 and test-2 is not misclassified
		// as BeforeSet.
		mm.ServeExactPool("", nil)
		gapRound := mm.MarkCallRound()
		if gapRound.BeforeFirstTest() {
			t.Fatalf("post-test-1 pool mark must have BeforeFirstTest=false, got %+v", gapRound)
		}
		p.ContinueTestErrorCapture()
		mm.OpenExactTest("test-2", nil, nil)
		gapReport := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "GET /between-tests",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		integrations.StampMark(gapReport, gapRound)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, gapReport), 0, 0, nil)

		got2, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-2"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-2): %v", err)
		}
		if len(got2) != 1 || got2[0].BeforeSet {
			t.Fatalf("gap miss after test-1 must NOT be BeforeSet, got %+v", got2)
		}
	})
}

// TestPostFirstTestPoolMissNotBeforeSet exercises the exact production replay
// order (BeginTestErrorCapture -> OpenExactTest("test-1") -> ServeExactPool ->
// post-test-1 pool miss before GetMockErrors("test-1") increments windowReads).
// Neither a miss delivered before GetMockErrors("test-1") nor one delivered
// after GetMockErrors("test-1") and carried into Test #2 may be misclassified
// as BeforeSet; the carried miss must remain Between under proven mode.
func TestPostFirstTestPoolMissNotBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		mm.ServeExactPool("", nil)

		// Open capture, open test-1, then restore pool view before
		// GetMockErrors("test-1") runs (so windowReads is still 0).
		p.BeginTestErrorCapture()
		mm.OpenExactTest("test-1", nil, nil)
		mm.ServeExactPool("", nil)

		earlyPoolRound := mm.MarkCallRound()
		latePoolRound := mm.MarkCallRound()
		if earlyPoolRound.BeforeFirstTest() || latePoolRound.BeforeFirstTest() {
			t.Fatalf("pool rounds marked after test-1 fired must have BeforeFirstTest=false: early=%+v late=%+v", earlyPoolRound, latePoolRound)
		}

		// Miss A arrives before GetMockErrors("test-1") runs.
		earlyRep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "POST /after-test-1-before-read",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		integrations.StampMark(earlyRep, earlyPoolRound)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, earlyRep), 0, 0, nil)

		got1, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-1): %v", err)
		}
		if len(got1) != 1 {
			t.Fatalf("test-1 got %d misses, want 1: %+v", len(got1), got1)
		}
		if got1[0].BeforeSet {
			t.Fatalf("post-test-1 pool miss delivered before GetMockErrors(test-1) must NOT be BeforeSet: %+v", got1[0])
		}

		// Miss B was also marked while windowReads was still 0, but finishes after
		// GetMockErrors("test-1") and is carried into test-2 via ContinueTestErrorCapture.
		lateRep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "POST /after-test-1-carried-to-test-2",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		integrations.StampMark(lateRep, latePoolRound)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, lateRep), 0, 0, nil)

		p.ContinueTestErrorCapture()
		mm.OpenExactTest("test-2", nil, nil)

		got2, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-2"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-2): %v", err)
		}
		if len(got2) != 1 {
			t.Fatalf("test-2 got %d misses, want 1: %+v", len(got2), got2)
		}
		if got2[0].BeforeSet {
			t.Fatalf("post-test-1 pool miss carried into test-2 must NOT be BeforeSet: %+v", got2[0])
		}
		if provenswitch.On() && !got2[0].Between {
			t.Fatalf("post-test-1 pool miss carried into test-2 under proven mode must be Between: %+v", got2[0])
		}
	})
}

// TestPreFirstTestWindowMissAttributedToBeforeSetBothModes verifies that when
// BeginTestErrorCapture opens Test #1's capture window before Test #1's mock
// window is staged, a boot call missing in that gap (while no test or scope
// has fired yet) is attributed to BeforeSet in both KEPLOY_PROVEN modes,
// whereas a miss after Test #1's window is staged belongs to Test #1.
func TestPreFirstTestWindowMissAttributedToBeforeSetBothModes(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}

		// BeginTestErrorCapture opens Test #1's capture window before Test #1's
		// mock window is staged.
		p.BeginTestErrorCapture()

		// Boot call misses in the gap before Test #1's mock window opens.
		bootReport := &models.MockMismatchReport{
			Protocol:      "MongoDB",
			ActualSummary: "createIndexes on eventsV2",
			MatchPhase:    models.MatchPhaseExhausted,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, bootReport), 0, 0, nil)

		// Test #1's mock window opens.
		now := time.Now()
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, now, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}

		// Test #1's own call misses after Test #1's mock window is active.
		t1Report := &models.MockMismatchReport{
			Protocol:      "MongoDB",
			ActualSummary: "find on app_config_audits",
			MatchPhase:    models.MatchPhaseSchema,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, t1Report), 0, 0, nil)

		got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1"))
		if err != nil {
			t.Fatalf("GetMockErrors: %v", err)
		}
		bySummary := map[string]models.UnmatchedCall{}
		for _, c := range got {
			bySummary[c.ActualSummary] = c
		}
		if c := bySummary["createIndexes on eventsV2"]; !c.BeforeSet || c.Kind != models.MissOutsideTest {
			t.Fatalf("pre-first-test-window boot miss must be BeforeSet+outside_test, got %+v", c)
		}
		if c := bySummary["find on app_config_audits"]; c.BeforeSet {
			t.Fatalf("in-test-1 miss must NOT be BeforeSet, got %+v", c)
		}
	})
}

// TestZeroStartWindowTestMissNotBeforeSet verifies that sets whose test cases
// carry zero recorded timestamps (for example Postman imports, hand-written
// tests, or non-HTTP/gRPC cases) still attribute pre-Test-#1 startup misses to
// BeforeSet, while misses after Test #1 stages its zero-start window are
// charged to Test #1 (not wrapped as BeforeSet), and the flag resets cleanly
// at the next test set boundary.
func TestZeroStartWindowTestMissNotBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}

		// Pre-Test-#1 startup miss before Test #1 stages its window.
		p.BeginTestErrorCapture()
		bootRep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "GET /startup-warmup",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, bootRep), 0, 0, nil)

		// Test #1 stages with a zero-start window (no recorded timestamps).
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if !p.getMockManager().HasAnyTestOrScopeFired() {
			t.Fatal("HasAnyTestOrScopeFired() = false after zero-start per-test staging, want true")
		}

		// Test #1's own miss must be charged to Test #1, NOT BeforeSet.
		t1Rep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "POST /postman-case-1",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, t1Rep), 0, 0, nil)

		got1, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-1): %v", err)
		}
		bySummary := map[string]models.UnmatchedCall{}
		for _, c := range got1 {
			bySummary[c.ActualSummary] = c
		}
		if c := bySummary["GET /startup-warmup"]; !c.BeforeSet || c.Kind != models.MissOutsideTest {
			t.Fatalf("pre-test-1 startup miss must be BeforeSet+outside_test, got %+v", c)
		}
		if c := bySummary["POST /postman-case-1"]; c.BeforeSet {
			t.Fatalf("zero-start test-1 miss must NOT be BeforeSet, got %+v", c)
		}

		// Next test set resets the sticky flag so Set #2 startup misses are
		// BeforeSet again until Set #2's first test stages.
		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true, StartsTestSet: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		if p.getMockManager().HasAnyTestOrScopeFired() {
			t.Fatal("HasAnyTestOrScopeFired() = true after new set initial staging, want false")
		}
		p.BeginTestErrorCapture()
		boot2Rep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "GET /set2-startup",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, boot2Rep), 0, 0, nil)
		gotSet2, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "set2-test-1"))
		if err != nil {
			t.Fatalf("GetMockErrors(set2-test-1): %v", err)
		}
		if len(gotSet2) != 1 || !gotSet2[0].BeforeSet {
			t.Fatalf("set-2 startup miss must be BeforeSet, got %+v", gotSet2)
		}
	})
}

// TestScopePostFirstTestMissNotBeforeSet verifies that after a keploy mock
// scope test closes via SetScopeMocks(..., open=false), WindowSnapshot().FirstTestFired
// reverts to false, while HasAnyTestOrScopeFired stays true so a miss between
// scope test #1 and scope test #2 is not misattributed to BeforeSet.
func TestScopePostFirstTestMissNotBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()

		// Scope test #1 opens, runs cleanly, and closes (open=false).
		p.BeginTestErrorCapture()
		mm.SetScopeMocks(nil, nil, true)
		if got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "scope-1")); err != nil || len(got) != 0 {
			t.Fatalf("scope-1 got %+v, %v; want empty", got, err)
		}
		mm.SetScopeMocks(nil, nil, false)
		if mm.WindowSnapshot().FirstTestFired {
			t.Fatal("expected WindowSnapshot().FirstTestFired to revert to false after SetScopeMocks(open=false)")
		}
		if !mm.HasAnyTestOrScopeFired() {
			t.Fatal("expected HasAnyTestOrScopeFired() to remain true after SetScopeMocks(open=false)")
		}

		// A miss between scope test #1 and scope test #2 must NOT be wrapped as BeforeSet.
		gapRep := &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: "GET /between-scope-tests",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, gapRep), 0, 0, nil)

		p.ContinueTestErrorCapture()
		mm.SetScopeMocks(nil, nil, true)
		got2, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "scope-2"))
		if err != nil {
			t.Fatalf("GetMockErrors(scope-2): %v", err)
		}
		if len(got2) != 1 || got2[0].BeforeSet {
			t.Fatalf("miss after scope test #1 closed must NOT be BeforeSet, got %+v", got2)
		}
	})
}

// TestRoundSnapshotAndRoundMetaBeforeFirstTest exercises the CallRound
// BeforeFirstTest propagation through both RoundSnapshot and RoundMeta across
// startup, in-test, and post-first-test pool phases under exact serving, plus
// the v==nil unset-background RoundSnapshot branch inside an open scope.
func TestRoundSnapshotAndRoundMetaBeforeFirstTest(t *testing.T) {
	t.Run("exact serving RoundSnapshot and RoundMeta", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.ServeExactPool("", nil)

		// Startup pool view before test-1: both paths stamp BeforeFirstTest=true.
		snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		if !ok {
			t.Fatal("startup RoundSnapshot returned ok=false")
		}
		if !snap.CallRound().BeforeFirstTest() {
			t.Fatalf("startup RoundSnapshot.CallRound().BeforeFirstTest() = false, want true (phase=%v fired=%v test=%q)", snap.Phase, snap.Fired, snap.Test)
		}
		meta, ok := mm.RoundMeta(nil)
		if !ok {
			t.Fatal("startup RoundMeta returned ok=false")
		}
		if !meta.CallRound().BeforeFirstTest() {
			t.Fatalf("startup RoundMeta.CallRound().BeforeFirstTest() = false, want true (phase=%v fired=%v test=%q)", meta.Phase, meta.Fired, meta.Test)
		}
		rep := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /boot"}
		integrations.StampMark(rep, meta.CallRound())
		if !rep.MarkedBeforeFirstTest {
			t.Fatal("StampMark(meta.CallRound()) left MarkedBeforeFirstTest=false, want true")
		}

		// Inside test-1: both paths stamp BeforeFirstTest=false.
		mm.OpenExactTest("test-1", nil, nil)
		snap1, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		if !ok || snap1.CallRound().BeforeFirstTest() {
			t.Fatalf("in-test RoundSnapshot: ok=%v BeforeFirstTest=%v, want ok=true false", ok, snap1.CallRound().BeforeFirstTest())
		}
		meta1, ok := mm.RoundMeta(nil)
		if !ok || meta1.CallRound().BeforeFirstTest() {
			t.Fatalf("in-test RoundMeta: ok=%v BeforeFirstTest=%v, want ok=true false", ok, meta1.CallRound().BeforeFirstTest())
		}

		// Post-test-1 pool view: both paths stamp BeforeFirstTest=false.
		mm.ServeExactPool("", nil)
		snapPool, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		if !ok || snapPool.CallRound().BeforeFirstTest() {
			t.Fatalf("post-test-1 pool RoundSnapshot: ok=%v BeforeFirstTest=%v, want ok=true false", ok, snapPool.CallRound().BeforeFirstTest())
		}
		metaPool, ok := mm.RoundMeta(nil)
		if !ok || metaPool.CallRound().BeforeFirstTest() {
			t.Fatalf("post-test-1 pool RoundMeta: ok=%v BeforeFirstTest=%v, want ok=true false", ok, metaPool.CallRound().BeforeFirstTest())
		}
	})

	t.Run("unset background RoundSnapshot with open scope stays false", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		sm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		t.Cleanup(sm.Close)
		now := time.Now()
		seed := &models.Mock{
			Name: "mock-startup-seed",
			Kind: models.HTTP,
			Spec: models.MockSpec{
				ReqTimestampMock: now.Add(-2 * time.Second),
				ResTimestampMock: now.Add(-time.Second),
				Metadata:         map[string]string{"type": "config"},
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://upstream/seed"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200},
			},
		}
		own := &models.Mock{
			Name: "mock-own-1",
			Kind: models.HTTP,
			Spec: models.MockSpec{
				ReqTimestampMock: now,
				ResTimestampMock: now.Add(time.Second),
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://upstream/own"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200},
			},
		}
		sm.SetMocksWithWindow(nil, []*models.Mock{seed, own}, models.BaseTime, now.Add(time.Hour))
		sm.StageExactBackground(models.RecordedWithS12Routing)
		sm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-startup-seed"}, nil)
		sm.SetScopeMocks([]*models.Mock{own}, nil, true)

		snap, ok := sm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		if !ok {
			t.Fatal("unset background RoundSnapshot in open scope returned ok=false, want true")
		}
		if snap.Phase != 0 || !snap.Open || !snap.Background {
			t.Fatalf("expected v==nil unset-background snapshot (Phase=0, Open=true, Background=true), got %+v", snap)
		}
		if snap.CallRound().BeforeFirstTest() {
			t.Fatalf("unset background RoundSnapshot in an open scope must NOT mark BeforeFirstTest=true: %+v", snap.CallRound())
		}
	})
}

// TestMockActivityDefaultModeReportsInFlightNotCounted verifies the real
// default configuration (KEPLOY_PROVEN unset, settleReads=false,
// countedFromStart=false): Proxy.MockActivity accurately counts Consumed and
// Missed and truthfully reports inFlightCounted=false with inFlight=0.
func TestMockActivityDefaultModeReportsInFlightNotCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	p.nsswitchData = []byte("preseeded-by-test")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	p.StartErrorDrain(ctx)

	if p.keepsConnections() {
		t.Fatal("default unset-mode proxy unexpectedly has keepsConnections()=true")
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	mm := p.getMockManager()
	if err := mm.recordConsume(models.MockState{Name: "boot-1"}, 0, true); err != nil {
		t.Fatal(err)
	}
	missRep := &models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "createIndexes on eventsV2"}
	p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, missRep), 0, 0, nil)

	consumed, missed, inFlight, inFlightCounted := p.MockActivity()
	if consumed != 1 || missed != 1 || inFlight != 0 || inFlightCounted {
		t.Fatalf("default mode MockActivity = (%d, %d, %d, %v), want (1, 1, 0, false)", consumed, missed, inFlight, inFlightCounted)
	}
}

// TestMockActivityCountsServedMissedAndResetsOnNewSession verifies Proxy.MockActivity
// returns non-draining served and missed counts (excluding ReportAnswer entries
// and non-miss decode/IO errors) and resets missed on Proxy.Mock and served on
// ResetForReplaySession.
func TestMockActivityCountsServedMissedAndResetsOnNewSession(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		if err := mm.recordConsume(models.MockState{Name: "boot-1"}, 0, true); err != nil {
			t.Fatal(err)
		}
		if err := mm.recordConsume(models.MockState{Name: "boot-2"}, 0, true); err != nil {
			t.Fatal(err)
		}
		// Non-served bookkeeping update must NOT increment consumed.
		if err := mm.recordConsume(models.MockState{Name: "boot-2"}, 0, false); err != nil {
			t.Fatal(err)
		}

		// Real structured miss increments missed (0 -> 1).
		missRep := &models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "createIndexes on eventsV2"}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, missRep), 0, 0, nil)

		// Unstructured ErrNoMockMatched also increments missed (1 -> 2).
		p.sendMockNotFoundErrorFrom(context.Background(), fmt.Errorf("unstructured miss: %w", models.ErrNoMockMatched), 0, 0, nil)

		// Answered report (Answer != "") must NOT increment missed.
		ansRep := &models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "find on ok", Answer: "exact"}
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, ansRep), 0, 0, nil)

		// Non-miss decode/IO error (no report, not ErrNoMockMatched) must NOT increment missed.
		p.sendMockNotFoundErrorFrom(context.Background(), errors.New("EOF decoding frame"), 0, 0, nil)

		consumed, missed, inFlight, inFlightCounted := p.MockActivity()
		if consumed != 2 {
			t.Fatalf("consumed = %d, want 2", consumed)
		}
		if missed != 2 {
			t.Fatalf("missed = %d, want 2 (ReportAnswer and non-miss decode/IO errors must not count as missed)", missed)
		}
		if inFlight != 0 {
			t.Fatalf("inFlight = %d, want 0", inFlight)
		}
		if inFlightCounted != provenswitch.On() {
			t.Fatalf("inFlightCounted = %v, want %v", inFlightCounted, provenswitch.On())
		}

		// Advancing to a per-test window must NOT reset totalServed.
		now := time.Now()
		mm.SetCurrentTestWindow(now, now.Add(time.Second))
		if got := mm.TotalServed(); got != 2 {
			t.Fatalf("TotalServed after SetCurrentTestWindow = %d, want 2", got)
		}

		// Starting a new replay session resets both counters.
		mm.ResetForReplaySession()
		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		c2, m2, _, _ := p.MockActivity()
		if c2 != 0 || m2 != 0 {
			t.Fatalf("after session reset got consumed=%d missed=%d, want 0, 0", c2, m2)
		}
	})
}

// TestMockActivityAggregatesWorkerConsumed verifies that Consumed sums serves
// from both the run's mock manager and active or retired test workers, and
// resets cleanly when a new replay session starts.
func TestMockActivityAggregatesWorkerConsumed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	p.nsswitchData = []byte("preseeded-by-test")
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })

	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.getMockManager().recordConsume(models.MockState{Name: "run-1"}, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(2); err != nil {
		t.Fatal(err)
	}
	if err := p.instanceManager(1).recordConsume(models.MockState{Name: "w1-a"}, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := p.instanceManager(2).recordConsume(models.MockState{Name: "w2-a"}, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := p.instanceManager(2).recordConsume(models.MockState{Name: "w2-b"}, 0, true); err != nil {
		t.Fatal(err)
	}

	if consumed, _, _, _ := p.MockActivity(); consumed != 4 {
		t.Fatalf("consumed with 2 live workers = %d, want 4", consumed)
	}
	// Stopping worker 2 must retain its served count in the current session.
	if err := p.StopServingInstance(2); err != nil {
		t.Fatal(err)
	}
	if consumed, _, _, _ := p.MockActivity(); consumed != 4 {
		t.Fatalf("consumed after stopping worker 2 = %d, want 4", consumed)
	}
	// Starting a new replay session resets both live and retired counts.
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if consumed, _, _, _ := p.MockActivity(); consumed != 0 {
		t.Fatalf("consumed after new session = %d, want 0", consumed)
	}
}

// TestMockActivityInFlightTracksActiveReplayRequest verifies Proxy.MockActivity
// queries InFlightState in replay mode (answers=true) and reports
// inFlightCounted=true when keepsConnections() is active (either under
// KEPLOY_PROVEN=1 or with settleReads+countedFromStart).
func TestMockActivityInFlightTracksActiveReplayRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		proven      string
		settleReads bool
	}{
		{name: "proven=on", proven: "1", settleReads: false},
		{name: "proven=off-settle-reads", proven: "", settleReads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			p := New(zap.NewNop(), fakeDestInfo{}, config.New())
			if tc.settleReads {
				p.settleReads = true
				p.countedFromStart.Store(true)
			}

			client, server := net.Pipe()
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
			})
			tcConn := p.inflight.track(server)
			t.Cleanup(func() { p.inflight.forget(tcConn) })

			// Simulate an idle parser handler blocked in Read waiting for the next call.
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				buf := make([]byte, 16)
				_, _ = tcConn.Read(buf)
			}()
			t.Cleanup(func() {
				_ = client.Close()
				<-readDone
			})

			// Wait via event notification (no polling sleep) for Read to enter blocked state.
			for {
				ch := p.InFlightChanged()
				if tcConn.view().reading > 0 {
					break
				}
				<-ch
			}

			if _, _, inFlight, counted := p.MockActivity(); inFlight != 0 || !counted {
				t.Fatalf("idle connection blocked in Read reported (inFlight=%d, counted=%v), want (0, true)", inFlight, counted)
			}

			// Parser holds an active replay request already read off the socket (untaken == 0, held == 1).
			release := tcConn.Hold()
			if _, _, inFlight, counted := p.MockActivity(); inFlight != 1 || !counted {
				release()
				t.Fatalf("active held replay request reported (inFlight=%d, counted=%v), want (1, true)", inFlight, counted)
			}

			// Once the parser answers/releases the request, inFlight drops back to 0.
			release()
			if _, _, inFlight, counted := p.MockActivity(); inFlight != 0 || !counted {
				t.Fatalf("after releasing held replay request reported (inFlight=%d, counted=%v), want (0, true)", inFlight, counted)
			}
		})
	}
}

// TestLateStartupMissCarriedIntoTest2StaysBeforeSet verifies that a startup
// call marked before Test #1 whose miss arrives after Test #1 closed and is
// carried into Test #2 via ContinueTestErrorCapture stays BeforeSet (not
// misclassified as Between or Test #2).
func TestLateStartupMissCarriedIntoTest2StaysBeforeSet(t *testing.T) {
	runProvenModes(t, func(t *testing.T) {
		p := New(zap.NewNop(), fakeDestInfo{}, config.New())
		p.nsswitchData = []byte("preseeded-by-test")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		p.StartErrorDrain(ctx)

		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		mm := p.getMockManager()
		mm.ServeExactPool("", nil)

		// Boot call marked before Test #1 opened.
		bootRound := mm.MarkCallRound()
		if !bootRound.BeforeFirstTest() {
			t.Fatalf("expected startup bootRound.BeforeFirstTest()=true, got %+v", bootRound)
		}

		// Test #1 runs and finishes before the slow boot call finishes.
		p.BeginTestErrorCapture()
		mm.OpenExactTest("test-1", nil, nil)
		if got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-1")); err != nil || len(got) != 0 {
			t.Fatalf("test-1 got %+v, %v; want empty", got, err)
		}

		// Slow boot call finally misses while no window is open (between test-1 and test-2).
		bootReport := &models.MockMismatchReport{
			Protocol:      "MongoDB",
			ActualSummary: "listIndexes on mockReference",
			MatchPhase:    models.MatchPhaseExhausted,
		}
		integrations.StampMark(bootReport, bootRound)
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, bootReport), 0, 0, nil)

		// Test #2 opens via ContinueTestErrorCapture (flushes errChannel + carries pending).
		p.ContinueTestErrorCapture()
		mm.OpenExactTest("test-2", nil, nil)

		got2, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "test-2"))
		if err != nil {
			t.Fatalf("GetMockErrors(test-2): %v", err)
		}
		if len(got2) != 1 || !got2[0].BeforeSet || got2[0].Between || got2[0].Kind != models.MissOutsideTest {
			t.Fatalf("slow startup miss carried into test-2 must stay BeforeSet (not Between), got %+v", got2)
		}
	})
}

// TestBeginTestErrorCaptureCtxRespectsCancelledContext verifies that
// openCaptureOn bounds its errChannel flush by the caller's context and logs a
// debug message with the reason when the flush cannot complete.
func TestBeginTestErrorCaptureCtxRespectsCancelledContext(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 4)}
	p.errDrainDone = make(chan struct{})
	p.errDrainActive.Store(true) // stalled drain that never consumes the marker

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		p.BeginTestErrorCaptureCtx(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("BeginTestErrorCaptureCtx blocked despite cancelled context")
	}
	if logs.FilterMessage("error drain flush before opening test capture window did not complete").Len() != 1 {
		t.Fatalf("expected 1 debug log on incomplete flush, got %d entries: %+v", logs.Len(), logs.All())
	}
}

// TestMarkedBeforeFirstTestGuardsStagingEpoch verifies that
// missWindow.markedBeforeFirstTest rejects marks when no set has been staged
// (setStaged == 0) or when the mark belongs to an earlier staging epoch
// (MarkEpoch < setStaged), and accepts marks from the current staging epoch.
func TestMarkedBeforeFirstTestGuardsStagingEpoch(t *testing.T) {
	mkMiss := func(epoch uint64) error {
		return models.ParserError{
			ParserErrorType: models.ErrMockNotFound,
			MismatchReport: &models.MockMismatchReport{
				Mark:                  models.RoundMarkedSettled,
				MarkedBeforeFirstTest: true,
				MarkEpoch:             epoch,
			},
		}
	}

	var w missWindow
	// Before any set is staged (setStaged == 0), even epoch 0 must be rejected.
	if w.markedBeforeFirstTest(mkMiss(0)) {
		t.Fatal("markedBeforeFirstTest returned true with setStaged==0 and MarkEpoch==0")
	}
	if w.markedBeforeFirstTest(mkMiss(1)) {
		t.Fatal("markedBeforeFirstTest returned true with setStaged==0 and MarkEpoch==1")
	}

	w.setStaged.Store(5)
	if w.markedBeforeFirstTest(mkMiss(4)) {
		t.Fatal("markedBeforeFirstTest returned true for earlier staging epoch 4 < 5")
	}
	if !w.markedBeforeFirstTest(mkMiss(5)) {
		t.Fatal("markedBeforeFirstTest returned false for current staging epoch 5 == 5")
	}
	if !w.markedBeforeFirstTest(mkMiss(6)) {
		t.Fatal("markedBeforeFirstTest returned false for subsequent staging epoch 6 > 5")
	}
}
