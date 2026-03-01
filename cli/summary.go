package cli

import "fmt"

// TestSummary holds aggregated test metrics
type TestSummary struct {
	TotalTests      int
	PassedTests     int
	FailedTests     int
	UniqueEndpoints map[string]struct{}
}

// NewSummary creates a new TestSummary
func NewSummary() *TestSummary {
	return &TestSummary{
		UniqueEndpoints: make(map[string]struct{}),
	}
}

// SuccessRate returns percentage of passed tests
func (ts *TestSummary) SuccessRate() float64 {
	if ts.TotalTests == 0 {
		return 0
	}
	return float64(ts.PassedTests) / float64(ts.TotalTests) * 100
}

// Print prints the test execution summary
func (ts *TestSummary) Print() {
	fmt.Println("===================================")
	fmt.Println("        TEST EXECUTION SUMMARY")
	fmt.Println("===================================")
	fmt.Printf("Total Tests Executed : %d\n", ts.TotalTests)
	fmt.Printf("Passed Tests         : %d\n", ts.PassedTests)
	fmt.Printf("Failed Tests         : %d\n", ts.FailedTests)
	fmt.Printf("Success Rate         : %.2f%%\n", ts.SuccessRate())
	fmt.Printf("Unique Endpoints     : %d\n", len(ts.UniqueEndpoints))
	fmt.Println("===================================")
}