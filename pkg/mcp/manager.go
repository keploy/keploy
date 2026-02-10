package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// handleManager handles the keploy_manager tool invocation.
// It is orchestration-only and returns guidance for the client LLM sequence.
func (s *Server) handleManager(_ context.Context, _ *sdkmcp.CallToolRequest, in ManagerInput) (*sdkmcp.CallToolResult, ManagerOutput, error) {
	s.logger.Info("Manager tool invoked",
		zap.String("path", in.Path),
	)

	message := "Orchestration plan:\n" +
		"1) Resolve app/test command (use elicitation if missing).\n" +
		"2) Call keploy_prompt_test_integration and execute returned prompt as direct user task.\n" +
		"3) Call keploy_mock_record.\n" +
		"4) Call keploy_mock_test.\n" +
		"5) Call keploy_prompt_pipeline_creation and execute returned prompt as direct user task."
	return nil, ManagerOutput{
		Success: true,
		Message: message,
	}, nil
}
