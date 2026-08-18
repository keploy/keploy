package telemetry

import "testing"

// TestCategorizeStopReason pins the mapping against the exact raw stopReason
// strings set in the record (pkg/service/record/record.go) and replay
// (pkg/service/replay/replay.go) flows. If one of those strings is reworded,
// this test fails loudly instead of silently degrading a live funnel to
// "other".
func TestCategorizeStopReason(t *testing.T) {
	cases := map[string]string{
		// clean stops
		"":                              StopReasonCompleted,
		"replay completed successfully": StopReasonCompleted,

		// record.go
		"failed to get new test-set id":                                StopReasonTestsetLookup,
		"failed setting up the environment":                            StopReasonSetupFailed,
		"failed to get data frames":                                    StopReasonNoTrafficFrames,
		"error in running the user application, hence stopping keploy": StopReasonAppError,
		"user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected": StopReasonAppExited,
		"internal error occurred while hooking into the application, hence stopping keploy":                                               StopReasonHookError,
		"keploy test mode binary stopped, hence stopping keploy":                                                                          StopReasonAppExited,
		"unknown error received from application, hence stopping keploy":                                                                  StopReasonAppError,
		"error while inserting test case into db, hence stopping keploy":                                                                  StopReasonDBWriteError,
		"error while inserting mock into db, hence stopping keploy":                                                                       StopReasonDBWriteError,

		// replay.go (these interpolate %v in production; prefix must still match)
		"no test sets found":                   StopReasonNoTestsets,
		"failed to get all test set ids: boom": StopReasonTestsetLookup,
		"failed to get next test run id: boom": StopReasonSetupFailed,
		"failed to instrument: boom":           StopReasonInstrumentFailed,
		"failed to run before test hook: boom": StopReasonHookError,
		"failed to run test set: boom":         StopReasonTestsetRunFailed,

		// unknown → other
		"something totally unexpected": StopReasonOther,
	}

	for raw, want := range cases {
		if got := CategorizeStopReason(raw); got != want {
			t.Errorf("CategorizeStopReason(%q) = %q, want %q", raw, got, want)
		}
	}
}
