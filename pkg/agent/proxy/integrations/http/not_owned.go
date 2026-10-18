package http

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// notOwnedRecording is, for a request no mock answered under exact serving
// (the proven switch), the staged recording that is this very call (the
// strict comparison, strictDiff) which the open view does not show: another
// test's, any test's with no test open, or a worker-scope one whose place the
// run has not reached (integrations.StagedFinder). The call was made where it
// was not recorded: record's judging reads it to bake the recording into the
// worker scope (design v14 S12, a2). nil when there is none, or the store
// keeps no staged mocks to look in. ctx carries the request's destination as
// it came (liveDestination).
func notOwnedRecording(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) *models.Mock {
	sf, ok := mockDb.(integrations.StagedFinder)
	if !ok {
		return nil
	}
	live := liveDestination(ctx, input)
	var found *models.Mock
	sf.FindStagedNotOwned(models.HTTP, func(m *models.Mock) bool {
		if m.Spec.HTTPReq == nil || len(strictDiff(input, live, m, n)) != 0 {
			return false
		}
		found = m
		return true
	})
	return found
}

// markNotOwned makes report the miss of a call that is m, a recording the
// open view does not show (notOwnedRecording): m is its closest mock, with no
// field named as differing, since none does, and its rendered request is m's.
func markNotOwned(report *models.MockMismatchReport, m *models.Mock) {
	report.ClosestMock, report.NotOwned = m.Name, true
	report.FieldDiffs, report.Differs = nil, nil
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.NextSteps = notOwnedNextSteps(m.Name)
}

// notOwnedNextSteps says what a miss on a call that is a recording the open
// view does not show (notOwnedRecording) likely means, and what to do.
func notOwnedNextSteps(name string) string {
	return fmt.Sprintf("This request is %s, which keploy recorded where this replay did not make it: in another test's window, or between tests after a test this replay had not reached. A call the application makes on its own (a queue consumer, a scheduler) falls in whichever window its timing puts it in. keploy record's judging reads this miss and moves %s to the test set's worker scope (mappings.yaml worker:), which answers it wherever the replay makes it from its place on; record again if this replay was not keploy record's.", name, name)
}
