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
func (r *Replayer) GetCompleteTestRunReport() map[string]TestRunReport {
	r.completeTestReportMu.RLock()
	defer r.completeTestReportMu.RUnlock()

	snapshot := make(map[string]TestRunReport, len(r.completeTestReport))
	for key, val := range r.completeTestReport {
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
func (r *Replayer) GetTestRunTotals() (total, passed, failed int) {
	r.completeTestReportMu.RLock()
	defer r.completeTestReportMu.RUnlock()
	return r.totalTests, r.totalTestPassed, r.totalTestFailed
}
