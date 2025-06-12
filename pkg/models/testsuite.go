package models

// TestSuite represents the structure of a test suite YAML file
type TestSuite struct {
	Version string        `yaml:"version"`
	Kind    string        `yaml:"kind"`
	Name    string        `yaml:"name"`
	Spec    TestSuiteSpec `yaml:"spec"`
}

// TestSuiteSpec contains the metadata and steps for a test suite
type TestSuiteSpec struct {
	Metadata TestSuiteMetadata `yaml:"metadata"`
	Load     LoadOptions       `yaml:"load,omitempty"`
	Steps    []TestStep        `yaml:"steps"`
}

// TestSuiteMetadata contains description and other metadata for a test suite
type TestSuiteMetadata struct {
	Description string `yaml:"description"`
}

// LoadOptions represents load testing options
type LoadOptions struct {
	Profile    string      `yaml:"profile"`
	VUs        int         `yaml:"vus"`
	Duration   string      `yaml:"duration"`
	RPS        int         `yaml:"rps"`
	Stages     []LoadStage `yaml:"stages,omitempty"`
	Thresholds []Threshold `yaml:"thresholds,omitempty"`
}

// LoadStage represents a single stage in a load test
type LoadStage struct {
	Duration string `yaml:"duration"`
	Target   int    `yaml:"target"`
}

// Threshold represents a performance threshold in load testing
type Threshold struct {
	Metric    string `yaml:"metric"`
	Condition string `yaml:"condition"`
}

// TestStep represents a single API call step in the test suite
type TestStep struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Body    string            `yaml:"body,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Extract map[string]string `yaml:"extract,omitempty"`
	Assert  []TSAssertion     `yaml:"assert,omitempty"`
}

// Assertion represents an assertion to validate API responses
type TSAssertion struct {
	Type           string `yaml:"type"`
	Key            string `yaml:"key,omitempty"`
	ExpectedString string `yaml:"expected_string,omitempty"`
}

// SuiteResult represents the results of executing a test suite
type SuiteResult struct {
	SuiteName     string       `json:"suite_name"`
	TotalSteps    int          `json:"total_steps"`
	PassedSteps   int          `json:"passed_steps"`
	FailedSteps   int          `json:"failed_steps"`
	StepResults   []StepResult `json:"step_results"`
	ExecutionTime float64      `json:"execution_time_ms"`
	Success       bool         `json:"success"`
}

// StepResult represents the result of executing a single test step
type StepResult struct {
	StepName      string            `json:"step_name"`
	Method        string            `json:"method"`
	URL           string            `json:"url"`
	Status        string            `json:"status"`
	StatusCode    int               `json:"status_code,omitempty"`
	ResponseTime  int64             `json:"response_time_ms"`
	FailureReason string            `json:"failure_reason,omitempty"`
	ExtractedVars map[string]string `json:"extracted_vars,omitempty"`
}
