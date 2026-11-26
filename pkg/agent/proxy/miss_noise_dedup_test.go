package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Mock-miss deduplication is logging hygiene, not proven-switch semantics:
// every isMockMiss(err) must be logged at Debug in mockFailedFrom and returned
// as reportedMiss in BOTH KEPLOY_PROVEN modes (keeping Error for parser
// failures). Under `keploy test` (once BeginTestErrorCapture opens a capture
// window for the end-of-run replay reader), in-window misses log at Debug in
// default mode too so each diff is printed ONCE in the end-of-run table.
// Sessions with no end-of-run replay reader (agent-only / `keploy mock`) still
// emit one Warn line in default mode.
func TestMockMissDedupAndWindowLevelInBothModes(t *testing.T) {
	miss := fmt.Errorf("decode: %w", models.ErrNoMockMatched)
	parserErr := errors.New("parser broke")

	for _, proven := range []string{"", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			core, logs := observer.New(zapcore.DebugLevel)
			logger := zap.New(core)
			mm := NewMockManager(nil, nil, logger)
			defer mm.Close()
			p := &Proxy{logger: logger, mockManager: mm, errChannel: make(chan error, 8)}

			// Outside any open test window: mockFailedFrom must not emit Error
			// and mismatchLog must emit Debug (not Warn).
			got := p.mockFailedFrom(context.Background(), logger, miss, 0, 0, nil)
			if _, ok := got.(reportedMiss); !ok {
				t.Fatalf("mockFailedFrom(miss) returned %T, want reportedMiss", got)
			}
			if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
				t.Errorf("expected 0 Error lines for mock miss, got %d: %v", len(errs), errs)
			}
			if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
				t.Errorf("expected 0 Warn lines for miss outside open test window, got %d: %v", len(warns), warns)
			}

			// Inside an open mock window WITHOUT a test capture reader
			// (agent-only / `keploy mock` flow): 1 Warn when proven switch is
			// off, 0 Warn when proven switch is on.
			logs.TakeAll()
			t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
			mm.SetMocksWithWindow(nil, nil, t0, t0.Add(time.Second))
			got = p.mockFailedFrom(context.Background(), logger, miss, 0, 0, nil)
			if _, ok := got.(reportedMiss); !ok {
				t.Fatalf("mockFailedFrom(in-window miss) returned %T, want reportedMiss", got)
			}
			if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
				t.Errorf("expected 0 Error lines for in-window miss, got %d: %v", len(errs), errs)
			}
			wantNoReaderWarns := 1
			if proven == "1" {
				wantNoReaderWarns = 0
			}
			if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != wantNoReaderWarns {
				t.Errorf("mock-only in-window Warn lines = %d, want %d: %v", len(warns), wantNoReaderWarns, logs.All())
			}

			// Inside an open test window UNDER `keploy test`
			// (BeginTestErrorCapture opened): ZERO Warn lines in BOTH default
			// mode and proven mode, and 1 Debug mismatch line.
			logs.TakeAll()
			p.BeginTestErrorCapture()
			got = p.mockFailedFrom(context.Background(), logger, miss, 0, 0, nil)
			if _, ok := got.(reportedMiss); !ok {
				t.Fatalf("mockFailedFrom(keploy test miss) returned %T, want reportedMiss", got)
			}
			if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
				t.Errorf("keploy test in-window miss expected 0 Error lines, got %d: %v", len(errs), errs)
			}
			if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
				t.Errorf("keploy test in-window miss expected 0 Warn lines (table is the one visible print), got %d: %v", len(warns), warns)
			}
			if dbgs := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").FilterLevelExact(zapcore.DebugLevel).All(); len(dbgs) != 1 {
				t.Errorf("keploy test in-window miss expected 1 Debug mismatch line, got %d: %v", len(dbgs), logs.All())
			}

			// Non-miss parser failure must still log Error and not wrap in reportedMiss.
			logs.TakeAll()
			gotErr := p.mockFailedFrom(context.Background(), logger, parserErr, 0, 0, nil)
			if _, ok := gotErr.(reportedMiss); ok {
				t.Fatalf("non-miss parser error must not be wrapped in reportedMiss")
			}
			if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 1 {
				t.Errorf("non-miss parser error expected 1 Error line, got %d: %v", len(errs), errs)
			}
		})
	}
}

// Reusing a long-lived agent from a `keploy test` session into a subsequent
// `keploy mock` session (Proxy.Mock without BeginTestErrorCapture) must restore
// the single visible Warn line in default mode.
func TestMockSessionResetRestoresWarnAfterTestSession(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(logger, nil, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatalf("p.Mock(1): %v", err)
	}
	defer p.getMockManager().Close()

	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	p.getMockManager().SetMocksWithWindow(nil, nil, t0, t0.Add(time.Second))

	// First: `keploy test` session -> 0 Warn.
	p.BeginTestErrorCapture()
	logs.TakeAll()
	miss := fmt.Errorf("decode: %w", models.ErrNoMockMatched)
	_ = p.mockFailedFrom(context.Background(), logger, miss, 0, 0, nil)
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("keploy test session emitted %d Warns, want 0", len(warns))
	}

	// Second: new `keploy mock` session calls Proxy.Mock without
	// BeginTestErrorCapture -> hasTestCaptureReader resets -> 1 Warn.
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatalf("p.Mock(2): %v", err)
	}
	p.getMockManager().SetMocksWithWindow(nil, nil, t0, t0.Add(time.Second))
	logs.TakeAll()
	_ = p.mockFailedFrom(context.Background(), logger, miss, 0, 0, nil)
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 1 {
		t.Fatalf("keploy mock session after Proxy.Mock emitted %d Warns, want 1: %v", len(warns), logs.All())
	}
}

// Carry-over tier overflow must accumulate across test sets and emit ONE
// visible Warn at run end with the run-wide total + actionable setting (zero
// per-set Warns mid-run), and attribute ONLY exact evicted mock IDs (never
// signature fallback without a mock ID, never payload diffs against a test's
// own candidate mock).
func TestCarryOverOverflowWarnAndScopedEvictionAttribution(t *testing.T) {
	registerSends(t)
	prev := carryOverCapBytes
	t.Cleanup(func() { carryOverCapBytes = prev })
	sample := brokerMockAt("sx", pulsarKind, "SEND", 0)
	one := approxMockBytes(sample)
	carryOverCapBytes = one + one/2 // room for only one

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	mm := NewMockManager(nil, nil, logger)
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	evicted := brokerMockAt("mock-evicted", pulsarKind, "SEND", 1500)
	evicted.Spec.Metadata["topic"] = "persistent://tenant/ns/orders"
	kept := brokerMockAt("mock-kept", pulsarKind, "SEND", 1200)
	kept.Spec.Metadata["topic"] = "persistent://tenant/ns/orders"
	mm.SetMocksWithWindow([]*models.Mock{kept, evicted}, nil, winStart(1), winEnd(1))

	// Zero mid-run Warn lines during set 1 overflow; logged at Debug instead.
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("expected 0 mid-run Warn lines during staging, got %d: %v", len(warns), warns)
	}
	// Second overflow in set 1: still 0 mid-run Warn lines.
	evicted2 := brokerMockAt("mock-evicted-2", pulsarKind, "SEND", 2500)
	evicted2.Spec.Metadata["topic"] = "persistent://tenant/ns/orders"
	mm.SetMocksWithWindow([]*models.Mock{evicted2}, nil, winStart(2), winEnd(2))
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("expected 0 mid-run Warn lines after second overflow, got %d: %v", len(warns), warns)
	}

	p := &Proxy{logger: logger, mockManager: mm, errChannel: make(chan error, 8)}
	p.BeginTestErrorCapture()

	drainReport := func(t *testing.T, report *models.MockMismatchReport) *models.MockMismatchReport {
		t.Helper()
		p.sendMockNotFoundErrorFrom(context.Background(), models.NewMockMismatchError(models.ErrNoMockMatched, report), 0, 0, nil)
		select {
		case raw := <-p.errChannel:
			pe, ok := raw.(models.ParserError)
			if !ok || pe.MismatchReport == nil {
				t.Fatalf("unexpected error on channel: %#v", raw)
			}
			return pe.MismatchReport
		default:
			t.Fatal("expected ParserError on errChannel")
			return nil
		}
	}

	t.Run("negative: payload diff against own candidate mock gets ZERO eviction note", func(t *testing.T) {
		got := drainReport(t, &models.MockMismatchReport{
			Protocol:      string(pulsarKind),
			ActualSummary: "SEND persistent://tenant/ns/orders",
			ClosestMock:   "mock-own-test-send",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Expected: "1", Actual: "2"}},
			NextSteps:     "Add drifting dynamic fields as noise.",
		})
		if strings.Contains(strings.ToLower(got.NextSteps), "evict") || strings.Contains(strings.ToLower(got.NextSteps), "carry-over") {
			t.Errorf("payload diff against own candidate mock must NOT be blamed on carry-over eviction: %q", got.NextSteps)
		}
		if got.ClosestMock != "mock-own-test-send" {
			t.Errorf("ClosestMock mutated to %q, want mock-own-test-send", got.ClosestMock)
		}
	})

	t.Run("negative: payload diff even with reused evicted mock name gets ZERO eviction note", func(t *testing.T) {
		got := drainReport(t, &models.MockMismatchReport{
			Protocol:      string(pulsarKind),
			ActualSummary: "SEND persistent://tenant/ns/orders",
			ClosestMock:   "mock-evicted",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Expected: "1", Actual: "2"}},
		})
		if strings.Contains(strings.ToLower(got.NextSteps), "evict") || strings.Contains(strings.ToLower(got.NextSteps), "carry-over") {
			t.Errorf("payload diff with reused name must NOT be blamed on carry-over eviction: %q", got.NextSteps)
		}
	})

	t.Run("negative: non-evicted ClosestMock with no FieldDiffs gets ZERO eviction note", func(t *testing.T) {
		got := drainReport(t, &models.MockMismatchReport{
			Protocol:      string(pulsarKind),
			ActualSummary: "SEND persistent://tenant/ns/orders",
			ClosestMock:   "mock-other-not-evicted",
			MatchPhase:    models.MatchPhaseNoMocks,
		})
		if strings.Contains(strings.ToLower(got.NextSteps), "evict") {
			t.Errorf("miss whose ClosestMock was NOT evicted must NOT carry eviction note: %q", got.NextSteps)
		}
	})

	t.Run("positive: exact evicted ID gets definitive eviction note", func(t *testing.T) {
		got := drainReport(t, &models.MockMismatchReport{
			Protocol:      string(pulsarKind),
			ActualSummary: models.MockSummaryFromSpec(evicted),
			ClosestMock:   "mock-evicted",
			MatchPhase:    models.MatchPhaseNoMocks,
		})
		if !strings.Contains(got.NextSteps, "The recorded mock mock-evicted was evicted from the carry-over tier") {
			t.Errorf("exact evicted ID NextSteps = %q, want definitive eviction note", got.NextSteps)
		}
	})

	t.Run("negative: no-candidate miss without a mock ID says NOTHING about eviction", func(t *testing.T) {
		got := drainReport(t, &models.MockMismatchReport{
			Protocol:      string(pulsarKind),
			ActualSummary: "SEND persistent://tenant/ns/orders",
			MatchPhase:    models.MatchPhaseNoMocks,
		})
		if strings.Contains(strings.ToLower(got.NextSteps), "evict") || strings.Contains(strings.ToLower(got.NextSteps), "carry-over") {
			t.Errorf("no-candidate miss without mock ID must NOT claim eviction: %q", got.NextSteps)
		}
		if got.ClosestMock != "" {
			t.Errorf("no-candidate miss must keep ClosestMock empty, got %q", got.ClosestMock)
		}
	})

	// Overflow again in a SECOND test set (ResetForReplaySession + new staging):
	// still zero mid-run Warns, and run-end Close() emits ONE Warn with the
	// run-wide total across BOTH sets (2 from set 1 + 2 from set 2 = 4).
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	set2Keep := brokerMockAt("set2-kept", pulsarKind, "SEND", 1200)
	set2Drop1 := brokerMockAt("set2-evict-1", pulsarKind, "SEND", 1500)
	set2Drop2 := brokerMockAt("set2-evict-2", pulsarKind, "SEND", 1800)
	mm.SetMocksWithWindow([]*models.Mock{set2Keep, set2Drop1, set2Drop2}, nil, winStart(1), winEnd(1))
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("expected 0 mid-run Warn lines during set 2, got %d: %v", len(warns), warns)
	}

	// End of run: emit once (calling LogCarryOverOverflowSummary again is idempotent).
	mm.Close()
	mm.LogCarryOverOverflowSummary()
	warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 run-end Warn across 2 sets, got %d: %v", len(warns), logs.All())
	}
	if got := warns[0].ContextMap()["leftOut"]; got != int64(4) {
		t.Errorf("run-wide leftOut = %v, want 4 (2 from set 1 + 2 from set 2)", got)
	}
	if ns, _ := warns[0].ContextMap()["next_step"].(string); !strings.Contains(ns, carryOverCapEnv) {
		t.Errorf("next_step = %q, want %s", ns, carryOverCapEnv)
	}
}

// Under parallel test workers (ServeInstance), each worker owns a MockManager
// of its own. Worker carry-over overflows must accumulate into the run
// manager's counter, worker Close (via StopServingInstance or endInstances)
// must emit ZERO carry-over overflow Warns, and the run manager must emit
// EXACTLY ONE Warn at run end with the combined run + workers leftOut total.
func TestCarryOverOverflowAggregatesAcrossWorkersSingleRunWarn(t *testing.T) {
	registerSends(t)
	prev := carryOverCapBytes
	t.Cleanup(func() { carryOverCapBytes = prev })
	one := approxMockBytes(brokerMockAt("sx", pulsarKind, "SEND", 0))
	carryOverCapBytes = one + one/2 // room for only one mock per manager

	t.Run("multi-worker concurrent overflow aggregates into single run-end Warn", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		logger := zap.New(core)
		p := New(logger, nil, config.New())
		run := p.shareWindows(NewMockManager(nil, nil, logger))
		p.setMockManager(run)
		t.Cleanup(func() {
			p.endInstances()
			run.Close()
		})
		p.SeedRecordedWindows(carryWindows())

		if _, err := p.ServeInstance(1); err != nil {
			t.Fatalf("ServeInstance(1): %v", err)
		}
		if _, err := p.ServeInstance(2); err != nil {
			t.Fatalf("ServeInstance(2): %v", err)
		}
		w1 := p.instanceManager(1)
		w2 := p.instanceManager(2)
		if w1 == nil || w2 == nil {
			t.Fatalf("expected non-nil worker managers, got w1=%v w2=%v", w1, w2)
		}

		// Prime startup windows on each manager before concurrent overflows.
		for _, mm := range []*MockManager{run, w1, w2} {
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			// run: 1 kept + 1 leftOut in win 1, then +1 leftOut in win 2 => 2
			run.SetMocksWithWindow([]*models.Mock{
				brokerMockAt("run-keep", pulsarKind, "SEND", 1200),
				brokerMockAt("run-drop-1", pulsarKind, "SEND", 1500),
			}, nil, winStart(1), winEnd(1))
			run.SetMocksWithWindow([]*models.Mock{
				brokerMockAt("run-drop-2", pulsarKind, "SEND", 2500),
			}, nil, winStart(2), winEnd(2))
		}()
		go func() {
			defer wg.Done()
			// w1: 1 kept + 2 leftOut => 2
			w1.SetMocksWithWindow([]*models.Mock{
				brokerMockAt("w1-keep", pulsarKind, "SEND", 1200),
				brokerMockAt("w1-drop-1", pulsarKind, "SEND", 1500),
				brokerMockAt("w1-drop-2", pulsarKind, "SEND", 1800),
			}, nil, winStart(1), winEnd(1))
		}()
		go func() {
			defer wg.Done()
			// w2: 1 kept + 3 leftOut => 3
			w2.SetMocksWithWindow([]*models.Mock{
				brokerMockAt("w2-keep", pulsarKind, "SEND", 1200),
				brokerMockAt("w2-drop-1", pulsarKind, "SEND", 1500),
				brokerMockAt("w2-drop-2", pulsarKind, "SEND", 1700),
				brokerMockAt("w2-drop-3", pulsarKind, "SEND", 1900),
			}, nil, winStart(1), winEnd(1))
		}()
		wg.Wait()

		// Zero Warns mid-run.
		if warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
			t.Fatalf("expected 0 mid-run Warns, got %d: %v", len(warns), warns)
		}

		// Closing worker 1 via StopServingInstance emits ZERO Warns.
		if err := p.StopServingInstance(1); err != nil {
			t.Fatalf("StopServingInstance(1): %v", err)
		}
		if warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
			t.Fatalf("expected 0 Warns after StopServingInstance(1), got %d: %v", len(warns), warns)
		}

		// Closing remaining worker 2 via endInstances emits ZERO Warns.
		p.endInstances()
		if warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
			t.Fatalf("expected 0 Warns after endInstances, got %d: %v", len(warns), warns)
		}

		// Closing run manager emits EXACTLY ONE Warn with combined leftOut = 2 + 2 + 3 = 7.
		run.Close()
		warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All()
		if len(warns) != 1 {
			t.Fatalf("expected exactly 1 run-end Warn, got %d: %v", len(warns), logs.All())
		}
		if got := warns[0].ContextMap()["leftOut"]; got != int64(7) {
			t.Errorf("combined leftOut = %v, want 7 (run=2 + w1=2 + w2=3)", got)
		}
		if got := warns[0].ContextMap()["capBytes"]; got != int64(carryOverCapBytes) {
			t.Errorf("capBytes = %v, want %d", got, carryOverCapBytes)
		}
		if ns, _ := warns[0].ContextMap()["next_step"].(string); !strings.Contains(ns, carryOverCapEnv) {
			t.Errorf("next_step = %q, want %s", ns, carryOverCapEnv)
		}

		// Idempotent: a subsequent LogCarryOverOverflowSummary call emits nothing more.
		run.LogCarryOverOverflowSummary()
		if warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 1 {
			t.Fatalf("expected still 1 Warn after repeated summary call, got %d", len(warns))
		}
	})

	t.Run("worker-only overflow emits zero worker Warns and single run-end Warn", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		logger := zap.New(core)
		p := New(logger, nil, config.New())
		run := p.shareWindows(NewMockManager(nil, nil, logger))
		p.setMockManager(run)
		t.Cleanup(func() {
			p.endInstances()
			run.Close()
		})
		p.SeedRecordedWindows(carryWindows())

		if _, err := p.ServeInstance(1); err != nil {
			t.Fatalf("ServeInstance(1): %v", err)
		}
		w1 := p.instanceManager(1)
		if w1 == nil {
			t.Fatal("expected non-nil worker manager 1")
		}
		// Run manager itself never overflows; only worker 1 overflows 2 mocks.
		w1.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		w1.SetMocksWithWindow([]*models.Mock{
			brokerMockAt("wo-keep", pulsarKind, "SEND", 1200),
			brokerMockAt("wo-drop-1", pulsarKind, "SEND", 1500),
			brokerMockAt("wo-drop-2", pulsarKind, "SEND", 1800),
		}, nil, winStart(1), winEnd(1))

		// Closing worker emits ZERO Warns even though worker had overflow.
		p.endInstances()
		if warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
			t.Fatalf("expected 0 Warns when closing worker with overflow, got %d: %v", len(warns), warns)
		}

		// Run manager emits the worker's overflow total (2) at run end.
		run.Close()
		warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All()
		if len(warns) != 1 {
			t.Fatalf("expected 1 run-end Warn for worker-only overflow, got %d: %v", len(warns), logs.All())
		}
		if got := warns[0].ContextMap()["leftOut"]; got != int64(2) {
			t.Errorf("run-end leftOut = %v, want 2", got)
		}
		if ns, _ := warns[0].ContextMap()["next_step"].(string); !strings.Contains(ns, carryOverCapEnv) {
			t.Errorf("next_step = %q, want %s", ns, carryOverCapEnv)
		}
	})

	t.Run("self-sharing is a no-op and preserves standalone run-end Warn", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		logger := zap.New(core)
		mm := NewMockManager(nil, nil, logger)
		defer mm.Close()
		mm.shareCarryOverOverflow(mm)
		mm.shareCarryOverOverflow(nil)
		mm.SeedRecordedWindows(carryWindows())
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow([]*models.Mock{
			brokerMockAt("solo-keep", pulsarKind, "SEND", 1200),
			brokerMockAt("solo-drop", pulsarKind, "SEND", 1500),
		}, nil, winStart(1), winEnd(1))
		mm.Close()
		warns := logs.FilterMessageSnippet("carry-over tier is full").FilterLevelExact(zapcore.WarnLevel).All()
		if len(warns) != 1 {
			t.Fatalf("expected 1 Warn on standalone manager, got %d: %v", len(warns), logs.All())
		}
		if got := warns[0].ContextMap()["leftOut"]; got != int64(1) {
			t.Errorf("standalone leftOut = %v, want 1", got)
		}
	})
}
