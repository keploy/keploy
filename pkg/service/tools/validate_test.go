package tools

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestValidateHTTPTestCase(t *testing.T) {
	tests := []struct {
		name       string
		testCase   *models.TestCase
		wantErrors int
	}{
		{
			name: "valid HTTP test case",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-1",
				HTTPReq: models.HTTPReq{
					Method: "GET",
					URL:    "/api/users",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
				},
			},
			wantErrors: 0,
		},
		{
			name: "invalid HTTP method",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-2",
				HTTPReq: models.HTTPReq{
					Method: "INVALID",
					URL:    "/api/users",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
				},
			},
			wantErrors: 1,
		},
		{
			name: "empty URL",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-3",
				HTTPReq: models.HTTPReq{
					Method: "GET",
					URL:    "",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
				},
			},
			wantErrors: 1,
		},
		{
			name: "invalid status code - too high",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-4",
				HTTPReq: models.HTTPReq{
					Method: "GET",
					URL:    "/api/users",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 999,
				},
			},
			wantErrors: 1,
		},
		{
			name: "invalid status code - too low",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-5",
				HTTPReq: models.HTTPReq{
					Method: "GET",
					URL:    "/api/users",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 50,
				},
			},
			wantErrors: 1,
		},
		{
			name: "invalid JSON request body",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-6",
				HTTPReq: models.HTTPReq{
					Method: "POST",
					URL:    "/api/users",
					Header: map[string]string{"Content-Type": "application/json"},
					Body:   "{invalid json}",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
				},
			},
			wantErrors: 1,
		},
		{
			name: "valid JSON request body",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-7",
				HTTPReq: models.HTTPReq{
					Method: "POST",
					URL:    "/api/users",
					Header: map[string]string{"Content-Type": "application/json"},
					Body:   `{"name": "test"}`,
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
				},
			},
			wantErrors: 0,
		},
		{
			name: "invalid JSON response body",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.HTTP,
				Name:    "test-8",
				HTTPReq: models.HTTPReq{
					Method: "GET",
					URL:    "/api/users",
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "application/json"},
					Body:       "{not valid json",
				},
			},
			wantErrors: 1,
		},
	}

	// Create a minimal Tools instance for testing
	tools := &Tools{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := tools.validateHTTPTestCase("test-set-1", tt.testCase)
			errorCount := 0
			for _, issue := range issues {
				if issue.Severity == SeverityError {
					errorCount++
				}
			}
			if errorCount != tt.wantErrors {
				t.Errorf("validateHTTPTestCase() got %d errors, want %d", errorCount, tt.wantErrors)
				for _, issue := range issues {
					t.Logf("  Issue: [%s] %s", issue.RuleID, issue.Message)
				}
			}
		})
	}
}

func TestValidateTestCase_MissingFields(t *testing.T) {
	tools := &Tools{}

	tc := &models.TestCase{
		// All fields empty
	}

	issues := tools.validateTestCase(context.TODO(), "test-set-1", tc)

	// Should have errors for: version, kind, name
	errorCount := 0
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			errorCount++
		}
	}

	if errorCount < 3 {
		t.Errorf("Expected at least 3 errors for missing fields, got %d", errorCount)
		for _, issue := range issues {
			t.Logf("  Issue: [%s] %s (severity: %s)", issue.RuleID, issue.Message, issue.Severity)
		}
	}
}

func TestValidateTestCase_ValidTestCase(t *testing.T) {
	tools := &Tools{}

	tc := &models.TestCase{
		Version: models.V1Beta1,
		Kind:    models.HTTP,
		Name:    "test-valid",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "/api/health",
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
		},
	}

	issues := tools.validateTestCase(context.TODO(), "test-set-1", tc)

	errorCount := 0
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			errorCount++
		}
	}

	if errorCount != 0 {
		t.Errorf("Expected 0 errors for valid test case, got %d", errorCount)
		for _, issue := range issues {
			t.Logf("  Issue: [%s] %s", issue.RuleID, issue.Message)
		}
	}
}

func TestValidateGRPCTestCase(t *testing.T) {
	tools := &Tools{}

	tests := []struct {
		name       string
		testCase   *models.TestCase
		wantErrors int
	}{
		{
			name: "valid gRPC test case",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.GRPC_EXPORT,
				Name:    "grpc-test-1",
				GrpcReq: models.GrpcReq{
					Headers: models.GrpcHeaders{
						PseudoHeaders: map[string]string{
							":path": "/package.Service/Method",
						},
					},
				},
			},
			wantErrors: 0,
		},
		{
			name: "empty gRPC method path",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.GRPC_EXPORT,
				Name:    "grpc-test-2",
				GrpcReq: models.GrpcReq{
					Headers: models.GrpcHeaders{
						PseudoHeaders: map[string]string{},
					},
				},
			},
			wantErrors: 1,
		},
		{
			name: "nil PseudoHeaders",
			testCase: &models.TestCase{
				Version: models.V1Beta1,
				Kind:    models.GRPC_EXPORT,
				Name:    "grpc-test-3",
				GrpcReq: models.GrpcReq{
					Headers: models.GrpcHeaders{},
				},
			},
			wantErrors: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := tools.validateGRPCTestCase("test-set-1", tt.testCase)
			errorCount := 0
			for _, issue := range issues {
				if issue.Severity == SeverityError {
					errorCount++
				}
			}
			if errorCount != tt.wantErrors {
				t.Errorf("validateGRPCTestCase() got %d errors, want %d", errorCount, tt.wantErrors)
				for _, issue := range issues {
					t.Logf("  Issue: [%s] %s", issue.RuleID, issue.Message)
				}
			}
		})
	}
}

func TestValidationSeverity(t *testing.T) {
	// Test that unknown version generates a warning, not an error
	tools := &Tools{}

	tc := &models.TestCase{
		Version: "unknown-version",
		Kind:    models.HTTP,
		Name:    "test-version",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "/api/users",
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
		},
	}

	issues := tools.validateTestCase(context.TODO(), "test-set-1", tc)

	warningCount := 0
	for _, issue := range issues {
		if issue.Severity == SeverityWarning && issue.RuleID == "S005" {
			warningCount++
		}
	}

	if warningCount != 1 {
		t.Errorf("Expected 1 warning for unknown version, got %d", warningCount)
	}
}
