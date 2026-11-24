package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// per-set Warns mid-run), attribute ONLY exact evicted mock IDs (never
// signature fallback without a mock ID, never payload diffs against a test's
// own candidate mock), and require Pulsar signatures to include the topic.
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

	t.Run("positive: Pulsar signature requires topic and never matches bare SEND", func(t *testing.T) {
		if got := normalizeCarryOverSig("Pulsar", "SEND"); got != "" {
			t.Errorf("Pulsar bare SEND sig = %q, want empty (must require topic)", got)
		}
		sigOrders := normalizeCarryOverSig("Pulsar", "SEND persistent://tenant/ns/orders")
		sigEvents := normalizeCarryOverSig("Pulsar", "SEND persistent://tenant/ns/events")
		if sigOrders == "" || sigEvents == "" || sigOrders == sigEvents {
			t.Errorf("Pulsar sigs must distinguish topics: orders=%q events=%q", sigOrders, sigEvents)
		}
		if got := carryOverMockSig(evicted); got != sigOrders {
			t.Errorf("carryOverMockSig(evicted) = %q, want %q", got, sigOrders)
		}
		bareMock := brokerMockAt("bare-pulsar", pulsarKind, "SEND", 1600)
		if got := carryOverMockSig(bareMock); got != "" {
			t.Errorf("carryOverMockSig(bare Pulsar SEND without topic) = %q, want empty", got)
		}
	})

	t.Run("positive: normalized HTTP signature matches full URL, empty path, and path-only summary", func(t *testing.T) {
		if got, want := normalizeCarryOverSig("HTTP", "POST http://auth.internal:8080/v3/oauth/token?x=1"), normalizeCarryOverSig("HTTP", "POST /v3/oauth/token"); got != want || got != "POST /v3/oauth/token" {
			t.Errorf("HTTP carry-over sig mismatch: full=%q path=%q", got, want)
		}
		if got, want := normalizeCarryOverSig("HTTP", "GET http://auth.internal:8080"), normalizeCarryOverSig("HTTP", "GET /"); got != want || got != "GET /" {
			t.Errorf("HTTP empty-path carry-over sig mismatch: full=%q path=%q", got, want)
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
