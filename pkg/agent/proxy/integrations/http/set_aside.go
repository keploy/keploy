package http

import (
	"context"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// setAsideAnswer is, for a request no mock answered under exact serving (the
// proven switch), the recording of a test the replay set aside before the
// open test that the request is exactly (integrations.SetAsideReader),
// claimed through the store's consume door and ready to serve: a test the
// replay skipped made none of its calls, so a cache it filled at record is
// empty, and the next test makes that call in its own window. It is served
// unchecked and never required of the test (the store names it on the test's
// evidence). nil when the store shows none, when the request is one call more
// than the open test recorded (its extra call stays a miss: keploy #4745's
// F20), when no set-aside recording is the request exactly, and when another
// call took the one it is first.
//
// The set-aside recordings are matched as the live pool's would be, on a view
// that holds only them and that no match can consume, and on a copy of the
// test's bindings: the answer binds nothing, as it is another test's.
func (h *HTTP) setAsideAnswer(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation bool) (*models.Mock, error) {
	sr, ok := mockDb.(integrations.SetAsideReader)
	if !ok {
		return nil, nil
	}
	aside := FilterHTTPMocks(sr.SetAsideMocks(models.HTTP))
	if len(aside) == 0 {
		return nil, nil
	}
	if n, _ := h.extraCall(ctx, input, mockDb, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseDetection, schemaNoiseStrict, mockCorrelation); n > 0 {
		return nil, nil
	}
	vars := variableTable(mockDb)
	if vars != nil {
		vars = vars.Clone()
	}
	// As the replay's matcher reads under the switch: no request noise is
	// detected, and a learned correlation is followed.
	matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: aside, vars: vars}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, false, schemaNoiseStrict, false, true)
	if err != nil || !matched || mk == nil || mk.Spec.HTTPReq == nil {
		return nil, nil
	}
	for _, orig := range aside {
		if orig.Name != mk.Name {
			continue
		}
		served, claimed, err := h.claim(ctx, orig, mockDb, nil, nil)
		if err != nil || !claimed {
			return nil, err
		}
		return served, nil
	}
	return nil, nil
}
