package replay

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Outside-test misses emit ONE Info summary line per set (never a per-call
// Warn loop) whose next_step points to the misses file and whose next_steps
// say each kind's own next step, and per-call detail at Debug.
func TestOutsideTestMissesSingleInfoSummary(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	r := &Replayer{logger: zap.New(core)}

	calls := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /intake", Destination: "apm.internal:8200", BeforeSet: true},
		{Protocol: "HTTP", ActualSummary: "POST /intake", Destination: "apm.internal:8200", BeforeSet: true},
		{Protocol: "HTTP", ActualSummary: "GET /config", Destination: "meta.internal:80", BeforeSet: true},
		{Protocol: "DNS", ActualSummary: "A db.example.", BeforeSet: true},
	}
	misses := models.OutsideTestMissesOf(calls)
	const file = "keploy/test-set-0/last-test-misses.yaml"
	r.logOutsideTestMisses("test-set-0", misses, file)

	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Errorf("expected 0 Warn lines for outside-test misses, got %d: %v", len(warns), warns)
	}
	infos := logs.FilterLevelExact(zapcore.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("expected 1 Info summary line, got %d: %v", len(infos), infos)
	}
	msg := infos[0].Message
	if !strings.Contains(msg, "4 calls outside every test got no recorded answer (fail nothing)") || !strings.Contains(msg, file) {
		t.Errorf("summary Info line = %q, want call count + misses file", msg)
	}
	ns := fmt.Sprint(infos[0].ContextMap()["next_step"])
	if !strings.Contains(ns, file) {
		t.Errorf("outside-test Info next_step = %q, want the misses file", ns)
	}
	// What to do is each kind's own next step, once: no recording answers
	// these calls, so the first is to record the set again (sessionCalls only
	// shares a call a recording holds); then the HTTP calls' bypassRules and
	// the lookup's allowEgress opt-ins.
	steps, _ := infos[0].ContextMap()["next_steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("outside-test Info next_steps = %v, want the HTTP calls' and the lookup's", infos[0].ContextMap()["next_steps"])
	}
	call, lookup := fmt.Sprint(steps[0]), fmt.Sprint(steps[1])
	if call != misses[0].NextStep || !strings.Contains(call, "record the test set again") || !strings.Contains(call, "bypassRules") {
		t.Errorf("next_steps[0] = %q, want the HTTP calls' next step (record again, bypassRules)", call)
	}
	if lookup != misses[len(misses)-1].NextStep || !strings.Contains(lookup, "record the test set again") || !strings.Contains(lookup, "allowEgress") {
		t.Errorf("next_steps[1] = %q, want the lookup's next step (record again, allowEgress)", lookup)
	}
	dbgs := logs.FilterLevelExact(zapcore.DebugLevel).All()
	if len(dbgs) != len(misses) {
		t.Errorf("expected %d Debug detail lines, got %d", len(misses), len(dbgs))
	}
}

// warnUnexercisedDependencies counts only PASSED tests and emits nothing
// when 0 PASSED tests have unexercised dependencies, and folds PASSED tests
// already covered by the outgoing-call diff table.
func TestWarnUnexercisedDependenciesPassedOnlyAndSilentAtZero(t *testing.T) {
	t.Run("silent when only failed or obsolete tests", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Replayer{logger: zap.New(core)}
		r.warnUnexercisedDependencies("test-set-0", map[string]models.TestStatus{
			"tc-fail":     models.TestStatusFailed,
			"tc-obsolete": models.TestStatusObsolete,
		})
		if logs.Len() != 0 {
			t.Errorf("expected 0 Warn lines when 0 PASSED tests have unexercised deps, got %d: %v", logs.Len(), logs.All())
		}
	})

	t.Run("mixed counts only passed", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Replayer{logger: zap.New(core)}
		r.warnUnexercisedDependencies("test-set-0", map[string]models.TestStatus{
			"tc-pass":     models.TestStatusPassed,
			"tc-fail":     models.TestStatusFailed,
			"tc-obsolete": models.TestStatusObsolete,
		})
		if logs.Len() != 1 {
			t.Fatalf("expected 1 Warn line, got %d", logs.Len())
		}
		fields := logs.All()[0].ContextMap()
		if fields["tests"] != int64(1) {
			t.Errorf("tests = %v, want 1 (only PASSED tests)", fields["tests"])
		}
	})

	t.Run("folded cleanly when unexercised mock matches missed ClosestMock", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		store := NewTestFailureStore()
		store.AddUnmatchedCallForTest("test-set-0", "tc-pass", models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/orders",
			ClosestMock:   "mock-pulsar-1",
			MatchPhase:    models.MatchPhaseBody,
		})
		r := &Replayer{
			logger:                 zap.New(core),
			mockMismatchFailures:   store,
			unexercisedMocksByTest: map[string][]string{"tc-pass": {"mock-pulsar-1"}},
		}
		r.warnUnexercisedDependencies("test-set-0", map[string]models.TestStatus{
			"tc-pass": models.TestStatusPassed,
		})
		if logs.Len() != 0 {
			t.Errorf("expected 0 duplicate Warn lines when unexercised mock matches missed ClosestMock, got %d: %v", logs.Len(), logs.All())
		}
	})

	t.Run("not folded when a different mock went unexercised in same test", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		store := NewTestFailureStore()
		store.AddUnmatchedCallForTest("test-set-0", "tc-pass", models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/orders",
			ClosestMock:   "mock-pulsar-1",
			MatchPhase:    models.MatchPhaseBody,
		})
		r := &Replayer{
			logger:                 zap.New(core),
			mockMismatchFailures:   store,
			unexercisedMocksByTest: map[string][]string{"tc-pass": {"mock-pulsar-1", "mock-other-db"}},
		}
		r.warnUnexercisedDependencies("test-set-0", map[string]models.TestStatus{
			"tc-pass": models.TestStatusPassed,
		})
		if logs.Len() != 1 {
			t.Fatalf("expected 1 Warn line so unexercised mock-other-db is not hidden, got %d", logs.Len())
		}
	})
}

// sayKeptInside says the run's kept-inside destinations in ONE Info line with
// the destination count, the call count, and every destination (kept), none
// left out however many there are, and no per-destination line at any level.
func TestKeptInsideSingleInfoLine(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	r := &Replayer{logger: zap.New(core)}
	for i := 0; i < 7; i++ {
		r.keptInside.add([]models.UnmatchedCall{
			{Protocol: "HTTP", ActualSummary: "POST /a", Destination: fmt.Sprintf("198.18.0.%d:80", i+1)},
		})
	}
	r.sayKeptInside()

	if n := logs.Len(); n != 1 {
		t.Fatalf("expected 1 line from sayKeptInside, got %d: %v", n, logs.All())
	}
	infos := logs.FilterLevelExact(zapcore.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("expected 1 Info line from sayKeptInside, got %d: %v", len(infos), infos)
	}
	fields := infos[0].ContextMap()
	if fields["destinations"] != int64(7) {
		t.Errorf("destinations = %v, want 7", fields["destinations"])
	}
	kept, ok := fields["kept"].([]any)
	if !ok || len(kept) != 7 {
		t.Fatalf("kept = %#v (%T), want all 7 destinations", fields["kept"], fields["kept"])
	}
	for i, k := range kept {
		if want := fmt.Sprintf("HTTP 198.18.0.%d:80 (1)", i+1); fmt.Sprint(k) != want {
			t.Errorf("kept[%d] = %v, want %s", i, k, want)
		}
	}
}

// Default-mode outgoing-call diffs: tests stay PASSED (even under
// --disable-mapping!) and each unique (test, call, fields) diff is printed ONCE
// in one clear table with xN count on the console.
func TestDefaultModeOutgoingDiffTablePrintedOnce(t *testing.T) {
	for _, disableMapping := range []bool{false, true} {
		t.Run(fmt.Sprintf("disableMapping=%v", disableMapping), func(t *testing.T) {
			provenOn(t, false)
			h := newPartialRunHarness(t, 2, 0)
			h.cases[0].Name = "tc-1"
			h.cases[1].Name = "tc-2"
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableMapping = disableMapping

			var consoleBuf bytes.Buffer
			h.replayer.console = &consoleBuf

			// Duplicate outgoing-call payload diff in tc-1 (x2), distinct diff in tc-2 (x1).
			diff1 := models.UnmatchedCall{
				Protocol:      "Pulsar",
				ActualSummary: "SEND persistent://tenant/ns/orders",
				Destination:   "broker:6650",
				MatchPhase:    models.MatchPhaseBody,
				FieldDiffs: []models.MockFieldDiff{
					{Path: "body.task.context", Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"},
					{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "12345678901"},
				},
			}
			diff2 := models.UnmatchedCall{
				Protocol:      "HTTP",
				ActualSummary: "POST /v1/notify",
				Destination:   "notify.internal:8080",
				MatchPhase:    models.MatchPhaseBody,
				FieldDiffs: []models.MockFieldDiff{
					{Path: "body.amount", Kind: models.DiffKindValueChanged, Expected: "10", Actual: "20"},
				},
			}
			h.instr.mockErrorsFor = misses(reads(diff1, diff1), reads(diff2))

			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)

			if utils.ErrCode != 0 {
				t.Fatalf("default mode outgoing-call payload diff must not fail tests, got exit code %d", utils.ErrCode)
			}
			out := consoleBuf.String()
			for _, want := range []string{"TEST", "CALL", "COUNT", "FIELDS", "tc-1", "SEND persistent://tenant/ns/orders", "x2", "body.id", "body.task.context", "tc-2", "POST /v1/notify", "x1", "body.amount"} {
				if !strings.Contains(out, want) {
					t.Errorf("console outgoing-diff table missing %q:\n%s", want, out)
				}
			}
			if n := strings.Count(out, "SEND persistent://tenant/ns/orders"); n != 1 {
				t.Errorf("duplicate outgoing diff printed %d times, want 1:\n%s", n, out)
			}
			for _, e := range logs.All() {
				if strings.Contains(strings.ToLower(e.Message), "failing test") {
					t.Errorf("default mode passing run must not say 'failing test': %q", e.Message)
				}
			}
		})
	}
}

// Strict-mode (KEPLOY_PROVEN=1) twin of TestDefaultModeOutgoingDiffTablePrintedOnce:
// the EXACT same outgoing-call payload diffs MUST FAIL tests and exit non-zero.
func TestStrictModeOutgoingDiffFailsTests(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.cases[0].Name = "tc-1"
	h.cases[1].Name = "tc-2"
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1

	var consoleBuf bytes.Buffer
	h.replayer.console = &consoleBuf

	diff1 := models.UnmatchedCall{
		Protocol:      "Pulsar",
		ActualSummary: "SEND persistent://tenant/ns/orders",
		Destination:   "broker:6650",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs: []models.MockFieldDiff{
			{Path: "body.task.context", Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"},
			{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "12345678901"},
		},
	}
	diff2 := models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /v1/notify",
		Destination:   "notify.internal:8080",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs: []models.MockFieldDiff{
			{Path: "body.amount", Kind: models.DiffKindValueChanged, Expected: "10", Actual: "20"},
		},
	}
	h.instr.mockErrorsFor = misses(reads(diff1, diff1), reads(diff2))

	core, logs := observer.New(zapcore.InfoLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)

	if utils.ErrCode == 0 {
		t.Fatalf("strict mode (KEPLOY_PROVEN=1) MUST fail tests on outgoing-call diffs, got exit code 0")
	}
	if len(h.replayer.failedTCsBySetID["test-set-0"]) != 2 {
		t.Fatalf("strict mode failed TCs = %v, want both tc-1 and tc-2", h.replayer.failedTCsBySetID["test-set-0"])
	}
	if n := logs.FilterMessage("Some testsets failed due to mock differences.").Len(); n != 1 {
		t.Errorf("strict mode failed on mocks: expected 1 'Some testsets failed due to mock differences.' line, got %d", n)
	}
	out := consoleBuf.String()
	if !strings.Contains(out, "=========================== MOCK MISMATCH ===========================") {
		t.Errorf("strict mode failed tests must print MOCK MISMATCH block:\n%s", out)
	}
	if !strings.Contains(out, "(occurred x2)") {
		t.Errorf("duplicated failed-test diff should be deduped with (occurred x2):\n%s", out)
	}
}

// Mixed default-mode run (1 test fails purely on HTTP response diff + 2 passing
// tests with outgoing diffs, including a duplicated diff):
//   - prints ONE OUTGOING CALL DIFFS table with the 2 passing rows (with x2/x1);
//   - dumps ZERO passing diffs in a failed MOCK MISMATCH block;
//   - NEVER logs "Some testsets failed due to mock differences.";
//   - records the response diff as the failure cause for the failed test.
func TestMixedRunResponseFailureAndPassingOutgoingDiffs(t *testing.T) {
	t.Run("response-only failure + 2 passing outgoing diffs", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 3, 0)
		h.cases[0].Name = "tc-fail-resp"
		h.cases[1].Name = "tc-pass-pulsar"
		h.cases[2].Name = "tc-pass-http"
		for _, tc := range h.cases {
			tc.HTTPResp.StatusCode = 200
			tc.HTTPResp.Body = `{"ok":true}`
		}
		h.replayer.hookImpl = statusHooks{
			respByTest: map[string]*models.HTTPResp{
				"tc-fail-resp":   {StatusCode: 500, Body: `{"error":"response broke"}`},
				"tc-pass-pulsar": {StatusCode: 200, Body: `{"ok":true}`},
				"tc-pass-http":   {StatusCode: 200, Body: `{"ok":true}`},
			},
		}
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf

		pulsarDiff := models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/events",
			Destination:   "broker:6650",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Expected: "1", Actual: "2"}},
		}
		httpDiff := models.UnmatchedCall{
			Protocol:      "HTTP",
			ActualSummary: "POST /v1/audit",
			Destination:   "audit.internal:8080",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.ts", Expected: "100", Actual: "200"}},
		}
		dnsNoise := models.UnmatchedCall{
			Protocol:      "DNS",
			ActualSummary: "A noise.internal.",
			MatchPhase:    models.MatchPhaseNoMocks,
		}
		// tc-fail-resp has ZERO counting mock diffs (only non-counting DNS noise);
		// tc-pass-pulsar has 2 identical Pulsar diffs; tc-pass-http has 1 HTTP diff.
		h.instr.mockErrorsFor = misses(reads(dnsNoise), reads(pulsarDiff, pulsarDiff), reads(httpDiff))

		core, logs := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)
		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		if utils.ErrCode == 0 {
			t.Fatalf("expected non-zero exit code for response-diff failure")
		}
		// Must NOT blame mocks for a response-diff failure.
		if mockBlame := logs.FilterMessage("Some testsets failed due to mock differences.").All(); len(mockBlame) != 0 {
			t.Errorf("response-only failure must NOT log 'Some testsets failed due to mock differences.': %v", mockBlame)
		}
		out := consoleBuf.String()
		if strings.Contains(out, "=========================== MOCK MISMATCH ===========================") {
			t.Errorf("passing diffs must NOT be dumped in a MOCK MISMATCH failed block:\n%s", out)
		}
		if n := strings.Count(out, "=========================== OUTGOING CALL DIFFS ==========================="); n != 1 {
			t.Fatalf("expected 1 OUTGOING CALL DIFFS table, got %d:\n%s", n, out)
		}
		tableIdx := strings.Index(out, "=========================== OUTGOING CALL DIFFS ===========================")
		tableSection := out[tableIdx:]
		for _, want := range []string{"tc-pass-pulsar", "SEND persistent://tenant/ns/events", "x2", "tc-pass-http", "POST /v1/audit", "x1"} {
			if !strings.Contains(tableSection, want) {
				t.Errorf("OUTGOING CALL DIFFS table missing %q:\n%s", want, tableSection)
			}
		}
		if strings.Contains(tableSection, "tc-fail-resp") || strings.Contains(tableSection, "noise.internal") {
			t.Errorf("OUTGOING CALL DIFFS table leaked failed test or DNS noise:\n%s", tableSection)
		}
		// Verify console output before the table contains the response diff for tc-fail-resp.
		if !strings.Contains(out[:tableIdx], "EXPECT STATUS") || !strings.Contains(out[:tableIdx], "response broke") {
			t.Errorf("console output missing response diff for tc-fail-resp:\n%s", out[:tableIdx])
		}
		// Verify tc-fail-resp failed on its response diff (status 200 -> 500).
		var failRes *models.TestResult
		for i := range h.report.results {
			if h.report.results[i].TestCaseID == "tc-fail-resp" {
				failRes = &h.report.results[i]
			}
		}
		if failRes == nil || failRes.Status != models.TestStatusFailed || failRes.Result.StatusCode.Normal {
			t.Fatalf("tc-fail-resp must record response status diff failure, got: %+v", failRes)
		}
	})

	t.Run("failed test with its own mock diff + passing test with outgoing diff stay separated", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 2, 0)
		h.cases[0].Name = "tc-fail-mock"
		h.cases[1].Name = "tc-pass-diff"
		h.cases[0].HTTPResp.StatusCode = 200
		h.cases[0].HTTPResp.Body = `{"ok":true}`
		h.cases[1].HTTPResp.StatusCode = 200
		h.cases[1].HTTPResp.Body = `{"ok":true}`
		h.replayer.hookImpl = statusHooks{
			respByTest: map[string]*models.HTTPResp{
				"tc-fail-mock": {StatusCode: 500, Body: `{"error":"upstream miss"}`},
				"tc-pass-diff": {StatusCode: 200, Body: `{"ok":true}`},
			},
		}
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf

		failMiss := models.UnmatchedCall{
			Protocol:      "HTTP",
			ActualSummary: "POST /v1/auth",
			Destination:   "auth.internal:8080",
			MatchPhase:    models.MatchPhaseSchema,
		}
		passDiff := models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/orders",
			Destination:   "broker:6650",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Expected: "1", Actual: "2"}},
		}
		h.instr.mockErrorsFor = misses(reads(failMiss, failMiss), reads(passDiff))

		core, logs := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)
		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		// Default mode must NEVER say "Some testsets failed due to mock
		// differences." — the verdict is the response diff, so the header
		// points to each test's response diff neutrally.
		if n := logs.FilterMessage("Some testsets failed due to mock differences.").Len(); n != 0 {
			t.Errorf("default mode must NOT log 'Some testsets failed due to mock differences.', got %d", n)
		}
		if n := logs.FilterMessageSnippet("see each test's response diff for why it failed").Len(); n != 1 {
			t.Errorf("expected 1 neutral failed-test mock header pointing to response diff, got %d: %v", n, logs.All())
		}
		out := consoleBuf.String()
		mockIdx := strings.Index(out, "=========================== MOCK MISMATCH ===========================")
		tableIdx := strings.Index(out, "=========================== OUTGOING CALL DIFFS ===========================")
		if mockIdx < 0 || tableIdx < 0 {
			t.Fatalf("expected both MOCK MISMATCH (failed test) and OUTGOING CALL DIFFS (passing test):\n%s", out)
		}
		failedSection := out[mockIdx:tableIdx]
		passingSection := out[tableIdx:]
		if !strings.Contains(failedSection, "tc-fail-mock") || !strings.Contains(failedSection, "(occurred x2)") {
			t.Errorf("MOCK MISMATCH section missing deduped tc-fail-mock:\n%s", failedSection)
		}
		if strings.Contains(failedSection, "tc-pass-diff") {
			t.Errorf("passing test tc-pass-diff leaked into MOCK MISMATCH failed block:\n%s", failedSection)
		}
		if !strings.Contains(passingSection, "tc-pass-diff") || strings.Contains(passingSection, "tc-fail-mock") {
			t.Errorf("OUTGOING CALL DIFFS section should contain only tc-pass-diff:\n%s", passingSection)
		}
	})

	t.Run("default-mode response failure with payload diff names response diff and keeps payload diff as table row", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 2, 0)
		h.cases[0].Name = "tc-fail-tz"
		h.cases[1].Name = "tc-pass-tz"
		h.cases[0].HTTPResp.StatusCode = 200
		h.cases[0].HTTPResp.Body = `{"updatedAt":"2026-11-24T17:30:00+05:30"}`
		h.cases[1].HTTPResp.StatusCode = 200
		h.cases[1].HTTPResp.Body = `{"ok":true}`
		h.replayer.hookImpl = statusHooks{
			respByTest: map[string]*models.HTTPResp{
				"tc-fail-tz": {StatusCode: 200, Body: `{"updatedAt":"2026-11-24T12:00:00Z"}`},
				"tc-pass-tz": {StatusCode: 200, Body: `{"ok":true}`},
			},
		}
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf

		failPayloadDiff := models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/events",
			Destination:   "broker:6650",
			ClosestMock:   "mock-pulsar-send-1",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.timestamp", Expected: "2026-11-24T17:30:00+05:30", Actual: "2026-11-24T12:00:00Z"}},
		}
		passPayloadDiff := models.UnmatchedCall{
			Protocol:      "Pulsar",
			ActualSummary: "SEND persistent://tenant/ns/audit",
			Destination:   "broker:6650",
			ClosestMock:   "mock-pulsar-send-2",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Expected: "1", Actual: "2"}},
		}
		h.instr.mockErrorsFor = misses(reads(failPayloadDiff, failPayloadDiff), reads(passPayloadDiff))

		core, logs := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)
		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		if utils.ErrCode == 0 {
			t.Fatalf("expected non-zero exit code for response body diff failure")
		}
		if mockBlame := logs.FilterMessage("Some testsets failed due to mock differences.").All(); len(mockBlame) != 0 {
			t.Errorf("default-mode response failure + payload diff must NOT say 'Some testsets failed due to mock differences.': %v", mockBlame)
		}
		if warns := logs.FilterMessageSnippet("see each failed test's response diff for why it failed").FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 1 {
			t.Errorf("expected 1 Warn pointing to failed test's response diff above table, got %d: %v", len(warns), logs.All())
		}
		if passingLabel := logs.FilterMessageSnippet("Passing tests had outgoing calls").All(); len(passingLabel) != 0 {
			t.Errorf("table containing a failed test's payload diff must NOT be labelled 'Passing tests': %v", passingLabel)
		}
		out := consoleBuf.String()
		if strings.Contains(out, "=========================== MOCK MISMATCH ===========================") {
			t.Errorf("default-mode payload diffs must stay table rows, not verbose MOCK MISMATCH boxes:\n%s", out)
		}
		tableIdx := strings.Index(out, "=========================== OUTGOING CALL DIFFS ===========================")
		if tableIdx < 0 {
			t.Fatalf("missing OUTGOING CALL DIFFS table:\n%s", out)
		}
		// Response diff printed before table showing why tc-fail-tz failed.
		if !strings.Contains(out[:tableIdx], "2026-11-24T12:00:00Z") {
			t.Errorf("console output missing response diff for tc-fail-tz:\n%s", out[:tableIdx])
		}
		tableSection := out[tableIdx:]
		for _, want := range []string{"tc-fail-tz", "SEND persistent://tenant/ns/events", "x2", "body.timestamp", "tc-pass-tz", "SEND persistent://tenant/ns/audit", "x1", "body.id"} {
			if !strings.Contains(tableSection, want) {
				t.Errorf("OUTGOING CALL DIFFS table missing %q:\n%s", want, tableSection)
			}
		}
	})
}

// PrintOutgoingDiffTable caps long raw Diff fallbacks, keys on the uncapped
// diff text (so two long diffs sharing their first 120 runes do NOT merge),
// and never merges rows whose call summary is empty.
func TestPrintOutgoingDiffTableCapAndNoEmptyCallMerge(t *testing.T) {
	store := NewTestFailureStore()
	longPrefix := strings.Repeat("field_diff_segment_", 10) // > 120 runes
	longDiffA := longPrefix + "TAIL_ALPHA"
	longDiffB := longPrefix + "TAIL_BETA"
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /v1/big",
		MatchPhase:    models.MatchPhaseBody,
		Diff:          longDiffA,
	})
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /v1/big",
		MatchPhase:    models.MatchPhaseBody,
		Diff:          longDiffB,
	})
	// Two distinct misses with empty call summaries must NOT collapse into one row.
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:   "Generic",
		MatchPhase: models.MatchPhaseExhausted,
	})
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:   "Generic",
		MatchPhase: models.MatchPhaseExhausted,
	})

	var buf bytes.Buffer
	store.PrintOutgoingDiffTable(&buf, store.GetFailures())
	out := buf.String()
	if !strings.Contains(out, "... (see report)") {
		t.Errorf("long raw Diff cell was not capped with '... (see report)':\n%s", out)
	}
	if strings.Contains(out, longDiffA) || strings.Contains(out, longDiffB) {
		t.Errorf("full uncapped raw Diff appeared in table:\n%s", out)
	}
	if n := strings.Count(out, "POST /v1/big"); n != 2 {
		t.Errorf("two long raw diffs sharing first 120 runes wrongly merged: count = %d, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, "[Generic] (no summary)"); n != 2 {
		t.Errorf("empty-summary rows merged: count = %d, want 2 separate rows:\n%s", n, out)
	}
}

// PrintFailuresTableRows must NOT merge misses on the same call and field path
// when the field values or hints differ, while still deduplicating truly
// identical repeats with (occurred xN).
func TestFailedMismatchBlocksDoNotMergeDifferentValuesOrHints(t *testing.T) {
	store := NewTestFailureStore()
	// Two calls changing body.id to DIFFERENT values (2 vs 3): must stay 2 separate blocks.
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "Pulsar",
		ActualSummary: "SEND persistent://tenant/ns/orders",
		Destination:   "broker:6650",
		ClosestMock:   "mock-1",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "value-two"}},
		NextSteps:     "Hint A",
	})
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "Pulsar",
		ActualSummary: "SEND persistent://tenant/ns/orders",
		Destination:   "broker:6650",
		ClosestMock:   "mock-1",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "value-three"}},
		NextSteps:     "Hint A",
	})
	// Same field + values as first, but DIFFERENT NextSteps hint: must stay separate.
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "Pulsar",
		ActualSummary: "SEND persistent://tenant/ns/orders",
		Destination:   "broker:6650",
		ClosestMock:   "mock-1",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "value-two"}},
		NextSteps:     "Hint B (evicted)",
	})
	// Exact duplicate of the first miss (same call, field, value, hint): must collapse with x2.
	store.AddUnmatchedCallForTest("test-set-0", "tc-1", models.UnmatchedCall{
		Protocol:      "Pulsar",
		ActualSummary: "SEND persistent://tenant/ns/orders",
		Destination:   "broker:6650",
		ClosestMock:   "mock-1",
		MatchPhase:    models.MatchPhaseBody,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.id", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "value-two"}},
		NextSteps:     "Hint A",
	})

	var buf bytes.Buffer
	store.PrintFailuresTableRows(&buf, store.GetFailures())
	out := buf.String()
	if n := strings.Count(out, "Mock mismatch: [Pulsar] SEND persistent://tenant/ns/orders"); n != 3 {
		t.Fatalf("expected 3 distinct blocks (value-two+HintA x2, value-three+HintA x1, value-two+HintB x1), got %d:\n%s", n, out)
	}
	for _, want := range []string{"value-two", "value-three", "Hint A", "Hint B (evicted)", "(occurred x2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("failed mismatch output missing %q:\n%s", want, out)
		}
	}
}

// Runs whose only mismatches are mapping-divergence rows must drive the Start()
// branch to print PrintMappingDiffs (with clean header) and NOT leak
// non-counting DNS misses or blame mocks as failed tests.
func TestPassingRunWithMappingMismatchOnlyPrintsMappingDiffs(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 1, 0)
	h.cases[0].Name = "tc-obsolete"
	h.cases[0].HTTPResp.StatusCode = 200
	h.cases[0].HTTPResp.Body = `{"ok":true}`
	// Response differs AND consumed mocks differ from mapping -> OBSOLETE (mapping row recorded, 0 FAILED tests).
	h.replayer.hookImpl = statusHooks{
		prHooks: prHooks{
			consumed: []models.MockState{{Name: "mock-actual-2", Kind: models.HTTP, Usage: models.Deleted}},
		},
		respByTest: map[string]*models.HTTPResp{
			"tc-obsolete": {StatusCode: 500, Body: `{"err":"stale"}`},
		},
	}
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{
		"tc-obsolete": {{Name: "mock-expected-1", Kind: string(models.HTTP)}},
	}
	h.mocks.filtered = []*models.Mock{
		{Name: "mock-expected-1", Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest}},
		{Name: "mock-actual-2", Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest}},
	}
	// Also inject a non-counting DNS miss that must NOT leak into console output.
	h.instr.mockErrorsFor = misses(reads(models.UnmatchedCall{
		Protocol:      "DNS",
		ActualSummary: "A startup-noise.internal.",
		MatchPhase:    models.MatchPhaseNoMocks,
	}))
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1

	var consoleBuf bytes.Buffer
	h.replayer.console = &consoleBuf

	core, logs := observer.New(zapcore.InfoLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)

	if n := logs.FilterMessageSnippet("recorded mock mappings diverged.").Len(); n != 1 {
		t.Errorf("expected 1 mapping-diverged Warn line, got %d: %v", n, logs.All())
	}
	if n := logs.FilterMessage("Some testsets failed due to mock differences.").Len(); n != 0 {
		t.Errorf("obsolete mapping-only run must not say failed due to mock differences, got %d", n)
	}
	out := consoleBuf.String()
	for _, want := range []string{"MOCK MAPPING DIFFS", "tc-obsolete", "Expected mocks not consumed: mock-expected-1", "Unexpected mocks consumed: mock-actual-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("mapping-only output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "startup-noise.internal") || strings.Contains(out, "MOCK MISMATCH") {
		t.Errorf("mapping-only branch leaked non-counting DNS miss or MOCK MISMATCH block:\n%s", out)
	}
}
