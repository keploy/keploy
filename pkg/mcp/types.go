package mcp

// Tool name constants for the MCP tools.
const (
	// ToolMockRecord is the name of the mock record tool.
	ToolMockRecord = "keploy_mock_record"
	// ToolMockTest is the name of the mock test tool.
	ToolMockTest = "keploy_mock_test"
	// ToolPromptTestCommand returns an LLM prompt to derive the best serialized test command.
	ToolPromptTestCommand = "keploy_prompt_test_command"
	// ToolPromptDependencyStart returns an LLM prompt to detect, verify, and start dependencies.
	ToolPromptDependencyStart = "keploy_prompt_dependency_start"
	// ToolPromptTestIntegration returns an LLM prompt to instrument tests with sandbox scope hooks.
	ToolPromptTestIntegration = "keploy_prompt_test_integration"
	// ToolPromptPipelineCreation returns an LLM prompt to generate CI/CD pipeline files.
	ToolPromptPipelineCreation = "keploy_prompt_pipeline_creation"
	// ToolManager is the name of the unified manager tool.
	ToolManager = "keploy_manager"
)

// ManagerInput defines the input parameters for the keploy_manager tool.
type ManagerInput struct {
	// Path is the sandbox location directory.
	// Optional for both keploy_mock_record and keploy_mock_test.
	// Defaults to "." when omitted.
	Path string `json:"path,omitempty" jsonschema:"Sandbox location directory. Optional for keploy_mock_record/keploy_mock_test; defaults to '.' when omitted."`

	// FallBackOnMiss indicates whether to fall back to real calls (for mock_test action).
	FallBackOnMiss bool `json:"fallBackOnMiss,omitempty" jsonschema:"Whether to fall back to real calls when mock not found (default: false)"`
}

// ManagerOutput defines the output of the keploy_manager tool.
type ManagerOutput struct {
	// Success indicates whether the operation was successful.
	Success bool `json:"success"`

	// Message is a human-readable status message.
	Message string `json:"message"`
}
