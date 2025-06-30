package testsuite

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

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
	Severity  string `yaml:"severity"`
	Comment   string `yaml:"comment,omitempty"`
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

// StepResult represents the result of executing a single test step
type StepResult struct {
	StepName      string            `json:"step_name"`
	Method        string            `json:"method"`
	URL           string            `json:"url"`
	Status        string            `json:"status"`
	StatusCode    int               `json:"status_code,omitempty"`
	ResponseTime  time.Duration     `json:"response_time"`
	FailureReason string            `json:"failure_reason,omitempty"`
	ExtractedVars map[string]string `json:"extracted_vars,omitempty"`
	ReqBytes      int64             `json:"req_bytes"`
	ResBytes      int64             `json:"res_bytes"`
}

// ExecutionReport represents the summary of the test suite execution
type ExecutionReport struct {
	SuiteName     string        `json:"suite_name"`
	TotalSteps    int           `json:"total_steps"`
	FailedSteps   int           `json:"failed_steps"`
	StepsResult   []StepResult  `json:"steps_result"`
	ExecutionTime time.Duration `json:"execution_time"` // Total execution of the test suite
}

type TSExecutor struct {
	config    *config.Config
	logger    *zap.Logger
	Testsuite *TestSuite
	client    *http.Client
	baseURL   string
	tsPath    string
	tsFile    string
	variables map[string]string
}

func NewTSExecutor(cfg *config.Config, logger *zap.Logger, skipParsing bool) (*TSExecutor, error) {
	var testsuite *TestSuite
	if !skipParsing {
		if cfg.TestSuite.TSPath == "" {
			logger.Error("test suite path is not set")
			return nil, fmt.Errorf("test suite path is not set, use --ts-path flag to set it")
		}

		if cfg.TestSuite.TSFile == "" {
			logger.Error("test suite file is not set")
			return nil, fmt.Errorf("test suite file is not set, use --ts-file flag to set it")
		}

		testsuitePath := filepath.Join(cfg.TestSuite.TSPath, cfg.TestSuite.TSFile)
		logger.Debug("parsing test suite file", zap.String("file", testsuitePath))

		ts, err := TSParser(testsuitePath)
		if err != nil {
			logger.Error("failed to parse test suite", zap.Error(err))
			return nil, err
		}
		testsuite = &ts
		logger.Info("test suite parsed successfully", zap.String("file", testsuitePath))
	}

	return &TSExecutor{
		config:    cfg,
		logger:    logger,
		Testsuite: testsuite,
		client: &http.Client{
			Timeout: time.Duration(30) * time.Second,
			Transport: &http.Transport{
				// disable tls check
				//nolint:gosec
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		baseURL:   cfg.TestSuite.BaseURL,
		tsPath:    cfg.TestSuite.TSPath,
		tsFile:    cfg.TestSuite.TSFile,
		variables: make(map[string]string),
	}, nil
}

func (e *TSExecutor) Execute(ctx context.Context, limiter *rate.Limiter) (*ExecutionReport, error) {
	if e.baseURL == "" {
		e.logger.Error("base URL is not set for the test suite execution")
		return nil, fmt.Errorf("base URL is not set for the test suite execution")
	}

	if e.client == nil {
		e.logger.Error("HTTP client is not initialized for the test suite execution")
		return nil, fmt.Errorf("HTTP client is not initialized for the test suite execution")
	}

	if e.Testsuite == nil {
		e.logger.Error("test suite is not set for execution")
		return nil, fmt.Errorf("test suite is not set for execution, please provide a valid test suite using --ts-file or -f flag")
	}

	if ctx.Value("command") == "testsuite" {
		e.logger.Info("executing test suite", zap.String("path", e.tsPath), zap.String("baseURL", e.baseURL))
	}

	e.logger.Debug("test suite details",
		zap.String("name", e.Testsuite.Name),
		zap.String("version", e.Testsuite.Version),
		zap.String("kind", e.Testsuite.Kind),
		zap.String("description", e.Testsuite.Spec.Metadata.Description),
	)
	e.logger.Debug("number of steps in the test suite", zap.Int("steps", len(e.Testsuite.Spec.Steps)))
	e.logger.Debug("base URL for the test suite", zap.String("baseURL", e.baseURL))

	er := &ExecutionReport{
		SuiteName:     e.Testsuite.Name,
		TotalSteps:    len(e.Testsuite.Spec.Steps),
		FailedSteps:   0,
		StepsResult:   make([]StepResult, 0, len(e.Testsuite.Spec.Steps)),
		ExecutionTime: time.Duration(0),
	}

	startTime := time.Now()

	for _, step := range e.Testsuite.Spec.Steps {
		e.logger.Debug("executing step", zap.String("name", step.Name), zap.String("method", step.Method), zap.String("url", step.URL))
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				e.logger.Debug("Rate limiter wait warn", zap.Error(err))
				continue
			}
		}
		result, err := e.executeStep(step)
		if err != nil {
			e.logger.Error("failed to execute step", zap.String("step", step.Name), zap.Error(err))
		}
		e.logger.Debug("step executed", zap.String("step", step.Name), zap.String("status", result.Status), zap.Any("result", result))
		er.StepsResult = append(er.StepsResult, *result)
		if result.Status == "failed" {
			er.FailedSteps++
		}
	}

	er.ExecutionTime = time.Since(startTime)

	if ctx.Value("command") == "testsuite" {
		fmt.Println("Test Suite Execution Report:")
		fmt.Printf("  Suite Name: %s\n", er.SuiteName)
		fmt.Printf("  Base URL: %s\n", e.baseURL)
		fmt.Printf("  Total Steps: %d\n", er.TotalSteps)
		fmt.Printf("  Failed Steps: %d\n", er.FailedSteps)
		fmt.Printf("  Execution Time: %s\n", er.ExecutionTime)
		fmt.Println("  Steps Result:")
		for _, stepResult := range er.StepsResult {
			fmt.Printf("    Step Name: %s\n", stepResult.StepName)
			fmt.Printf("      Status: %s\n", stepResult.Status)
			if stepResult.FailureReason != "" {
				fmt.Printf("      Failure Reason: %s\n", stepResult.FailureReason)
			}
		}

		reportDir := filepath.Join(e.tsPath, "ts_reports")
		if err := os.MkdirAll(reportDir, 0755); err != nil {
			e.logger.Error("failed to create report directory", zap.String("dir", reportDir), zap.Error(err))
			return nil, fmt.Errorf("failed to create report directory: %v", err)
		}
		reportFile := filepath.Join(reportDir, fmt.Sprintf("%s_report_%s", time.Now().Format("20060102150405"), e.tsFile))
		file, err := os.Create(reportFile)
		if err != nil {
			e.logger.Error("failed to create report file", zap.String("file", reportFile), zap.Error(err))
			return nil, fmt.Errorf("failed to create report file: %v", err)
		}
		defer file.Close()

		data, err := yaml.Marshal(er)
		if err != nil {
			e.logger.Error("failed to marshal report data", zap.String("file", reportFile), zap.Error(err))
			return nil, fmt.Errorf("failed to marshal report data: %v", err)
		}
		_, err = file.Write(data)
		if err != nil {
			e.logger.Error("failed to write report data to file", zap.String("file", reportFile), zap.Error(err))
			return nil, fmt.Errorf("failed to write report data to file: %v", err)
		}
		e.logger.Info("test suite execution report saved", zap.String("file", reportFile))
	}

	return er, nil
}

// executeStep executes a single test step and returns the result
func (e *TSExecutor) executeStep(step TestStep) (*StepResult, error) {
	result := &StepResult{
		StepName:      step.Name,
		Method:        step.Method,
		URL:           step.URL,
		Status:        "failed", // Default to failed, will update to passed if successful
		ExtractedVars: make(map[string]string),
	}

	interpolatedURL := e.interpolateVariables(step.URL)
	interpolatedBody := e.interpolateVariables(step.Body)

	fullURL := e.baseURL + interpolatedURL
	e.logger.Debug("sending request", zap.String("url", fullURL), zap.String("method", step.Method))

	req, err := http.NewRequest(step.Method, fullURL, strings.NewReader(interpolatedBody))
	if err != nil {
		result.FailureReason = fmt.Sprintf("failed to create request: %v", err)
		return result, err
	}
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(strings.NewReader(interpolatedBody))
		if err != nil {
			result.FailureReason = fmt.Sprintf("failed to read request body: %v", err)
			return result, err
		}
		result.ReqBytes = int64(len(bodyBytes))
	} else {
		result.ReqBytes = 0
	}

	for key, value := range step.Headers {
		interpolatedValue := e.interpolateVariables(value)
		req.Header.Add(key, interpolatedValue)
	}

	startTime := time.Now()

	resp, err := e.client.Do(req)
	if err != nil {
		result.FailureReason = fmt.Sprintf("failed to send request: %v", err)
		return result, err
	}
	defer resp.Body.Close()

	result.ResponseTime = time.Since(startTime)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.FailureReason = fmt.Sprintf("failed to read response body: %v", err)
		return result, err
	}
	result.ResBytes = int64(len(body))

	result.StatusCode = resp.StatusCode

	assertionsPassed := true
	for _, assertion := range step.Assert {
		interpolatedExpectedString := e.interpolateVariables(assertion.ExpectedString)
		assertionCopy := assertion
		assertionCopy.ExpectedString = interpolatedExpectedString

		passed, reason := e.processAssertion(assertionCopy, resp, body)
		if !passed {
			assertionsPassed = false
			result.FailureReason = reason
			e.logger.Debug("assertion failed",
				zap.String("type", assertion.Type),
				zap.String("reason", reason))
			break
		}
		e.logger.Debug("assertion passed", zap.String("type", assertion.Type))
	}

	if assertionsPassed && len(step.Extract) > 0 {
		extracted, err := e.extractVariables(step.Extract, body)
		if err != nil {
			result.FailureReason = fmt.Sprintf("failed to extract variables: %v", err)
			return result, err
		}
		result.ExtractedVars = extracted

		for k, v := range extracted {
			e.variables[k] = v
			e.logger.Debug("variable extracted", zap.String("name", k), zap.String("value", v))
		}
	}

	if assertionsPassed {
		result.Status = "passed"
	}

	return result, nil
}

// Helper function to process assertions
func (e *TSExecutor) processAssertion(assertion TSAssertion, resp *http.Response, body []byte) (bool, string) {
	switch assertion.Type {
	case "status_code":
		expectedCode := assertion.ExpectedString
		actualCode := fmt.Sprintf("%d", resp.StatusCode)
		if expectedCode != actualCode {
			return false, fmt.Sprintf("expected status code %s but got %s", expectedCode, actualCode)
		}
	case "json_equal":
		var jsonData interface{}
		if err := json.Unmarshal(body, &jsonData); err != nil {
			return false, fmt.Sprintf("failed to parse JSON response: %v", err)
		}

		actualValue, err := extractJsonValue(jsonData, assertion.Key)
		if err != nil {
			return false, fmt.Sprintf("failed to extract JSON value for key %s: %v", assertion.Key, err)
		}

		actualString := fmt.Sprintf("%v", actualValue)

		if actualString != assertion.ExpectedString {
			return false, fmt.Sprintf("for key %s, expected value '%s' but got '%s'",
				assertion.Key, assertion.ExpectedString, actualString)
		}
	default:
		return false, fmt.Sprintf("unsupported assertion type: %s", assertion.Type)
	}

	return true, ""
}

// Helper function to interpolate variables in strings
func (e *TSExecutor) interpolateVariables(input string) string {
	if len(e.variables) == 0 || input == "" {
		return input
	}

	result := input
	variableRegex := regexp.MustCompile(`\{\{(\w+)\}\}`)

	matches := variableRegex.FindAllStringSubmatch(input, -1)
	for _, match := range matches {
		if len(match) == 2 {
			placeholder := match[0] // {{varname}}
			varName := match[1]     // varname

			if value, exists := e.variables[varName]; exists {
				result = strings.Replace(result, placeholder, value, -1)
			}
		}
	}

	return result
}

// Helper function to extract variables from response
func (e *TSExecutor) extractVariables(extractMap map[string]string, body []byte) (map[string]string, error) {
	var jsonData interface{}
	if err := json.Unmarshal(body, &jsonData); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %v", err)
	}

	result := make(map[string]string)

	for varName, jsonPath := range extractMap {
		value, err := extractJsonValue(jsonData, jsonPath)
		if err != nil {
			return nil, fmt.Errorf("failed to extract variable %s from path %s: %v",
				varName, jsonPath, err)
		}

		result[varName] = fmt.Sprintf("%v", value)
	}

	return result, nil
}
