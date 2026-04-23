package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	keployPkg "go.keploy.io/server/v3/pkg"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Runner implements Service with all mock-loading and test execution inline.
type Runner struct {
	logger          *zap.Logger
	testCaseDB      TestCaseDB
	instrumentation Instrumentation
	mockDB          MockDB
	mappingDB       MappingDB
	config          *config.Config
	globalNoise     models.GlobalNoise

	// Per-test-set setup is cached so MockOutgoing / GetMocks / StoreMocks
	// run once per TestSetID instead of once per step. Mirrors replay.go's
	// RunTestSet layout — load the full mock pool for the set, then issue
	// per-test UpdateMockParams as each step runs. Stateful-protocol
	// matchers (PostgresV2 / MySQL) key off per-connID sortOrder across
	// tests in a set; replacing the pool between steps invalidates that
	// progression and produces spurious mock-miss errors on reused
	// connections (HikariCP, pgBouncer).
	setupMu    sync.Mutex
	currentSet *testSetSetup
}

// testSetSetup holds the cached per-test-set state so repeated RunTest
// calls for the same TestSetID skip the heavy setup path.
//
// mockKindByName is built from the loaded mock pool so mismatch
// reporting can filter DNS entries — DNS resolution order is
// non-deterministic and MockEntry.Kind on mappings is frequently empty,
// so replay.go:1499-1510 uses the same name→Kind fallback.
//
// useMappingBased mirrors replay's determineMockingStrategy output
// (replay.go:3443) so the DisableMapping config flag is honoured
// instead of being hardcoded to true.
//
// totalConsumed accumulates across every step in the set. It is sent
// on each UpdateMockParams as TotalConsumedMocks so stateful matchers
// (PostgresV2 / MySQL / Mongo v2) can see which mocks earlier steps
// already consumed — replay.go maintains the equivalent local map
// inside RunTestSet.
type testSetSetup struct {
	id              string
	mappings        map[string][]models.MockEntry
	mockKindByName  map[string]models.Kind
	useMappingBased bool
	cleanup         func()

	consumedMu    sync.Mutex
	totalConsumed map[string]models.MockState
}

func (s *testSetSetup) mergeConsumed(mocks []models.MockState) {
	if len(mocks) == 0 {
		return
	}
	s.consumedMu.Lock()
	defer s.consumedMu.Unlock()
	for _, m := range mocks {
		s.totalConsumed[m.Name] = m
	}
}

func (s *testSetSetup) snapshotConsumed() map[string]models.MockState {
	s.consumedMu.Lock()
	defer s.consumedMu.Unlock()
	out := make(map[string]models.MockState, len(s.totalConsumed))
	for k, v := range s.totalConsumed {
		out[k] = v
	}
	return out
}

// NewRunner creates a Runner with all dependencies.
func New(
	logger *zap.Logger,
	testCaseDB TestCaseDB,
	instrumentation Instrumentation,
	mockDB MockDB,
	mappingDB MappingDB,
	cfg *config.Config,
	globalNoise models.GlobalNoise,
) Service {
	return &Runner{
		logger:          logger,
		testCaseDB:      testCaseDB,
		instrumentation: instrumentation,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		config:          cfg,
		globalNoise:     globalNoise,
	}
}

// Close tears down the current test-set's instrumentation state
// (NotifyGracefulShutdown + MockOutgoing errgroup). Callers that hold a
// Runner across multiple test-sets should invoke this when the session
// ends so the final set's resources are released; transitions between
// test-sets during normal operation call the same teardown internally.
func (r *Runner) Close() error {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.currentSet != nil && r.currentSet.cleanup != nil {
		r.currentSet.cleanup()
	}
	r.currentSet = nil
	return nil
}

func (r *Runner) RunTest(ctx context.Context, opts RunTestOpts) *TestResult {

	if opts.TestSetID == "" {
		return &TestResult{Passed: false, Error: "TestSetID is required"}
	}

	if opts.TestStepID == "" {
		return &TestResult{Passed: false, Error: "TestStepID is required"}
	}

	if opts.ServiceURL == "" {
		return &TestResult{Passed: false, Error: "ServiceURL is required"}
	}

	// 1. Fetch the recorded test case.
	tc, err := r.loadTestCase(ctx, opts.TestSetID, opts.TestStepID)
	if err != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("failed to load test case: %v", err)}
	}

	// 2. Set up the test-set (idempotent per TestSetID — runs MockOutgoing,
	//    GetMocks, StoreMocks once per set, then caches).
	setup, err := r.ensureTestSetLoaded(ctx, opts.TestSetID, tc.HTTPReq.Timestamp)
	if err != nil {
		r.logger.Error("failed to load mocks; verify the recorded mocks exist and the mock store is accessible, then rerun the test",
			zap.String("testCase", tc.Name),
			zap.String("testSetID", opts.TestSetID),
			zap.Error(err),
		)
		return &TestResult{Passed: false, Error: fmt.Sprintf("failed to load mocks for test case %q: %v", tc.Name, err), Noise: tc.Noise}
	}

	// 3. Wait for app. Do this before per-test filter params so the
	//    agent's initial wide-window UpdateMockParams (set in setup)
	//    remains active for any startup-init traffic the app emits
	//    before this step's narrower window kicks in. Matches replay's
	//    ordering where the initial broad UpdateMockParams precedes
	//    the first per-test one (replay.go:1047 / 1156 → 1433).
	if err := waitForApp(ctx, opts.ServiceURL, 2*time.Minute, r.logger); err != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("app not reachable: %v", err), Noise: tc.Noise}
	}

	// 4. Apply this step's per-test filter parameters. TotalConsumedMocks
	//    carries the cumulative consumption from earlier steps so
	//    stateful-protocol matchers can advance sortOrder correctly.
	expectedMocks := expectedMocksForTest(setup, tc.Name)
	if err := r.sendPerTestParams(ctx, setup, expectedMocks, tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp); err != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("failed to apply per-test filter params: %v", err), Noise: tc.Noise}
	}

	// 5. Global noise.
	noise := r.globalNoise

	// 6. Execute: rewrite URL, fire HTTP request, compare response.
	passed, respCompare, execErr := r.executeAndCompare(ctx, tc, opts.ServiceURL, noise)
	if execErr != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("test execution failed: %v", execErr), Noise: tc.Noise}
	}

	// 7. Capture this step's consumed mocks — used both for mismatch
	//    reporting and merged into the set-level totalConsumed that's
	//    sent on subsequent UpdateMockParams (replay.go:1470-1486).
	consumed, consumedErr := r.instrumentation.GetConsumedMocks(ctx)
	if consumedErr != nil {
		r.logger.Debug("failed to get consumed mocks", zap.Error(consumedErr))
	}
	setup.mergeConsumed(consumed)

	// 8. Check mock mismatches (DNS-filtered — DNS resolution order is
	//    non-deterministic, so it would produce spurious diffs).
	mismatch := r.checkMockMismatches(setup, expectedMocks, consumed)

	diffJSON, err := json.Marshal(respCompare)
	if err != nil {
		return &TestResult{
			Passed:         false,
			Error:          fmt.Sprintf("failed to serialize response diff: %v", err),
			Diff:           fmt.Sprintf("%+v", respCompare),
			MockMismatches: mismatch,
			Noise:          tc.Noise,
		}
	}

	return &TestResult{
		Passed:         passed,
		Diff:           string(diffJSON),
		MockMismatches: mismatch,
		Noise:          tc.Noise,
	}
}

// --- internal implementation ---

func (r *Runner) executeAndCompare(ctx context.Context, tc *models.TestCase, serviceURL string, noise models.GlobalNoise) (bool, models.RespCompare, error) {
	tcCopy := *tc

	orig, err := url.Parse(tc.HTTPReq.URL)
	if err != nil {
		return false, models.RespCompare{}, fmt.Errorf("invalid original URL: %w", err)
	}
	svc, err := url.Parse(serviceURL)
	if err != nil {
		return false, models.RespCompare{}, fmt.Errorf("invalid service URL: %w", err)
	}
	orig.Scheme = svc.Scheme
	orig.Host = svc.Host
	httpReq := tc.HTTPReq
	httpReq.URL = orig.String()
	tcCopy.HTTPReq = httpReq

	actual, err := keployPkg.SimulateHTTP(ctx, &tcCopy, "", r.logger, keployPkg.SimulationConfig{})
	if err != nil {
		return false, models.RespCompare{}, err
	}

	actualTC := *tc
	actualTC.HTTPResp = *actual

	passed, diff := httpMatcher.CompareHTTPResp(tc, &actualTC, noise, false, r.logger)
	return passed, diff, nil
}

func (r *Runner) loadTestCase(ctx context.Context, testSetID, testCaseName string) (*models.TestCase, error) {
	if r.testCaseDB == nil {
		return nil, fmt.Errorf("testCaseDB not configured")
	}
	return r.testCaseDB.GetTestCase(ctx, testSetID, testCaseName)
}

// ensureTestSetLoaded is idempotent per TestSetID. It primes the agent
// with the full mock pool for the set — MockOutgoing, GetFiltered/
// UnFilteredMocks, StoreMocks, MakeAgentReadyForDockerCompose — once,
// then caches the setup. Subsequent calls for the same TestSetID are
// a noop. A new TestSetID tears down the previous set before re-running
// the setup.
//
// The "once per set, not per step" design mirrors replay.go:RunTestSet
// (pkg/service/replay/replay.go:960-1055). Stateful-protocol matchers
// track per-connID sortOrder across tests in a set; replacing the pool
// between steps invalidates that progression and starves reused
// connections (HikariCP, pgBouncer) of their matching per-test mocks.
func (r *Runner) ensureTestSetLoaded(ctx context.Context, testSetID string, backdate time.Time) (*testSetSetup, error) {
	if r.instrumentation == nil && r.mockDB == nil {
		return nil, fmt.Errorf("mock loading requires instrumentation and mockDB; initialize both dependencies before running integration tests with mocks")
	}
	if r.instrumentation == nil {
		return nil, fmt.Errorf("mock loading requires instrumentation; initialize the instrumentation dependency before running integration tests with mocks")
	}
	if r.mockDB == nil {
		return nil, fmt.Errorf("mock loading requires mockDB; initialize the mock database dependency before running integration tests with mocks")
	}

	r.setupMu.Lock()
	defer r.setupMu.Unlock()

	if r.currentSet != nil && r.currentSet.id == testSetID {
		return r.currentSet, nil
	}

	if r.currentSet != nil && r.currentSet.cleanup != nil {
		r.currentSet.cleanup()
	}
	r.currentSet = nil

	setup, err := r.setupTestSet(ctx, testSetID, backdate)
	if err != nil {
		return nil, err
	}
	r.currentSet = setup
	return setup, nil
}

// setupTestSet runs the one-shot per-set work: errgroup-wrapped
// MockOutgoing + disk-level GetMocks + StoreMocks. Returns the setup
// record with a cleanup closure the caller stores on r.currentSet.
func (r *Runner) setupTestSet(parentCtx context.Context, testSetID string, backdate time.Time) (*testSetSetup, error) {
	g, gCtx := errgroup.WithContext(parentCtx)
	gCtx, cancel := context.WithCancel(context.WithValue(gCtx, models.ErrGroupKey, g))

	cleanupOnce := sync.Once{}
	cleanup := func() {
		cleanupOnce.Do(func() {
			if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
				r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
			}
			cancel()
			if err := g.Wait(); err != nil {
				r.logger.Error("mock teardown failed; check agent shutdown and cleanup logs to identify the failing teardown step",
					zap.Error(err),
					zap.String("testSetID", testSetID),
				)
			}
		})
	}
	// Until the setup succeeds, abort via cleanup on error.
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	if r.config != nil {
		if err := r.instrumentation.Setup(gCtx, r.config.Command, models.SetupOptions{
			Container:   r.config.ContainerName,
			CommandType: r.config.CommandType,
			DockerDelay: r.config.BuildDelay,
			BuildDelay:  r.config.BuildDelay,
			Mode:        models.MODE_TEST,
		}); err != nil {
			return nil, fmt.Errorf("setup failed: %w", err)
		}
	}

	// Gate every subsequent instrumentation call on the Keploy agent
	// actually being reachable. In sandbox / docker-compose runs the
	// keploy container was created moments ago and its host port forward
	// is still settling — firing MockOutgoing immediately produces a
	// "connect: connection refused" on the agent URL. Mirrors replay's
	// pre-MockOutgoing AgentHealthTicker gate (replay.go:939-952).
	if r.config != nil && r.config.Agent.AgentURI != "" {
		agentCtx, cancel := context.WithTimeout(gCtx, 120*time.Second)
		agentReadyCh := make(chan bool, 1)
		go keployPkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)
		select {
		case <-gCtx.Done():
			cancel()
			return nil, gCtx.Err()
		case <-agentCtx.Done():
			cancel()
			return nil, fmt.Errorf("keploy-agent at %s did not become ready within 120s; check the agent container logs and ensure its host port is reachable", r.config.Agent.AgentURI)
		case <-agentReadyCh:
		}
		cancel()
	}

	outOpts := models.OutgoingOptions{
		Mocking:  true,
		Backdate: backdate,
	}
	if r.config != nil {
		outOpts.Rules = r.config.BypassRules
		outOpts.MongoPassword = r.config.Test.MongoPassword
		outOpts.SQLDelay = time.Duration(r.config.Test.Delay) * time.Second
		outOpts.DisableAutoHeaderNoise = r.config.Test.DisableAutoHeaderNoise
	}
	if headerNoise, ok := r.globalNoise["header"]; ok {
		outOpts.NoiseConfig = map[string]map[string][]string{"header": headerNoise}
	}
	if err := r.instrumentation.MockOutgoing(gCtx, outOpts); err != nil {
		return nil, fmt.Errorf("mock-outgoing failed: %w", err)
	}

	mappings, mocksThatHaveMappings, mocksWeNeed, err := r.loadMappingsForSet(gCtx, testSetID)
	if err != nil {
		return nil, err
	}

	// Disk fetch uses the widest window; per-test containment is
	// enforced by the agent via UpdateMockParams at step time.
	filtered, err := r.mockDB.GetFilteredMocks(gCtx, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return nil, fmt.Errorf("failed to get filtered mocks: %w", err)
	}
	unfiltered, err := r.mockDB.GetUnFilteredMocks(gCtx, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return nil, fmt.Errorf("failed to get unfiltered mocks: %w", err)
	}

	// mockKindByName lets the mismatch-reporting path filter DNS
	// entries even when the mapping-entry Kind is empty
	// (replay.go:1499-1510 uses the same name→Kind fallback).
	mockKindByName := make(map[string]models.Kind, len(filtered)+len(unfiltered))
	for _, m := range filtered {
		mockKindByName[m.Name] = m.Kind
	}
	for _, m := range unfiltered {
		mockKindByName[m.Name] = m.Kind
	}

	// Seed the process-wide sort counter past the highest recorded
	// mock sortOrder so any live-generated mocks during replay don't
	// collide with the recorded mock ordering — replay.go:1128 does
	// the same on the non-compose path.
	keployPkg.InitSortCounter(int64(max(len(filtered), len(unfiltered))))

	if err := r.instrumentation.StoreMocks(gCtx, filtered, unfiltered); err != nil {
		return nil, fmt.Errorf("failed to store mocks: %w", err)
	}

	if err := r.instrumentation.MakeAgentReadyForDockerCompose(gCtx); err != nil {
		r.logger.Debug("failed to mark agent ready", zap.Error(err))
	}

	// useMappingBased mirrors replay's determineMockingStrategy
	// (replay.go:3443). Mapping-based filtering is enabled when
	// mappings exist AND DisableMapping is not set.
	useMappingBased := len(mappings) > 0
	if r.config != nil && r.config.DisableMapping {
		useMappingBased = false
	}

	// Send an initial wide-window UpdateMockParams with empty
	// MockMapping so the agent has a valid filter active for any
	// startup-init traffic (HikariCP pool warm-up, driver handshakes)
	// that lands before the first per-test RunTest narrows the
	// window. Matches replay.go:1047 / 1156.
	initialParams := models.MockFilterParams{
		AfterTime:          models.BaseTime,
		BeforeTime:         time.Now(),
		MockMapping:        []string{},
		UseMappingBased:    useMappingBased,
		TotalConsumedMocks: map[string]models.MockState{},
		StrictMockWindow:   r.strictMockWindow(),
	}
	if err := r.instrumentation.UpdateMockParams(gCtx, initialParams); err != nil {
		return nil, fmt.Errorf("failed to set initial mock filter params: %w", err)
	}

	r.logger.Info("mocks loaded", zap.String("testSetID", testSetID),
		zap.Int("filtered", len(filtered)), zap.Int("unfiltered", len(unfiltered)),
		zap.Bool("useMappingBased", useMappingBased))

	success = true
	return &testSetSetup{
		id:              testSetID,
		mappings:        mappings,
		mockKindByName:  mockKindByName,
		useMappingBased: useMappingBased,
		totalConsumed:   map[string]models.MockState{},
		cleanup:         cleanup,
	}, nil
}

// strictMockWindow resolves the strict-window flag. cfg==nil
// (embedders) follows the shipped default of true; the agent-side env
// override KEPLOY_STRICT_MOCK_WINDOW=0 still wins.
func (r *Runner) strictMockWindow() bool {
	if r.config == nil {
		return true
	}
	return r.config.Test.StrictMockWindow
}

// loadMappingsForSet returns the full per-test-case mapping and the
// set-level UNION of expected mocks across every test in the set.
// The UNION is what we pass to GetFilteredMocks / GetUnFilteredMocks
// so the StoreMocks call covers every step — per-step filtering then
// happens via UpdateMockParams.
func (r *Runner) loadMappingsForSet(ctx context.Context, testSetID string) (map[string][]models.MockEntry, map[string]bool, map[string]bool, error) {
	if r.mappingDB == nil {
		return nil, nil, nil, fmt.Errorf("mappingDB not configured; initialize the mapping database dependency to resolve which mocks are needed for test execution")
	}
	testMockMappings, hasMeaningful, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get mock mappings for test set %q: %w", testSetID, err)
	}
	if !hasMeaningful {
		return nil, nil, nil, fmt.Errorf("no mock mappings found for test set %q", testSetID)
	}
	mocksThatHaveMappings := make(map[string]bool)
	mocksWeNeed := make(map[string]bool)
	for _, mocks := range testMockMappings {
		for _, m := range mocks {
			mocksThatHaveMappings[m.Name] = true
			mocksWeNeed[m.Name] = true
		}
	}
	return testMockMappings, mocksThatHaveMappings, mocksWeNeed, nil
}

// expectedMocksForTest returns this step's expected mock-name list from
// the cached mapping. No error case — a missing test case name means
// the step has no mapped mocks, which is legal.
func expectedMocksForTest(setup *testSetSetup, testCaseName string) []string {
	if setup == nil {
		return nil
	}
	entries, ok := setup.mappings[testCaseName]
	if !ok {
		return nil
	}
	expected := make([]string, 0, len(entries))
	for _, m := range entries {
		expected = append(expected, m.Name)
	}
	return expected
}

// sendPerTestParams issues the per-step UpdateMockParams. The window
// [tcReqTime, tcRespTime] is what the agent's strict-window filter
// uses to bucket per-test mocks for this specific step.
// TotalConsumedMocks is a snapshot of everything consumed so far in
// the set so stateful matchers can advance sortOrder correctly across
// steps — replay.go:2440-2447 does the same.
func (r *Runner) sendPerTestParams(ctx context.Context, setup *testSetSetup, expected []string, tcReqTime, tcRespTime time.Time) error {
	params := models.MockFilterParams{
		AfterTime:          tcReqTime,
		BeforeTime:         tcRespTime,
		UseMappingBased:    setup.useMappingBased,
		MockMapping:        expected,
		TotalConsumedMocks: setup.snapshotConsumed(),
		StrictMockWindow:   r.strictMockWindow(),
	}
	return r.instrumentation.UpdateMockParams(ctx, params)
}

// checkMockMismatches filters DNS entries from both the expected and
// consumed sets before reporting — DNS resolution order is
// non-deterministic (replay.go:1499-1517 does the same). MockEntry.Kind
// in mappings is frequently empty, so we fall back to the kind registry
// built from the loaded mock pool.
func (r *Runner) checkMockMismatches(setup *testSetSetup, expected []string, consumed []models.MockState) *MockMismatch {
	if r.instrumentation == nil {
		return nil
	}
	isDNS := func(name string, kind models.Kind) bool {
		if kind == models.DNS {
			return true
		}
		if setup != nil {
			if k, ok := setup.mockKindByName[name]; ok && k == models.DNS {
				return true
			}
		}
		return false
	}

	filteredExpected := make([]string, 0, len(expected))
	for _, name := range expected {
		if !isDNS(name, "") {
			filteredExpected = append(filteredExpected, name)
		}
	}
	filteredConsumed := make([]string, 0, len(consumed))
	for _, s := range consumed {
		if !isDNS(s.Name, s.Kind) {
			filteredConsumed = append(filteredConsumed, s.Name)
		}
	}
	return &MockMismatch{
		ExpectedMocks: filteredExpected,
		ConsumedMocks: filteredConsumed,
	}
}

// waitForApp blocks until the app at serviceURL responds to an HTTP
// request, or the timeout elapses. TCP reachability is no longer
// checked separately — the HTTP probe's underlying dial subsumes it
// and catches the strictly larger set of not-ready conditions (see
// below).
//
// The HTTP probe is what makes this correct for docker-compose
// deployments. `ports: a:b` has dockerd bind the host listener at
// container-create time, so a plain TCP dial succeeds against
// dockerd's port forwarder while the in-container app is still
// booting. Requests that follow get forwarded to a dead inner socket
// and come back ECONNRESET — the exact symptom that broke the
// enterprise sandbox's auto-replay phase on macOS.
//
// Any HTTP status code (200/404/400/405/501/...) counts as ready —
// we're not asserting anything about a specific endpoint, just that
// the app has accepted a connection, routed it, and produced a
// response. Only a transport-level error means it's still coming up.
//
// Native processes (bind-ready == traffic-ready) and Kubernetes
// Services (gated on Pod readiness by the control plane) satisfy
// the HTTP probe trivially. The HTTP probe adds failure modes that
// a TCP dial alone would have missed — client-side request timeout
// (3s per probe), TLS handshake / cert errors, slow header writes —
// and each of those conditions legitimately represents an app that
// isn't yet ready to serve traffic end-to-end, which is the signal
// we want during startup.
func waitForApp(ctx context.Context, serviceURL string, timeout time.Duration, logger *zap.Logger) error {
	parsed, err := url.Parse(serviceURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid service URL %q: check the ServiceURL and ensure it includes a valid host", serviceURL)
	}
	// Scheme is validated up front — the HTTP probe below can only dial
	// http/https. An unsupported scheme would make every probe attempt
	// fail at http.NewRequestWithContext time and spin the loop all
	// the way to the timeout with a misleading "timed out" error.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid service URL %q: scheme must be http or https (got %q)", serviceURL, parsed.Scheme)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	// Scope the overall waiting deadline up front so every probe
	// attempt — including the very first — is cancelled when the
	// budget runs out. Using the parent ctx for the probe while the
	// outer select watches waitCtx would let a single slow probe run
	// past the budget.
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probeClient := &http.Client{
		Timeout: 3 * time.Second,
		// New connection each probe so we're exercising real readiness
		// rather than a cached keep-alive from an earlier attempt.
		Transport: &http.Transport{DisableKeepAlives: true},
		// Don't follow redirects. Our readiness contract is "any HTTP
		// status code = ready": a 301/302 from a warming-up app IS a
		// valid response, but the default http.Client would chase it
		// and potentially error out on the redirect target (TLS failure,
		// unreachable host, redirect loop) — surfacing a false negative.
		// Returning ErrUseLastResponse lets Do() return the raw 3xx.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Build the probe URL from scheme + host only. `executeAndCompare`
	// later uses only `svc.Scheme` / `svc.Host` from the configured
	// ServiceURL (see runner.go:executeAndCompare), so a user setting
	// ServiceURL = "http://localhost:8080/api/v1" should NOT cause the
	// readiness probe to HEAD /api/v1 — the app's root may respond
	// with 200 while /api/v1 is a 404 that doesn't exist yet, or vice
	// versa. Probing the root gives us the same liveness signal every
	// deployment type (native / k8s / docker-compose) satisfies.
	probeURL := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/"}).String()

	// probe issues a single HTTP HEAD against probeURL and returns:
	//   nil          — response received (any status code; we care
	//                  about liveness, not correctness).
	//   fatal=true   — deterministic request-construction error; no
	//                  point retrying.
	//   fatal=false  — transport error (connect refused, reset, TLS
	//                  handshake fail, etc.); retry on the next tick.
	//
	// HEAD instead of GET: we only care that the handler pipeline is
	// live. HEAD doesn't trigger response-body generation, so it's
	// safer against apps whose `/` has side effects (analytics pings,
	// DB reads, expensive rendering) during startup. Servers that
	// don't implement HEAD for the route will reply 405 Method Not
	// Allowed / 501 Not Implemented — still counts as ready because
	// the connection was accepted and routed.
	//
	// The HTTP client performs its own TCP dial, so an explicit
	// net.DialTimeout before the HEAD would double the number of
	// connections per probe without catching anything the HEAD
	// doesn't already catch. Dropped for that reason.
	probe := func() (fatal bool, err error) {
		req, reqErr := http.NewRequestWithContext(waitCtx, http.MethodHead, probeURL, nil)
		if reqErr != nil {
			return true, reqErr
		}
		resp, httpErr := probeClient.Do(req)
		if httpErr != nil {
			return false, httpErr
		}
		_ = resp.Body.Close()
		return false, nil
	}

	// Track the most recent probe error so the timeout message can
	// surface the actual failure mode (TLS handshake error, https-vs-
	// http mismatch, connection reset, DNS failure, ...) instead of a
	// generic "timed out" — operators need the last observed error to
	// diagnose why the app never became reachable.
	var lastErr error
	if fatal, err := probe(); err == nil {
		logger.Debug("app is reachable", zap.String("addr", addr))
		return nil
	} else if fatal {
		return fmt.Errorf("failed to probe %s: %w", addr, err)
	} else {
		lastErr = err
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for app at %s (last probe error: %v); check that the service is running and the ServiceURL is correct", addr, lastErr)
			}
			return fmt.Errorf("timed out waiting for app at %s; check that the service is running and the ServiceURL is correct", addr)
		case <-ticker.C:
			if fatal, err := probe(); err == nil {
				logger.Debug("app is reachable", zap.String("addr", addr))
				return nil
			} else if fatal {
				return fmt.Errorf("failed to probe %s: %w", addr, err)
			} else {
				lastErr = err
			}
		}
	}
}
