package replay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type statusHooks struct {
	prHooks
	respByTest map[string]*models.HTTPResp
}

func (s statusHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if r, ok := s.respByTest[tc.Name]; ok {
		return r, nil
	}
	return s.prHooks.SimulateRequest(ctx, tc, testSetID)
}

// A test request that exceeds test.apiTimeout must emit one clear ERROR line
// naming the test and test.apiTimeout with an actionable next_step (not two
// raw errors), and categorize the failed test result as APP_TIMEOUT rather
// than synthetic STATUS_CODE_CHANGED / SCHEMA_BROKEN.
func TestAppTimeoutSingleLineAndCategory(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	tc := &models.TestCase{
		Name: "post-events-1",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "POST",
			URL:    srv.URL + "/events",
			Header: map[string]string{"Content-Type": "application/json"},
			Body:   `{"id":1}`,
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"ok":true}`,
		},
	}

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	resp, err := pkg.SimulateHTTP(context.Background(), tc, "test-set-0", logger, pkg.SimulationConfig{
		APITimeout: 1,
	})
	if err == nil || resp != nil {
		t.Fatalf("expected timeout error, got resp=%v err=%v", resp, err)
	}
	sayNoAnswer(context.Background(), logger, tc.Name, err, "failed to simulate request")

	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 ERROR line on timeout, got %d: %v", len(errs), errs)
	}
	msg := errs[0].Message
	if !strings.Contains(msg, "the app did not answer "+tc.Name+" within test.apiTimeout (1s)") {
		t.Errorf("timeout line = %q, want it to name %s and test.apiTimeout (1s)", msg, tc.Name)
	}
	nextStep := fmt.Sprint(errs[0].ContextMap()["next_step"])
	if !strings.Contains(nextStep, "test.apiTimeout") {
		t.Errorf("timeout next_step = %q, want actionable test.apiTimeout guidance", nextStep)
	}

	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	res := r.CreateFailedTestResult(tc, "test-set-0", time.Now(), err)
	wantCats := []models.FailureCategory{models.AppTimeout}
	if len(res.FailureInfo.Category) != 1 || res.FailureInfo.Category[0] != wantCats[0] {
		t.Errorf("FailureInfo.Category = %v, want %v", res.FailureInfo.Category, wantCats)
	}
}

// A failed replay run must never log stopping Keploy with
// reason="replay completed successfully".
func TestFailedRunNeverLogsCompletedSuccessfully(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			h := newPartialRunHarness(t, 1, 0)
			h.cases[0].HTTPResp.StatusCode = 200
			h.cases[0].HTTPResp.Body = `{"ok":true}`
			h.replayer.hookImpl = statusHooks{
				respByTest: map[string]*models.HTTPResp{
					h.cases[0].Name: {StatusCode: 500, Body: `{"error":"boom"}`},
				},
			}
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1

			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)

			if utils.ErrCode == 0 {
				t.Fatalf("precondition failed: expected non-zero exit code for failed test")
			}
			stops := logs.FilterMessage("stopping Keploy").All()
			if len(stops) != 1 {
				t.Fatalf("expected 1 'stopping Keploy' line, got %d: %v", len(stops), stops)
			}
			reason := fmt.Sprint(stops[0].ContextMap()["reason"])
			if reason == runCompletedReason {
				t.Errorf("failed run logged stopping Keploy reason=%q, must not say completed successfully", reason)
			}
		})
	}
}

// finalStopReason must name the real cause when a run did not pass and never
// blame "test failures" when no test failed (unread evidence, earlier-set
// capture losses, earlier-set unmatched calls).
func TestFinalStopReasonNamesRealCauseWhenNoTestFailed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          stopReasonInput
		wantSubstr  string
		forbidTests bool
	}{
		{
			name:       "real test failure names test failures",
			in:         stopReasonInput{testRunResult: false, failedTests: 2},
			wantSubstr: runFailedReason,
		},
		{
			name:        "not judged set names unread evidence not test failures",
			in:          stopReasonInput{testRunResult: false, failedTests: 0, notJudgedSets: []string{"test-set-0"}},
			wantSubstr:  "test-set test-set-0 not judged",
			forbidTests: true,
		},
		{
			name:        "earlier-set losses name capture losses not test failures",
			in:          stopReasonInput{testRunResult: false, failedTests: 0, hasEarlierLosses: true},
			wantSubstr:  "capture losses",
			forbidTests: true,
		},
		{
			name:        "earlier-set unmatched calls name unmatched calls not test failures",
			in:          stopReasonInput{testRunResult: false, failedTests: 0, hasEarlierCalls: true},
			wantSubstr:  "unmatched calls decided after it ended",
			forbidTests: true,
		},
		{
			name:       "all passed keeps completed successfully",
			in:         stopReasonInput{testRunResult: true},
			wantSubstr: runCompletedReason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := finalStopReason(runCompletedReason, tc.in)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("finalStopReason = %q, want substring %q", got, tc.wantSubstr)
			}
			if tc.forbidTests && strings.Contains(got, "test failures") {
				t.Errorf("finalStopReason = %q, must not blame test failures when no test failed", got)
			}
		})
	}
}

// When a test fails 200 -> 500 because an outgoing call missed and that call
// was recorded in another test of the whole mock set, replay must:
//   - for heuristic endpoint matches (no matcher owner evidence): word
//     NextSteps honestly ("A call to the same endpoint was recorded in ..."),
//     and NOT overwrite ClosestMock or RecordedIn;
//   - for real owner evidence (NotOwned + ClosestMock): populate RecordedIn,
//     state "This call was recorded in ...", and include "(recorded in <test>)"
//     on the per-test ERROR line;
//   - lead the per-test ERROR with the real blame ("response 200 -> 500; the
//     call ... got no recorded answer") instead of blaming --strict-failure;
//   - derive the run's end next_step from the actual unmatched calls (advising
//     sessionCalls / re-recording, not "Add drifting dynamic fields as noise").
func TestFailureBlameWholeSetLookupAndDerivedNextStep(t *testing.T) {
	for _, tc := range []struct {
		name       string
		proven     bool
		realOwner  bool
		wantClause string
	}{
		{name: "heuristic/proven=false", proven: false, realOwner: false, wantClause: "A call to the same endpoint was recorded in post-events-1 (mock-3238)."},
		{name: "heuristic/proven=true", proven: true, realOwner: false, wantClause: "A call to the same endpoint was recorded in post-events-1 (mock-3238)."},
		{name: "real-owner/proven=false", proven: false, realOwner: true, wantClause: "This call was recorded in post-events-1 (mock-3238)."},
		{name: "real-owner/proven=true", proven: true, realOwner: true, wantClause: "This call was recorded in post-events-1 (mock-3238)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.proven)
			h := newPartialRunHarness(t, 2, 0)
			h.cases[0].Name = "post-events-1"
			h.cases[1].Name = "post-events-2"

			h.cases[0].HTTPResp.StatusCode = 200
			h.cases[0].HTTPResp.Body = `{"ok":true}`
			h.cases[1].HTTPResp.StatusCode = 200
			h.cases[1].HTTPResp.Body = `{"ok":true}`
			h.replayer.hookImpl = statusHooks{
				respByTest: map[string]*models.HTTPResp{
					"post-events-1": {StatusCode: 200, Body: `{"ok":true}`},
					"post-events-2": {StatusCode: 500, Body: `{"error":"token missing"}`},
				},
			}
			h.replayer.config.Test.StrictFailure = true
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1

			// Whole mock set includes mock-3238 (POST /v3/oauth/token) owned by post-events-1.
			t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
			h.cases[0].HTTPReq.Timestamp = t0
			h.cases[0].HTTPResp.Timestamp = t0.Add(time.Second)
			h.cases[1].HTTPReq.Timestamp = t0.Add(5 * time.Second)
			h.cases[1].HTTPResp.Timestamp = t0.Add(6 * time.Second)

			h.mocks.filtered = []*models.Mock{
				{
					Name: "mock-3238",
					Kind: models.HTTP,
					Spec: models.MockSpec{
						ReqTimestampMock: t0.Add(200 * time.Millisecond),
						HTTPReq: &models.HTTPReq{
							Method: "POST",
							URL:    "http://auth.internal:8080/v3/oauth/token",
							Header: map[string]string{"Host": "auth.internal:8080"},
						},
						HTTPResp: &models.HTTPResp{StatusCode: 200},
					},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
				},
				{
					Name: "mock-other",
					Kind: models.HTTP,
					Spec: models.MockSpec{
						ReqTimestampMock: t0.Add(5200 * time.Millisecond),
						HTTPReq: &models.HTTPReq{
							Method: "GET",
							URL:    "http://orders.internal:8080/v1/item",
							Header: map[string]string{"Host": "orders.internal:8080"},
						},
						HTTPResp: &models.HTTPResp{StatusCode: 200},
					},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
				},
			}
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{
				"post-events-1": {{Name: "mock-3238", Kind: string(models.HTTP)}},
				"post-events-2": {{Name: "mock-other", Kind: string(models.HTTP)}},
			}

			miss := models.UnmatchedCall{
				Protocol:       "HTTP",
				ActualSummary:  "POST /v3/oauth/token",
				Destination:    "auth.internal:8080",
				MatchPhase:     models.MatchPhaseSchema,
				CandidateCount: 1,
				NextSteps:      `Each test is served the HTTP calls it recorded. If another test recorded this call and every test may make it (a token, a shared configuration read), declare it in keploy.yaml to serve it to every test (sessionCalls: [{host: "auth.internal:8080", method: "POST", pathPrefix: "/v3/oauth/token"}]), or record again with 'keploy record' so this test records it.`,
			}
			if tc.realOwner {
				miss.NotOwned = true
				miss.ClosestMock = "mock-3238"
			}
			h.instr.mockErrorsFor = misses(nil, reads(miss))

			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)

			var tc2Result *models.TestResult
			for i := range h.report.results {
				if h.report.results[i].TestCaseID == "post-events-2" {
					tc2Result = &h.report.results[i]
				}
			}
			if tc2Result == nil || len(tc2Result.FailureInfo.UnmatchedCalls) == 0 {
				t.Fatalf("missing UnmatchedCalls on post-events-2 result: %+v", tc2Result)
			}
			gotCall := tc2Result.FailureInfo.UnmatchedCalls[0]
			if tc.realOwner {
				if gotCall.RecordedIn != "post-events-1" {
					t.Errorf("real owner RecordedIn = %q, want %q", gotCall.RecordedIn, "post-events-1")
				}
			} else {
				if gotCall.ClosestMock != "" {
					t.Errorf("heuristic match must NOT overwrite ClosestMock, got %q", gotCall.ClosestMock)
				}
				if gotCall.RecordedIn != "" {
					t.Errorf("heuristic match must NOT set RecordedIn (reserved for real owner evidence), got %q", gotCall.RecordedIn)
				}
			}
			if !strings.Contains(gotCall.NextSteps, tc.wantClause) {
				t.Errorf("NextSteps = %q, want clause %q", gotCall.NextSteps, tc.wantClause)
			}
			if strings.Contains(gotCall.NextSteps, "If another test recorded this call") {
				t.Errorf("NextSteps still uses conditional phrasing: %q", gotCall.NextSteps)
			}

			errEntries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			foundRealBlame := false
			for _, e := range errEntries {
				if strings.HasPrefix(e.Message, "mock mapping mismatch detected; marking testcase as FAILED") {
					t.Errorf("still leading with mock mapping mismatch / strict-failure blame: %q fields=%v", e.Message, e.ContextMap())
				}
				if strings.Contains(e.Message, "response 200 -> 500") && strings.Contains(e.Message, "got no recorded answer") {
					if !tc.realOwner || strings.Contains(e.Message, "post-events-1") {
						foundRealBlame = true
					}
				}
			}
			if !foundRealBlame {
				t.Errorf("expected per-test ERROR leading with 'response 200 -> 500 ... got no recorded answer', got errors: %v", errEntries)
			}

			wantSummaryMsg := "Failed tests also made outgoing calls that did not match the recorded mocks (see each test's response diff for why it failed)."
			if tc.proven {
				wantSummaryMsg = "Some testsets failed due to mock differences."
			}
			summaryLines := logs.FilterMessage(wantSummaryMsg).All()
			if len(summaryLines) != 1 {
				t.Fatalf("expected 1 end-of-run mock-differences line (%q), got %d: %v", wantSummaryMsg, len(summaryLines), logs.All())
			}
			ns := fmt.Sprint(summaryLines[0].ContextMap()["next_step"])
			if !strings.Contains(ns, "sessionCalls") {
				t.Errorf("next_step = %q, want sessionCalls advice derived from unmatched call", ns)
			}
			if strings.Contains(ns, "Add drifting dynamic fields as noise") {
				t.Errorf("next_step wrongly gives noise advice for an unmatched call: %q", ns)
			}
		})
	}
}

// Whole-set lookup must skip cross-test attribution for payload diffs against
// the current test's own mocks, never label a test's own mock as "(outside any
// test window)", and label genuinely unowned mocks outside all test windows
// accurately.
func TestWholeSetLookupSkipsOwnPayloadDiffAndNeverMislabelsOwnMockOutsideWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	cases := []*models.TestCase{
		{
			Name:     "tc-1",
			Kind:     models.HTTP,
			HTTPReq:  models.HTTPReq{Timestamp: t0.Add(10 * time.Second)},
			HTTPResp: models.HTTPResp{Timestamp: t0.Add(11 * time.Second)},
		},
		{
			Name:     "tc-2",
			Kind:     models.HTTP,
			HTTPReq:  models.HTTPReq{Timestamp: t0.Add(20 * time.Second)},
			HTTPResp: models.HTTPResp{Timestamp: t0.Add(21 * time.Second)},
		},
	}
	lookup := map[string]mockDisplayInfo{
		"mock-own": {
			protocol:   "HTTP",
			summary:    "POST /v1/orders",
			target:     "orders.internal:8080",
			recordedAt: t0.Add(10*time.Second + 100*time.Millisecond),
		},
		"mock-own-solo": {
			protocol:   "HTTP",
			summary:    "POST /v1/solo",
			target:     "orders.internal:8080",
			recordedAt: t0.Add(10*time.Second + 200*time.Millisecond),
		},
		"mock-other-orders": {
			protocol:   "HTTP",
			summary:    "POST /v1/orders",
			target:     "orders.internal:8080",
			recordedAt: t0.Add(20*time.Second + 100*time.Millisecond),
		},
		"mock-boot": {
			protocol:   "HTTP",
			summary:    "GET /v1/config",
			target:     "cfg.internal:8080",
			recordedAt: t0.Add(1 * time.Second), // before every test window, no owner
		},
	}
	mappings := map[string][]models.MockEntry{
		"tc-1": {
			{Name: "mock-own", Kind: string(models.HTTP)},
			{Name: "mock-own-solo", Kind: string(models.HTTP)},
		},
		"tc-2": {{Name: "mock-other-orders", Kind: string(models.HTTP)}},
	}
	idx := newWholeSetMockIndex(lookup, mappings, nil, cases)

	t.Run("own payload diff is not misattributed", func(t *testing.T) {
		origNext := "Add drifting dynamic fields as noise."
		calls := []models.UnmatchedCall{{
			Protocol:      "HTTP",
			ActualSummary: "POST /v1/orders",
			Destination:   "orders.internal:8080",
			ClosestMock:   "mock-own",
			MatchPhase:    models.MatchPhaseBody,
			FieldDiffs:    []models.MockFieldDiff{{Path: "body.amount", Expected: "1", Actual: "2"}},
			NextSteps:     origNext,
		}}
		idx.enrichCalls(calls, "tc-1")
		if calls[0].RecordedIn != "" || calls[0].NextSteps != origNext {
			t.Errorf("own payload diff was mutated: RecordedIn=%q NextSteps=%q", calls[0].RecordedIn, calls[0].NextSteps)
		}
	})

	t.Run("own mock is never labelled outside any test window", func(t *testing.T) {
		origNext := "original hint"
		calls := []models.UnmatchedCall{{
			Protocol:      "HTTP",
			ActualSummary: "POST /v1/solo",
			Destination:   "orders.internal:8080",
			MatchPhase:    models.MatchPhaseSchema,
			NextSteps:     origNext,
		}}
		idx.enrichCalls(calls, "tc-1")
		if calls[0].RecordedIn != "" || calls[0].NextSteps != origNext {
			t.Errorf("own mock was misattributed or mutated: RecordedIn=%q NextSteps=%q", calls[0].RecordedIn, calls[0].NextSteps)
		}
	})

	t.Run("unowned mock outside all test windows is labelled outside any test window", func(t *testing.T) {
		calls := []models.UnmatchedCall{{
			Protocol:      "HTTP",
			ActualSummary: "GET /v1/config",
			Destination:   "cfg.internal:8080",
			MatchPhase:    models.MatchPhaseSchema,
			NextSteps:     "If another test recorded this call and every test may make it, declare sessionCalls.",
		}}
		idx.enrichCalls(calls, "tc-2")
		if calls[0].ClosestMock != "" || calls[0].RecordedIn != "" {
			t.Errorf("heuristic match must not mutate ClosestMock/RecordedIn: got ClosestMock=%q RecordedIn=%q", calls[0].ClosestMock, calls[0].RecordedIn)
		}
		if !strings.Contains(calls[0].NextSteps, "A call to the same endpoint was recorded in mock-boot (outside any test window)") {
			t.Errorf("unowned boot mock NextSteps = %q, want honest endpoint match phrasing", calls[0].NextSteps)
		}
	})

	t.Run("AppTimeoutError implements Timeout() bool", func(t *testing.T) {
		var err error = &pkg.AppTimeoutError{TestCase: "tc-1", Duration: time.Second}
		te, ok := err.(interface{ Timeout() bool })
		if !ok || !te.Timeout() {
			t.Fatalf("AppTimeoutError must implement Timeout() bool == true")
		}
	})
}
