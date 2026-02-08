package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ListMocksInput defines the input parameters for the list mocks tool.
type ListMocksInput struct {
	// Path is the optional path to search for mocks (default: ./keploy).
	Path string `json:"path,omitempty" jsonschema:"Path to search for mock files (default: ./keploy)"`
}

// ListMocksOutput defines the output of the list mocks tool.
type ListMocksOutput struct {
	// Success indicates whether the operation was successful.
	Success bool `json:"success"`
	// MockSets is the list of available mock set names/IDs.
	MockSets []string `json:"mockSets"`
	// Count is the number of mock sets found.
	Count int `json:"count"`
	// Path is the path where mocks were searched.
	Path string `json:"path"`
	// Message is a human-readable status message.
	Message string `json:"message"`
}

// MockRecordInput defines the input parameters for the mock record tool.
type MockRecordInput struct {
	// Command is the command to run. If empty, server attempts elicitation.
	Command string `json:"command,omitempty" jsonschema:"Command to run (prefer test commands like 'go test -v ./...'). If empty, server will elicit it from user."`
	// Path is the path to store mock files (default: ./keploy).
	Path string `json:"path,omitempty" jsonschema:"Path to store mock files (default: ./keploy)"`
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
	// Configuration shows the configuration that was used.
	Configuration *RecordConfiguration `json:"configuration,omitempty"`
}

// RecordConfiguration shows the configuration used for recording.
type RecordConfiguration struct {
	Command string `json:"command"`
	Path    string `json:"path"`
}

// MockReplayInput defines the input parameters for the mock replay tool.
type MockReplayInput struct {
	// Command is the command to run with mocks.
	Command string `json:"command" jsonschema:"Command to run with mocks (e.g. 'go test -v', 'npm test', 'go run main.go', or any other command)."`
	// Path is the path to load mock files from. If omitted, replay resolves latest run automatically.
	Path string `json:"path,omitempty" jsonschema:"Path to load mock files from (optional). Omit unless user explicitly wants a specific path. If omitted, replay service selects latest run automatically."`
	// FallBackOnMiss indicates whether to fall back to real calls when no mock matches (optional, default: false).
	FallBackOnMiss bool `json:"fallBackOnMiss,omitempty" jsonschema:"Whether to fall back to real calls when no mock matches (default: false)"`
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
	// Configuration shows the configuration that was used.
	Configuration *ReplayConfiguration `json:"configuration,omitempty"`
}

// ReplayConfiguration shows the configuration used for replay.
type ReplayConfiguration struct {
	Command        string `json:"command"`
	Path           string `json:"path"`
	FallBackOnMiss bool   `json:"fallBackOnMiss"`
}

// handleListMocks handles the keploy_list_mocks tool invocation.
func (s *Server) handleListMocks(ctx context.Context, req *sdkmcp.CallToolRequest, in ListMocksInput) (*sdkmcp.CallToolResult, ListMocksOutput, error) {
	s.logger.Info("List mocks tool invoked", zap.String("path", in.Path))

	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = "./keploy"
	}

	s.logger.Info("Scanning directory for mock sets", zap.String("path", path))

	// Scan the directory directly for mock sets (fixes path mismatch issue)
	mockSets, err := s.scanMockSets(path)
	if err != nil {
		s.logger.Error("Failed to scan mock sets", zap.Error(err), zap.String("path", path))
		return nil, ListMocksOutput{
			Success: false,
			Path:    path,
			Message: fmt.Sprintf("Failed to list mock sets: %s", err.Error()),
		}, nil
	}

	if len(mockSets) == 0 {
		return nil, ListMocksOutput{
			Success:  true,
			MockSets: []string{},
			Count:    0,
			Path:     path,
			Message:  "No mock sets found. Use keploy_mock_record to create mocks first.",
		}, nil
	}

	message := fmt.Sprintf("Found %d mock set(s). The latest is '%s'.", len(mockSets), mockSets[0])

	return nil, ListMocksOutput{
		Success:  true,
		MockSets: mockSets,
		Count:    len(mockSets),
		Path:     path,
		Message:  message,
	}, nil
}

// scanMockSets scans the directory for mock sets (subdirectories containing mocks.yaml).
func (s *Server) scanMockSets(basePath string) ([]string, error) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.logger.Info("Mock directory does not exist", zap.String("path", basePath))
			return []string{}, nil
		}
		return nil, err
	}

	type mockSetInfo struct {
		name    string
		modTime time.Time
	}
	var mockSets []mockSetInfo

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip special directories
		if name == "reports" || name == "testReports" || name == "schema" {
			continue
		}

		// Check if this directory contains a mocks.yaml or mocks.yml file
		mockPath := filepath.Join(basePath, name, "mocks.yaml")
		info, err := os.Stat(mockPath)
		if err != nil {
			mockPath = filepath.Join(basePath, name, "mocks.yml")
			info, err = os.Stat(mockPath)
			if err != nil {
				continue
			}
		}

		mockSets = append(mockSets, mockSetInfo{name: name, modTime: info.ModTime()})
	}

	// Sort by modification time (most recent first)
	sort.SliceStable(mockSets, func(i, j int) bool {
		return mockSets[i].modTime.After(mockSets[j].modTime)
	})

	out := make([]string, len(mockSets))
	for i, set := range mockSets {
		out[i] = set.name
	}

	s.logger.Info("Found mock sets", zap.String("path", basePath), zap.Int("count", len(out)), zap.Strings("mockSets", out))
	return out, nil
}

// handleMockRecord handles the keploy_mock_record tool invocation.
func (s *Server) handleMockRecord(ctx context.Context, req *sdkmcp.CallToolRequest, in MockRecordInput) (*sdkmcp.CallToolResult, MockRecordOutput, error) {
	s.logger.Info("Mock record tool invoked",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
	)

	command := strings.TrimSpace(in.Command)
	if command == "" {
		elictedCommand, err := s.elicitRecordCommand(ctx)
		if err != nil {
			return nil, MockRecordOutput{
				Success: false,
				Message: fmt.Sprintf("Error: failed to get command via elicitation: %s", err.Error()),
			}, nil
		}
		command = strings.TrimSpace(elictedCommand)
		if command == "" {
			return nil, MockRecordOutput{
				Success: false,
				Message: "Mock recording cancelled: no command provided.",
			}, nil
		}
	}

	// Parse and validate configuration
	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = "./keploy"
	}

	config := &RecordConfiguration{
		Command: command,
		Path:    path,
	}

	// Check if mock recorder is available
	if s.mockRecorder == nil {
		return nil, MockRecordOutput{
			Success:       false,
			Protocols:     []string{}, // Must be non-nil for JSON schema validation
			Configuration: config,
			Message:       "Error: Mock recorder service is not available.",
		}, nil
	}

	// Show configuration and execute
	s.logger.Info("Starting mock recording with configuration",
		zap.String("command", command),
		zap.String("path", path),
	)

	// Execute recording
	result, err := s.mockRecorder.Record(ctx, models.RecordOptions{
		Command: command,
		Path:    path,
	})
	if err != nil {
		s.logger.Error("Mock recording failed", zap.Error(err))
		return nil, MockRecordOutput{
			Success:       false,
			Protocols:     []string{}, // Must be non-nil for JSON schema validation
			Configuration: config,
			Message:       fmt.Sprintf("Recording failed: %s", err.Error()),
		}, nil
	}

	// Ensure protocols is never nil for JSON schema validation (must be array, not null)
	protocols := []string{}
	if result.Metadata != nil && result.Metadata.Protocols != nil {
		protocols = result.Metadata.Protocols
	}

	s.logger.Info("Mock recording completed successfully",
		zap.String("mockFilePath", result.MockFilePath),
		zap.Int("mockCount", result.MockCount),
		zap.Strings("protocols", protocols),
	)

	return nil, MockRecordOutput{
		Success:       true,
		MockFilePath:  result.MockFilePath,
		MockCount:     result.MockCount,
		Protocols:     protocols,
		Configuration: config,
		Message:       fmt.Sprintf("Successfully recorded %d mock(s) to '%s'. Detected protocols: %v", result.MockCount, result.MockFilePath, protocols),
	}, nil
}

func (s *Server) elicitRecordCommand(ctx context.Context) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for elicitation")
	}

	s.logger.Info("Eliciting mock record command from user")
	result, err := session.Elicit(ctx, &sdkmcp.ElicitParams{
		Mode: "form",
		Message: "Please provide the command for `keploy mock record`.\n\n" +
			"Policy:\n" +
			"- Prefer test commands over run commands.\n" +
			"- For Go projects, prefer `go test` commands (for example `go test -v -run \"TestA|TestB\"` or `go test -v ./...`).\n" +
			"- Do not default to `go run main.go` when tests exist.\n" +
			"- Avoid long-running/watch/interactive commands.\n",
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Command to execute for mock recording",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	})
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	if result == nil {
		return "", nil
	}
	if result.Action != "accept" {
		return "", nil
	}

	rawCommand, ok := result.Content["command"]
	if !ok {
		return "", nil
	}
	command, _ := rawCommand.(string)
	return strings.TrimSpace(command), nil
}

// handleMockReplay handles the keploy_mock_test tool invocation.
func (s *Server) handleMockReplay(ctx context.Context, req *sdkmcp.CallToolRequest, in MockReplayInput) (*sdkmcp.CallToolResult, MockReplayOutput, error) {
	s.logger.Info("Mock replay tool invoked",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Validate input
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: 'command' is required. Please provide the test command to run (e.g., 'go test -v', 'npm test').",
		}, nil
	}
	path := strings.TrimSpace(in.Path)
	if req != nil && !hasArgument(req, "path") {
		// If caller omitted "path", keep it empty so replay service resolves latest run.
		path = ""
	}

	// Check if mock replayer is available
	if s.mockReplayer == nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: Mock replayer service is not available.",
		}, nil
	}

	config := &ReplayConfiguration{
		Command:        command,
		Path:           path,
		FallBackOnMiss: in.FallBackOnMiss,
	}

	s.logger.Info("Starting mock replay with configuration",
		zap.String("command", command),
		zap.String("path", path),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Execute replay
	result, err := s.mockReplayer.Replay(ctx, models.ReplayOptions{
		Command:        command,
		Path:           path,
		FallBackOnMiss: in.FallBackOnMiss,
	})
	if err != nil {
		s.logger.Error("Mock replay failed", zap.Error(err))
		return nil, MockReplayOutput{
			Success:       false,
			Configuration: config,
			Message:       fmt.Sprintf("Replay failed: %s", err.Error()),
		}, nil
	}

	// Build result message
	var messageParts []string
	mocksLoaded := result.MocksReplayed + result.MocksMissed
	mocksUnused := result.MocksMissed
	messageParts = append(messageParts, fmt.Sprintf("Loaded %d mock(s)", mocksLoaded))
	messageParts = append(messageParts, fmt.Sprintf("Replayed %d mock(s)", result.MocksReplayed))
	if mocksUnused > 0 {
		messageParts = append(messageParts, fmt.Sprintf("%d mock(s) unused", mocksUnused))
	}

	if result.AppExitCode != 0 {
		messageParts = append(messageParts, fmt.Sprintf("app exited with code %d", result.AppExitCode))
	} else {
		messageParts = append(messageParts, "app exited successfully")
	}

	message := strings.Join(messageParts, ", ")
	if result.Success {
		message = "Test passed! " + message
	} else {
		message = "Test completed with issues. " + message
	}

	s.logger.Info("Mock replay completed",
		zap.Bool("success", result.Success),
		zap.Int("mocksReplayed", result.MocksReplayed),
		zap.Int("mocksLoaded", mocksLoaded),
		zap.Int("mocksUnused", mocksUnused),
		zap.Int("exitCode", result.AppExitCode),
	)

	return nil, MockReplayOutput{
		Success:       result.Success,
		MocksReplayed: result.MocksReplayed,
		MocksMissed:   result.MocksMissed,
		AppExitCode:   result.AppExitCode,
		Configuration: config,
		Message:       message,
	}, nil
}

func hasArgument(req *sdkmcp.CallToolRequest, key string) bool {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return false
	}

	args := map[string]json.RawMessage{}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return false
	}

	_, ok := args[key]
	return ok
}
