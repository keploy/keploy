package http

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
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
	matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: answered}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, false, mockCorrelation)
	if err != nil || !matched || mk == nil || mk.Spec.HTTPReq == nil {
		return 0
	}
	n := 0
	for _, a := range answered {
		if a.Spec.HTTPReq != nil && a.Spec.HTTPReq.Method == mk.Spec.HTTPReq.Method &&
			a.Spec.HTTPReq.URL == mk.Spec.HTTPReq.URL && a.Spec.HTTPReq.Body == mk.Spec.HTTPReq.Body {
			n++
		}
	}
	return max(n, 1)
}

// answeredOnly is a read-only view of a test's answered mocks, for extraCall:
// it shows them as per-test mocks and nothing else, and every consume
// "succeeds" without touching anything, so a match on it changes no state.
// It keeps no variable table (integrations.VariableStore) for the same
// reason: a match that follows a variable may bind it.
type answeredOnly struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

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
