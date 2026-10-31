package http

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// extraCall tells, for a request no mock answered under exact serving (the
// proven switch), whether it is one call more than the open test recorded:
// identical, as the matcher judges it, to a call the test's own mocks already
// answered. It returns how many times that call was recorded for the test,
// 0 when the request is not such a call or exact serving is off, and the
// recording it repeats: of the test's answered recordings of the call, the
// one recorded last (integrations.RepeatedRecording).
//
// The test's answered mocks are matched as the live pool would be, on a view
// that holds only them and that no match can consume, so one definition of
// "the same call" serves both the answer and this verdict.
func (h *HTTP) extraCall(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation bool) (int, *models.Mock) {
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return 0, nil
	}
	answered := FilterHTTPMocks(er.ConsumedTestMocks())
	if len(answered) == 0 {
		return 0, nil
	}
	// The test's bindings, on a copy: a mock that declares a variable is
	// matched as it was answered, and the dry match binds nothing.
	vars := variableTable(mockDb)
	dry := func(mocks []*models.Mock) *models.Mock {
		v := vars
		if v != nil {
			v = v.Clone()
		}
		matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: mocks, vars: v}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, false, mockCorrelation)
		if err != nil || !matched || mk == nil || mk.Spec.HTTPReq == nil {
			return nil
		}
		return mk
	}
	first := dry(answered)
	if first == nil {
		return 0, nil
	}
	// How often the test recorded the call: each answered mock the call
	// matches on its own (recordings of one call may differ where its
	// variables sit). The match over all of them names one when none
	// matches alone.
	var copies []*models.Mock
	for _, a := range answered {
		if dry([]*models.Mock{a}) != nil {
			copies = append(copies, a)
		}
	}
	if len(copies) == 0 {
		return 1, first
	}
	return len(copies), integrations.RepeatedRecording(copies)
}

// repeatsOwn reports whether the request is, as the matcher reads it under
// the switch (setAsideAnswer's reading), or but for fields that moved with
// its clock (clockMovedRecordings, the rule the pool answers by), a call one
// of the open test's own recordings already answered
// (integrations.OwnRecordings): one call more than the test recorded, which
// the pool never answers (integrations.FromPool). A set-aside test's or the
// pool's copy the test was served is none of its own: a timer's second tick
// in a test that recorded none is no extra call.
func (h *HTTP) repeatsOwn(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseStrict bool) bool {
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return false
	}
	own := FilterHTTPMocks(integrations.OwnRecordings(er.ConsumedTestMocks()))
	if len(own) == 0 {
		return false
	}
	vars := variableTable(mockDb)
	if vars != nil {
		vars = vars.Clone()
	}
	matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: own, vars: vars}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, false, schemaNoiseStrict, false, true)
	if err == nil && matched && mk != nil && mk.Spec.HTTPReq != nil {
		return true
	}
	return len(clockMovedRecordings(ctx, input, own, h.newStrictNoise(headerNoise, bodyNoise, urlNoise))) > 0
}

// backgroundAgain is, for a request no mock answered under exact serving (the
// proven switch), the recording of the open test that judging marked as the
// application's background work and that the request repeats: identical, as
// the matcher judges it, to such a recording that already answered in the
// test's window (integrations.AnsweredBackgroundReader). A timer or a
// start-up job ran more often at replay than at record, and its call is
// answered again with that recording, unchecked, where it would otherwise be
// one call more than the test recorded (extraCall). nil when the request
// repeats none, or the store marks nothing.
//
// The marked recordings are matched as the live pool's would be, on a view
// that holds only them and that no match can consume, and on a copy of the
// test's bindings: the answer binds nothing, as the background work's answer
// is not the test's.
func (h *HTTP) backgroundAgain(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseStrict bool) *models.Mock {
	br, ok := mockDb.(integrations.AnsweredBackgroundReader)
	if !ok {
		return nil
	}
	marked := FilterHTTPMocks(br.AnsweredBackground())
	if len(marked) == 0 {
		return nil
	}
	vars := variableTable(mockDb)
	if vars != nil {
		vars = vars.Clone()
	}
	// As the replay's matcher reads under the switch: no request noise is
	// detected, and a learned correlation is followed.
	matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: marked, vars: vars}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, false, schemaNoiseStrict, false, true)
	if err != nil || !matched || mk == nil || mk.Spec.HTTPReq == nil {
		return nil
	}
	return mk
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
// stateful walk's (matchDiag.answered): that is what each reports. The caller leaves an extra call's diagnosis as the
// match found it.
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
