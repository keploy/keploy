package testsuite

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
	Steps    []TestStep        `yaml:"steps"`
}

// TestSuiteMetadata contains description and other metadata for a test suite
type TestSuiteMetadata struct {
	Description string `yaml:"description"`
}

// TestStep represents a single API call step in the test suite
type TestStep struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Body    string            `yaml:"body,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Extract map[string]string `yaml:"extract,omitempty"`
	Assert  []Assertion       `yaml:"assert,omitempty"`
}

// Assertion represents an assertion to validate API responses
type Assertion struct {
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
