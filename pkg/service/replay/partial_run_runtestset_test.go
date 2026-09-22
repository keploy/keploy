package replay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// This file exists because the two statements that make the partial-run fix
// real — `stoppedEarly = true` at the end of a cycle, and the downgrade block
// that reads it — live inside RunTestSet, and nothing in this repo called
// RunTestSet. A review proved the point: both could be deleted with the whole
// suite still green, and swapping the cycle's `len(testsToRun)` for
// `testCasesCount` would go unnoticed while turning any set with an ignored or
// deferred test into an APP_FAULT that aborts the run.
//
// The loop is reachable without a network: requests go through the TestHooks
// seam (SimulateRequest) and the per-test agent call through Instrumentation
// (UpdateMockParams), so a fake pair drives the real control flow.

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type prTestDB struct{ cases []*models.TestCase }

func (f *prTestDB) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0"}, nil
}
func (f *prTestDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return f.cases, nil
}
func (f *prTestDB) UpdateTestCase(context.Context, *models.TestCase, string, bool) error { return nil }
func (f *prTestDB) DeleteTests(context.Context, string, []string) error                  { return nil }
func (f *prTestDB) DeleteTestSet(context.Context, string) error                          { return nil }

type prMockDB struct{}

func (prMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (prMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (prMockDB) UpdateMocks(context.Context, string, map[string]models.MockState, time.Time, time.Time) error {
	return nil
}

type prMappingDB struct{}

func (prMappingDB) Insert(context.Context, *models.Mapping) error { return nil }
func (prMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}
func (prMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) { return nil, nil }
func (prMappingDB) Exists(context.Context, string) (bool, error)                   { return false, nil }

// prReportDB records the report the run persisted. That report — not the CLI
// log line — is what the YAML, JUnit, --format json and the cloud UI render,
// so the assertions read it.
type prReportDB struct {
	mu          sync.Mutex
	results     []models.TestResult
	report      *models.TestReport
	failInserts bool
}

func (f *prReportDB) GetAllTestRunIDs(context.Context) ([]string, error) { return nil, nil }
func (f *prReportDB) GetTestCaseResults(context.Context, string, string) ([]models.TestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.TestResult, len(f.results))
	copy(out, f.results)
	return out, nil
}
func (f *prReportDB) GetReport(context.Context, string, string) (*models.TestReport, error) {
	return nil, nil
}
func (f *prReportDB) ClearTestCaseResults(context.Context, string, string) {}
func (f *prReportDB) InsertTestCaseResult(_ context.Context, _ string, _ string, r *models.TestResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInserts {
		return fmt.Errorf("report store is unwritable")
	}
	f.results = append(f.results, *r)
	return nil
}
func (f *prReportDB) InsertReport(_ context.Context, _ string, _ string, rep *models.TestReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.report = rep
	return nil
}
func (f *prReportDB) UpdateReport(context.Context, string, any) error { return nil }

type prTestSetConf struct{}

func (prTestSetConf) Read(context.Context, string) (*models.TestSet, error) { return nil, nil }
func (prTestSetConf) ReadForUpdate(context.Context, string) (*models.TestSet, error) {
	return nil, nil
}
func (prTestSetConf) Write(context.Context, string, *models.TestSet) error { return nil }
func (prTestSetConf) ReadSecret(context.Context, string) (map[string]interface{}, error) {
	return nil, nil
}

type prTelemetry struct{}

func (prTelemetry) TestSetRun(int, int, string, string)                        {}
func (prTelemetry) TestRun(int, int, int, int, string, map[string]interface{}) {}
func (prTelemetry) TestRunAborted(string)                                      {}
func (prTelemetry) MockTestRun(int)                                            {}

// prInstr fails UpdateMockParams from the Nth call onwards, which is exactly
// how a replaced agent behaves: it refuses the per-test filter params for the
// rest of the set (#4614 → #4618).
type prInstr struct {
	mu                sync.Mutex
	updateCalls       int
	failUpdateFromNth int // 0 = never fail
	recentAppLogs     string
	// stopAppAfterNUpdates makes the stand-in application exit part-way through
	// the set, the way a crash or an OOM kill does. 0 = it stays up.
	stopAppAfterNUpdates int
	appStopped           chan struct{}
	// lastParams is the filter-params payload of the most recent send, so a
	// test can assert what the agent was actually told.
	lastParams models.MockFilterParams
}

func (f *prInstr) Setup(context.Context, string, models.SetupOptions) error     { return nil }
func (f *prInstr) MockOutgoing(context.Context, models.OutgoingOptions) error   { return nil }
func (f *prInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) { return nil, nil }

// Run models a long-running application: it stays up until the run's context
// is cancelled. Returning immediately instead makes the app-watcher goroutine
// report the app as having exited, and the set lands on APP_HALTED no matter
// what the tests did — which would mask the very status this file asserts on.
func (f *prInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case <-ctx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
	case <-f.appStopped:
		return models.AppError{
			AppErrorType: models.ErrAppStopped,
			ExitCode:     1,
			AppLogs:      "panic: runtime error: out of memory\n",
		}
	}
}
func (f *prInstr) GetErrorChannel() <-chan error                                    { return make(chan error) }
func (f *prInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error)    { return nil, nil }
func (f *prInstr) BeforeSimulate(context.Context, *time.Time, string, string) error { return nil }
func (f *prInstr) AfterSimulate(context.Context, string, string) error              { return nil }
func (f *prInstr) BeforeTestRun(context.Context, string) error                      { return nil }
func (f *prInstr) BeforeTestSetCompose(context.Context, string, string, bool) error { return nil }
func (f *prInstr) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	return nil
}
func (f *prInstr) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error { return nil }
func (f *prInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastParams = params
	f.updateCalls++
	if f.stopAppAfterNUpdates != 0 && f.updateCalls == f.stopAppAfterNUpdates {
		close(f.appStopped) // the application exits mid-set
		// Wait for the replayer to ACTUALLY observe the exit rather than
		// guessing how long that takes. The app-watcher raises the stop signal
		// (a buffered send, so already queued) and then cancels the run
		// context — and `ctx` here IS that run context, so its Done is the
		// deterministic edge, ordered after the signal. Without waiting, the
		// fakes finish the whole set before the crash lands and the test proves
		// nothing; with a sleep it is merely likely to land.
		f.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second): // a miswired harness must not hang
		}
		f.mu.Lock()
	}
	if f.failUpdateFromNth != 0 && f.updateCalls >= f.failUpdateFromNth {
		return fmt.Errorf("agent rejected filter params: no mock session for this run")
	}
	return nil
}
func (f *prInstr) GetMockStats(context.Context) (models.MockStats, error) {
	return models.MockStats{}, nil
}
func (f *prInstr) GetRecentAppLogs(context.Context) string              { return f.recentAppLogs }
func (f *prInstr) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (f *prInstr) NotifyGracefulShutdown(context.Context) error         { return nil }
func (f *prInstr) ComposeDownOnSetupFailure(context.Context) error      { return nil }

// prHooks answers every simulated request with the test case's own recorded
// response, so a test that runs, passes.
type prHooks struct{ wrongType bool }

func (h prHooks) SimulateRequest(_ context.Context, tc *models.TestCase, _ string) (interface{}, error) {
	if h.wrongType {
		// Answer with the OTHER kind's response type, so the assertion in the
		// replay loop fails for whichever arm this test case takes.
		if tc.Kind == models.GRPC_EXPORT {
			return &models.HTTPResp{}, nil
		}
		return &models.GrpcResp{}, nil
	}
	if tc.Kind == models.GRPC_EXPORT {
		resp := tc.GrpcResp
		return &resp, nil
	}
	resp := tc.HTTPResp
	return &resp, nil
}
func (prHooks) GetConsumedMocks(context.Context) ([]models.MockState, error)     { return nil, nil }
func (prHooks) GetNoisyTestCaseNames(string) []string                            { return nil }
func (prHooks) BeforeTestRun(context.Context, string) error                      { return nil }
func (prHooks) BeforeTestSetCompose(context.Context, string, string, bool) error { return nil }
func (prHooks) BeforeTestSetRun(context.Context, string) error                   { return nil }
func (prHooks) BeforeTestSetReplay(context.Context, string) error                { return nil }
func (prHooks) BeforeTestResult(context.Context, string, string, []models.TestResult) error {
	return nil
}
func (prHooks) AfterTestSetRun(context.Context, string, bool) error { return nil }
func (prHooks) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	return nil
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// prTimeout bounds a harness run; a hang shows up as a failure, not a 10m suite.
var prTimeout = 60 * time.Second

func prLogger() *zap.Logger { return zap.NewNop() }

// prAppListener stands in for the user application. RunTestSet gates the whole
// test loop on waitForAppReady, which first dials the host:port taken from the
// test cases' own URLs and then waits for that address to actually SERVE an
// HTTP path — a bare TCP accept is not enough. It never has to return the
// RECORDED response: the comparison is fed by the TestHooks fake, and this
// server exists only to open the readiness gate.
func prAppListener(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func prTestCase(name, addr string) *models.TestCase {
	return &models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    name,
		HTTPReq: models.HTTPReq{
			Method:    models.Method("GET"),
			URL:       "http://" + addr + "/" + name,
			Timestamp: time.Now(),
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"ok":true}`,
			Timestamp:  time.Now(),
		},
	}
}

type prRun struct {
	replayer *Replayer
	report   *prReportDB
	instr    *prInstr
	cases    []*models.TestCase
}

func newPartialRunHarness(t *testing.T, n int, failUpdateFromNth int) *prRun {
	t.Helper()
	addr := prAppListener(t)
	cases := make([]*models.TestCase, 0, n)
	for i := 1; i <= n; i++ {
		cases = append(cases, prTestCase(fmt.Sprintf("test-%d", i), addr))
	}
	cfg := &config.Config{}
	cfg.Path = t.TempDir()
	// The readiness gate polls for HealthPollTimeout, which defaults to THREE
	// minutes. With it left at the default, a host where the probe cannot reach
	// the stand-in listener burns the whole prTimeout per test and then fails
	// with "context canceled" — a message that points at the wrong thing. Two
	// seconds keeps the gate real while making that failure fast and honest;
	// the probe warns and proceeds rather than failing the run.
	cfg.Test.HealthPollTimeout = 2 * time.Second
	// Pin the rewrite target to the loopback literal the listener is bound to.
	// Left unset, the resolver rewrites the host to "localhost", and the
	// readiness probe then dials whichever of ::1 / 127.0.0.1 DNS answers with
	// first — which on a v6-first host is not where the listener is.
	cfg.Test.Host = "127.0.0.1"
	reportDB := &prReportDB{}
	instr := &prInstr{failUpdateFromNth: failUpdateFromNth, appStopped: make(chan struct{})}
	r := &Replayer{
		logger:          prLogger(),
		testDB:          &prTestDB{cases: cases},
		mockDB:          prMockDB{},
		mappingDB:       prMappingDB{},
		reportDB:        reportDB,
		testSetConf:     prTestSetConf{},
		telemetry:       prTelemetry{},
		instrumentation: instr,
		config:          cfg,
		// Load-bearing: instrument=true is what puts SendMockFilterParamsToAgent
		// on the per-test path at all. With it false the agent is never called
		// and the defect under test cannot occur.
		instrument:           true,
		hookImpl:             prHooks{},
		completeTestReport:   make(map[string]TestReportVerdict),
		failedTCsBySetID:     make(map[string][]string),
		mockMismatchFailures: NewTestFailureStore(),
	}
	return &prRun{replayer: r, report: reportDB, instr: instr, cases: cases}
}

func (p *prRun) run(t *testing.T) models.TestSetStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := p.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if err != nil {
		t.Fatalf("RunTestSet returned an error, which is a different path from the one under test: %v", err)
	}
	return status
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestRunTestSetHealthyRunPasses is the control. Without it every assertion
// below is satisfiable by a harness that never reaches the loop at all, and by
// a downgrade that fires unconditionally.
func TestRunTestSetHealthyRunPasses(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0) // never fail
	status := h.run(t)

	if status != models.TestSetStatusPassed {
		t.Fatalf("a run where every test passed reported %q; want PASSED", status)
	}
	if got := len(h.report.results); got != 4 {
		t.Fatalf("recorded %d test results; want 4 — the loop did not run every test", got)
	}
}

// TestRunTestSetPartialRunIsNotReportedAsPassed is #4618 itself, end to end:
// the agent refuses the per-test filter params from test 3 on, the loop breaks,
// tests 3 and 4 never run, and the set used to report PASSED.
func TestRunTestSetPartialRunIsNotReportedAsPassed(t *testing.T) {
	// UpdateMockParams is called once before the loop and then once per test, so
	// the 3rd call is test-2's: test-1 runs, test-2 is refused, tests 2-4 never
	// produce a verdict.
	h := newPartialRunHarness(t, 4, 3)
	status := h.run(t)

	if status == models.TestSetStatusPassed {
		t.Fatal("the run stopped part-way through a 4-test set and still reported PASSED — a green suite " +
			"for tests that were never verified (#4618)")
	}
	if h.report.report == nil {
		t.Fatal("no report was persisted; the assertions below cannot run")
	}
	if h.report.report.Status == string(models.TestSetStatusPassed) {
		t.Fatalf("the PERSISTED report says %q; the YAML, JUnit and cloud UI read this field, not the "+
			"returned status", h.report.report.Status)
	}
	// The gap must be visible, not silently reconciled: the totals still say
	// four tests were loaded while only two were scored.
	if h.report.report.Total != 4 {
		t.Fatalf("report Total = %d; want 4 (tests loaded)", h.report.report.Total)
	}
	if scored := h.report.report.Success + h.report.report.Failure; scored >= 4 {
		t.Fatalf("report scored %d of 4 tests; the run was supposed to stop part-way", scored)
	}
}

// TestRunTestSetPartialRunReportsWhyItStopped pins the user-facing half. The
// status alone is not the deliverable: the persisted report carries a
// FailureReason and an AppLogs field, and both were wrong here. APP_FAULT's
// existing wording blames the application, but the case that motivated this is
// a REPLACED AGENT — the application is still running and still healthy — and
// lastAppErr is written only from the app-error channel, so the report told the
// reader to "check application logs in the app_logs field" next to an empty
// app_logs field.
func TestRunTestSetPartialRunReportsWhyItStopped(t *testing.T) {
	h := newPartialRunHarness(t, 4, 3)
	h.instr.recentAppLogs = "app: still serving\n"
	_ = h.run(t)

	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	reason := h.report.report.FailureReason
	if reason == "" {
		t.Fatal("the report carries no FailureReason; someone opening it sees a non-green status with " +
			"nothing saying the run ended early")
	}
	if strings.Contains(reason, "application stopped during replay") {
		t.Fatalf("the report blames the application: %q. The app never stopped here — the agent refused "+
			"the per-test filter params — so this sends the reader to a healthy process", reason)
	}
	if !strings.Contains(reason, "not") || !strings.Contains(reason, "verified") {
		t.Fatalf("the reason does not say the missing tests went unverified: %q", reason)
	}
	if h.report.report.TestSet == "" {
		t.Fatal("report is not addressed to a test set")
	}
	// The fallback that fetches recent app logs was gated on APP_HALTED, which
	// this path never reaches — so the field came back empty on the exact status
	// whose own reason string points at it.
	if h.report.report.AppLogs == "" {
		t.Fatal("the report's app_logs is empty while its reason points the reader at it; the " +
			"GetRecentAppLogs fallback did not run for this status")
	}
}

// TestRunTestSetIgnoredAndSelectedTestsStillPass is the false-positive guard,
// and the reason the cycle compares against its OWN testsToRun rather than the
// loaded count. Here 4 tests are loaded but only 2 are ever meant to run: one
// is ignored and one is not selected. Comparing verdicts against the loaded
// count would call this healthy run incomplete, mark it APP_FAULT, and abort
// every remaining test set on the native and docker-run paths.
//
// Note what it does NOT reach: the pre-loop filter builds activeTestCases, so
// neither test enters testsToRun and the in-loop skip guards never run. This
// exercises the comparison, not the currentSkipped counter — that counter
// guards the in-loop duplicates of those guards, which are unreachable today
// and deliberately counted anyway (see cycleIncomplete).
func TestRunTestSetIgnoredAndSelectedTestsStillPass(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0)
	h.replayer.config.Test.SelectedTests = map[string][]string{
		"test-set-0": {"test-1", "test-2", "test-3"},
	}
	h.replayer.config.Test.IgnoredTests = map[string][]string{
		"test-set-0": {"test-3"},
	}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("a healthy run with one ignored and one deselected test reported %q; want PASSED. "+
			"Comparing the cycle's verdicts against the LOADED count instead of its own testsToRun "+
			"produces exactly this, and APP_FAULT aborts the remaining test sets", status)
	}
}

// TestRunTestSetTypeAssertionFailureCountsTheTestOnce is the guard for a
// double-count that made a test which NEVER RAN show up in the failure total.
//
// The two type-assertion failure paths live inside `switch testCase.Kind`, so
// their `break` left the switch rather than the loop. Execution fell through to
// the scoring switch with testPass=false and testResult=nil, whose `default`
// arm counted the same test a second time, and then bailed out at the
// nil-result check. With two tests loaded and the first one failing this way,
// the report claimed two failures — one of them a test the run never reached.
func TestRunTestSetTypeAssertionFailureCountsTheTestOnce(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = prHooks{wrongType: true}
	h.report.failInserts = true // force the insert-failure branch that breaks out

	_ = h.run(t)

	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	if h.report.report.Failure > 1 {
		t.Fatalf("report counts %d failures for a run that abandoned the set on its FIRST test; the "+
			"second test never ran and must not be counted as failed", h.report.report.Failure)
	}
}

// TestRunTestSetGRPCTypeAssertionFailureCountsTheTestOnce is the gRPC twin of
// the HTTP guard above. The two type-assertion arms are separate code with the
// identical bug, and a test that only drives the HTTP one leaves the gRPC
// `break testLoop` free to be reverted with the suite green — which is exactly
// what a reviewer demonstrated.
func TestRunTestSetGRPCTypeAssertionFailureCountsTheTestOnce(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	for _, tc := range h.cases {
		tc.Kind = models.GRPC_EXPORT
	}
	h.replayer.hookImpl = prHooks{wrongType: true}
	h.report.failInserts = true // force the insert-failure branch that breaks out

	_ = h.run(t)

	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	if h.report.report.Failure > 1 {
		t.Fatalf("report counts %d failures for a gRPC run that abandoned the set on its FIRST test; "+
			"the second test never ran and must not be counted as failed", h.report.report.Failure)
	}
}

// TestRunTestSetAppCrashKeepsItsOwnFailureReason drives the call site that
// TestAppHaltedKeepsItsOwnFailureReason cannot reach.
//
// An application that exits part-way through a set also sets stoppedEarly —
// the loop breaks on the exit signal with tests left to run. But it already has
// an honest diagnosis (APP_HALTED, "application stopped during replay"), and
// keying the report's reason off stoppedEarly rather than off whether the
// partial-run downgrade actually fired replaced that with "check keploy's own
// logs", sending the reader away from the app that just crashed.
func TestRunTestSetAppCrashKeepsItsOwnFailureReason(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0)
	h.instr.stopAppAfterNUpdates = 3 // the app dies after a couple of tests
	// If the refetch below ever stops being a FALLBACK, this is what lands in
	// the report instead of the crash output.
	h.instr.recentAppLogs = "THIS MUST NOT APPEAR"

	_ = h.run(t)

	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	reason := h.report.report.FailureReason
	if reason == "" {
		t.Fatalf("no FailureReason for a run whose app crashed; status was %q", h.report.report.Status)
	}
	if !strings.Contains(reason, "application") {
		t.Fatalf("the app crashed mid-run and the report says %q — it must keep the application "+
			"diagnosis rather than pointing the reader at keploy's own logs", reason)
	}
	// The GetRecentAppLogs call is a FALLBACK for when the app-error channel
	// captured nothing. Making it unconditional throws away the crash output —
	// the single most valuable field in this report — and replaces it with a
	// refetch that returns "" for any non-docker app. The report would then say
	// "check application logs in the app_logs field" beside an empty field,
	// which is the defect removing the APP_HALTED gate set out to fix, arriving
	// from the other side.
	if got := h.report.report.AppLogs; got != "panic: runtime error: out of memory\n" {
		t.Fatalf("report app_logs = %q; the crash output the app-error channel captured was "+
			"overwritten by the recent-logs fallback", got)
	}
}

// TestRunTestSetClearsTheRetryStalenessAtTheBoundary pins the CALL SITE. The
// helper's own tests call mockOutgoingForTestSet directly, so reverting
// RunTestSet's per-set call back to instrumentation.MockOutgoing — a complete
// revert of the fix on the native and docker-run paths — leaves them green.
//
// This drives the real RunTestSet with the set-scoped latch already set, as if
// a previous test set had ended in a --retry-passing-test rewind, and asserts
// the boundary dropped it. The run-scoped latch must survive the same boundary:
// a replaced agent's history stays incomplete for the rest of the run.
func TestRunTestSetClearsTheRetryStalenessAtTheBoundary(t *testing.T) {
	t.Run("the retry rewind is dropped", func(t *testing.T) {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.agentHistoryStaleForSet.Store(true)

		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("healthy run reported %q", status)
		}
		if h.replayer.agentConsumedHistoryUnusable() {
			t.Fatal("RunTestSet did not drop the previous set's retry staleness, so " +
				"KEPLOY_AGENT_OWNS_CONSUMED stays disabled for every remaining test set")
		}
	})

	t.Run("a replaced agent stays disqualified", func(t *testing.T) {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.agentHistoryIncompleteForRun.Store(true)

		_ = h.run(t)

		if !h.replayer.agentConsumedHistoryUnusable() {
			t.Fatal("the test-set boundary cleared a replacement agent's missing history; what it lost " +
				"is gone for the run, so the CLI's map stays the only complete record")
		}
	})
}

// TestSendMockFilterParamsFallsBackAfterARetryRewind pins the READER through
// the real send path. Splitting one latch into two lost coverage the single
// field used to have: the repair tests exercise only the run-scoped one now, so
// a reader that consults it alone passes the suite while silently deleting the
// whole #4622 fix.
func TestSendMockFilterParamsFallsBackAfterARetryRewind(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_OWNS_CONSUMED", "1")

	h := newPartialRunHarness(t, 1, 0)
	r := h.replayer
	consumed := map[string]models.MockState{"mock-1": {Name: "mock-1", Usage: models.Deleted}}

	// Baseline: with a trustworthy agent history the flag is honoured and the
	// CLI's map is NOT sent. Without this the assertion below proves nothing.
	if err := r.SendMockFilterParamsToAgent(context.Background(), nil,
		models.BaseTime, time.Now(), consumed, false, time.Time{}); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if !h.instr.lastParams.AgentOwnsConsumed {
		t.Fatal("precondition: the flag was not honoured on a healthy run, so this test cannot detect " +
			"the fallback it is about to assert")
	}

	// A retry cycle rewinds the CLI's map; nothing rewinds the agent's.
	r.rewindConsumedForRetryCycle(map[string]models.MockState{}, map[string]models.MockState{}, map[string]models.MockState{})

	if err := r.SendMockFilterParamsToAgent(context.Background(), nil,
		models.BaseTime, time.Now(), consumed, false, time.Time{}); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if h.instr.lastParams.AgentOwnsConsumed {
		t.Fatal("after a retry rewind the agent was still told to filter from its OWN history, which " +
			"still holds the previous cycle — the retry fails with match_phase=no_mocks (#4622)")
	}
	if len(h.instr.lastParams.TotalConsumedMocks) == 0 {
		t.Fatal("the fallback did not carry the CLI's consumed map")
	}
}
