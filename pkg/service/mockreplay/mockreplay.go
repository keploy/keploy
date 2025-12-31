package mockreplay

import (
	"context"
	"errors"
	"sort"
	"time"

	"facette.io/natsort"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// MockReplayer implements the Service interface for running commands with mock injection
type MockReplayer struct {
	logger          *zap.Logger
	mockDB          MockDB
	testDB          TestDB
	instrumentation Instrumentation
	telemetry       Telemetry
	config          *config.Config
}

// New creates a new MockReplayer instance
func New(
	logger *zap.Logger,
	mockDB MockDB,
	testDB TestDB,
	telemetry Telemetry,
	instrumentation Instrumentation,
	cfg *config.Config,
) Service {
	return &MockReplayer{
		logger:          logger,
		mockDB:          mockDB,
		testDB:          testDB,
		instrumentation: instrumentation,
		telemetry:       telemetry,
		config:          cfg,
	}
}

// Start runs the user's command with mock injection
func (m *MockReplayer) Start(ctx context.Context) error {
	m.logger.Info("🟢 Starting Keploy mock replay...")

	// Get test sets to use
	testSetIDs, err := m.getTestSets(ctx)
	if err != nil {
		return err
	}

	if len(testSetIDs) == 0 {
		m.logger.Warn("No test sets found with mocks. Please run 'keploy mock record' first.")
		return errors.New("no test sets found")
	}

	m.logger.Info("Test Sets to be Replayed", zap.Strings("testSets", testSetIDs))

	// Create error group for goroutines
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	// Setup instrumentation (proxy/agent)
	setupOpts := models.SetupOptions{
		CommandType:   m.config.CommandType,
		Mode:          models.MODE_TEST,
		EnableTesting: true,
	}

	if err := m.instrumentation.Setup(ctx, m.config.Command, setupOpts); err != nil {
		utils.LogError(m.logger, err, "failed to setup instrumentation")
		return err
	}

	// Load and store mocks for all test sets
	totalMocksLoaded := 0
	for _, testSetID := range testSetIDs {
		filtered, unfiltered, err := m.loadMocks(ctx, testSetID)
		if err != nil {
			utils.LogError(m.logger, err, "failed to load mocks", zap.String("testSetID", testSetID))
			continue
		}

		mockCount := len(filtered) + len(unfiltered)
		if mockCount > 0 {
			m.logger.Info("🟢 Loaded mocks for test set",
				zap.String("testSetID", testSetID),
				zap.Int("filteredMocks", len(filtered)),
				zap.Int("unfilteredMocks", len(unfiltered)),
			)

			// Send mocks to agent for injection
			if err := m.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
				utils.LogError(m.logger, err, "failed to store mocks in agent", zap.String("testSetID", testSetID))
				continue
			}
			totalMocksLoaded += mockCount
		}
	}

	if totalMocksLoaded == 0 {
		m.logger.Warn("No mocks loaded. Tests will run without mock injection.")
	} else {
		m.logger.Info("🟢 Total mocks loaded for injection", zap.Int("count", totalMocksLoaded))
	}

	// Run the user's command
	m.logger.Info("🏃 Running user command with mock injection", zap.String("command", m.config.Command))

	appErr := m.instrumentation.Run(ctx, models.RunOptions{})

	// Handle completion
	return m.handleCompletion(ctx, appErr, totalMocksLoaded)
}

// getTestSets returns the list of test set IDs to use
func (m *MockReplayer) getTestSets(ctx context.Context) ([]string, error) {
	// If specific test sets are selected, use those
	if len(m.config.Test.SelectedTests) > 0 {
		var selectedSets []string
		for testSetID := range m.config.Test.SelectedTests {
			selectedSets = append(selectedSets, testSetID)
		}
		return selectedSets, nil
	}

	// Otherwise, get all test sets
	testSetIDs, err := m.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		utils.LogError(m.logger, err, "failed to get test set IDs")
		return nil, err
	}

	// Sort naturally
	sort.Slice(testSetIDs, func(i, j int) bool {
		return natsort.Compare(testSetIDs[i], testSetIDs[j])
	})

	return testSetIDs, nil
}

// loadMocks loads mocks from the mock database for a specific test set
func (m *MockReplayer) loadMocks(ctx context.Context, testSetID string) ([]*models.Mock, []*models.Mock, error) {
	// Use a wide time range to get all mocks
	afterTime := time.Time{}                                 // Zero time (get all)
	beforeTime := time.Now().Add(time.Hour * 24 * 365 * 100) // Far future

	filtered, err := m.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime)
	if err != nil {
		return nil, nil, err
	}

	unfiltered, err := m.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime)
	if err != nil {
		return nil, nil, err
	}

	return filtered, unfiltered, nil
}

// handleCompletion processes the result of running the user's command
func (m *MockReplayer) handleCompletion(ctx context.Context, appErr models.AppError, mockCount int) error {
	switch appErr.AppErrorType {
	case models.ErrCtxCanceled:
		m.logger.Info("Mock replay was cancelled")
		return nil

	case models.ErrAppStopped:
		// Application completed successfully
		m.logger.Info("✅ User command completed successfully")
		m.telemetry.MockTestRun(mockCount)
		return nil

	case models.ErrInternal:
		utils.LogError(m.logger, appErr.Err, "internal error during mock replay")
		return appErr.Err

	case models.ErrUnExpected:
		utils.LogError(m.logger, appErr.Err, "unexpected error during mock replay")
		return appErr.Err

	case models.ErrCommandError:
		// Application failed with non-zero exit code
		m.logger.Error("User command failed", zap.Error(appErr.Err))
		return appErr.Err

	default:
		// Empty AppError or unknown type - check if there's an underlying error
		if appErr.Err != nil {
			utils.LogError(m.logger, appErr.Err, "error during mock replay")
			return appErr.Err
		}
		// No error - success
		m.logger.Info("✅ Mock replay completed successfully")
		m.telemetry.MockTestRun(mockCount)
		return nil
	}
}
