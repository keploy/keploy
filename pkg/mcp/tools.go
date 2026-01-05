package mcp

import (
	"context"
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MockRecordInput defines the input parameters for the mock record tool.
type MockRecordInput struct {
	// Command is the application command to run (e.g., "go run main.go", "npm start").
	Command string `json:"command" jsonschema:"required,description=Application command to run (e.g. 'go run main.go' or 'npm start')"`
	// Path is the path to store mock files (default: ./keploy).
	Path string `json:"path" jsonschema:"description=Path to store mock files (default: ./keploy)"`
	// Duration is the recording duration (e.g., "60s", "5m").
	Duration string `json:"duration" jsonschema:"description=Recording duration (e.g. '60s' or '5m'). Default: 60s"`
	// SkipConfirmation skips the user confirmation step if true.
	SkipConfirmation bool `json:"skipConfirmation" jsonschema:"description=Skip user confirmation of command (default: false). Set to true if user has already confirmed the command."`
}

// MockRecordOutput defines the output of the mock record tool.
type MockRecordOutput struct {
	// Success indicates whether recording was successful.
	Success bool `json:"success"`
	// MockFilePath is the path to the generated mock file.
	MockFilePath string `json:"mockFilePath"`
	// MockCount is the number of mocks recorded.
	MockCount int `json:"mockCount"`
	// Protocols lists the protocols detected in recorded mocks.
	Protocols []string `json:"protocols"`
	// Message is a human-readable status message.
	Message string `json:"message"`
}

// MockReplayInput defines the input parameters for the mock replay tool.
type MockReplayInput struct {
	// Command is the application command to run.
	Command string `json:"command" jsonschema:"required,description=Application command to run"`
	// MockFilePath is the path to the mock file or directory to replay.
	MockFilePath string `json:"mockFilePath" jsonschema:"required,description=Path to mock file or directory to replay"`
	// FallBackOnMiss indicates whether to fall back to real calls when no mock matches.
	FallBackOnMiss bool `json:"fallBackOnMiss" jsonschema:"description=Whether to fall back to real calls when no mock matches (default: false)"`
}

// MockReplayOutput defines the output of the mock replay tool.
type MockReplayOutput struct {
	// Success indicates whether replay was successful.
	Success bool `json:"success"`
	// MocksReplayed is the number of mocks that were replayed.
	MocksReplayed int `json:"mocksReplayed"`
	// MocksMissed is the number of unmatched calls.
	MocksMissed int `json:"mocksMissed"`
	// AppExitCode is the application exit code.
	AppExitCode int `json:"appExitCode"`
	// Message is a human-readable status message.
	Message string `json:"message"`
}

// handleMockRecord handles the keploy_mock_record tool invocation.
func (s *Server) handleMockRecord(ctx context.Context, req *sdkmcp.CallToolRequest, in MockRecordInput) (*sdkmcp.CallToolResult, MockRecordOutput, error) {
	s.logger.Info("Mock record tool invoked",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
		zap.String("duration", in.Duration),
		zap.Bool("skipConfirmation", in.SkipConfirmation),
	)

	// Validate input
	if in.Command == "" {
		return nil, MockRecordOutput{
			Success: false,
			Message: "Command is required",
		}, nil
	}

	// Ask for user confirmation unless skipped
	if !in.SkipConfirmation {
		confirmed, err := s.confirmMockRecordCommand(ctx, in.Command, in.Path, in.Duration)
		if err != nil {
			s.logger.Warn("Failed to get user confirmation, proceeding anyway",
				zap.Error(err),
			)
			// Continue with recording if elicitation fails (client may not support it)
		} else if !confirmed {
			return nil, MockRecordOutput{
				Success: false,
				Message: "User declined to proceed with the command. Please provide the correct command and try again.",
			}, nil
		}
	}

	// Parse duration
	duration := 60 * time.Second
	if in.Duration != "" {
		parsed, err := time.ParseDuration(in.Duration)
		if err != nil {
			return nil, MockRecordOutput{
				Success: false,
				Message: fmt.Sprintf("Invalid duration format: %s", err.Error()),
			}, nil
		}
		duration = parsed
	}

	// Check if mock recorder is available
	if s.mockRecorder == nil {
		return nil, MockRecordOutput{
			Success: false,
			Message: "Mock recorder service is not available",
		}, nil
	}

	// Execute recording
	result, err := s.mockRecorder.Record(ctx, models.RecordOptions{
		Command:  in.Command,
		Path:     in.Path,
		Duration: duration,
	})
	if err != nil {
		s.logger.Error("Mock recording failed", zap.Error(err))
		return nil, MockRecordOutput{
			Success: false,
			Message: fmt.Sprintf("Recording failed: %s", err.Error()),
		}, nil
	}

	// Generate contextual name using LLM callback
	contextualName, err := s.generateContextualName(ctx, result.Metadata)
	if err != nil {
		s.logger.Warn("Failed to generate contextual name, using fallback",
			zap.Error(err),
		)
		contextualName = s.fallbackName(result.Metadata)
	}

	// Rename mock file with contextual name
	newPath := s.renameMockFile(result.MockFilePath, contextualName)

	s.logger.Info("Mock recording completed successfully",
		zap.String("mockFilePath", newPath),
		zap.Int("mockCount", result.MockCount),
		zap.Strings("protocols", result.Metadata.Protocols),
	)

	return nil, MockRecordOutput{
		Success:      true,
		MockFilePath: newPath,
		MockCount:    result.MockCount,
		Protocols:    result.Metadata.Protocols,
		Message:      fmt.Sprintf("Successfully recorded %d mocks to %s", result.MockCount, newPath),
	}, nil
}

// handleMockReplay handles the keploy_mock_test tool invocation.
func (s *Server) handleMockReplay(ctx context.Context, req *sdkmcp.CallToolRequest, in MockReplayInput) (*sdkmcp.CallToolResult, MockReplayOutput, error) {
	s.logger.Info("Mock replay tool invoked",
		zap.String("command", in.Command),
		zap.String("mockFilePath", in.MockFilePath),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Validate input
	if in.Command == "" {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Command is required",
		}, nil
	}
	if in.MockFilePath == "" {
		return nil, MockReplayOutput{
			Success: false,
			Message: "MockFilePath is required",
		}, nil
	}

	// Check if mock replayer is available
	if s.mockReplayer == nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Mock replayer service is not available",
		}, nil
	}

	// Execute replay
	result, err := s.mockReplayer.Replay(ctx, models.ReplayOptions{
		Command:        in.Command,
		MockFilePath:   in.MockFilePath,
		FallBackOnMiss: in.FallBackOnMiss,
	})
	if err != nil {
		s.logger.Error("Mock replay failed", zap.Error(err))
		return nil, MockReplayOutput{
			Success: false,
			Message: fmt.Sprintf("Replay failed: %s", err.Error()),
		}, nil
	}

	message := fmt.Sprintf("Replayed %d mocks", result.MocksReplayed)
	if result.MocksMissed > 0 {
		message += fmt.Sprintf(", %d mocks missed", result.MocksMissed)
	}
	if result.AppExitCode != 0 {
		message += fmt.Sprintf(", app exited with code %d", result.AppExitCode)
	}

	s.logger.Info("Mock replay completed",
		zap.Bool("success", result.Success),
		zap.Int("mocksReplayed", result.MocksReplayed),
		zap.Int("mocksMissed", result.MocksMissed),
		zap.Int("exitCode", result.AppExitCode),
	)

	return nil, MockReplayOutput{
		Success:       result.Success,
		MocksReplayed: result.MocksReplayed,
		MocksMissed:   result.MocksMissed,
		AppExitCode:   result.AppExitCode,
		Message:       message,
	}, nil
}

// confirmMockRecordCommand asks the user to confirm the command before executing mock recording.
// Returns true if confirmed, false if declined, or error if elicitation fails.
func (s *Server) confirmMockRecordCommand(ctx context.Context, command, path, duration string) (bool, error) {
	session := s.getActiveSession()
	if session == nil {
		return true, fmt.Errorf("no active session for elicitation")
	}

	// Check if client supports elicitation
	initParams := session.InitializeParams()
	if initParams == nil || initParams.Capabilities == nil || initParams.Capabilities.Elicitation == nil {
		s.logger.Debug("Client doesn't support elicitation, skipping confirmation")
		return true, nil
	}

	// Build confirmation message
	pathDisplay := path
	if pathDisplay == "" {
		pathDisplay = "./keploy (default)"
	}
	durationDisplay := duration
	if durationDisplay == "" {
		durationDisplay = "60s (default)"
	}

	message := fmt.Sprintf(
		"I'm about to record outgoing calls (HTTP APIs, databases, etc.) from your application.\n\n"+
			"**Command:** `%s`\n"+
			"**Mock storage path:** `%s`\n"+
			"**Recording duration:** `%s`\n\n"+
			"This will:\n"+
			"1. Start your application with the above command\n"+
			"2. Intercept all outgoing calls (HTTP, gRPC, databases, etc.)\n"+
			"3. Save them as mocks for later testing\n\n"+
			"Is this the correct command to run your application?",
		command, pathDisplay, durationDisplay,
	)

	// Use elicitation to ask user for confirmation
	result, err := session.Elicit(ctx, &sdkmcp.ElicitParams{
		Message: message,
	})
	if err != nil {
		return false, fmt.Errorf("elicitation failed: %w", err)
	}

	// Check if user confirmed
	if result.Action == "accept" {
		s.logger.Info("User confirmed mock record command", zap.String("command", command))
		return true, nil
	}

	// User declined or cancelled
	s.logger.Info("User declined mock record command",
		zap.String("command", command),
		zap.String("action", result.Action),
	)
	return false, nil
}
