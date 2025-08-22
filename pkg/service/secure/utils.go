package secure

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// convertExecutionReportToSteps converts testsuite execution results to Step structures for security checks
func (s *SecurityChecker) convertExecutionReportToSteps(report *testsuite.ExecutionReport, executor *testsuite.TSExecutor) []Step {
	steps := make([]Step, 0, len(report.StepsResult))

	for i, stepResult := range report.StepsResult {
		// Skip failed steps - don't run security checks on failed steps
		if stepResult.Status == "failed" {
			s.logger.Info("Skipping security checks for failed step",
				zap.String("stepName", stepResult.StepName),
				zap.String("failureReason", stepResult.FailureReason))
			continue
		}

		// Get the corresponding test step from the testsuite
		if i >= len(executor.Testsuite.Spec.Steps) {
			continue
		}
		testStep := executor.Testsuite.Spec.Steps[i]

		requestHeaders := make(http.Header)
		for key, value := range testStep.Headers {
			interpolatedValue := executor.InterpolateVariables(value)
			requestHeaders.Add(key, interpolatedValue)
		}

		// Get interpolated body
		interpolatedBody := executor.InterpolateVariables(testStep.Body)

		responseHeaders := stepResult.Header
		if responseHeaders == nil {
			responseHeaders = make(http.Header)
		}

		step := Step{
			Endpoint: stepResult.URL,
			StepName: stepResult.StepName,
			StepRequest: StepRequest{
				Method:  stepResult.Method,
				Headers: requestHeaders,
				Body:    interpolatedBody,
			},
			StepResponse: StepResponse{
				StatusCode: stepResult.StatusCode,
				Headers:    responseHeaders,
				Body:       stepResult.Body,
			},
		}

		steps = append(steps, step)
	}

	return steps
}

// readInputWithCancel reads a line of input from stdin with cancellation support
func readInputWithCancel(ctx context.Context, prompt string) (string, error) {
	fmt.Print(prompt)

	inputChan := make(chan string, 1)
	errorChan := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			inputChan <- scanner.Text()
		} else {
			if err := scanner.Err(); err != nil {
				errorChan <- err
			} else {
				errorChan <- fmt.Errorf("EOF")
			}
		}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("operation cancelled")
	case input := <-inputChan:
		return strings.TrimSpace(input), nil
	case err := <-errorChan:
		return "", err
	}
}

func (s *SecurityChecker) isInAllowList(category, value string) bool {
	if s.testsuite.Spec.Security.AllowList.Headers != nil && category == "headers" {
		for _, header := range s.testsuite.Spec.Security.AllowList.Headers {
			if strings.EqualFold(header, value) {
				return true
			}
		}
	}

	if s.testsuite.Spec.Security.AllowList.Keys != nil && category == "keys" {
		for _, key := range s.testsuite.Spec.Security.AllowList.Keys {
			if strings.Contains(strings.ToLower(value), strings.ToLower(key)) {
				return true
			}
		}
	}

	return false
}

func (s *SecurityChecker) printSecurityReport(report *SecurityReport) {
	fmt.Printf("\n🔒 Security Analysis Report\n")
	fmt.Printf("Test Suite: %s\n", report.TestSuite)
	fmt.Printf("Timestamp: %s\n", report.Timestamp)
	fmt.Printf("Total Checks: %d\n", report.TotalChecks)
	fmt.Printf("✅ Passed: %d\n", report.Passed)
	fmt.Printf("❌ Failed: %d\n", report.Failed)
	fmt.Printf("⚠️  Warnings: %d\n\n", report.Warnings)

	// Print results grouped by step
	for i, stepResult := range report.Steps {
		fmt.Printf("=== STEP %d: %s ===\n", i+1, stepResult.StepName)
		fmt.Printf("Method: %s | URL: %s\n", stepResult.StepMethod, stepResult.StepURL)
		fmt.Printf("✅ Passed: %d | ❌ Failed: %d | ⚠️  Warnings: %d\n\n",
			stepResult.Passed, stepResult.Failed, stepResult.Warnings)

		// Group step results by severity
		severities := []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"}
		for _, severity := range severities {
			results := s.filterResultsBySeverity(stepResult.Results, severity)
			if len(results) > 0 {
				fmt.Printf("--- %s SEVERITY ---\n", severity)
				for _, result := range results {
					var status string
					switch result.Status {
					case "passed":
						status = "✅"
					case "failed":
						status = "❌"
					case "warning":
						status = "⚠️"
					default:
						status = "❓" // Unknown status
					}

					fmt.Printf("%s [%s] %s\n", status, result.CheckID, result.CheckName)
					fmt.Printf("   Target: %s | %s\n", result.Target, result.Details)
					if result.Recommendation != "" {
						fmt.Printf("   💡 %s\n", result.Recommendation)
					}
					fmt.Println()
				}
			}
		}
		fmt.Println("================================================")
	}
}

func (s *SecurityChecker) filterResultsBySeverity(results []SecurityResult, severity string) []SecurityResult {
	filtered := make([]SecurityResult, 0)
	for _, result := range results {
		if result.Severity == severity {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func (s *SecurityChecker) getEnabledChecks(ctx context.Context) []SecurityCheck {
	var enabled []SecurityCheck

	switch s.ruleset {
	case "basic":
		// Use built-in checks only
		for _, check := range BuiltInSecurityChecks {
			// Check effective status considering both Status field and disable list
			effectiveStatus := s.getEffectiveStatus(check)
			if effectiveStatus == "disabled" {
				continue
			}
			enabled = append(enabled, check)
		}

	case "custom":
		// Use custom checks only
		customChecks, err := s.loadCustomChecks(ctx)
		if err != nil {
			s.logger.Error("Failed to load custom checks", zap.Error(err))
			// Fallback to built-in checks if custom checks can't be loaded
			for _, check := range BuiltInSecurityChecks {
				effectiveStatus := s.getEffectiveStatus(check)
				if effectiveStatus == "disabled" {
					continue
				}
				enabled = append(enabled, check)
			}
		} else {
			if len(customChecks) == 0 {
				s.logger.Warn("No custom checks found, consider adding some or using 'basic' ruleset")
			}
			for _, check := range customChecks {
				effectiveStatus := s.getEffectiveStatus(check)
				if effectiveStatus == "disabled" {
					continue
				}
				enabled = append(enabled, check)
			}
		}

	default:
		// Default to built-in checks for unknown rulesets
		s.logger.Warn("Unknown ruleset, falling back to basic", zap.String("ruleset", s.ruleset))
		for _, check := range BuiltInSecurityChecks {
			effectiveStatus := s.getEffectiveStatus(check)
			if effectiveStatus == "disabled" {
				continue
			}
			enabled = append(enabled, check)
		}
	}

	s.logger.Debug("Loaded security checks",
		zap.String("ruleset", s.ruleset),
		zap.Int("totalChecks", len(enabled)))

	return enabled
}

// getEffectiveStatus returns the effective status of a check considering both
// the Status field and the disable list in the testsuite
func (s *SecurityChecker) getEffectiveStatus(check SecurityCheck) string {
	// If explicitly disabled in testsuite, it's disabled regardless of Status field
	if s.isCheckDisabled(check.ID) {
		return "disabled"
	}

	// If Status field is set to disabled, it's disabled
	if check.Status == "disabled" {
		return "disabled"
	}

	// Default to enabled
	return "enabled"
}

func (s *SecurityChecker) isCheckDisabled(checkID string) bool {
	if s.testsuite.Spec.Security.Disable != nil {
		for _, disabledID := range s.testsuite.Spec.Security.Disable {
			if fmt.Sprintf("%v", disabledID) == checkID {
				return true
			}
		}
	}
	return false
}

// Helper methods for custom checks file management
func (s *SecurityChecker) saveCustomCheck(ctx context.Context, check SecurityCheck) error {
	customChecks, err := s.loadCustomChecks(ctx)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Check if check with same ID already exists
	for _, existingCheck := range customChecks {
		if existingCheck.ID == check.ID {
			return fmt.Errorf("custom check with ID '%s' already exists", check.ID)
		}
	}

	customChecks = append(customChecks, check)
	return s.saveCustomChecks(ctx, customChecks)
}

func (s *SecurityChecker) loadCustomChecks(ctx context.Context) ([]SecurityCheck, error) {
	customPath := s.getCustomChecksPath(ctx)

	data, err := os.ReadFile(customPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []SecurityCheck{}, nil
		}
		return nil, err
	}

	var customChecks []SecurityCheck
	if err := yaml.Unmarshal(data, &customChecks); err != nil {
		return nil, fmt.Errorf("failed to parse custom checks file: %w", err)
	}

	return customChecks, nil
}

func (s *SecurityChecker) saveCustomChecks(ctx context.Context, checks []SecurityCheck) error {
	customPath := s.getCustomChecksPath(ctx)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(customPath), 0755); err != nil {
		return fmt.Errorf("failed to create custom checks directory: %w", err)
	}

	data, err := yaml.Marshal(checks)
	if err != nil {
		return fmt.Errorf("failed to marshal custom checks: %w", err)
	}

	if err := os.WriteFile(customPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write custom checks file: %w", err)
	}

	return nil
}

func (s *SecurityChecker) getCustomChecksPath(ctx context.Context) string {
	path, _ := ctx.Value("checks-path").(string)

	// CLI Override
	if path != "keploy/secure/custom-checks.yaml" {
		return path
	}

	if s.testsuite.Spec.Security.CustomPath != "" {
		return s.testsuite.Spec.Security.CustomPath
	}

	return path
}
