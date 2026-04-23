package mock

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// MockLoader implements Service. It sets up the proxy and loads mocks for a
// given test set (and optionally a single test case) so that outgoing calls
// from the application under test are served from those mocks. It has no
// knowledge of running test cases or producing test reports.
type MockLoader struct {
	logger          *zap.Logger
	instrumentation Instrumentation
	mockDB          MockDB
	mappingDB       MappingDB
	config          *config.Config
	outgoingCfg     OutgoingConfig
}

// NewMockLoader constructs a MockLoader.
func NewMockLoader(
	logger *zap.Logger,
	instrumentation Instrumentation,
	mockDB MockDB,
	mappingDB MappingDB,
	cfg *config.Config,
	outgoingCfg OutgoingConfig,
) Service {
	return &MockLoader{
		logger:          logger,
		instrumentation: instrumentation,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		config:          cfg,
		outgoingCfg:     outgoingCfg,
	}
}

// LoadMocks fetches mocks for testSetID (filtered to testCaseName when
// non-empty) and pushes them into the proxy.
//
// The function mirrors the mock-loading path inside RunTestSet:
//  1. Creates a managed context with an errgroup for clean shutdown.
//  2. Calls Setup to load eBPF hooks and start the proxy.
//  3. Calls MockOutgoing to put the proxy in mock-serving mode.
//  4. Resolves the mapping-based or timestamp-based filtering strategy.
//  5. Fetches filtered and unfiltered mocks from MockDB.
//  6. Calls StoreMocks to push them into the proxy.
func (r *MockLoader) LoadMocks(ctx context.Context, testSetID string, testCaseName string) error {
	r.logger.Debug("MockLoader: loading mocks", zap.String("testSetID", testSetID), zap.String("testCaseName", testCaseName))

	// Build a managed context so every goroutine spawned inside this call
	// shuts down cleanly when the caller cancels or an error occurs.
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	defer func() {
		if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
			r.logger.Debug("MockLoader: failed to notify agent of graceful shutdown", zap.Error(err))
		}
		cancel()
		if err := g.Wait(); err != nil {
			r.logger.Error("MockLoader: error during shutdown", zap.Error(err))
		}
	}()

	// Step 1 – load eBPF hooks and start the proxy.
	if err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{
		Container:   r.config.ContainerName,
		CommandType: r.config.CommandType,
		DockerDelay: r.config.BuildDelay,
		BuildDelay:  r.config.BuildDelay,
		Mode:        models.MODE_TEST,
	}); err != nil {
		return fmt.Errorf("MockLoader: failed to set up instrumentation: %w", err)
	}

	// Step 2 – put the proxy in mock-serving mode.
	if err := r.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Rules:         r.outgoingCfg.BypassRules,
		MongoPassword: r.outgoingCfg.MongoPassword,
		SQLDelay:      r.outgoingCfg.SQLDelay,
		Mocking:       r.outgoingCfg.Mocking,
	}); err != nil {
		return fmt.Errorf("MockLoader: failed to enable mock-outgoing: %w", err)
	}

	// Step 3 – resolve which mocks are needed using the mapping table.
	mocksThatHaveMappings, mocksWeNeed := r.resolveMockSets(ctx, testSetID, testCaseName)

	// Step 4 – fetch mocks from the database.
	filteredMocks, unfilteredMocks, err := r.getMocks(ctx, testSetID, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return fmt.Errorf("MockLoader: failed to fetch mocks: %w", err)
	}

	// Step 5 – push the mocks into the proxy.
	if err := r.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
		return fmt.Errorf("MockLoader: failed to store mocks: %w", err)
	}

	expectedMockMapping := []string{}
	useMappingBased := len(expectedMockMapping) > 0
	err = r.SendMockFilterParamsToAgent(ctx, expectedMockMapping, models.BaseTime, time.Now(), nil, useMappingBased)
	if err != nil {
		return fmt.Errorf("MockLoader: failed to send mock filter params to agent: %w", err)
	}

	err = r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
	if err != nil {
		utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
	}

	r.logger.Info("MockLoader: mocks loaded successfully",
		zap.String("testSetID", testSetID),
		zap.Int("filtered", len(filteredMocks)),
		zap.Int("unfiltered", len(unfilteredMocks)),
	)
	return nil
}

// resolveMockSets uses the MappingDB (when available) to determine which mock
// names are relevant, mirroring the logic in Replayer.determineMockingStrategy
// and the mapping population block inside RunTestSet.
//
// Returns:
//   - mocksThatHaveMappings: all mock names that appear in any mapping entry
//     for the test set (used by GetFilteredMocks / GetUnFilteredMocks to decide
//     which bucket a mock belongs to).
//   - mocksWeNeed: the subset of mapped mocks that this particular load
//     actually requires (restricted to testCaseName when provided; all mapped
//     mocks otherwise).
//
// Both maps are empty when no meaningful mappings exist, which causes MockDB to
// fall back to timestamp-based filtering.
func (r *MockLoader) resolveMockSets(ctx context.Context, testSetID string, testCaseName string) (mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) {
	mocksThatHaveMappings = make(map[string]bool)
	mocksWeNeed = make(map[string]bool)

	if r.mappingDB == nil {
		r.logger.Debug("MockLoader: no mapping DB, using timestamp-based filtering")
		return
	}

	testMockMappings, hasMeaningfulMappings, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		// Downgraded from Warn to Info per repo logging guideline — this
		// path is a recoverable fallback (timestamp-based filtering is the
		// documented next step), not an operator-actionable warning.
		r.logger.Info("MockLoader: failed to get mappings, falling back to timestamp-based filtering",
			zap.String("testSetID", testSetID), zap.Error(err))
		return
	}

	if !hasMeaningfulMappings {
		r.logger.Debug("MockLoader: no meaningful mappings found, using timestamp-based filtering",
			zap.String("testSetID", testSetID))
		return
	}

	// Populate the full set of mapped mock names.
	for _, mocks := range testMockMappings {
		for _, m := range mocks {
			mocksThatHaveMappings[m.Name] = true
		}
	}

	if testCaseName != "" {
		// Only load the mocks that belong to this specific test case.
		if mocks, ok := testMockMappings[testCaseName]; ok {
			for _, m := range mocks {
				mocksWeNeed[m.Name] = true
			}
		}
	} else {
		// No specific test case — load all mapped mocks.
		mocksWeNeed = mocksThatHaveMappings
	}

	return
}

// getMocks fetches filtered and unfiltered mocks from MockDB for the given
// testSetID, using the full time range (BaseTime → now) as the window.
func (r *MockLoader) getMocks(ctx context.Context, testSetID string, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (filtered, unfiltered []*models.Mock, err error) {
	afterTime := models.BaseTime
	beforeTime := time.Now()

	filtered, err = r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		r.logger.Error("MockLoader: failed to get filtered mocks", zap.String("testSetID", testSetID), zap.Error(err))
		return nil, nil, err
	}

	unfiltered, err = r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		r.logger.Error("MockLoader: failed to get unfiltered mocks", zap.String("testSetID", testSetID), zap.Error(err))
		return nil, nil, err
	}

	return filtered, unfiltered, nil
}

func (r *MockLoader) SendMockFilterParamsToAgent(ctx context.Context, expectedMockMapping []string, afterTime, beforeTime time.Time, totalConsumedMocks map[string]models.MockState, useMappingBased bool) error {

	// Build filter parameters
	params := models.MockFilterParams{
		AfterTime:          afterTime,
		BeforeTime:         beforeTime,
		MockMapping:        expectedMockMapping,
		UseMappingBased:    useMappingBased,
		TotalConsumedMocks: totalConsumedMocks,
	}

	// Send parameters to agent for filtering and mock updates
	err := r.instrumentation.UpdateMockParams(ctx, params)
	if err != nil {
		utils.LogError(r.logger, err, "failed to update mock parameters on agent")
		return err
	}

	r.logger.Debug("Successfully sent mock filter parameters to agent",
		zap.Bool("useMappingBased", useMappingBased),
		zap.Int("mockMappingCount", len(expectedMockMapping)))

	return nil
}
