package replay

import "time"

// TestRunReport exposes a snapshot of a test-set's replay status.
type TestRunReport struct {
	Total     int
	Passed    int
	Failed    int
	Ignored   int
	Status    bool
	Duration  time.Duration
	TimeTaken string
}

// GetCompleteTestRunReport returns a copy of the current test run report map.
func (r *Replayer) GetCompleteTestRunReport() map[string]TestRunReport {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()

	snapshot := make(map[string]TestRunReport, len(completeTestReport))
	for key, val := range completeTestReport {
		snapshot[key] = TestRunReport{
			Total:     val.total,
			Passed:    val.passed,
			Failed:    val.failed,
			Ignored:   val.ignored,
			Status:    val.status,
			Duration:  val.duration,
			TimeTaken: val.timeTaken,
		}
	}
	return snapshot
}

// GetTestRunTotals returns aggregate totals across all test sets in the current run.
func (r *Replayer) GetTestRunTotals() (total, passed, failed int) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return totalTests, totalTestPassed, totalTestFailed
}
