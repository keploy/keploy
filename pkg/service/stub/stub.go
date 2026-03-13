// Package stub provides functionality for recording and replaying stubs/mocks for external tests.
package stub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Stub implements the Service interface for stub recording and replaying
type Stub struct {
	logger          *zap.Logger
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
}

// New creates a new Stub service instance
func New(logger *zap.Logger, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, cfg *config.Config) Service {
	return &Stub{
		logger:          logger,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          cfg,
	}
}

// Record captures outgoing calls as mocks while running external tests
func (s *Stub) Record(ctx context.Context) error {
	// Create error group to manage goroutines
	errGrp, _ := errgroup.WithContext(ctx)
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

	// Get or generate stub set ID
	stubSetID, err := s.getStubSetID(ctx)
	if err != nil {
		utils.LogError(s.logger, err, "failed to get stub set ID")
		return err
	}

	s.logger.Info("📼 Recording stubs", zap.String("stubSet", stubSetID))

	// Cleanup on exit
	defer func() {
		select {
		case <-ctx.Done():
		default:
			if stopReason == "" {
				stopReason = "stub recording completed"
			}
			s.logger.Info("🛑 Stopping stub recording", zap.String("reason", stopReason))
		}

		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(s.logger, err, "failed to stop test command")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
			utils.LogError(s.logger, err, "failed to stop setup")
		}

		err = errGrp.Wait()
		if err != nil {
			utils.LogError(s.logger, err, "failed to stop recording")
		}

		// Report telemetry
		s.telemetry.RecordedMocks(mockCountMap)

		// Print summary
		s.printRecordSummary(stubSetID, mockCountMap)
	}()

	defer close(appErrChan)
	defer close(insertMockErrChan)

	// Get bypass ports
	passPortsUint := config.GetByPassPorts(s.config)

	// Setup instrumentation for recording mode
	err = s.instrumentation.Setup(setupCtx, s.config.Command, models.SetupOptions{
		Container:         s.config.ContainerName,
		DockerDelay:       s.config.BuildDelay,
		Mode:              models.MODE_RECORD,
		CommandType:       s.config.CommandType,
		EnableTesting:     false,
		GlobalPassthrough: false, // We want to capture all outgoing calls
		BuildDelay:        s.config.BuildDelay,
		PassThroughPorts:  passPortsUint,
		ConfigPath:        s.config.ConfigPath,
	})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Channel to receive mocks from the outgoing goroutine
	outgoingChan := make(chan *models.Mock)

	// Process captured mocks in a goroutine
	errGrp.Go(func() error {
		defer close(outgoingChan)

		// Create a cancelable child context for mock capturing
		mockCtx, mockCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer mockCancel()

		// Cancel mock context when parent is done
		go func() {
			<-ctx.Done()
			mockCancel()
		}()

		// Get outgoing mocks channel
		s.mockDB.ResetCounterID()
		ch, err := s.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
			Rules:          s.config.BypassRules,
			MongoPassword:  s.config.Stub.MongoPassword,
			FallBackOnMiss: false,
		})
		if err != nil {
			return fmt.Errorf("failed to get outgoing channel: %w", err)
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case mock, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					outgoingChan <- mock
					return ctx.Err()
				case outgoingChan <- mock:
				}
			}
		}
	})

	// Process and store captured mocks
	errGrp.Go(func() error {
		for mock := range outgoingChan {
			err := s.mockDB.InsertMock(ctx, mock, stubSetID)
			if err != nil {
				if ctx.Err() == context.Canceled {
					continue
				}
				insertMockErrChan <- err
			} else {
				mockCountMap[mock.GetKind()]++
				s.logger.Info("📦 Captured mock", zap.String("kind", mock.GetKind()), zap.String("name", mock.Name))
			}
		}
		return nil
	})

	// Run the external test command
	runAppErrGrp.Go(func() error {
		runAppError = s.instrumentation.Run(runAppCtx, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})

	// Set up timer if configured
	if s.config.Stub.RecordTimer != 0 {
		errGrp.Go(func() error {
			s.logger.Info("Setting a timer for recording", zap.Duration("duration", s.config.Stub.RecordTimer))
			timer := time.After(s.config.Stub.RecordTimer)
			select {
			case <-timer:
				s.logger.Warn("⏰ Time up! Stopping recording")
				err := utils.Stop(s.logger, "Timer expired")
				if err != nil {
					utils.LogError(s.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	// Wait for completion
	select {
	case appErr := <-appErrChan:
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "test command failed"
		case models.ErrUnExpected:
			stopReason = "test command terminated unexpectedly"
		case models.ErrInternal:
			stopReason = "internal error occurred"
		case models.ErrAppStopped:
			stopReason = "test command completed"
			return nil
		case models.ErrCtxCanceled:
			return nil
		case models.ErrTestBinStopped:
			stopReason = "test binary stopped"
			return nil
		default:
			stopReason = "test completed"
		}
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock"
	case <-ctx.Done():
		return nil
	}

	if err != nil {
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}
	return nil
}

// Replay serves recorded mocks while running external tests
func (s *Stub) Replay(ctx context.Context) error {
	// Create error group to manage goroutines
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var stopReason = "stub replay completed"

	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			s.logger.Info("🛑 Stopping stub replay", zap.String("reason", stopReason))
		}
		cancel()
		err := g.Wait()
		if err != nil {
			utils.LogError(s.logger, err, "failed to stop replaying")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
			utils.LogError(s.logger, err, "failed to stop setup")
		}
	}()

	// Get the stub set to replay
	stubSetID, err := s.getStubSetForReplay(ctx)
	if err != nil {
		stopReason = "failed to find stub set for replay"
		utils.LogError(s.logger, err, stopReason)
		return err
	}

	s.logger.Info("▶️ Replaying stubs", zap.String("stubSet", stubSetID))

	// Get bypass ports
	passPortsUint := config.GetByPassPorts(s.config)

	// Setup instrumentation for test mode (mock serving)
	err = s.instrumentation.Setup(setupCtx, s.config.Command, models.SetupOptions{
		Container:         s.config.ContainerName,
		DockerDelay:       s.config.BuildDelay,
		Mode:              models.MODE_TEST,
		CommandType:       s.config.CommandType,
		EnableTesting:     false,
		GlobalPassthrough: false,
		BuildDelay:        s.config.BuildDelay,
		PassThroughPorts:  passPortsUint,
		ConfigPath:        s.config.ConfigPath,
	})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Get the mocks for this stub set
	filteredMocks, err := s.mockDB.GetFilteredMocks(ctx, stubSetID, time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get mocks"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	unfilteredMocks, err := s.mockDB.GetUnFilteredMocks(ctx, stubSetID, time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get unfiltered mocks"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	totalMocks := len(filteredMocks) + len(unfilteredMocks)
	if totalMocks == 0 {
		s.logger.Warn("⚠️ No mocks found in stub set", zap.String("stubSet", stubSetID))
		s.logger.Info("Recording stubs first using: keploy stub record -c \"<your-test-command>\"")
		return errors.New("no mocks found in stub set")
	}

	s.logger.Info("📦 Loaded mocks", zap.Int("filtered", len(filteredMocks)), zap.Int("unfiltered", len(unfilteredMocks)))

	// Store mocks in the instrumentation for serving
	err = s.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks)
	if err != nil {
		stopReason = "failed to store mocks in proxy"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Setup mock server BEFORE UpdateMockParams - MockManager must exist first
	err = s.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Rules:          s.config.BypassRules,
		MongoPassword:  s.config.Stub.MongoPassword,
		FallBackOnMiss: s.config.Test.FallBackOnMiss,
		Mocking:        true, // Enable mocking to serve recorded mocks
	})
	if err != nil {
		stopReason = "failed to setup mock server"
		utils.LogError(s.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Update mock parameters for proper filtering - AFTER MockOutgoing creates MockManager
	err = s.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime:  time.Time{},
		BeforeTime: time.Now(),
	})
	if err != nil {
		s.logger.Warn("failed to update mock params", zap.Error(err))
	}

	// Add delay before running tests
	if s.config.Test.Delay > 0 {
		s.logger.Info("⏳ Waiting before running tests", zap.Uint64("delay", s.config.Test.Delay))
		time.Sleep(time.Duration(s.config.Test.Delay) * time.Second)
	}

	// Run the external test command
	runAppError := s.instrumentation.Run(ctx, models.RunOptions{})
	if runAppError.AppErrorType != models.ErrCtxCanceled && runAppError.AppErrorType != models.ErrAppStopped && runAppError.AppErrorType != models.ErrTestBinStopped {
		if runAppError.AppErrorType == models.ErrCommandError {
			stopReason = "test command failed"
			s.logger.Error("❌ Test command failed", zap.Any("error", runAppError))
		} else {
			stopReason = fmt.Sprintf("test command error: %v", runAppError.AppErrorType)
		}
	} else {
		s.logger.Info("✅ Test command completed")
	}

	return nil
}

// getStubSetID returns the stub set ID, either from config or auto-generated
func (s *Stub) getStubSetID(ctx context.Context) (string, error) {
	if s.config.Stub.Name != "" {
		return s.config.Stub.Name, nil
	}

	// Auto-generate stub set ID based on existing sets
	return s.getNextStubSetID(ctx)
}

// getNextStubSetID generates the next stub set ID
func (s *Stub) getNextStubSetID(ctx context.Context) (string, error) {
	stubDir := s.config.Path
	
	// Ensure the directory exists
	if err := os.MkdirAll(stubDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("failed to create stub directory: %w", err)
	}

	// Find existing stub sets
	entries, err := os.ReadDir(stubDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "stub-0", nil
		}
		return "", fmt.Errorf("failed to read stub directory: %w", err)
	}

	maxNum := -1
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "stub-") {
			numStr := strings.TrimPrefix(entry.Name(), "stub-")
			var num int
			if _, err := fmt.Sscanf(numStr, "%d", &num); err == nil {
				if num > maxNum {
					maxNum = num
				}
			}
		}
	}

	return fmt.Sprintf("stub-%d", maxNum+1), nil
}

// getStubSetForReplay returns the stub set to replay
func (s *Stub) getStubSetForReplay(ctx context.Context) (string, error) {
	// If a specific name is provided, use it
	if s.config.Stub.Name != "" {
		stubPath := filepath.Join(s.config.Path, s.config.Stub.Name)
		if _, err := os.Stat(stubPath); os.IsNotExist(err) {
			return "", fmt.Errorf("stub set '%s' not found at %s", s.config.Stub.Name, stubPath)
		}
		return s.config.Stub.Name, nil
	}

	// Otherwise, find the latest stub set
	return s.getLatestStubSetID(ctx)
}

// getLatestStubSetID finds the most recent stub set
func (s *Stub) getLatestStubSetID(ctx context.Context) (string, error) {
	stubDir := s.config.Path
	
	entries, err := os.ReadDir(stubDir)
	if err != nil {
		return "", fmt.Errorf("failed to read stub directory: %w", err)
	}

	var stubSets []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "stub-") {
			stubSets = append(stubSets, entry.Name())
		}
	}

	if len(stubSets) == 0 {
		return "", errors.New("no stub sets found")
	}

	// Sort to get the latest (highest numbered) stub set
	sort.Slice(stubSets, func(i, j int) bool {
		var numI, numJ int
		fmt.Sscanf(strings.TrimPrefix(stubSets[i], "stub-"), "%d", &numI)
		fmt.Sscanf(strings.TrimPrefix(stubSets[j], "stub-"), "%d", &numJ)
		return numI > numJ
	})

	return stubSets[0], nil
}

// printRecordSummary prints a summary of recorded mocks
func (s *Stub) printRecordSummary(stubSetID string, mockCountMap map[string]int) {
	total := 0
	for _, count := range mockCountMap {
		total += count
	}

	s.logger.Info("📊 Recording Summary")
	s.logger.Info("━━━━━━━━━━━━━━━━━━━━")
	s.logger.Info("Stub Set", zap.String("id", stubSetID))
	s.logger.Info("Total Mocks", zap.Int("count", total))
	
	for kind, count := range mockCountMap {
		s.logger.Info("  "+kind, zap.Int("count", count))
	}
	
	s.logger.Info("━━━━━━━━━━━━━━━━━━━━")
	s.logger.Info("To replay these mocks, run:")
	s.logger.Info(fmt.Sprintf("  keploy stub replay -c \"<your-test-command>\" --name %s", stubSetID))
}
