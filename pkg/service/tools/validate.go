package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Severity represents the severity level of a validation issue
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// ValidationCategory represents the type of validation
type ValidationCategory string

const (
	CategoryStructural ValidationCategory = "structural"
	CategorySemantic   ValidationCategory = "semantic"
	CategoryMock       ValidationCategory = "mock"
	CategoryHTTP       ValidationCategory = "http"
	CategoryGRPC       ValidationCategory = "grpc"
)

// ValidationIssue represents a single validation problem
type ValidationIssue struct {
	RuleID     string             `json:"rule_id"`
	Severity   Severity           `json:"severity"`
	Category   ValidationCategory `json:"category"`
	Message    string             `json:"message"`
	TestSetID  string             `json:"test_set_id"`
	TestCaseID string             `json:"test_case_id,omitempty"`
	Field      string             `json:"field,omitempty"`
	Suggestion string             `json:"suggestion,omitempty"`
}

// ValidationResult holds the complete validation output
type ValidationResult struct {
	TotalTestSets  int               `json:"total_test_sets"`
	TotalTestCases int               `json:"total_test_cases"`
	ValidTestCases int               `json:"valid_test_cases"`
	Issues         []ValidationIssue `json:"issues"`
	Summary        struct {
		Errors   int  `json:"errors"`
		Warnings int  `json:"warnings"`
		Info     int  `json:"info"`
		Passed   bool `json:"passed"`
	} `json:"summary"`
}

// Validate performs validation on test cases
func (t *Tools) Validate(ctx context.Context) error {
	t.logger.Info("Starting validation process...")

	result := &ValidationResult{}

	// Get test sets to validate
	testSets := t.extractTestSetIDs()
	if len(testSets) == 0 {
		var err error
		testSets, err = t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			t.logger.Error("Failed to get test sets", zap.Error(err))
			return fmt.Errorf("failed to get test sets: %w", err)
		}
		t.logger.Info("No test sets specified, processing all test sets", zap.Int("count", len(testSets)))
	} else {
		t.logger.Info("Processing specified test sets", zap.Strings("testSets", testSets))
	}

	if len(testSets) == 0 {
		t.logger.Info("No test sets found to validate")
		fmt.Println("\n⚠️  No test sets found in the keploy directory.")
		fmt.Println("   Run 'keploy record' first to generate test cases.")
		return nil
	}

	result.TotalTestSets = len(testSets)

	for _, testSetID := range testSets {
		select {
		case <-ctx.Done():
			t.logger.Info("Validate process cancelled by context")
			return ctx.Err()
		default:
		}

		t.logger.Info("Validating test set", zap.String("testSetID", testSetID))

		testCases, err := t.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			result.Issues = append(result.Issues, ValidationIssue{
				RuleID:    "S001",
				Severity:  SeverityError,
				Category:  CategoryStructural,
				Message:   fmt.Sprintf("Failed to read test set: %v", err),
				TestSetID: testSetID,
			})
			continue
		}

		result.TotalTestCases += len(testCases)

		for _, tc := range testCases {
			issues := t.validateTestCase(ctx, testSetID, tc)
			result.Issues = append(result.Issues, issues...)

			hasError := false
			for _, issue := range issues {
				if issue.Severity == SeverityError {
					hasError = true
					break
				}
			}
			if !hasError {
				result.ValidTestCases++
			}
		}

		// Validate mocks
		mockIssues := t.validateMocks(ctx, testSetID)
		result.Issues = append(result.Issues, mockIssues...)
	}

	// Calculate summary
	for _, issue := range result.Issues {
		switch issue.Severity {
		case SeverityError:
			result.Summary.Errors++
		case SeverityWarning:
			result.Summary.Warnings++
		case SeverityInfo:
			result.Summary.Info++
		}
	}
	result.Summary.Passed = result.Summary.Errors == 0

	// Print results
	t.printValidationResults(result)

	if !result.Summary.Passed {
		return fmt.Errorf("validation failed with %d errors", result.Summary.Errors)
	}

	t.logger.Info("Validation process completed successfully")
	return nil
}

func (t *Tools) validateTestCase(_ context.Context, testSetID string, tc *models.TestCase) []ValidationIssue {
	var issues []ValidationIssue

	// S002: Version validation
	if tc.Version == "" {
		issues = append(issues, ValidationIssue{
			RuleID:     "S002",
			Severity:   SeverityError,
			Category:   CategoryStructural,
			Message:    "Missing required field: version",
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "version",
			Suggestion: "Add 'version: api.keploy.io/v1beta1' to the test case",
		})
	} else if tc.Version != models.V1Beta1 {
		issues = append(issues, ValidationIssue{
			RuleID:     "S005",
			Severity:   SeverityWarning,
			Category:   CategoryStructural,
			Message:    fmt.Sprintf("Unknown version: %s", tc.Version),
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "version",
		})
	}

	// S003: Kind validation
	if tc.Kind == "" {
		issues = append(issues, ValidationIssue{
			RuleID:     "S003",
			Severity:   SeverityError,
			Category:   CategoryStructural,
			Message:    "Missing required field: kind",
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "kind",
			Suggestion: "Add 'kind: HTTP' or 'kind: GRPC_EXPORT'",
		})
	}

	// S004: Name validation
	if tc.Name == "" {
		issues = append(issues, ValidationIssue{
			RuleID:     "S004",
			Severity:   SeverityError,
			Category:   CategoryStructural,
			Message:    "Missing required field: name",
			TestSetID:  testSetID,
			TestCaseID: "(unnamed)",
			Field:      "name",
		})
	}

	// Kind-specific validation
	switch tc.Kind {
	case models.HTTP:
		httpIssues := t.validateHTTPTestCase(testSetID, tc)
		issues = append(issues, httpIssues...)
	case models.GRPC_EXPORT:
		grpcIssues := t.validateGRPCTestCase(testSetID, tc)
		issues = append(issues, grpcIssues...)
	}

	return issues
}

func (t *Tools) validateHTTPTestCase(testSetID string, tc *models.TestCase) []ValidationIssue {
	var issues []ValidationIssue

	// H001: HTTP method validation
	validMethods := map[models.Method]bool{
		"GET": true, "POST": true, "PUT": true, "DELETE": true,
		"PATCH": true, "HEAD": true, "OPTIONS": true, "TRACE": true,
	}
	if tc.HTTPReq.Method != "" && !validMethods[tc.HTTPReq.Method] {
		issues = append(issues, ValidationIssue{
			RuleID:     "H001",
			Severity:   SeverityError,
			Category:   CategoryHTTP,
			Message:    fmt.Sprintf("Invalid HTTP method: %s", tc.HTTPReq.Method),
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "http_req.method",
			Suggestion: "Use a valid HTTP method: GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS, TRACE",
		})
	}

	// H002: URL validation
	if tc.HTTPReq.URL == "" {
		issues = append(issues, ValidationIssue{
			RuleID:     "H002",
			Severity:   SeverityError,
			Category:   CategoryHTTP,
			Message:    "HTTP request URL is empty",
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "http_req.url",
		})
	} else {
		if _, err := url.Parse(tc.HTTPReq.URL); err != nil {
			issues = append(issues, ValidationIssue{
				RuleID:     "H002",
				Severity:   SeverityError,
				Category:   CategoryHTTP,
				Message:    fmt.Sprintf("Invalid URL format: %v", err),
				TestSetID:  testSetID,
				TestCaseID: tc.Name,
				Field:      "http_req.url",
			})
		}
	}

	// H003: Status code validation
	if tc.HTTPResp.StatusCode < 100 || tc.HTTPResp.StatusCode > 599 {
		issues = append(issues, ValidationIssue{
			RuleID:     "H003",
			Severity:   SeverityError,
			Category:   CategoryHTTP,
			Message:    fmt.Sprintf("Invalid HTTP status code: %d", tc.HTTPResp.StatusCode),
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "http_resp.status_code",
			Suggestion: "Status code must be between 100-599",
		})
	}

	// H006: JSON body validation for request
	contentType := tc.HTTPReq.Header["Content-Type"]
	if strings.Contains(contentType, "application/json") && tc.HTTPReq.Body != "" {
		if !json.Valid([]byte(tc.HTTPReq.Body)) {
			issues = append(issues, ValidationIssue{
				RuleID:     "H006",
				Severity:   SeverityError,
				Category:   CategoryHTTP,
				Message:    "Request body is not valid JSON but Content-Type is application/json",
				TestSetID:  testSetID,
				TestCaseID: tc.Name,
				Field:      "http_req.body",
				Suggestion: "Fix the JSON syntax in the request body",
			})
		}
	}

	// Validate response body JSON
	respContentType := tc.HTTPResp.Header["Content-Type"]
	if strings.Contains(respContentType, "application/json") && tc.HTTPResp.Body != "" {
		if !json.Valid([]byte(tc.HTTPResp.Body)) {
			issues = append(issues, ValidationIssue{
				RuleID:     "H006",
				Severity:   SeverityError,
				Category:   CategoryHTTP,
				Message:    "Response body is not valid JSON but Content-Type is application/json",
				TestSetID:  testSetID,
				TestCaseID: tc.Name,
				Field:      "http_resp.body",
				Suggestion: "Fix the JSON syntax in the response body",
			})
		}
	}

	return issues
}

func (t *Tools) validateGRPCTestCase(testSetID string, tc *models.TestCase) []ValidationIssue {
	var issues []ValidationIssue

	// G001: Method name validation - gRPC method is stored in PseudoHeaders[":path"]
	grpcPath := ""
	if tc.GrpcReq.Headers.PseudoHeaders != nil {
		grpcPath = tc.GrpcReq.Headers.PseudoHeaders[":path"]
	}

	if grpcPath == "" {
		issues = append(issues, ValidationIssue{
			RuleID:     "G001",
			Severity:   SeverityError,
			Category:   CategoryGRPC,
			Message:    "gRPC method path is empty",
			TestSetID:  testSetID,
			TestCaseID: tc.Name,
			Field:      "grpc_req.headers.pseudo_headers.:path",
			Suggestion: "Specify the gRPC method path in format: /package.Service/Method",
		})
	}

	return issues
}

func (t *Tools) validateMocks(_ context.Context, testSetID string) []ValidationIssue {
	var issues []ValidationIssue

	mockDir := filepath.Join(t.config.Path, testSetID, "mocks")
	if _, err := os.Stat(mockDir); os.IsNotExist(err) {
		// No mocks directory - not an error
		return issues
	}

	entries, err := os.ReadDir(mockDir)
	if err != nil {
		issues = append(issues, ValidationIssue{
			RuleID:    "M001",
			Severity:  SeverityError,
			Category:  CategoryMock,
			Message:   fmt.Sprintf("Failed to read mocks directory: %v", err),
			TestSetID: testSetID,
		})
		return issues
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		mockPath := filepath.Join(mockDir, entry.Name())
		data, err := os.ReadFile(mockPath)
		if err != nil {
			issues = append(issues, ValidationIssue{
				RuleID:    "M002",
				Severity:  SeverityError,
				Category:  CategoryMock,
				Message:   fmt.Sprintf("Failed to read mock file %s: %v", entry.Name(), err),
				TestSetID: testSetID,
			})
			continue
		}

		// Try to parse as YAML
		var mockDoc interface{}
		if err := yaml.Unmarshal(data, &mockDoc); err != nil {
			issues = append(issues, ValidationIssue{
				RuleID:    "M002",
				Severity:  SeverityError,
				Category:  CategoryMock,
				Message:   fmt.Sprintf("Invalid YAML in mock file %s: %v", entry.Name(), err),
				TestSetID: testSetID,
			})
		}
	}

	return issues
}

func (t *Tools) printValidationResults(result *ValidationResult) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    VALIDATION RESULTS                        ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Test Sets:  %-48d║\n", result.TotalTestSets)
	fmt.Printf("║  Test Cases: %-3d (Valid: %-3d)                                ║\n", result.TotalTestCases, result.ValidTestCases)
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")

	if result.Summary.Passed {
		fmt.Println("║  ✅ VALIDATION PASSED                                        ║")
	} else {
		fmt.Println("║  ❌ VALIDATION FAILED                                        ║")
	}

	fmt.Printf("║  Errors: %-3d  |  Warnings: %-3d  |  Info: %-3d                 ║\n",
		result.Summary.Errors, result.Summary.Warnings, result.Summary.Info)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	if len(result.Issues) > 0 {
		fmt.Println()
		fmt.Println("Issues Found:")
		fmt.Println("─────────────")

		for _, issue := range result.Issues {
			icon := "ℹ️ "
			switch issue.Severity {
			case SeverityError:
				icon = "❌"
			case SeverityWarning:
				icon = "⚠️ "
			}

			fmt.Printf("%s [%s] %s\n", icon, issue.RuleID, issue.Message)
			fmt.Printf("   Test Set: %s", issue.TestSetID)
			if issue.TestCaseID != "" {
				fmt.Printf(" | Test Case: %s", issue.TestCaseID)
			}
			if issue.Field != "" {
				fmt.Printf(" | Field: %s", issue.Field)
			}
			fmt.Println()
			if issue.Suggestion != "" {
				fmt.Printf("   💡 Suggestion: %s\n", issue.Suggestion)
			}
			fmt.Println()
		}
	}
}
