package replay

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

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

// SetCallsHook is an optional TestHooks extension, under the proven switch:
// it is told, with each run of each test set's end (SetEndHook's moment), the
// calls no mock answered that the set's end charged to the set, no test (the
// final report's UnclaimedCalls): read at the set's end but made before its
// last test's misses were read (filed late), or at a time keploy cannot
// place; none when there were none. A call made outside every test's window
// is not one of them: it is background work (TestReport.BackgroundCalls),
// or, when its recorded answer reaches a test, that test's miss. Each
// carries what its parser found of it, as a test's own misses do in its result
// (UnmatchedCall.NotOwned, ClosestMock): what record's judging reads to bake
// a recording made elsewhere into the worker scope. It is a notification: it
// changes nothing the replayer does.
type SetCallsHook interface {
	SetCalls(testSetID string, calls []models.UnmatchedCall)
}

// SetSharesHook is an optional TestHooks extension, under the proven switch:
// it is told, with each run of each test set's end (SetEndHook's moment),
// what keploy record's full round settled of the recordings it shared between
// the tests whose requests were open together while they were recorded
// (models.SetShares): which test made each call, and where each recording it
// could not give to one test goes. Only a replay that shares
// (config.Test.ShareConcurrent) tells it; the hook writes it onto
// mappings.yaml (keploy record's pin step). It is a notification: it changes
// nothing the replayer does.
type SetSharesHook interface {
	SetShares(testSetID string, shares models.SetShares)
}

// SetAnswersHook is an optional TestHooks extension, under the proven switch:
// with each run of each set's end (SetCallsHook's moment) it is told the
// answered calls that end charged to the set, no test (TestReport.SetAnswers):
// made before its first test or after its last. A notification only.
type SetAnswersHook interface {
	SetAnswers(testSetID string, answers []models.UnmatchedCall)
}

// noteSetEnd keeps how a final report of the set RunTestSet is running ends
// it, for tellSetEnd: each place RunTestSet writes one (the end of the set,
// a set with no test case to run, a set ignored whole, a set refused when it
// loaded) notes it before writing it.
func (r *Replayer) noteSetEnd(rep *models.TestReport) {
	r.setEnd = &models.SetEnd{Status: models.TestSetStatus(rep.Status), Why: rep.FailureReason}
	r.setEndCalls = rep.UnclaimedCalls
	r.setEndAnswers = rep.SetAnswers
}

// tellSetEnd tells the hooks, when they take it (SetEndHook), how a run of
// testSetID ended: status, and why, from the final report this run of the set
// noted when it says that status (noteSetEnd), else from err, the error that
// ended it first: one before any final report, or the failure to write the
// one at the set's end, which ends the set INTERNAL_ERR whatever it said.
func (r *Replayer) tellSetEnd(testSetID string, status models.TestSetStatus, err error) {
	if h, ok := r.hookImpl.(SetSharesHook); ok {
		if shares, sharing := r.exact.setShares(status); sharing {
			h.SetShares(testSetID, shares)
		}
	}
	if h, ok := r.hookImpl.(SetCallsHook); ok {
		var calls []models.UnmatchedCall
		if noted := r.setEnd; noted != nil && noted.Status == status {
			calls = append(calls, r.setEndCalls...)
		}
		h.SetCalls(testSetID, calls)
	}
	if h, ok := r.hookImpl.(SetAnswersHook); ok && provenswitch.On() {
		var answers []models.UnmatchedCall
		if noted := r.setEnd; noted != nil && noted.Status == status {
			answers = append(answers, r.setEndAnswers...)
		}
		h.SetAnswers(testSetID, answers)
	}
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

// UncheckedHook is an optional TestHooks extension: a hook that changes what
// a test set's run reads (decrypts its recordings, say) tells keploy when it
// could not, so that some of the set's tests could not be checked, and why.
// Under the proven switch a set that would be PASSED or FAILED, or is not
// judged already, is not judged with that reason; any other (the switch
// off, or a set that halted, was stopped or faulted) keeps its status, and
// its report gives the reason first. Either way keploy writes nothing of
// that run back to the recording: no mock prune, no mappings.yaml, nothing
// its learn step learned.
type UncheckedHook interface {
	// Unchecked is why this run of testSetID could not check every test it
	// ran, "" when it could. It is asked once per run of the set, as that
	// run's report is written: a set run again (--max-flaky-checks,
	// --must-pass) is asked again, after its next run, so the answer is
	// that run's alone, never an earlier run's. A run that ends before its
	// report (an error that stops keploy test) is not asked. It must not
	// block: it is asked on the set's end, with no deadline.
	Unchecked(testSetID string) string
}

// hookUnchecked is what the hooks say keeps testSetID's run from checking
// its tests (UncheckedHook), "" when they take no such hook or say nothing.
func (r *Replayer) hookUnchecked(testSetID string) string {
	h, ok := r.hookImpl.(UncheckedHook)
	if !ok {
		return ""
	}
	return strings.TrimSpace(h.Unchecked(testSetID))
}

// withUnchecked puts why first in a report's failure reason (a set that
// keeps its status, UncheckedHook): the reason the run could not check its
// tests explains the rest.
func withUnchecked(why, reason string) string {
	switch {
	case why == "":
		return reason
	case reason == "":
		return "keploy could not check every test: " + why
	}
	return "keploy could not check every test: " + why + "; " + reason
}
