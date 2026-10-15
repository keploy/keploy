package http

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
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
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return 0
	}
	answered := FilterHTTPMocks(er.ConsumedTestMocks())
	if len(answered) == 0 {
		return 0
	}
	// The test's bindings, on a copy: a mock that declares a variable is
	// matched as it was answered, and the dry match binds nothing.
	vars := variableTable(mockDb)
	dry := func(mocks []*models.Mock) bool {
		v := vars
		if v != nil {
			v = v.Clone()
		}
		matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: mocks, vars: v}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, false, mockCorrelation)
		return err == nil && matched && mk != nil && mk.Spec.HTTPReq != nil
	}
	if !dry(answered) {
		return 0
	}
	// How often the test recorded the call: each answered mock the call
	// matches on its own (recordings of one call may differ where its
	// variables sit).
	n := 0
	for _, a := range answered {
		if dry([]*models.Mock{a}) {
			n++
		}
	}
	return max(n, 1)
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
