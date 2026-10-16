package replay

import "go.keploy.io/server/v3/pkg/models"

// SetEndHook is an optional TestHooks extension: it is told how keploy test
// ended each run of each test set (models.SetEnd), as soon as RunTestSet
// returns, before the run goes on to the next set or stops. A hook that
// judges the run from its tests' results (BeforeTestResult) cannot tell
// from them alone a set that ended APP_HALTED or INTERNAL_ERR after its
// tests that ran passed, or one that failed with no test failing. It is a
// notification: it changes nothing the replayer does.
type SetEndHook interface {
	SetEnd(testSetID string, end models.SetEnd)
}

// noteSetEnd keeps how a final report of the set RunTestSet is running ends
// it, for tellSetEnd: each place RunTestSet writes one (the end of the set,
// a set with no test case to run, a set ignored whole, a set refused when it
// loaded) notes it before writing it.
func (r *Replayer) noteSetEnd(rep *models.TestReport) {
	r.setEnd = &models.SetEnd{Status: models.TestSetStatus(rep.Status), Why: rep.FailureReason}
}

// tellSetEnd tells the hooks, when they take it (SetEndHook), how a run of
// testSetID ended: status, and why, from the final report this run of the set
// noted when it says that status (noteSetEnd), else from err, the error that
// ended it first: one before any final report, or the failure to write the
// one at the set's end, which ends the set INTERNAL_ERR whatever it said.
func (r *Replayer) tellSetEnd(testSetID string, status models.TestSetStatus, err error) {
	h, ok := r.hookImpl.(SetEndHook)
	if !ok {
		return
	}
	end := models.SetEnd{Status: status}
	switch noted := r.setEnd; {
	case noted != nil && noted.Status == status:
		end.Why = noted.Why
	case err != nil:
		end.Why = err.Error()
	}
	h.SetEnd(testSetID, end)
}
