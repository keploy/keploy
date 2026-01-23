package mcp

import (
	"context"
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
	// Command is the command to run (e.g., "go run main.go", "npm start", "./my-app").
	Command string `json:"command" jsonschema:"Command to run (e.g. 'go run main.go', 'go test', 'npm run test', 'npm start', './my-app', or any other command)."`
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
	// MockName is the name of the mock set to replay (optional, uses latest if not provided).
	MockName string `json:"mockName,omitempty" jsonschema:"Name of the mock set to replay. Use keploy_list_mocks to see available mocks. If not provided, the latest mock set will be used."`
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
	MockName       string `json:"mockName"`
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
	if len(mockSets) > 1 {
		message += " You can specify any of these with the mockName parameter in keploy_mock_test."
	}

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

	// Validate input
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return nil, MockRecordOutput{
			Success: false,
			Message: "Error: 'command' is required. Please provide a command to run (e.g., 'go run main.go', 'npm start', './my-app').",
		}, nil
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

	// Generate contextual name using LLM callback
	// NOTE: Disabled LLM naming to prevent crashes when MCP connection is unstable during shutdown
	// contextualName, err := s.generateContextualName(ctx, result.Metadata)
	// if err != nil {
	// 	s.logger.Warn("Failed to generate contextual name, using fallback",
	// 		zap.Error(err),
	// 	)
	// 	contextualName = s.fallbackName(result.Metadata)
	// }

	// Use fallback naming directly (deterministic based on metadata)
	contextualName := s.fallbackName(result.Metadata)

	// Rename mock file with contextual name
	newPath := s.renameMockFile(result.MockFilePath, contextualName)

	// Ensure protocols is never nil for JSON schema validation (must be array, not null)
	protocols := []string{}
	if result.Metadata != nil && result.Metadata.Protocols != nil {
		protocols = result.Metadata.Protocols
	}

	s.logger.Info("Mock recording completed successfully",
		zap.String("mockFilePath", newPath),
		zap.Int("mockCount", result.MockCount),
		zap.Strings("protocols", protocols),
	)

	return nil, MockRecordOutput{
		Success:       true,
		MockFilePath:  newPath,
		MockCount:     result.MockCount,
		Protocols:     protocols,
		Configuration: config,
		Message:       fmt.Sprintf("Successfully recorded %d mock(s) to '%s'. Detected protocols: %v", result.MockCount, newPath, protocols),
	}, nil
}

// handleMockReplay handles the keploy_mock_test tool invocation.
func (s *Server) handleMockReplay(ctx context.Context, req *sdkmcp.CallToolRequest, in MockReplayInput) (*sdkmcp.CallToolResult, MockReplayOutput, error) {
	s.logger.Info("Mock replay tool invoked",
		zap.String("command", in.Command),
		zap.String("mockName", in.MockName),
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

	// Check if mock replayer is available
	if s.mockReplayer == nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: Mock replayer service is not available.",
		}, nil
	}

	mockName := strings.TrimSpace(in.MockName)

	// If no mock name provided, get the latest
	if mockName == "" {
		mockSets, err := s.mockReplayer.ListMockSets(ctx)
		if err != nil {
			return nil, MockReplayOutput{
				Success: false,
				Message: fmt.Sprintf("Failed to get available mock sets: %s. Please specify mockName explicitly.", err.Error()),
			}, nil
		}
		if len(mockSets) == 0 {
			return nil, MockReplayOutput{
				Success: false,
				Message: "No mock sets found. Use keploy_mock_record to create mocks first.",
			}, nil
		}
		mockName = mockSets[0]
		s.logger.Info("Using latest mock set", zap.String("mockName", mockName))
	}

	config := &ReplayConfiguration{
		Command:        command,
		MockName:       mockName,
		FallBackOnMiss: in.FallBackOnMiss,
	}

	s.logger.Info("Starting mock replay with configuration",
		zap.String("command", command),
		zap.String("mockName", mockName),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Execute replay
	result, err := s.mockReplayer.Replay(ctx, models.ReplayOptions{
		Command:        command,
		MockName:       mockName,
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
	messageParts = append(messageParts, fmt.Sprintf("Replayed %d mock(s)", result.MocksReplayed))

	if result.MocksMissed > 0 {
		messageParts = append(messageParts, fmt.Sprintf("%d mock(s) missed", result.MocksMissed))
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
		zap.Int("mocksMissed", result.MocksMissed),
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
