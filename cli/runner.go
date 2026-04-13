package cli

import "fmt"

// TestResult represents result of a single test case
type TestResult struct {
	TestName string
	Passed   bool
	Endpoint string
}

// RunTests processes test results and prints execution summary
func RunTests(testResults []TestResult) {
	summary := NewSummary()

	fmt.Println("Running tests...\n")

	for _, result := range testResults {
		summary.TotalTests++

		if result.Passed {
			summary.PassedTests++
		} else {
			summary.FailedTests++
		}

		// Track unique endpoint (avoid empty string)
		if result.Endpoint != "" {
			summary.UniqueEndpoints[result.Endpoint] = struct{}{}
		}

		status := "FAILED"
		if result.Passed {
			status = "PASSED"
		}

		fmt.Printf(
			"Test: %-25s | Endpoint: %-20s | Status: %s\n",
			result.TestName,
			result.Endpoint,
			status,
		)
	}

	fmt.Println()
	summary.Print()
}