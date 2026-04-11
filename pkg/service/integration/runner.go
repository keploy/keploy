package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"time"

	keployPkg "go.keploy.io/server/v3/pkg"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/config"
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
func NewRunner(
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
	// 1. Fetch the recorded test case.
	tc, err := r.loadTestCase(ctx, opts.TestSetID, opts.TestStepID)
	if err != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("failed to load test case: %v", err)}
	}

	// 2. Load mocks and get the expected mock names for this test case.
	expectedMocks, err := r.loadMocks(ctx, opts.TestSetID, tc.Name)
	if err != nil {
		r.logger.Error("failed to load mocks", zap.String("testCase", tc.Name), zap.Error(err))
	}

	// 3. Wait for app.
	if opts.ServiceURL != "" {
		waitForApp(ctx, opts.ServiceURL, 2*time.Minute, r.logger)
	}

	// 4. Global noise.
	noise := r.globalNoise

	// to rule out config mocks from consumed list
	_, err = r.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		r.logger.Debug("failed to get consumed mocks", zap.Error(err))
		return nil
	}

	// 5. Execute: rewrite URL, fire HTTP request, compare response.
	passed, respCompare, execErr := r.executeAndCompare(ctx, tc, opts.ServiceURL, noise)
	if execErr != nil {
		return &TestResult{Passed: false, Error: fmt.Sprintf("test execution failed: %v", execErr), Noise: tc.Noise}
	}

	diffJSON, _ := json.Marshal(respCompare)

	// 6. Check mock mismatches (expected from mapping vs actually consumed).
	mismatch := r.checkMockMismatches(ctx, expectedMocks)

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

	if serviceURL != "" {
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
	}

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

func (r *Runner) loadMocks(ctx context.Context, testSetID, testCaseName string) ([]string, error) {
	if r.instrumentation == nil || r.mockDB == nil {
		return nil, nil
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))
	defer func() {
		if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		cancel()
		_ = g.Wait()
	}()

	// Setup eBPF hooks / proxy.
	if r.config != nil {
		if err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{
			Container:   r.config.ContainerName,
			CommandType: r.config.CommandType,
			DockerDelay: r.config.BuildDelay,
			BuildDelay:  r.config.BuildDelay,
			Mode:        models.MODE_TEST,
		}); err != nil {
			return nil, fmt.Errorf("setup failed: %w", err)
		}
	}

	// Enable mock-outgoing mode.
	if err := r.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
		return nil, fmt.Errorf("mock-outgoing failed: %w", err)
	}

	// Resolve which mocks are needed.
	mocksThatHaveMappings, mocksWeNeed := r.resolveMockSets(ctx, testSetID, testCaseName)

	// Build the expected mock names list from the mapping.
	var expected []string
	for name := range mocksWeNeed {
		expected = append(expected, name)
	}

	// Fetch mocks.
	afterTime := models.BaseTime
	beforeTime := time.Now()
	filtered, err := r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return expected, fmt.Errorf("failed to get filtered mocks: %w", err)
	}
	unfiltered, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return expected, fmt.Errorf("failed to get unfiltered mocks: %w", err)
	}

	// Push to proxy.
	if err := r.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
		return expected, fmt.Errorf("failed to store mocks: %w", err)
	}

	// Send filter params.
	params := models.MockFilterParams{
		AfterTime:       afterTime,
		BeforeTime:      beforeTime,
		UseMappingBased: true,
	}
	if err := r.instrumentation.UpdateMockParams(ctx, params); err != nil {
		return expected, fmt.Errorf("failed to update mock params: %w", err)
	}

	if err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx); err != nil {
		r.logger.Debug("failed to mark agent ready", zap.Error(err))
	}

	r.logger.Info("mocks loaded", zap.String("testSetID", testSetID),
		zap.Int("filtered", len(filtered)), zap.Int("unfiltered", len(unfiltered)))
	return expected, nil
}

func (r *Runner) resolveMockSets(ctx context.Context, testSetID, testCaseName string) (mocksThatHaveMappings, mocksWeNeed map[string]bool) {
	mocksThatHaveMappings = make(map[string]bool)
	mocksWeNeed = make(map[string]bool)

	if r.mappingDB == nil {
		return
	}
	testMockMappings, hasMeaningful, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil || !hasMeaningful {
		return
	}
	for _, mocks := range testMockMappings {
		for _, m := range mocks {
			mocksThatHaveMappings[m.Name] = true
		}
	}
	if testCaseName != "" {
		if mocks, ok := testMockMappings[testCaseName]; ok {
			for _, m := range mocks {
				mocksWeNeed[m.Name] = true
			}
		}
	} else {
		mocksWeNeed = mocksThatHaveMappings
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

func waitForApp(ctx context.Context, serviceURL string, timeout time.Duration, logger *zap.Logger) {
	parsed, err := url.Parse(serviceURL)
	if err != nil || parsed.Host == "" {
		return
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

	if conn, dialErr := net.DialTimeout("tcp", addr, 500*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			logger.Warn("timed out waiting for app", zap.String("addr", addr))
			return
		case <-ticker.C:
			if conn, dialErr := net.DialTimeout("tcp", addr, 500*time.Millisecond); dialErr == nil {
				_ = conn.Close()
				logger.Info("app is reachable", zap.String("addr", addr))
				return
			}
		}
	}
}
