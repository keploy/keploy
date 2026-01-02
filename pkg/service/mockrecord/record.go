package mockrecord

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// recorder implements the Service interface for recording outgoing calls.
type recorder struct {
	logger *zap.Logger
	cfg    *config.Config
	agent  AgentService
	mockDB MockDB
}

// New creates a new mockrecord Service.
func New(logger *zap.Logger, cfg *config.Config, agent AgentService, mockDB MockDB) Service {
	return &recorder{
		logger: logger,
		cfg:    cfg,
		agent:  agent,
		mockDB: mockDB,
	}
}

// Record starts recording outgoing calls for the given command.
func (r *recorder) Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error) {
	r.logger.Info("Starting mock recording",
		zap.String("command", opts.Command),
		zap.String("path", opts.Path),
		zap.Duration("duration", opts.Duration),
	)

	// Apply defaults
	if opts.Path == "" {
		opts.Path = r.cfg.Path
	}
	if opts.Path == "" {
		opts.Path = "./keploy"
	}
	if opts.Duration == 0 {
		opts.Duration = 60 * time.Second
	}

	// Create context with timeout
	recordCtx, cancel := context.WithTimeout(ctx, opts.Duration)
	defer cancel()

	// Setup agent
	startCh := make(chan int, 1)
	if err := r.agent.Setup(recordCtx, startCh); err != nil {
		return nil, fmt.Errorf("failed to setup agent: %w", err)
	}

	// Start capturing outgoing calls
	outgoingOpts := models.OutgoingOptions{
		Rules:          r.cfg.BypassRules,
		MongoPassword:  r.cfg.Test.MongoPassword,
		SQLDelay:       time.Duration(r.cfg.Test.Delay) * time.Second,
		FallBackOnMiss: false,
	}

	mockCh, err := r.agent.GetOutgoing(recordCtx, outgoingOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to start outgoing capture: %w", err)
	}

	// Collect mocks
	var mocks []*models.Mock
	var mocksMu sync.Mutex
	done := make(chan struct{})

	go func() {
		defer close(done)
		for mock := range mockCh {
			if mock != nil {
				mocksMu.Lock()
				mocks = append(mocks, mock)
				mocksMu.Unlock()
				r.logger.Debug("Captured mock",
					zap.String("name", mock.Name),
					zap.String("kind", string(mock.Kind)),
				)
			}
		}
	}()

	// Signal agent is ready
	startCh <- 1

	// Run the application command
	cmdErr := r.runCommand(recordCtx, opts.Command)
	if cmdErr != nil {
		r.logger.Warn("Application command completed with error", zap.Error(cmdErr))
	}

	// Wait for mock collection to finish
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.logger.Warn("Timeout waiting for mock collection to finish")
	}

	mocksMu.Lock()
	collectedMocks := mocks
	mocksMu.Unlock()

	if len(collectedMocks) == 0 {
		r.logger.Warn("No mocks were recorded")
	}

	// Generate test set ID for storing mocks
	testSetID := fmt.Sprintf("mock-%d", time.Now().Unix())

	// Store mocks
	if err := r.agent.StoreMocks(recordCtx, collectedMocks, nil); err != nil {
		r.logger.Warn("Failed to store mocks via agent", zap.Error(err))
	}

	// Generate mock file path
	mockFilePath := filepath.Join(opts.Path, testSetID, "mocks.yaml")

	// Extract metadata for contextual naming
	metadata := ExtractMetadata(collectedMocks, opts.Command)

	r.logger.Info("Mock recording completed",
		zap.Int("mockCount", len(collectedMocks)),
		zap.String("mockFilePath", mockFilePath),
		zap.Strings("protocols", metadata.Protocols),
	)

	return &models.RecordResult{
		MockFilePath: mockFilePath,
		Metadata:     metadata,
		MockCount:    len(collectedMocks),
		Mocks:        collectedMocks,
	}, nil
}

// runCommand executes the application command.
func (r *recorder) runCommand(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdout = nil // Discard stdout
	cmd.Stderr = nil // Discard stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		// Check if context was cancelled (timeout)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	return nil
}
