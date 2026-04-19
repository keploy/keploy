package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
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

	// 2. Load mocks and get the expected mock names for this test case.
	expectedMocks, cleanup, err := r.loadMocks(ctx, opts.TestSetID, tc.Name, tc.HTTPReq.Timestamp)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		r.logger.Error("failed to load mocks; verify the recorded mocks exist and the mock store is accessible, then rerun the test",
			zap.String("testCase", tc.Name),
			zap.String("testSetID", opts.TestSetID),
			zap.Error(err),
		)
		return &TestResult{Passed: false, Error: fmt.Sprintf("failed to load mocks for test case %q: %v", tc.Name, err), Noise: tc.Noise}
	}

	// 3. Wait for app.
	if err := waitForApp(ctx, opts.ServiceURL, 2*time.Minute, r.logger); err != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("app not reachable: %v", err), Noise: tc.Noise}
	}

	// 4. Global noise.
	noise := r.globalNoise

	// to rule out config mocks from consumed list
	_, err = r.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		return &TestResult{
			Passed: false,
			Error:  fmt.Sprintf("failed to get consumed mocks: %v. Verify the instrumentation is running correctly and retry the test", err),
			Noise:  tc.Noise,
		}
	}

	// 5. Execute: rewrite URL, fire HTTP request, compare response.
	passed, respCompare, execErr := r.executeAndCompare(ctx, tc, opts.ServiceURL, noise)
	if execErr != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("test execution failed: %v", execErr), Noise: tc.Noise}
	}

	// 6. Check mock mismatches (expected from mapping vs actually consumed).
	mismatch := r.checkMockMismatches(ctx, expectedMocks)

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

func (r *Runner) loadMocks(ctx context.Context, testSetID, testCaseName string, backdate time.Time) ([]string, func(), error) {

	if r.instrumentation == nil && r.mockDB == nil {
		return nil, nil, fmt.Errorf("mock loading requires instrumentation and mockDB; initialize both dependencies before running integration tests with mocks")
	}
	if r.instrumentation == nil {
		return nil, nil, fmt.Errorf("mock loading requires instrumentation; initialize the instrumentation dependency before running integration tests with mocks")
	}
	if r.mockDB == nil {
		return nil, nil, fmt.Errorf("mock loading requires mockDB; initialize the mock database dependency before running integration tests with mocks")
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	// cleanup tears down the instrumentation context. The caller must defer
	// this so mocks remain active throughout test execution.
	cleanup := func() {
		if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		cancel()
		if err := g.Wait(); err != nil {
			r.logger.Error("mock teardown failed; check agent shutdown and cleanup logs to identify the failing teardown step",
				zap.Error(err),
				zap.String("testSetID", testSetID),
				zap.String("testCaseName", testCaseName),
			)
		}
	}

	// Setup eBPF hooks / proxy.
	if r.config != nil {
		if err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{
			Container:   r.config.ContainerName,
			CommandType: r.config.CommandType,
			DockerDelay: r.config.BuildDelay,
			BuildDelay:  r.config.BuildDelay,
			Mode:        models.MODE_TEST,
		}); err != nil {
			return nil, cleanup, fmt.Errorf("setup failed: %w", err)
		}
	}

	// Enable mock-outgoing mode with full config options.
	outOpts := models.OutgoingOptions{
		Mocking:  true,
		Backdate: backdate,
	}
	if r.config != nil {
		outOpts.Rules = r.config.BypassRules
		outOpts.MongoPassword = r.config.Test.MongoPassword
		outOpts.SQLDelay = time.Duration(r.config.Test.Delay)
		outOpts.DisableAutoHeaderNoise = r.config.Test.DisableAutoHeaderNoise
	}
	// Extract header noise for mock matching (mirrors replay behavior).
	if headerNoise, ok := r.globalNoise["header"]; ok {
		outOpts.NoiseConfig = map[string]map[string][]string{"header": headerNoise}
	}
	if err := r.instrumentation.MockOutgoing(ctx, outOpts); err != nil {
		return nil, cleanup, fmt.Errorf("mock-outgoing failed: %w", err)
	}

	// Resolve which mocks are needed.
	mocksThatHaveMappings, mocksWeNeed, expected, err := r.resolveMockSets(ctx, testSetID, testCaseName)
	if err != nil {
		return nil, cleanup, fmt.Errorf("failed to resolve mock sets: %w", err)
	}

	// Fetch mocks.
	afterTime := models.BaseTime
	beforeTime := time.Now()
	filtered, err := r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return expected, cleanup, fmt.Errorf("failed to get filtered mocks: %w", err)
	}
	unfiltered, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return expected, cleanup, fmt.Errorf("failed to get unfiltered mocks: %w", err)
	}

	// Push to proxy.
	if err := r.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
		return expected, cleanup, fmt.Errorf("failed to store mocks: %w", err)
	}

	// Send filter params.
	params := models.MockFilterParams{
		AfterTime:       afterTime,
		BeforeTime:      beforeTime,
		UseMappingBased: true,
		MockMapping:     expected,
	}
	// When config is nil (unit tests, embedders), follow the shipped
	// config.Test.StrictMockWindow default (false in Phase 1, opt-in
	// via keploy.yaml or KEPLOY_STRICT_MOCK_WINDOW=1). The env override
	// still applies at the agent, so users can force strict without
	// editing code.
	if r.config != nil {
		params.StrictMockWindow = r.config.Test.StrictMockWindow
	} else {
		params.StrictMockWindow = false
	}
	if err := r.instrumentation.UpdateMockParams(ctx, params); err != nil {
		return expected, cleanup, fmt.Errorf("failed to update mock params: %w", err)
	}

	if err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx); err != nil {
		r.logger.Debug("failed to mark agent ready", zap.Error(err))
	}

	r.logger.Info("mocks loaded", zap.String("testSetID", testSetID),
		zap.Int("filtered", len(filtered)), zap.Int("unfiltered", len(unfiltered)))
	return expected, cleanup, nil
}

func (r *Runner) resolveMockSets(ctx context.Context, testSetID, testCaseName string) (mocksThatHaveMappings, mocksWeNeed map[string]bool, expected []string, err error) {
	mocksThatHaveMappings = make(map[string]bool)
	mocksWeNeed = make(map[string]bool)

	if r.mappingDB == nil {
		err = fmt.Errorf("mappingDB not configured; initialize the mapping database dependency to resolve which mocks are needed for test execution")
		return
	}
	testMockMappings, hasMeaningful, getErr := r.mappingDB.Get(ctx, testSetID)
	if getErr != nil {
		err = fmt.Errorf("failed to get mock mappings for test set %q: %w", testSetID, getErr)
		return
	}
	if !hasMeaningful {
		err = fmt.Errorf("no mock mappings found for test set %q", testSetID)
		return
	}
	for _, mocks := range testMockMappings {
		for _, m := range mocks {
			mocksThatHaveMappings[m.Name] = true
		}
	}

	mocks, ok := testMockMappings[testCaseName]
	if !ok {
		return
	}
	for _, m := range mocks {
		mocksWeNeed[m.Name] = true
		expected = append(expected, m.Name)
	}
	return
}

func (r *Runner) checkMockMismatches(ctx context.Context, expected []string) *MockMismatch {
	if r.instrumentation == nil {
		return nil
	}
	states, err := r.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		r.logger.Debug("failed to get consumed mocks", zap.Error(err))
		return nil
	}
	var consumed []string
	for _, s := range states {
		consumed = append(consumed, s.Name)
	}
	return &MockMismatch{
		ExpectedMocks: expected,
		ConsumedMocks: consumed,
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
