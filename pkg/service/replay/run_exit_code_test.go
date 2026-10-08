package replay

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// The rule both ways out of Start use to record a failing run.
//
// It exists as a function because the two used to disagree: the ordinary end
// of the loop set utils.ErrCode = 1 for a red run, and the user-abort case
// returned with no code at all -- so a `keploy test` whose suite had already
// gone red and was then interrupted exited 0, and CI recorded a pass.
//
// What this covers is the RULE. Start's loop has no test seam today (it needs
// a live testDB, reportDB, instrumentation and hooks), so that the abort case
// calls this is not covered here; the enterprise side pins the other half of
// the path in TestRunRoot_InterruptKeepsACodeAlreadyArmed.
func TestArmRunExitCode(t *testing.T) {
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })

	utils.ErrCode = 0
	armRunExitCode(nil, true, 0)
	if utils.ErrCode != 0 {
		t.Errorf("a run where every test passed exited %d", utils.ErrCode)
	}

	utils.ErrCode = 0
	armRunExitCode(nil, false, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a run with a failed test set exited %d, want 1", utils.ErrCode)
	}

	// The case the abort path actually hits. testRunResult is folded in AFTER
	// the switch the abort returns from, so it is still true when the set
	// being interrupted is the one with the failures in it -- which is every
	// single-test-set suite, the common shape. Only the failure count knows.
	utils.ErrCode = 0
	armRunExitCode(nil, true, 3)
	if utils.ErrCode != 1 {
		t.Errorf("a run interrupted with 3 tests already failed exited %d, want 1", utils.ErrCode)
	}

	// ...and the other way round, so neither signal can be dropped: a set can
	// fail without any test failing (the app never came up, no tests to run).
	utils.ErrCode = 0
	armRunExitCode(nil, false, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a set that failed with no failing test exited %d, want 1", utils.ErrCode)
	}

	// A run that never reached the loop at all -- no tests recorded, the
	// report store unreadable, the agent never healthy. `keploy test` throws
	// that error away so cobra will not print usage over a failure already
	// reported, which left nothing to carry it to the exit status.
	utils.ErrCode = 0
	armRunExitCode(errors.New("no test sets found"), true, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a run that never started exited %d, want 1", utils.ErrCode)
	}

	// A code already armed is MORE specific than this one -- a wrapped
	// runner's own status, or one of Keploy's own (utils/exitcodes.go) --
	// and overwriting it with a flat 1 tells the caller the tests failed
	// when the truth was a privilege failure or a refused session.
	utils.ErrCode = 5
	armRunExitCode(nil, false, 9)
	if utils.ErrCode != 5 {
		t.Errorf("an armed 5 became %d", utils.ErrCode)
	}
}
