package telemetry

import "strings"

// Stop-reason categories: a small, stable, low-cardinality enum describing
// WHY a record/replay session ended. These are emitted only on graceful
// stops (via RecordSessionCompleted / TestRunAborted) so dashboards can see
// where users get stuck. Hard crashes/panics are intentionally NOT modelled
// here — those are Sentry's job.
//
// Keep this list short and stable: it feeds a LowCardinality ClickHouse
// column and a Superset funnel. Add a new code only for a genuinely new
// class of graceful stop, never for one-off error text.
const (
	StopReasonCompleted        = "completed"             // clean exit / user Ctrl+C / record timer elapsed
	StopReasonSetupFailed      = "setup_failed"          // environment/instrumentation setup failed
	StopReasonNoTrafficFrames  = "no_traffic_frames"     // agent/proxy never established data frames
	StopReasonTestsetLookup    = "testset_lookup_failed" // could not resolve/list test-set id(s)
	StopReasonNoTestsets       = "no_testsets"           // `keploy test` run before anything was recorded
	StopReasonInstrumentFailed = "instrument_failed"     // replay instrumentation failed
	StopReasonHookError        = "hook_error"            // before-test / app-hook failure
	StopReasonAppError         = "app_error"             // user application errored while running
	StopReasonAppExited        = "app_exited"            // user application terminated unexpectedly
	StopReasonDBWriteError     = "db_write_error"        // failed persisting test case / mock
	StopReasonTestsetRunFailed = "testset_run_failed"    // a test set failed to run
	StopReasonOther            = "other"                 // graceful stop with an uncategorized reason
)

// CategorizeStopReason maps a free-form stopReason string (as set in the
// record/replay flows) to one of the stable categories above.
//
// The raw strings are deliberately NOT emitted: several interpolate the
// underlying error via %v, which would explode column cardinality and
// duplicate what Sentry already captures. An empty reason means a clean
// stop and maps to "completed".
func CategorizeStopReason(raw string) string {
	r := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case r == "" || strings.Contains(r, "completed successfully"):
		return StopReasonCompleted
	case strings.Contains(r, "no test sets found"):
		return StopReasonNoTestsets
	case strings.Contains(r, "test-set id") || strings.Contains(r, "all test set ids"):
		return StopReasonTestsetLookup
	case strings.Contains(r, "setting up the environment") || strings.Contains(r, "next test run id"):
		return StopReasonSetupFailed
	case strings.Contains(r, "data frames"):
		return StopReasonNoTrafficFrames
	case strings.Contains(r, "instrument"):
		return StopReasonInstrumentFailed
	case strings.Contains(r, "hook"):
		return StopReasonHookError
	case strings.Contains(r, "inserting test case") || strings.Contains(r, "inserting mock"):
		return StopReasonDBWriteError
	case strings.Contains(r, "terminated unexpectedly") || strings.Contains(r, "binary stopped"):
		return StopReasonAppExited
	case strings.Contains(r, "running the user application") || strings.Contains(r, "unknown error received from application"):
		return StopReasonAppError
	case strings.Contains(r, "run test set"):
		return StopReasonTestsetRunFailed
	default:
		return StopReasonOther
	}
}
