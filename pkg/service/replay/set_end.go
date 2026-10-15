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

// tellSetEnd tells the hooks, when they take it (SetEndHook), how a run of
// testSetID ended: status, and why, from the set's final report when this run
// wrote one with that status, else from err, the error that ended it first.
func (r *Replayer) tellSetEnd(testSetID string, status models.TestSetStatus, err error) {
	h, ok := r.hookImpl.(SetEndHook)
	if !ok {
		return
	}
	end := models.SetEnd{Status: status}
	switch rep := r.setReport; {
	case rep != nil && rep.TestSet == testSetID && rep.Status == string(status):
		end.Why = rep.FailureReason
	case err != nil:
		end.Why = err.Error()
	}
	h.SetEnd(testSetID, end)
}
