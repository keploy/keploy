// Package mock provides functionality for recording and replaying mocks for external dependencies.
package mock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// maxTime represents the maximum time value for loading all mocks regardless of timestamp.
var maxTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// MockService handles recording and replaying mocks for standalone commands.
type MockService struct {
	logger          *zap.Logger
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
}

// New creates a new mock service instance.
func New(logger *zap.Logger, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, cfg *config.Config) Service {
	return &MockService{
		logger:          logger,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          cfg,
	}
}

// Record starts recording outgoing calls as mocks.
func (m *MockService) Record(ctx context.Context) error {
	m.logger.Info("🔴 Starting Keploy mock recording... Please wait.")
	m.logger.Info("Recording outgoing calls from command", zap.String("command", m.config.Command))

	// Create error group for managing goroutines
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var stopReason string
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertMockErrChan = make(chan error, 10)
	var mockCountMap = make(map[string]int)
	mockSetID := m.config.MockCmd.MockSetID

	// Defer cleanup
	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(m.logger, stopReason)
			if err != nil {
				utils.LogError(m.logger, err, "failed to stop mock recording")
			}
		}

		m.logger.Info("🔴 Stopping Keploy mock recording...")

		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(m.logger, err, "failed to stop application")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
			utils.LogError(m.logger, err, "failed to stop setup")
		}

		err = errGrp.Wait()
		if err != nil {
			utils.LogError(m.logger, err, "failed to stop mock recording")
		}

		m.telemetry.RecordedMocks(mockCountMap)
	}()

	defer close(appErrChan)
	defer close(insertMockErrChan)

	// Get or create mock set ID
	if mockSetID == "" {
		var err error
		mockSetID, err = m.getNextMockSetID(ctx)
		if err != nil {
			stopReason = "failed to get new mock-set id"
			utils.LogError(m.logger, err, stopReason)
			return fmt.Errorf("%s: %w", stopReason, err)
		}
	}

	m.logger.Info("Recording mocks to set", zap.String("mockSetID", mockSetID))

	// Check for context cancellation
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	// Get bypass ports
	passPortsUint := config.GetByPassPorts(m.config)

	// Setup the instrumentation
	err := m.instrumentation.Setup(setupCtx, m.config.Command, models.SetupOptions{
		Container:         m.config.ContainerName,
		DockerDelay:       m.config.BuildDelay,
		Mode:              models.MODE_RECORD,
		CommandType:       m.config.CommandType,
		EnableTesting:     false,
		GlobalPassthrough: m.config.MockCmd.GlobalPassthrough,
		BuildDelay:        m.config.BuildDelay,
		PassThroughPorts:  passPortsUint,
		ConfigPath:        m.config.ConfigPath,
	})

	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	m.logger.Info("🟢 Keploy agent is ready to record mocks.")

	// Reset mock ID counter for new recording session
	m.mockDB.ResetCounterID()

	// Start the application first (so there are outgoing calls to capture)
	m.logger.Info("Starting Application:", zap.String("command", m.config.Command))
	runAppErrGrp.Go(func() error {
		runAppError = m.instrumentation.Run(runAppCtx, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		// App exited normally (no error) - trigger graceful shutdown
		if runAppError.AppErrorType == "" && runAppError.Err == nil {
			m.logger.Info("Application completed successfully")
			_ = utils.Stop(m.logger, "Application completed successfully")
			return nil
		}
		appErrChan <- runAppError
		return nil
	})

	// Get the outgoing channel for mocks (this connects to the streaming endpoint)
	outgoing, err := m.instrumentation.GetOutgoing(ctx, models.OutgoingOptions{})
	if err != nil {
		stopReason = "failed to get outgoing mocks channel"
		utils.LogError(m.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	m.logger.Info("🟢 Recording outgoing calls as mocks...")

	// Process mocks
	errGrp.Go(func() error {
		for mock := range outgoing {
			err := m.mockDB.InsertMock(ctx, mock, mockSetID)
			if err != nil {
				if ctx.Err() == context.Canceled {
					continue
				}
				insertMockErrChan <- err
			} else {
				mockCountMap[mock.GetKind()]++
				m.logger.Debug("Recorded mock", zap.String("kind", mock.GetKind()), zap.String("name", mock.Name))
			}
		}
		return nil
	})

	// Handle record timer if set
	if m.config.MockCmd.RecordTimer != 0 {
		errGrp.Go(func() error {
			m.logger.Info("Setting a timer for recording", zap.Duration("duration", m.config.MockCmd.RecordTimer))
			timer := time.After(m.config.MockCmd.RecordTimer)
			select {
			case <-timer:
				m.logger.Warn("Time up! Stopping mock recording")
				err := utils.Stop(m.logger, "Recording timer expired")
				if err != nil {
					utils.LogError(m.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
			}
			return nil
		})
	}

	// Wait for completion
	select {
	case <-ctx.Done():
		return nil
	case appErr := <-appErrChan:
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "user application failed to start"
		case models.ErrUnExpected:
			stopReason = "user application encountered an unexpected error"
		case models.ErrAppStopped:
			stopReason = "user application stopped"
		case models.ErrCtxCanceled:
			return nil
		default:
			stopReason = "unknown error occurred"
		}
		utils.LogError(m.logger, appErr.Err, stopReason)
	case insertMockErr := <-insertMockErrChan:
		stopReason = "failed to insert mock"
		utils.LogError(m.logger, insertMockErr, stopReason)
	}

	// Log summary
	totalMocks := 0
	for kind, count := range mockCountMap {
		m.logger.Info("Recorded mocks", zap.String("kind", kind), zap.Int("count", count))
		totalMocks += count
	}
	m.logger.Info("🎉 Mock recording completed!", zap.Int("totalMocks", totalMocks), zap.String("mockSetID", mockSetID))

	return nil
}

// Replay starts replaying mocks for outgoing calls.
func (m *MockService) Replay(ctx context.Context) error {
	m.logger.Info("🟢 Starting Keploy mock replay... Please wait.")
	m.logger.Info("Replaying mocks for command", zap.String("command", m.config.Command))

	// Create error group for managing goroutines
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var stopReason = "mock replay completed successfully"

	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			m.logger.Info("stopping mock replay", zap.String("reason", stopReason))
		}
		cancel()
		err := g.Wait()
		if err != nil {
			utils.LogError(m.logger, err, "failed to stop mock replay")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
			utils.LogError(m.logger, err, "failed to stop setup")
		}
	}()

	mockSetID := m.config.MockCmd.MockSetID

	// Get mock set IDs if not specified
	if mockSetID == "" {
		mockSetIDs, err := m.mockDB.GetAllMockSetIDs(ctx)
		if err != nil {
			stopReason = "failed to get mock set ids"
			utils.LogError(m.logger, err, stopReason)
			return fmt.Errorf("%s: %w", stopReason, err)
		}

		if len(mockSetIDs) == 0 {
			recordCmd := models.HighlightGrayString("keploy mock record")
			errMsg := fmt.Sprintf("No mock sets found. Please record mocks using %s command", recordCmd)
			utils.LogError(m.logger, nil, errMsg)
			return errors.New(errMsg)
		}

		// Use the first mock set by default
		mockSetID = mockSetIDs[0]
		m.logger.Info("Using mock set", zap.String("mockSetID", mockSetID))
	}

	// Load mocks from storage
	m.logger.Info("Loading mocks from storage...", zap.String("mockSetID", mockSetID))
	
	// Get all mocks (filtered and unfiltered) for the mock set
	var startTime time.Time
	var endTime = maxTime
	
	filteredMocks, err := m.mockDB.GetFilteredMocks(ctx, mockSetID, startTime, endTime)
	if err != nil {
		stopReason = "failed to load filtered mocks"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	unfilteredMocks, err := m.mockDB.GetUnFilteredMocks(ctx, mockSetID, startTime, endTime)
	if err != nil {
		stopReason = "failed to load unfiltered mocks"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	m.logger.Info("Loaded mocks", 
		zap.Int("filtered", len(filteredMocks)), 
		zap.Int("unfiltered", len(unfilteredMocks)))

	if len(filteredMocks) == 0 && len(unfilteredMocks) == 0 {
		m.logger.Warn("No mocks found for the mock set", zap.String("mockSetID", mockSetID))
	}

	// Get bypass ports
	passPortsUint := config.GetByPassPorts(m.config)

	// Setup the instrumentation for replay mode
	err = m.instrumentation.Setup(setupCtx, m.config.Command, models.SetupOptions{
		Container:         m.config.ContainerName,
		DockerDelay:       m.config.BuildDelay,
		Mode:              models.MODE_TEST,
		CommandType:       m.config.CommandType,
		EnableTesting:     false,
		GlobalPassthrough: m.config.MockCmd.GlobalPassthrough,
		BuildDelay:        m.config.BuildDelay,
		PassThroughPorts:  passPortsUint,
		ConfigPath:        m.config.ConfigPath,
	})

	if err != nil {
		stopReason = "failed setting up the environment for replay"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	m.logger.Info("🟢 Keploy agent is ready for mock replay.")

	// Setup mock matching FIRST (creates the MockManager)
	err = m.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Mocking: true, // Enable mocking so responses are served from recorded mocks
	})
	if err != nil {
		stopReason = "failed to setup mock matching"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Store mocks in the proxy for matching (now MockManager exists)
	err = m.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks)
	if err != nil {
		stopReason = "failed to store mocks in proxy"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Update mock params for best-effort matching (sequence + schema)
	err = m.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		MatchSequence: true,
		MatchSchema:   true,
		BestEffort:    true,
	})
	if err != nil {
		m.logger.Warn("Failed to update mock params for best-effort matching", zap.Error(err))
	}

	m.logger.Info("🟢 Mock replay is ready. Running command...")

	// Run the application
	var runAppError models.AppError
	runAppError = m.instrumentation.Run(ctx, models.RunOptions{})
	
	if runAppError.AppErrorType != models.ErrCtxCanceled && runAppError.Err != nil {
		switch runAppError.AppErrorType {
		case models.ErrCommandError:
			stopReason = "user application failed to start"
		case models.ErrUnExpected:
			stopReason = "user application encountered an unexpected error"
		case models.ErrAppStopped:
			stopReason = "user application stopped"
		default:
			stopReason = "application completed"
		}
		if runAppError.Err != nil {
			utils.LogError(m.logger, runAppError.Err, stopReason)
		}
	}

	m.logger.Info("🎉 Mock replay completed!")

	return nil
}

// getNextMockSetID generates the next mock set ID.
func (m *MockService) getNextMockSetID(ctx context.Context) (string, error) {
	mockSetIDs, err := m.mockDB.GetAllMockSetIDs(ctx)
	if err != nil {
		return "", err
	}

	// Generate new ID based on existing count
	nextID := fmt.Sprintf("mock-set-%d", len(mockSetIDs)+1)
	return nextID, nil
}
