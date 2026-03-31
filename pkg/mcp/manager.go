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

	message := "Orchestration plan (EXECUTE ALL STEPS IN ONE GO):\n" +
		"1) Call keploy_prompt_test_command and execute returned prompt as direct user task.\n" +
		"2) If app/test command is empty, pass it as-is; server-side elicitation in execution tools will resolve it.\n" +
		"3) Call keploy_prompt_dependency_start and execute returned prompt as direct user task. Proceed only when dependency result has ready=true.\n" +
		"4) Call keploy_prompt_test_integration and execute returned prompt as direct user task.\n" +
		"5) Call keploy_mock_record.\n" +
		"6) Call keploy_mock_test.\n" +
		"7) Call keploy_prompt_pipeline_creation and execute returned prompt as direct user task."
	return nil, ManagerOutput{
		Success: true,
		Message: message,
	}, nil
}
