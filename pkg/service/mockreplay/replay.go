package mockreplay

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// replayer implements the Service interface for replaying recorded mocks.
type replayer struct {
	logger *zap.Logger
	cfg    *config.Config
	agent  AgentService
	mockDB MockDB
}

// New creates a new mockreplay Service.
func New(logger *zap.Logger, cfg *config.Config, agent AgentService, mockDB MockDB) Service {
	return &replayer{
		logger: logger,
		cfg:    cfg,
		agent:  agent,
		mockDB: mockDB,
	}
}

// Replay runs the app command with mocks from the specified file.
func (r *replayer) Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error) {
	r.logger.Info("Starting mock replay",
		zap.String("command", opts.Command),
		zap.String("mockFilePath", opts.MockFilePath),
	)

	// Apply defaults
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}

	// Create context with timeout
	replayCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Load mocks from file
	mocks, err := r.loadMocksFromFile(opts.MockFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load mocks from file: %w", err)
	}

	r.logger.Info("Loaded mocks from file",
		zap.String("mockFilePath", opts.MockFilePath),
		zap.Int("mockCount", len(mocks)),
	)

	if len(mocks) == 0 {
		r.logger.Warn("No mocks found in file", zap.String("mockFilePath", opts.MockFilePath))
	}

	// Setup agent
	startCh := make(chan int, 1)
	if err := r.agent.Setup(replayCtx, startCh); err != nil {
		return nil, fmt.Errorf("failed to setup agent: %w", err)
	}

	// Set mocks for replay
	if err := r.agent.SetMocks(replayCtx, mocks, nil); err != nil {
		return nil, fmt.Errorf("failed to set mocks: %w", err)
	}

	// Enable mock mode for outgoing calls
	outgoingOpts := models.OutgoingOptions{
		Rules:          r.cfg.BypassRules,
		MongoPassword:  r.cfg.Test.MongoPassword,
		SQLDelay:       time.Duration(r.cfg.Test.Delay) * time.Second,
		FallBackOnMiss: opts.FallBackOnMiss,
	}

	if err := r.agent.MockOutgoing(replayCtx, outgoingOpts); err != nil {
		return nil, fmt.Errorf("failed to enable mock outgoing: %w", err)
	}

	// Signal agent is ready
	startCh <- 1

	// Run the application command
	output, exitCode, cmdErr := r.runCommand(replayCtx, opts.Command)

	// Get consumed mocks
	consumedMocks, err := r.agent.GetConsumedMocks(replayCtx)
	if err != nil {
		r.logger.Warn("Failed to get consumed mocks", zap.Error(err))
	}

	// Calculate replay statistics
	mocksReplayed := 0
	for _, m := range consumedMocks {
		if m.Usage == models.Updated {
			mocksReplayed++
		}
	}
	mocksMissed := len(mocks) - mocksReplayed

	success := cmdErr == nil && mocksMissed == 0

	r.logger.Info("Mock replay completed",
		zap.Bool("success", success),
		zap.Int("mocksReplayed", mocksReplayed),
		zap.Int("mocksMissed", mocksMissed),
		zap.Int("exitCode", exitCode),
	)

	return &models.ReplayResult{
		Success:       success,
		MocksReplayed: mocksReplayed,
		MocksMissed:   mocksMissed,
		AppExitCode:   exitCode,
		Output:        output,
		ConsumedMocks: consumedMocks,
	}, nil
}

// loadMocksFromFile loads mocks from a YAML file.
func (r *replayer) loadMocksFromFile(filePath string) ([]*models.Mock, error) {
	// Handle both direct file path and directory path
	var mockFile string
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat path: %w", err)
	}

	if info.IsDir() {
		// If it's a directory, look for mocks.yaml inside
		mockFile = filepath.Join(filePath, "mocks.yaml")
	} else {
		mockFile = filePath
	}

	data, err := os.ReadFile(mockFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read mock file: %w", err)
	}

	var mocks []*models.Mock

	// Parse YAML (may contain multiple documents)
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var mock models.Mock
		if err := decoder.Decode(&mock); err != nil {
			if err.Error() == "EOF" {
				break
			}
			// Try to continue parsing
			r.logger.Warn("Failed to decode mock entry", zap.Error(err))
			continue
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

// runCommand executes the application command and returns output, exit code, and error.
func (r *replayer) runCommand(ctx context.Context, command string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	err := cmd.Start()
	if err != nil {
		return "", -1, fmt.Errorf("failed to start command: %w", err)
	}

	waitErr := cmd.Wait()
	output := outputBuf.String()

	exitCode := 0
	if waitErr != nil {
		// Try to extract exit code
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			// Context cancelled/timeout
			return output, -1, ctx.Err()
		} else {
			exitCode = 1
		}
	}

	return strings.TrimSpace(output), exitCode, waitErr
}
