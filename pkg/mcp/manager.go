package mcp

import (
	"context"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// handleManager handles the keploy_manager tool invocation.
// This is the unified controller that routes to different actions:
// - keploy_mock_record: delegates to mock recording
// - keploy_mock_test: delegates to mock testing
// - pipeline: generates CI/CD pipeline for regression testing
func (s *Server) handleManager(ctx context.Context, req *sdkmcp.CallToolRequest, in ManagerInput) (*sdkmcp.CallToolResult, ManagerOutput, error) {
	s.logger.Info("Manager tool invoked",
		zap.String("action", in.Action),
		zap.String("command", in.Command),
		zap.String("path", in.Path),
	)

	action := strings.TrimSpace(in.Action)
	if action == "" {
		return nil, ManagerOutput{
			Success: false,
			Action:  "",
			Message: "Error: 'action' is required. Valid actions: 'keploy_mock_record', 'keploy_mock_test', 'pipeline'",
		}, nil
	}

	switch action {
	case ActionMockRecord:
		return s.handleManagerMockRecord(ctx, in)
	case ActionMockTest:
		return s.handleManagerMockTest(ctx, in)
	case ActionPipeline:
		return s.handleManagerPipeline(ctx, in)
	default:
		return nil, ManagerOutput{
			Success: false,
			Action:  action,
			Message: fmt.Sprintf("Error: Unknown action '%s'. Valid actions: 'keploy_mock_record', 'keploy_mock_test', 'pipeline'", action),
		}, nil
	}
}

// handleManagerMockRecord handles the mock_record action by delegating to the mock record handler.
func (s *Server) handleManagerMockRecord(ctx context.Context, in ManagerInput) (*sdkmcp.CallToolResult, ManagerOutput, error) {
	s.logger.Info("Manager: Executing mock_record action",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
	)

	// Validate command
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return nil, ManagerOutput{
			Success: false,
			Action:  ActionMockRecord,
			Message: "Error: 'command' is required for mock_record action. Please provide the command to run your application.",
		}, nil
	}

	// Create input for the mock record handler
	recordInput := MockRecordInput{
		Command: command,
		Path:    in.Path,
	}

	// Call the mock record handler
	_, recordOutput, err := s.handleMockRecord(ctx, nil, recordInput)
	if err != nil {
		return nil, ManagerOutput{
			Success: false,
			Action:  ActionMockRecord,
			Message: fmt.Sprintf("Mock recording failed: %s", err.Error()),
		}, nil
	}

	return nil, ManagerOutput{
		Success:      recordOutput.Success,
		Action:       ActionMockRecord,
		Message:      recordOutput.Message,
		RecordResult: &recordOutput,
	}, nil
}

// handleManagerMockTest handles the mock_test action by delegating to the mock replay handler.
func (s *Server) handleManagerMockTest(ctx context.Context, in ManagerInput) (*sdkmcp.CallToolResult, ManagerOutput, error) {
	s.logger.Info("Manager: Executing mock_test action",
		zap.String("command", in.Command),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Validate command
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return nil, ManagerOutput{
			Success: false,
			Action:  ActionMockTest,
			Message: "Error: 'command' is required for mock_test action. Please provide the test command to run.",
		}, nil
	}

	// Create input for the mock replay handler
	replayInput := MockReplayInput{
		Command:        command,
		FallBackOnMiss: in.FallBackOnMiss,
	}

	// Call the mock replay handler
	_, replayOutput, err := s.handleMockReplay(ctx, nil, replayInput)
	if err != nil {
		return nil, ManagerOutput{
			Success: false,
			Action:  ActionMockTest,
			Message: fmt.Sprintf("Mock testing failed: %s", err.Error()),
		}, nil
	}

	return nil, ManagerOutput{
		Success:    replayOutput.Success,
		Action:     ActionMockTest,
		Message:    replayOutput.Message,
		TestResult: &replayOutput,
	}, nil
}

// handleManagerPipeline handles the pipeline action for CI/CD generation.
func (s *Server) handleManagerPipeline(ctx context.Context, in ManagerInput) (*sdkmcp.CallToolResult, ManagerOutput, error) {
	s.logger.Info("Manager: Executing pipeline action",
		zap.String("appCommand", in.AppCommand),
		zap.String("defaultBranch", in.DefaultBranch),
		zap.String("mockPath", in.MockPath),
		zap.String("cicdTool", in.CICDTool),
	)

	// Create pipeline input from manager input
	pipelineInput := PipelineInput{
		AppCommand:    in.AppCommand,
		DefaultBranch: in.DefaultBranch,
		MockPath:      in.MockPath,
		CICDTool:      in.CICDTool,
	}

	// If command is provided but appCommand is not, use command as appCommand
	if pipelineInput.AppCommand == "" && in.Command != "" {
		pipelineInput.AppCommand = in.Command
	}

	// Call the pipeline handler
	pipelineOutput, err := s.handlePipeline(ctx, pipelineInput)
	if err != nil {
		return nil, ManagerOutput{
			Success: false,
			Action:  ActionPipeline,
			Message: fmt.Sprintf("Pipeline generation failed: %s", err.Error()),
		}, nil
	}

	return nil, ManagerOutput{
		Success:        pipelineOutput.Success,
		Action:         ActionPipeline,
		Message:        pipelineOutput.Message,
		PipelineResult: pipelineOutput,
	}, nil
}
