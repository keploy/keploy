package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// beforeTestCaseRunFailed is what keploy says when a TestCaseMutator's
// BeforeTestCaseRun fails on a test case. The test case is not failed here:
// it goes on to the SimulateRequest hook, which sends it only if it allows
// it. Keploy Enterprise's refuses a test case whose BeforeTestCaseRun failed
// on a sealed value; that error then fails the test case, and the set goes
// on to its next test case.
const beforeTestCaseRunFailed = "the BeforeTestCaseRun hook failed on this test case (the error says why): the test case goes to the SimulateRequest hook as the failed hook left it, and is sent only if that hook allows it; if it does not, the test case fails with that hook's error and the test set goes on"

// sayBeforeTestCaseRunFailed says that BeforeTestCaseRun failed on tc, with
// the hook's error.
func (r *Replayer) sayBeforeTestCaseRunFailed(err error, tc *models.TestCase) {
	utils.LogError(r.logger, err, beforeTestCaseRunFailed,
		zap.String("testcase", tc.Name),
		zap.String("next_step", "check the BeforeTestCaseRun implementation and any external dependencies it uses (e.g. KMS, auth, network)"))
}

// mockRefusal is the refusal err carries when a replay hook refused one of a
// test set's mocks on its way to the agent (models.MockPayloadRefusal), else
// nil. It is nil too once ctx is done: a hook that failed because the run was
// stopped refused nothing, and the run stops as a stop does.
func mockRefusal(ctx context.Context, err error) *models.MockPayloadRefusal {
	var refusal *models.MockPayloadRefusal
	if ctx.Err() != nil || !errors.As(err, &refusal) {
		return nil
	}
	return refusal
}

// failSetRefusedMock fails a test set one of whose mocks a replay hook
// refused, without sending any of its test cases: the agent cannot serve the
// set as it was recorded. Each test case the run would have sent (those it
// selects, else every one, less those its ignore list skips) fails, unsent,
// with the hook's reason, after notFound, the set's selected test cases not
// on disk, failed already. The set's report and the run's summary name the
// mock and the reason; the run goes on to its next set, and exits non-zero.
func (r *Replayer) failSetRefusedMock(ctx context.Context, testRunID, testSetID string, testCases []*models.TestCase, notFound []models.TestResult, refusal *models.MockPayloadRefusal, started time.Time) error {
	why := "keploy refused the test set and sent none of its test cases: " + refusal.Error()
	utils.LogError(r.logger, refusal.Err, "keploy test refused this test set, and ran none of its test cases: a replay hook refused one of its mocks; the run goes on to the next test set",
		zap.String("testSet", testSetID),
		zap.String("mock", refusal.Mock),
		zap.String("next_step", "do what the error says of the mock (open its sealed value, or record the test set again), then run keploy test again"))
	failures := []models.TemplateFailure{{
		Where:   "mock " + refusal.Mock,
		Why:     "refused when the test set loaded: " + refusal.Error(),
		Refused: true,
	}}
	selected := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID])
	ignored := matcherUtils.ArrayToMap(r.config.Test.IgnoredTests[testSetID])
	results := append(make([]models.TestResult, 0, len(notFound)+len(testCases)), notFound...)
	for _, tc := range testCases {
		if (len(selected) > 0 && !selected[tc.Name]) || ignored[tc.Name] {
			continue
		}
		res := r.refusedResult(testSetID, tc, failures, models.TestSetRefused)
		if err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, res); err != nil {
			utils.LogError(r.logger, err, "failed to insert test case result for a test that was not sent")
			return err
		}
		results = append(results, *res)
	}
	r.completeTestReportMu.Lock()
	if r.refusedSets == nil {
		r.refusedSets = map[string]string{}
	}
	r.refusedSets[testSetID] = why
	r.completeTestReportMu.Unlock()
	return r.failSetUnrun(ctx, testRunID, testSetID, results, why, started)
}

// refusedSetsSummary is the run summary's list of the test sets keploy
// refused because a replay hook refused one of their mocks, each with its
// reason; "" when it refused none.
func (r *Replayer) refusedSetsSummary() string {
	r.completeTestReportMu.RLock()
	sets := make([]string, 0, len(r.refusedSets))
	for s := range r.refusedSets {
		sets = append(sets, s)
	}
	sort.Strings(sets)
	var b strings.Builder
	if len(sets) > 0 {
		fmt.Fprintf(&b, "\n\n\tTest sets refused, none of their test cases sent: %d\n", len(sets))
	}
	for _, s := range sets {
		fmt.Fprintf(&b, "\t\t%s: %s\n", s, strings.Join(strings.Fields(r.refusedSets[s]), " "))
	}
	r.completeTestReportMu.RUnlock()
	return b.String()
}
