package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// prMockDB counts UpdateMocks calls. UpdateMocks is the destructive prune: it
// deletes every recorded mock outside the keep-set and the startup window, so
// whether a run reached it at all is what the prune tests below assert.
type prMockDB struct {
	mu          sync.Mutex
	updateCalls int
	// kept is the keep-set of the last prune.
	kept map[string]models.MockState
	// filtered is the set's per-test mocks; none by default.
	filtered []*models.Mock
}

func (m *prMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return m.filtered, nil
}
func (*prMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (m *prMockDB) UpdateMocks(_ context.Context, _ string, keep map[string]models.MockState, _ time.Time, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	m.kept = keep
	return nil
}
func (m *prMockDB) pruneCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateCalls
}

// prMappingDB counts Insert calls. Every mappings.yaml write goes through
// Insert: the create-if-absent write, StoreMappings and the startup backfill.
// By default no file exists, the shape of a DaemonSet recording; exists makes
// Exists report one, which is what the StoreMappings merge path writes into.
type prMappingDB struct {
	mu          sync.Mutex
	inserts     int
	exists      bool
	existsCalls int
	// last is the mapping of the last Insert.
	last *models.Mapping
	// onDisk is the per-test mapping Get reports, and onDiskStartup the
	// startup section GetStartup reports, as the file on disk holds them;
	// none by default. getCalls counts the reads of the per-test mapping.
	onDisk        map[string][]models.MockEntry
	onDiskStartup []models.MockEntry
	getCalls      int
}

func (m *prMappingDB) Insert(_ context.Context, mapping *models.Mapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inserts++
	m.last = mapping
	return nil
}
func (m *prMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	return m.onDisk, len(m.onDisk) > 0, nil
}
func (m *prMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onDiskStartup, nil
}
func (m *prMappingDB) Exists(context.Context, string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.existsCalls++
	return m.exists, nil
}
func (m *prMappingDB) insertCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inserts
}

// prReportDB records the report the run persisted. That report — not the CLI
// log line — is what the YAML, JUnit, --format json and the cloud UI render,
// so the assertions read it.
type prReportDB struct {
	mu          sync.Mutex
	results     []models.TestResult
	report      *models.TestReport
	failInserts bool
	// failInsertOf fails only the named test's result insert, with that error.
	failInsertOf map[string]error
	// Read-back faults: onRead runs first (a test cancels the run from it),
	// dropOnRead leaves the named test's result out, as a store that lost or
	// paged away a result does, and readErr fails the read alongside whatever it
	// returned.
	onRead     func()
	readErr    error
	dropOnRead string
}

func (f *prReportDB) GetAllTestRunIDs(context.Context) ([]string, error) { return nil, nil }
func (f *prReportDB) GetTestCaseResults(context.Context, string, string) ([]models.TestResult, error) {
	if f.onRead != nil {
		f.onRead()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.TestResult, 0, len(f.results))
	for _, r := range f.results {
		if r.TestCaseID != f.dropOnRead {
			out = append(out, r)
		}
	}
	return out, f.readErr
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
	if err := f.failInsertOf[r.TestCaseID]; err != nil {
		return err
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
	// stopErr is how the app exits; ErrAppStopped when empty.
	stopErr models.AppErrorType
	// lastParams is the filter-params payload of the most recent send, so a
	// test can assert what the agent was actually told.
	lastParams models.MockFilterParams
	// allParams is every send, in order.
	allParams []models.MockFilterParams
	// perTestScope makes the stand-in agent answer each send the way an agent
	// that reads the consumed history only for its per-test mocks does
	// (models.ConsumedScopeHeader); scopeSaid is what the client has taken from
	// the last answer, cleared by a store as the real client clears it.
	perTestScope bool
	scopeSaid    bool
	// storedFiltered and storedUnfiltered are the pools of the last store.
	storedFiltered, storedUnfiltered []*models.Mock
	// release, when set, holds the stand-in application after the run's
	// context is cancelled until it is closed: an application still stopping,
	// for as long as the test says. stopped, when set, is closed as Run
	// returns.
	release chan struct{}
	stopped chan struct{}
}

func (f *prInstr) AgentReadsConsumedPerTestOnly() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scopeSaid
}

func (f *prInstr) Setup(context.Context, string, models.SetupOptions) error     { return nil }
func (f *prInstr) MockOutgoing(context.Context, models.OutgoingOptions) error   { return nil }
func (f *prInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) { return nil, nil }

// Run models a long-running application: it stays up until the run's context
// is cancelled. Returning immediately instead makes the app-watcher goroutine
// report the app as having exited, and the set lands on APP_HALTED no matter
// what the tests did — which would mask the very status this file asserts on.
func (f *prInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	if f.stopped != nil {
		defer close(f.stopped)
	}
	select {
	case <-ctx.Done():
		if f.release != nil {
			<-f.release
		}
		return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
	case <-f.appStopped:
		errType := models.ErrAppStopped
		if f.stopErr != "" {
			errType = f.stopErr
		}
		return models.AppError{
			AppErrorType: errType,
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
func (f *prInstr) StoreMocks(_ context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopeSaid = false
	f.storedFiltered, f.storedUnfiltered = filtered, unfiltered
	return nil
}
func (f *prInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastParams = params
	f.allParams = append(f.allParams, params)
	f.updateCalls++
	f.scopeSaid = f.perTestScope
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
// response, so a test that runs, passes. simErr makes the named tests fail the
// way SimulateHTTP does when no response comes back; streamErr fails the named
// streaming tests while their body is read.
type prHooks struct {
	wrongType bool
	simErr    map[string]error
	streamErr map[string]error
	// consumed is what every GetConsumedMocks call returns, unless consumedFor
	// is set, which is then asked on every call.
	consumed    []models.MockState
	consumedFor func() []models.MockState
	// wrongBody answers the named tests with a body that does not match, so
	// they fail on the response comparison.
	wrongBody map[string]bool
	// consumedFailsOnStop fails GetConsumedMocks once the run's context is
	// cancelled, as the real agent call does after the app exits.
	consumedFailsOnStop bool
}

func (h prHooks) SimulateRequest(_ context.Context, tc *models.TestCase, _ string) (interface{}, error) {
	if err := h.simErr[tc.Name]; err != nil {
		return nil, err
	}
	if err := h.streamErr[tc.Name]; err != nil {
		return &pkg.StreamingHTTPResponse{StatusCode: 200, Header: tc.HTTPResp.Header,
			Reader: io.NopCloser(iotest.ErrReader(err)), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}, nil
	}
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
	if h.wrongBody[tc.Name] {
		resp.Body = `{"ok":false}`
	}
	return &resp, nil
}
func (h prHooks) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if h.consumedFailsOnStop && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if h.consumedFor != nil {
		return h.consumedFor(), nil
	}
	return h.consumed, nil
}
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
	mocks    *prMockDB
	mappings *prMappingDB
	cases    []*models.TestCase
	// cancel stops the run from outside, as a SIGINT does; set by run.
	cancel context.CancelFunc
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
	mocks := &prMockDB{}
	mappings := &prMappingDB{}
	r := &Replayer{
		logger:          prLogger(),
		testDB:          &prTestDB{cases: cases},
		mockDB:          mocks,
		mappingDB:       mappings,
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
		consumedMockNames:    make(map[string]struct{}),
	}
	return &prRun{replayer: r, report: reportDB, instr: instr, mocks: mocks, mappings: mappings, cases: cases}
}

// streaming makes the named cases NDJSON streams, which RunTestSet takes out of
// its first phase and runs afterwards, one at a time.
func (p *prRun) streaming(names ...string) {
	for _, tc := range p.cases {
		for _, n := range names {
			if tc.Name == n {
				tc.HTTPResp.Header = map[string]string{"Content-Type": "application/x-ndjson"}
			}
		}
	}
}

func (p *prRun) run(t *testing.T) models.TestSetStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	p.cancel = cancel
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
//
// It is also the control for the prune tests: with RemoveUnusedMocks on (what
// k8s-proxy auto-replay sets), a complete, fully passing run must still prune
// exactly once, or the guards below are satisfiable by never pruning at all.
func TestRunTestSetHealthyRunPasses(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0) // never fail
	h.replayer.config.Test.RemoveUnusedMocks = true
	status := h.run(t)

	if status != models.TestSetStatusPassed {
		t.Fatalf("a run where every test passed reported %q; want PASSED", status)
	}
	if got := len(h.report.results); got != 4 {
		t.Fatalf("recorded %d test results; want 4 — the loop did not run every test", got)
	}
	if got := h.mocks.pruneCalls(); got != 1 {
		t.Fatalf("a complete, passing run with RemoveUnusedMocks made %d UpdateMocks calls; want 1 — "+
			"the prune must stay exactly as it was for a run that scored every test", got)
	}
}

// TestRunTestSetPartialRunDoesNotPruneMocks: a run that stopped before scoring
// every test must not prune. The keep-set holds only what the tests that ran
// consumed, so every mock of a test the run never reached looks unused. Only
// mappings.yaml protects those, and a recording without one (a DaemonSet
// recording has none) lost them all.
func TestRunTestSetPartialRunDoesNotPruneMocks(t *testing.T) {
	h := newPartialRunHarness(t, 4, 3) // test-1 runs, test-2 is refused, tests 2-4 never run
	h.replayer.config.Test.RemoveUnusedMocks = true
	_ = h.run(t)

	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	if scored := h.report.report.Success + h.report.report.Failure; scored >= 4 {
		t.Fatalf("precondition: the run scored %d of 4 tests; it was supposed to stop part-way", scored)
	}
	if got := h.mocks.pruneCalls(); got != 0 {
		t.Fatalf("a run that stopped after %d of 4 tests made %d UpdateMocks calls; it must make none — "+
			"the tests it never reached would lose every mock outside the startup window",
			h.report.report.Success+h.report.report.Failure, got)
	}
}

// prClientTimeoutErr returns the error net/http itself produces when the app
// accepts a request and never answers before the client gives up, so the test
// classifies the error SimulateHTTP hands to CreateFailedTestResult: the
// no-answer mark (pkg.IsAppNoAnswer) reads the error, not its text.
func prClientTimeoutErr(t *testing.T) error {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	resp, err := (&http.Client{Timeout: 50 * time.Millisecond}).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server that never answers produced a response")
	}
	return err
}

// prRefusedErr returns the error net/http itself produces for a request to a
// port nothing listens on: a refusal is known by its errno
// (pkg.IsAppConnectionError), not by its text, so the test classifies what
// SimulateHTTP hands to CreateFailedTestResult.
func prRefusedErr(t *testing.T) error {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/test-4", closedLocalPort(t)))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a request to a closed port got an answer")
	}
	return err
}

// prCanceledErr returns the error net/http produces for a request whose context
// was cancelled, which is what a stop does to the request in flight.
func prCanceledErr(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a cancelled request produced a response")
	}
	return err
}

// TestRunTestSetNoAnswerFailuresDoNotPruneMocks: a run that scored every test,
// but where the app never answered some of them, must not prune. A request that
// timed out or was cancelled consumed nothing for want of an answer, not for
// want of need, so its test's mocks look unused and PreserveFailedMocks=false
// (what k8s-proxy auto-replay sets) would let the prune delete them.
func TestRunTestSetNoAnswerFailuresDoNotPruneMocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  func(*testing.T) error
	}{
		{"client timeout", prClientTimeoutErr},
		{"context canceled", prCanceledErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-2": err, "test-3": err, "test-4": err}}
			h.replayer.config.Test.RemoveUnusedMocks = true
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 1 || rep.Failure != 3 {
				t.Fatalf("precondition: want a complete run with 1 pass and 3 failures, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("3 of 4 tests got no answer (%v) and the run still made %d UpdateMocks calls; "+
					"it must make none", err, got)
			}
		})
	}
}

// TestRunTestSetUnrunStreamingTestsBlockThePrune: streaming tests are taken out
// of testsToRun and run in a second phase, so stoppedEarly never counts them.
// Every way that phase is cut short or skipped leaves streaming tests that
// consumed nothing, and without mappings.yaml nothing else keeps their mocks
// out of the delete.
func TestRunTestSetUnrunStreamingTestsBlockThePrune(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failUpdateNth int // agent refuses filter params from this call (1 before the loop, then 1 per test)
		stopAppNth    int // the app exits during this filter-params call
		failOnStop    bool
		wantPrunes    int
	}{
		// The replaced-agent shape (#4614), refusing the first streaming test.
		{name: "agent refuses the first streaming test", failUpdateNth: 4},
		{name: "agent refuses the second streaming test", failUpdateNth: 5},
		// The app exits during the last non-streaming test. With the agent call
		// failing on the stop, the loop sets exitLoop and the phase never starts;
		// without it, the phase starts and breaks on the exit signal.
		{name: "app exits and the streaming phase is skipped", stopAppNth: 3, failOnStop: true},
		{name: "app exits and the streaming phase breaks", stopAppNth: 3},
		// Control: every streaming test ran, so the run is complete and prunes.
		{name: "every streaming test ran", wantPrunes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, tc.failUpdateNth)
			h.streaming("test-3", "test-4")
			h.instr.stopAppAfterNUpdates = tc.stopAppNth
			h.replayer.hookImpl = prHooks{consumedFailsOnStop: tc.failOnStop}
			h.replayer.config.Test.RemoveUnusedMocks = true
			_ = h.run(t)

			rep := h.report.report
			if rep == nil {
				t.Fatal("no report was persisted")
			}
			ran := rep.Success + rep.Failure + rep.Obsolete
			if complete := ran == 4; complete != (tc.wantPrunes == 1) {
				t.Fatalf("precondition: %d of 4 tests produced a verdict (%+v)", ran, rep)
			}
			if got := h.mocks.pruneCalls(); got != tc.wantPrunes {
				t.Fatalf("%d of 4 tests produced a verdict and the run made %d UpdateMocks calls; want %d",
					ran, got, tc.wantPrunes)
			}
		})
	}
}

// TestRunTestSetNoAnswerInTheStreamingPhaseBlocksThePrune pins the two
// streaming-phase sites where a transport error becomes a synthetic result:
// the request itself, and the body read after the headers arrived.
func TestRunTestSetNoAnswerInTheStreamingPhaseBlocksThePrune(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, tc := range []struct {
		name  string
		hooks prHooks
	}{
		{"request timed out", prHooks{simErr: map[string]error{"test-4": timeout}}},
		{"body read timed out", prHooks{streamErr: map[string]error{"test-4": timeout}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			h.streaming("test-4")
			h.replayer.hookImpl = tc.hooks
			h.replayer.config.Test.RemoveUnusedMocks = true
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 3 || rep.Failure != 1 {
				t.Fatalf("precondition: want a complete run with 3 passes and 1 failure, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("the streaming test got no answer and the run made %d UpdateMocks calls; want 0", got)
			}
		})
	}
}

// TestRunTestSetLostResultBlocksThePrune: when a result insert fails the report
// is short of that result, so a rule that reads the report back cannot see why
// the test failed. The run is INTERNAL_ERR and must not prune.
func TestRunTestSetLostResultBlocksThePrune(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  func(*testing.T) error
	}{
		{"client timeout", prClientTimeoutErr},
		{"connection refused", prRefusedErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-2": tc.err(t)}}
			h.report.failInserts = true
			h.replayer.config.Test.RemoveUnusedMocks = true
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 1 || rep.Failure != 1 {
				t.Fatalf("precondition: want both tests scored, 1 pass and 1 failure, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("test-2's result never reached the report and the run made %d UpdateMocks calls; want 0", got)
			}
		})
	}
}

// prDoesNotDecodeErr returns the error pkg.SimulateHTTP gives for an answer
// that comes back whole but whose body does not decode as its Content-Encoding
// says: the app answered.
func prDoesNotDecodeErr(t *testing.T, encoding string, status int, body []byte) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", encoding)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	tc := &models.TestCase{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{
		Method: http.MethodGet, URL: srv.URL + "/test-2",
		// As recorded traffic carries it: net/http leaves the body to keploy.
		Header: map[string]string{"Accept-Encoding": encoding},
	}}
	_, err := pkg.SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), pkg.SimulationConfig{APITimeout: 5})
	if err == nil || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("got %v; want the error of an answer that does not decode", err)
	}
	return err
}

// TestRunTestSetOrdinaryFailureStillPrunes is the other side of the no-answer
// rule: a test that failed for any other reason is an ordinary failure, and
// with PreserveFailedMocks=false (what k8s-proxy auto-replay sets) the run
// prunes exactly as before. That includes a failure whose recorded body was too
// large to keep: the synthetic result then carries no body at all, and that is
// not evidence that the app never answered. It includes an answer that does
// not decode, too: its decoder's io.ErrUnexpectedEOF (a gzip body cut short; a
// br body with no bytes) is kept as text only, and the no-answer mark
// (pkg.IsAppNoAnswer) reads the error, not its text.
func TestRunTestSetOrdinaryFailureStillPrunes(t *testing.T) {
	var whole bytes.Buffer
	zw := gzip.NewWriter(&whole)
	if _, err := zw.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	malformed := errors.New(`Get "http://127.0.0.1:8080/test-2": net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00"`)
	for _, tc := range []struct {
		name        string
		err         error
		bodySkipped bool
	}{
		{"recorded body skipped=false", malformed, false},
		{"recorded body skipped=true", malformed, true},
		{"a gzip answer cut short does not decode", prDoesNotDecodeErr(t, "gzip", http.StatusOK, whole.Bytes()[:whole.Len()-6]), false},
		{"a br 204 with no body does not decode", prDoesNotDecodeErr(t, "br", http.StatusNoContent, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.cases[1].HTTPResp.BodySkipped = tc.bodySkipped
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-2": tc.err}}
			h.replayer.config.Test.RemoveUnusedMocks = true
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 1 || rep.Failure != 1 {
				t.Fatalf("precondition: want a complete run with 1 pass and 1 failure, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != 1 {
				t.Fatalf("a complete run with one ordinary failure (%v) made %d UpdateMocks calls; want 1", tc.err, got)
			}
		})
	}
}

// TestRunTestSetIncompleteRunDoesNotCreateMappings: with UpdateTestMapping off
// (the default) a run creates mappings.yaml only when none exists, and never
// touches it again. A run that left tests without a verdict or an answer has no
// entries for them, so creating the file from it leaves those tests out for
// good. The startup-only backfill would create the file just the same.
// --update-test-mapping writes a run with an unanswered test either way:
// mapdb.Insert replaces only the entries this run has, so into an existing file
// the unanswered test keeps its own, and a new file gains it on the next such
// run. A run that stopped early writes nothing under either flag.
func TestRunTestSetIncompleteRunDoesNotCreateMappings(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	timeout := prClientTimeoutErr(t)
	refused := prRefusedErr(t)
	for _, tc := range []struct {
		name          string
		failNth       int
		simErr        map[string]error
		stopAppNth    int
		stopErr       models.AppErrorType
		updateMapping bool
		fileExists    bool
		wantInserts   int
	}{
		{name: "stopped early", failNth: 3},
		{name: "a request got no answer", simErr: map[string]error{"test-4": timeout}},
		{name: "a request was refused", simErr: map[string]error{"test-4": refused}},
		// The app runner fails with an internal error during test-4's filter call,
		// and test-4 still runs: every verdict and result is in, the set is
		// INTERNAL_ERR.
		{name: "keploy hit an internal error", stopAppNth: 5, stopErr: models.ErrInternal},
		{name: "every test answered", wantInserts: 1},
		// Into a file that exists, only the startup backfill writes.
		{name: "a request got no answer, file exists", simErr: map[string]error{"test-4": timeout},
			fileExists: true, wantInserts: 1},
		{name: "--update-test-mapping, stopped early, file exists", failNth: 3, updateMapping: true, fileExists: true},
		{name: "--update-test-mapping, stopped early", failNth: 3, updateMapping: true},
		{name: "--update-test-mapping, a request got no answer", simErr: map[string]error{"test-4": timeout},
			updateMapping: true, wantInserts: 1},
		{name: "--update-test-mapping, a request got no answer, file exists", simErr: map[string]error{"test-4": timeout},
			updateMapping: true, fileExists: true, wantInserts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, tc.failNth)
			h.replayer.hookImpl = prHooks{consumed: session, simErr: tc.simErr}
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.mappings.exists = tc.fileExists
			h.instr.stopAppAfterNUpdates, h.instr.stopErr = tc.stopAppNth, tc.stopErr
			_ = h.run(t)

			if h.report.report == nil {
				t.Fatal("no report was persisted")
			}
			if tc.stopErr == models.ErrInternal && (h.report.report.Status != string(models.TestSetStatusInternalErr) ||
				h.report.report.Success != 4 || len(h.report.results) != 4) {
				t.Fatalf("precondition: want an INTERNAL_ERR set with all 4 tests passed and recorded, got %+v", h.report.report)
			}
			if got := h.mappings.insertCalls(); got != tc.wantInserts {
				t.Fatalf("mappings.yaml writes = %d, want %d", got, tc.wantInserts)
			}
			if h.mappings.existsCalls > 1 {
				t.Fatalf("the run asked whether mappings.yaml exists %d times; once is enough", h.mappings.existsCalls)
			}
		})
	}
}

// TestRunTestSetSubsetRunDoesNotPruneOrWriteMappings: the prune keeps only what
// this run's tests consumed, so it is sound only when every loaded, non-ignored
// test got a verdict. A run over part of the set leaves the rest's mocks
// unconsumed, and stoppedEarly cannot see it: it counts against the cycle's own
// testsToRun, which the selection already narrowed. That covers a selected-test
// list (a rerun of the failed tests) and a selection loop that broke on a failed
// result insert. Such a run writes no mappings.yaml either: no create, no
// --update-test-mapping merge, no startup backfill. Holding a create or an
// asked-for write is logged; a default subset run on a mapped set stays quiet.
func TestRunTestSetSubsetRunDoesNotPruneOrWriteMappings(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	for _, tc := range []struct {
		name          string
		selected      []string
		ignored       []string
		failInsertOf  map[string]error
		updateMapping bool
		fileExists    bool
		want          int // UpdateMocks calls, and mappings.yaml writes
		quiet         bool
	}{
		{name: "selected tests narrow the set", selected: []string{"test-1", "test-2"}},
		{name: "a rerun of the failed test", selected: []string{"test-3"}},
		{name: "a subset with --update-test-mapping", selected: []string{"test-1", "test-2"},
			updateMapping: true, fileExists: true},
		{name: "a subset on a set that has mappings.yaml", selected: []string{"test-1", "test-2"},
			fileExists: true, quiet: true},
		// The selection loop inserts the ignored test's result and breaks when that
		// insert fails, so test-3 and test-4 never enter the run. Cancelled, the
		// insert error leaves the status off INTERNAL_ERR.
		{name: "the selection loop broke on a cancelled insert", ignored: []string{"test-2"},
			failInsertOf: map[string]error{"test-2": fmt.Errorf("report store: %w", context.Canceled)}},
		{name: "the selection loop broke on a failed insert", ignored: []string{"test-2"},
			failInsertOf: map[string]error{"test-2": errors.New("report store is unwritable")}},
		// Controls. A selection that covers the set is not a subset, and an
		// ignored test is out of the count.
		{name: "every test selected", selected: []string{"test-1", "test-2", "test-3", "test-4"}, want: 1},
		{name: "an ignored test", ignored: []string{"test-2"}, want: 1},
		{name: "a complete run with --update-test-mapping", updateMapping: true, fileExists: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.logger = zap.New(core)
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.mappings.exists = tc.fileExists
			if tc.selected != nil {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": tc.selected}
			}
			if tc.ignored != nil {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignored}
			}
			h.report.failInsertOf = tc.failInsertOf
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success == 0 || rep.Failure != 0 || rep.Obsolete != 0 {
				t.Fatalf("precondition: want every test that ran to pass, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != tc.want {
				t.Fatalf("%d of the set's %d non-ignored tests got a verdict and the run made %d UpdateMocks calls; want %d",
					rep.Success, 4-len(tc.ignored), got, tc.want)
			}
			if got := h.mappings.insertCalls(); got != tc.want {
				t.Fatalf("%d of the set's %d non-ignored tests got a verdict and the run wrote mappings.yaml %d times; want %d",
					rep.Success, 4-len(tc.ignored), got, tc.want)
			}
			if h.mappings.existsCalls > 1 {
				t.Fatalf("the run asked whether mappings.yaml exists %d times; once is enough", h.mappings.existsCalls)
			}
			held := len(logs.FilterMessage("not writing mappings.yaml").All())
			if wantHeld := tc.want == 0 && !tc.quiet; (held == 1) != wantHeld || held > 1 {
				t.Fatalf("\"not writing mappings.yaml\" warnings = %d, want %v", held, wantHeld)
			}
		})
	}
}

// TestRunTestSetUnreadableResultsBlockThePrune: the connection-error rule reads
// the results back, so a read that fails or comes back short blinds it, and the
// prune went ahead on a run whose refused request it could not see. A read that
// errors or is short of the verdicts refuses the prune and the mappings.yaml
// create (fail closed).
func TestRunTestSetUnreadableResultsBlockThePrune(t *testing.T) {
	refused := map[string]error{"test-4": errors.New(
		`Get "http://127.0.0.1:8080/test-4": dial tcp 127.0.0.1:8080: connect: connection refused`)}
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	for _, tc := range []struct {
		name    string
		simErr  map[string]error
		fault   func(*prRun)
		ignored bool // add a fifth test, ignored
	}{
		{name: "the read came back short", simErr: refused, fault: func(h *prRun) { h.report.dropOnRead = "test-4" }},
		// The ignored test's own result must not stand in for the lost one.
		{name: "the read came back short beside an ignored result", simErr: refused, ignored: true,
			fault: func(h *prRun) { h.report.dropOnRead = "test-4" }},
		// A stop landing after the last verdict cancels the read. Its error is
		// then swallowed and the status left as it was.
		{name: "the read was cancelled part-way", simErr: refused, fault: func(h *prRun) {
			h.report.onRead = func() { h.cancel() }
			h.report.readErr = context.Canceled
			h.report.dropOnRead = "test-4"
		}},
		{name: "the read was cancelled with every result", fault: func(h *prRun) {
			h.report.onRead = func() { h.cancel() }
			h.report.readErr = context.Canceled
		}},
		{name: "the read failed", simErr: refused, fault: func(h *prRun) { h.report.readErr = errors.New("report store is unreadable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 4
			if tc.ignored {
				n = 5
			}
			h := newPartialRunHarness(t, n, 0)
			if tc.ignored {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-5"}}
			}
			h.replayer.hookImpl = prHooks{consumed: session, simErr: tc.simErr}
			h.replayer.config.Test.RemoveUnusedMocks = true
			tc.fault(h)
			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success+rep.Failure != 4 || rep.Failure != len(tc.simErr) {
				t.Fatalf("precondition: want every test scored, failing only %v, got %+v", tc.simErr, rep)
			}
			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("the results were unreadable and the run made %d UpdateMocks calls; want 0", got)
			}
			if got := h.mappings.insertCalls(); got != 0 {
				t.Fatalf("the results were unreadable and the run created mappings.yaml (%d writes); want 0", got)
			}
		})
	}
}

// TestRunTestSetPruneRefusalNamesTheRule: the one "skipping mock pruning" line a
// set logs names the rule that refused and, for requests that got no answer,
// the tests. A set where one test keeps timing out is never pruned again, and
// this line is the only place that says why.
func TestRunTestSetPruneRefusalNamesTheRule(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	refused := prRefusedErr(t)
	ordinary := errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00"`)
	for _, tc := range []struct {
		name     string
		setup    func(*prRun)
		reason   string
		noAnswer string
	}{
		{"a subset run", func(h *prRun) {
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-1"}}
		}, "not_every_test_got_a_verdict", ""},
		{"a stopped run", func(h *prRun) { h.instr.failUpdateFromNth = 3 }, "not_every_test_got_a_verdict", ""},
		{"a lost result insert", func(h *prRun) {
			h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
		}, "internal_error", ""},
		{"an internal error with every result in", func(h *prRun) {
			h.instr.stopAppAfterNUpdates, h.instr.stopErr = 5, models.ErrInternal
		}, "internal_error", ""},
		{"a short read", func(h *prRun) { h.report.dropOnRead = "test-4" }, "results_unreadable", ""},
		{"requests that got no answer", func(h *prRun) {
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-2": timeout, "test-4": timeout}}
		}, "no_answer", "[test-2 test-4]"},
		{"a refused request", func(h *prRun) {
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-4": refused}}
		}, "app_unreachable", ""},
		{"no test passed", func(h *prRun) {
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-1": ordinary, "test-2": ordinary, "test-3": ordinary, "test-4": ordinary}}
		}, "no_test_passed", ""},
		{"preserveFailedMocks", func(h *prRun) {
			h.replayer.config.Test.PreserveFailedMocks = true
			h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-4": ordinary}}
		}, "preserve_failed_mocks", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.RemoveUnusedMocks = true
			tc.setup(h)
			_ = h.run(t)

			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("precondition: the run made %d UpdateMocks calls; want 0", got)
			}
			warns := logs.FilterMessageSnippet("skipping mock pruning").All()
			if len(warns) != 1 {
				t.Fatalf("want one \"skipping mock pruning\" warning for the set, got %d", len(warns))
			}
			fields := warns[0].ContextMap()
			if got := fields["reason"]; got != tc.reason {
				t.Fatalf("the warning's reason = %v, want %q (fields %v)", got, tc.reason, fields)
			}
			got, named := fields["noAnswerTests"]
			if tc.noAnswer == "" && named {
				t.Fatalf("the warning names no-answer tests %v for a run where every request got an answer", got)
			}
			if tc.noAnswer != "" && fmt.Sprint(got) != tc.noAnswer {
				t.Fatalf("the warning's noAnswerTests = %v, want %s", got, tc.noAnswer)
			}
		})
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
		models.BaseTime, time.Now(), consumed, false, recordedSetShape{}); err != nil {
		t.Fatalf("SendMockFilterParamsToAgent: %v", err)
	}
	if !h.instr.lastParams.AgentOwnsConsumed {
		t.Fatal("precondition: the flag was not honoured on a healthy run, so this test cannot detect " +
			"the fallback it is about to assert")
	}

	// A retry cycle rewinds the CLI's map; nothing rewinds the agent's.
	r.rewindConsumedForRetryCycle(map[string]models.MockState{}, map[string]models.MockState{}, map[string]models.MockState{})

	if err := r.SendMockFilterParamsToAgent(context.Background(), nil,
		models.BaseTime, time.Now(), consumed, false, recordedSetShape{}); err != nil {
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
