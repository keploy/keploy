package mcp

// Tool name constants for the MCP tools.
const (
	// ToolListMocks is the name of the list mocks tool.
	ToolListMocks = "keploy_list_mocks"
	// ToolMockRecord is the name of the mock record tool.
	ToolMockRecord = "keploy_mock_record"
	// ToolMockTest is the name of the mock test tool.
	ToolMockTest = "keploy_mock_test"
	// ToolManager is the name of the unified manager tool.
	ToolManager = "keploy_manager"
)

// Action constants for the keploy_manager tool.
const (
	// ActionMockRecord delegates to keploy_mock_record.
	ActionMockRecord = "keploy_mock_record"
	// ActionMockTest delegates to keploy_mock_test.
	ActionMockTest = "keploy_mock_test"
	// ActionPipeline generates CI/CD pipeline for regression testing.
	ActionPipeline = "pipeline"
)

// CI/CD platform constants.
const (
	CICDGitHubActions      = "github-actions"
	CICDGitLabCI           = "gitlab-ci"
	CICDJenkins            = "jenkins"
	CICDCircleCI           = "circleci"
	CICDAzurePipelines     = "azure-pipelines"
	CICDBitbucketPipelines = "bitbucket-pipelines"
)

// ManagerInput defines the input parameters for the keploy_manager tool.
type ManagerInput struct {
	// Action is the action to perform: "keploy_mock_record", "keploy_mock_test", or "pipeline".
	Action string `json:"action" jsonschema:"Action to perform: keploy_mock_record (record mocks), keploy_mock_test (replay mocks), or pipeline (generate CI/CD)"`

	// Command is the application or test command (for mock_record and mock_test actions).
	Command string `json:"command,omitempty" jsonschema:"Application or test command to run (required for keploy_mock_record and keploy_mock_test actions)"`

	// Path is the base path for mock storage.
	// Required for keploy_mock_test action. Optional for keploy_mock_record.
	Path string `json:"path,omitempty" jsonschema:"Path for mock storage. Required for keploy_mock_test, optional for keploy_mock_record."`

	// FallBackOnMiss indicates whether to fall back to real calls (for mock_test action).
	FallBackOnMiss bool `json:"fallBackOnMiss,omitempty" jsonschema:"Whether to fall back to real calls when mock not found (default: false)"`

	// AppCommand is the application command for pipeline generation (for pipeline action).
	AppCommand string `json:"appCommand,omitempty" jsonschema:"Application command for CI/CD pipeline (for pipeline action). If not provided will prompt user."`

	// DefaultBranch is the default branch for CI/CD triggers (for pipeline action).
	DefaultBranch string `json:"defaultBranch,omitempty" jsonschema:"Default branch for CI/CD triggers (default: main)"`

	// MockPath is the path where Keploy mocks are stored (for pipeline action).
	MockPath string `json:"mockPath,omitempty" jsonschema:"Path where Keploy mocks are stored (default: ./keploy)"`

	// CICDTool is the CI/CD platform to use (for pipeline action).
	// Values: "github-actions", "gitlab-ci", "jenkins", "circleci", "azure-pipelines", "bitbucket-pipelines"
	CICDTool string `json:"cicdTool,omitempty" jsonschema:"CI/CD platform: github-actions, gitlab-ci, jenkins, circleci, azure-pipelines, or bitbucket-pipelines (auto-detected if not provided)"`
}

// ManagerOutput defines the output of the keploy_manager tool.
type ManagerOutput struct {
	// Success indicates whether the operation was successful.
	Success bool `json:"success"`

	// Action is the action that was performed.
	Action string `json:"action"`

	// Message is a human-readable status message.
	Message string `json:"message"`

	// RecordResult contains the result of mock recording (if action was mock_record).
	RecordResult *MockRecordOutput `json:"recordResult,omitempty"`

	// TestResult contains the result of mock testing (if action was mock_test).
	TestResult *MockReplayOutput `json:"testResult,omitempty"`

	// PipelineResult contains the result of pipeline generation (if action was pipeline).
	PipelineResult *PipelineOutput `json:"pipelineResult,omitempty"`
}

// PipelineInput defines the input parameters for pipeline generation.
type PipelineInput struct {
	// AppCommand is the application command for Keploy mock testing.
	AppCommand string `json:"appCommand"`

	// DefaultBranch is the default branch for merge triggers.
	DefaultBranch string `json:"defaultBranch,omitempty"`

	// MockPath is the path where Keploy mocks are stored.
	MockPath string `json:"mockPath,omitempty"`

	// CICDTool is the CI/CD platform to use.
	CICDTool string `json:"cicdTool,omitempty"`
}

// PipelineOutput defines the output of the pipeline action.
type PipelineOutput struct {
	// Success indicates whether pipeline generation was successful.
	Success bool `json:"success"`

	// CICDTool is the CI/CD platform used.
	CICDTool string `json:"cicdTool"`

	// FilePath is the path to the generated pipeline file.
	FilePath string `json:"filePath"`

	// Content is the content of the generated pipeline file.
	Content string `json:"content,omitempty"`

	// Message is a human-readable status message.
	Message string `json:"message"`

	// Configuration shows the configuration that was used.
	Configuration *PipelineConfiguration `json:"configuration,omitempty"`

	// DetectedProject shows the detected project information (language, framework, etc.)
	DetectedProject *DetectedProjectInfo `json:"detectedProject,omitempty"`
}

// PipelineConfiguration shows the configuration used for pipeline generation.
type PipelineConfiguration struct {
	AppCommand    string `json:"appCommand"`
	DefaultBranch string `json:"defaultBranch"`
	MockPath      string `json:"mockPath"`
	CICDTool      string `json:"cicdTool"`
}

// DetectedProjectInfo shows the detected project information for transparency.
type DetectedProjectInfo struct {
	Language       string   `json:"language,omitempty"`
	Framework      string   `json:"framework,omitempty"`
	PackageManager string   `json:"packageManager,omitempty"`
	RuntimeVersion string   `json:"runtimeVersion,omitempty"`
	SetupSteps     []string `json:"setupSteps,omitempty"`
}

// PipelineConfig holds the configuration for generating a CI/CD pipeline.
type PipelineConfig struct {
	AppCommand    string
	DefaultBranch string
	MockPath      string
	CICDTool      string
}

// PlatformDetails contains CI/CD platform-specific details.
type PlatformDetails struct {
	FilePath     string
	PipelineName string
	TriggerText  string
}

// CICDFiles represents the CI/CD configuration files to check for auto-detection.
type CICDFiles struct {
	GitHubWorkflows    bool
	GitLabCI           bool
	Jenkinsfile        bool
	CircleCI           bool
	AzurePipelines     bool
	BitbucketPipelines bool
}

// ElicitationResponse represents a response from an elicitation request.
type ElicitationResponse struct {
	Action  string                 `json:"action"` // "accept", "decline", or "cancel"
	Content map[string]interface{} `json:"content,omitempty"`
}
