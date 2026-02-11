package mcp

// Tool name constants for the MCP tools.
const (
	// ToolMockRecord is the name of the mock record tool.
	ToolMockRecord = "keploy_mock_record"
	// ToolMockTest is the name of the mock test tool.
	ToolMockTest = "keploy_mock_test"
	// ToolPromptTestIntegration returns an LLM prompt to instrument tests with start-session hooks.
	ToolPromptTestIntegration = "keploy_prompt_test_integration"
	// ToolPromptPipelineCreation returns an LLM prompt to generate CI/CD pipeline files.
	ToolPromptPipelineCreation = "keploy_prompt_pipeline_creation"
	// ToolManager is the name of the unified manager tool.
	ToolManager = "keploy_manager"
)

// ManagerInput defines the input parameters for the keploy_manager tool.
type ManagerInput struct {
	// Path is the base path for mock storage.
	// Optional for both keploy_mock_record and keploy_mock_test.
	// For mock_test, omit unless user explicitly asks for a specific path.
	// If omitted, latest mock set is selected automatically by replay service.
	Path string `json:"path,omitempty" jsonschema:"Path for mock storage. Optional for mock_record/mock_test. For mock_test, omit unless user explicitly asks for a specific path; when omitted, latest mock set is used."`

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
