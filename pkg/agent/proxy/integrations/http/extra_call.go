package http

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// extraCall tells, for a request no mock answered under exact serving (the
// proven switch), whether it is one call more than the open test recorded:
// identical, as the matcher judges it, to a call the test's own mocks already
// answered. It returns how many times that call was recorded for the test,
// 0 when the request is not such a call or exact serving is off.
//
// The test's answered mocks are matched as the live pool would be, on a view
// that holds only them and that no match can consume, so one definition of
// "the same call" serves both the answer and this verdict.
func (h *HTTP) extraCall(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation bool) int {
	n, _ := h.extraCallOf(ctx, input, mockDb, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation)
	return n
}

// extraCallOf is extraCall with the recording the call repeats: of the
// test's answered mocks the call matches on its own, the first to answer
// (models.MockMismatchReport.ExtraOf); nil when n is 0, and when the call
// matched only the answered mocks together.
func (h *HTTP) extraCallOf(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation bool) (n int, of *models.Mock) {
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return 0, nil
	}
	answered := FilterHTTPMocks(er.ConsumedTestMocks())
	if len(answered) == 0 {
		return 0, nil
	}
	dry := h.answeredMatch(ctx, input, mockDb, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation)
	if !dry(answered) {
		return 0, nil
	}
	// How often the test recorded the call: each answered mock the call
	// matches on its own (recordings of one call may differ where its
	// variables sit).
	for _, a := range answered {
		if dry([]*models.Mock{a}) {
			if n == 0 {
				of = a
			}
			n++
		}
	}
	return max(n, 1), of
}

// markExtraCall makes report the miss of a call one more than the test
// recorded (extraCallOf): identical, as the matcher judges it (noise and
// learned places included), to of, the test's own recording that already
// answered it, which the test recorded recorded times. of is its closest
// mock, with no field named as differing, since none does: the live pool's
// closest is another recording (of answered and left every read), whose
// differences say nothing of this call, and a reader that took them for the
// call's would re-record a call that did not change. The next step says how
// often the test recorded the call and names the mark that answers a read
// the application repeats on its own timing (mismatch.ExtraCallNextSteps).
func markExtraCall(report *models.MockMismatchReport, of *models.Mock, recorded int) {
	report.ClosestMock = of.Name
	report.FieldDiffs, report.Differs, report.FieldReasons, report.Diff = nil, nil, nil, ""
	report.ClosestMockReq = renderMockRequest(of.Spec.HTTPReq, of.Noise)
	report.NextSteps = mismatch.ExtraCallNextSteps(of.Name, recorded)
}

// ownCall reports whether the request is one of the open test's own calls
// (integrations.OwnRepeatRead): whether it matches one of the test's own HTTP
// recordings, answered or not, as the matcher judges it (answeredMatch). It
// reads only the recordings the strict pass's index of the test's own files
// for the request (strictOwned.at: those of its method and masked path, those
// of its method that learned a place of their URL, and those that declare
// variables), the only ones the variable and strict passes, which decide
// every call under the proven switch, may answer it with: never every
// recording the test owns. db is the call's held store, base the store it is
// held from, whose index the strict pass keeps too.
func (h *HTTP) ownCall(ctx context.Context, input *req, db, base integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseStrict bool) bool {
	sr, ok := db.(integrations.SharedReads)
	if !ok {
		return false
	}
	n := h.newStrictNoise(headerNoise, bodyNoise, urlNoise)
	ix, _, owned, ok := h.strictIx.owned.of(base, sr, urlNoiseKey(n.url), func(owned []*models.Mock) *strictOwned { return buildStrictOwned(owned, n) })
	if !ok {
		return false
	}
	at := ix.at(newStrictSieve(input, n))
	if len(at) == 0 {
		return false
	}
	mocks := make([]*models.Mock, len(at))
	for i, p := range at {
		mocks[i] = owned[p]
	}
	return h.answeredMatch(ctx, input, db, headerNoise, bodyNoise, urlNoise, autoURLDynamic, false, schemaNoiseStrict, true)(mocks)
}

// answeredMatch is the dry match extraCall and ownCall read a request
// against some of the open test's own mocks with: whether the request
// matches one of mocks, as the matcher judges it, on a view that holds only
// them and that no match can consume, with the test's bindings on a copy (a
// mock that declares a variable is matched as it was answered, and the dry
// match binds nothing).
func (h *HTTP) answeredMatch(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation bool) func(mocks []*models.Mock) bool {
	vars := variableTable(mockDb)
	return func(mocks []*models.Mock) bool {
		v := vars
		if v != nil {
			v = v.Clone()
		}
		matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: mocks, vars: v}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, false, mockCorrelation)
		return err == nil && matched && mk != nil && mk.Spec.HTTPReq != nil
	}
}

// answeredDiag widens the diagnosis of a miss, under exact serving (the
// proven switch), to the open test's answered mocks. Each test mock answers
// once and is then in no read, so a call a test makes after the recording
// it most resembles answered finds that recording nowhere: the miss would
// name no closest recording ("no_mocks"), or a farther one, although the
// test recorded one to say what differs from. The answered mocks compete
// with the live pool's closest recording by the same rule (closerThan), the
// live pool's first on a tie, and the diagnosis takes the closer. As in the
// live pool, a mock that declares variable places is not compared strictly
// (the variable pass's evidence reports those). They are only compared,
// never served: the match already failed. diag is returned as
// it is when exact serving is off, when the test has no answered mock, and
// for a miss whose recording fit but whose variables did not hold, and for a
// stateful walk's (matchDiag.answered): that is what each reports. An extra
// call is diagnosed against the recording it repeats (markExtraCall).
func (h *HTTP) answeredDiag(ctx context.Context, input *req, mockDb integrations.MockMemDb, diag *matchDiag, headerNoise, bodyNoise map[string][]string, urlNoise []string) *matchDiag {
	if !provenswitch.On() || (diag != nil && (diag.variable != nil || diag.answered != nil)) {
		return diag
	}
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return diag
	}
	_, answered := splitVariableMocks(FilterHTTPMocks(er.ConsumedTestMocks()))
	if len(answered) == 0 {
		return diag
	}
	live := liveDestination(ctx, input)
	_, near, err := strictCompare(ctx, input, live, answered, h.newStrictNoise(headerNoise, bodyNoise, urlNoise))
	if err != nil {
		return diag
	}
	var out matchDiag
	if diag != nil {
		out = *diag
	}
	if !near.closerThan(nearest{mock: out.closest, diffs: out.differs, route: out.closest != nil && out.phase == models.MatchPhaseStrict}) {
		return diag
	}
	out.phase = models.MatchPhaseSchema
	out.schemaMatched = nil
	if near.route {
		out.phase = models.MatchPhaseStrict
		out.schemaMatched = []*models.Mock{near.mock}
	}
	out.pool = append(append(make([]*models.Mock, 0, len(out.pool)+len(answered)), out.pool...), answered...)
	out.candidates = len(out.pool)
	out.live = &live
	out.closest, out.differs = near.mock, near.diffs
	return &out
}

// answeredOnly is a read-only view of a test's answered mocks, for extraCall:
// it shows them as per-test mocks and nothing else, and every consume
// "succeeds" without touching anything, so a match on it changes no state.
// Its variable table (integrations.VariableStore) is a copy of the test's,
// for the same reason: a match that follows a variable may bind it.
type answeredOnly struct {
	integrations.MockMemDb
	mocks []*models.Mock
	vars  *variables.Table
}

// Variables implements integrations.VariableStore: the copy of the test's
// bindings (nil when the store keeps none).
func (a answeredOnly) Variables() *variables.Table { return a.vars }

func (a answeredOnly) GetPerTestMocksInWindow() ([]*models.Mock, error) { return a.mocks, nil }
func (a answeredOnly) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return a.mocks, nil
}
func (a answeredOnly) GetFilteredMocks() ([]*models.Mock, error)    { return a.mocks, nil }
func (answeredOnly) GetSessionMocks() ([]*models.Mock, error)       { return nil, nil }
func (answeredOnly) GetUnFilteredMocks() ([]*models.Mock, error)    { return nil, nil }
func (answeredOnly) GetStartupMocks() ([]*models.Mock, error)       { return nil, nil }
func (answeredOnly) GetSessionScopedMocks() ([]*models.Mock, error) { return nil, nil }
func (answeredOnly) GetSessionMocksInWindow(time.Time, time.Time) ([]*models.Mock, error) {
	return nil, nil
}
func (answeredOnly) UpdateUnFilteredMock(*models.Mock, *models.Mock) bool { return true }
func (answeredOnly) DeleteFilteredMock(models.Mock) bool                  { return true }
func (answeredOnly) DeleteUnFilteredMock(models.Mock) bool                { return true }
func (answeredOnly) DeleteStartupMock(models.Mock) bool                   { return true }
func (answeredOnly) MarkMockAsUsed(models.Mock) bool                      { return true }
