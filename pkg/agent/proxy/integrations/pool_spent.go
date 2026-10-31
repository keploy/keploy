package integrations

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
)

// PoolSpend is a recording the set made while no test was open (keploy mock
// replay's pool slice) that a call of the replay already used in the
// staging, and where that call was: In names the test whose window it
// answered in, and is empty when a call between tests consumed it.
type PoolSpend struct {
	Mock *models.Mock
	In   string
}

// SpentPoolReader is an optional MockMemDb extension of a store under exact
// serving: SpentPool is, of kind, the set's recordings made while no test
// was open that a call already used up in the staging (a per-test one a
// call consumed, between tests or in a test; a session or connection one a
// test was served, which the calls between tests still reuse), filed under
// one of keys when keys are given and the kind declares its own
// (models.RequestKeys), in recorded order, each with where it went. A parser
// asks for them only for a call nothing answered: a call that is exactly
// one of them is no call the set never recorded, and its miss names the
// recording and where it went (MarkPoolSpent), never asking to record a
// recording that exists. None without exact serving.
type SpentPoolReader interface {
	SpentPool(kind models.Kind, keys []string) []PoolSpend
}

// MarkPoolSpent makes report the miss of a call that is exactly s.Mock, a
// start-up or between-tests recording another call already used
// (SpentPoolReader): s.Mock is its closest, with no field named as
// differing, since none does, and the report says where the recording went
// (models.MockMismatchReport.PoolSpent, SpentIn) and what that means.
func MarkPoolSpent(report *models.MockMismatchReport, s PoolSpend) {
	if report == nil || s.Mock == nil {
		return
	}
	report.ClosestMock, report.PoolSpent, report.SpentIn = s.Mock.Name, true, s.In
	report.FieldDiffs, report.Differs, report.FieldReasons, report.Diff = nil, nil, nil, ""
	report.NextSteps = PoolSpentNextSteps(s)
}

// PoolSpentNextSteps says what a miss on a call that is exactly a spent
// start-up or between-tests recording (MarkPoolSpent) means: the
// application made the call more often at this replay than when it was
// recorded (a timer that ticked again, a start-up job run once more), and
// keploy answers each such recording once. The recording exists, so
// recording again is not what it asks.
func PoolSpentNextSteps(s PoolSpend) string {
	where := "a call between tests"
	if s.In != "" {
		where = "a call in test " + s.In
	}
	return fmt.Sprintf("This request is %s, which keploy recorded while no test was open (the application's start-up or background work); it already answered %s at this replay, and keploy answers each such recording once. The application made this call more often than it did when recorded: a timer that ticked again, or a start-up job that ran once more.", s.Mock.Name, where)
}
