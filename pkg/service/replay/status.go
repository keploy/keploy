package replay

import "time"

// TestRunReport exposes a snapshot of a test-set's replay status.
type TestRunReport struct {
	Total     int
	Passed    int
	Failed    int
	Obsolete  int
	Ignored   int
	Status    bool
	Duration  time.Duration
	TimeTaken string
}

// GetCompleteTestRunReport returns a copy of the current test run report map.
func GetCompleteTestRunReport() map[string]TestRunReport {
	completeTestReportMu.RLock()
	defer completeTestReportMu.RUnlock()

	snapshot := make(map[string]TestRunReport, len(completeTestReport))
	for key, val := range completeTestReport {
		snapshot[key] = TestRunReport{
			Total:     val.total,
			Passed:    val.passed,
			Failed:    val.failed,
			Obsolete:  val.obsolete,
			Ignored:   val.ignored,
			Status:    val.status,
			Duration:  val.duration,
			TimeTaken: val.timeTaken,
		}
	}
	return snapshot
}

// GetTestRunTotals returns aggregate totals across all test sets in the current run.
func GetTestRunTotals() (total, passed, failed int) {
	completeTestReportMu.RLock()
	defer completeTestReportMu.RUnlock()
	return totalTests, totalTestPassed, totalTestFailed
}
