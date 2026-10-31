package http

import (
	"context"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/variables"

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
// evidence). nil when the store shows none, when no set-aside recording is
// the request exactly, and when another call took the one it is first.
//
// A request one more than the open test recorded (extraCall) is served a
// set-aside test's copy too when one is left: the copy is the set's own
// recording of the call, and each set-aside recording answers once in the
// staging, so the calls of that shape across the set never exceed what the
// set recorded. The pool's copies (integrations.FromPool, offered last) never
// answer a request one more than the open test's own recordings of it
// (repeatsOwn): a duplicate of a recorded call stays the test's extra call
// (keploy #4745's F20), whatever start-up or between-tests copy the set
// holds. A call the test never recorded (a start-up call, a timer's tick,
// each tick of it) is served from the pool, a timer's tick by a pool
// recording whose clock reads moved with the call's clock too, when no pool
// copy is the call exactly (clockMoved, pool_clock.go). Past every exact
// copy, the test's own and the set-aside ones, the request stays an extra
// call (never another recording of the call, nor one served again).
//
// The set-aside recordings are matched as the live pool's would be, on a view
// that holds only them and that no match can consume, and on a copy of the
// test's bindings: the answer binds nothing, as it is another test's. They
// are read one handed test at a time, in hand order, by the call's shape key
// alone (its method and number of path segments: RequestKeysOf, loose),
// under which every recording a comparison could accept is filed, whatever
// noise, learned place or correlation it excuses; the first test that holds
// the call answers it.
func (h *HTTP) setAsideAnswer(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise, bodyNoise map[string][]string, urlNoise []string, autoURLDynamic, schemaNoiseStrict bool) (*models.Mock, error) {
	sr, ok := mockDb.(integrations.SetAsideReader)
	if !ok {
		return nil, nil
	}
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	keys := RequestKeysOf(input.method, path, "", true)
	vars := variableTable(mockDb)
	var served *models.Mock
	var serr error
	sr.RangeSetAside(models.HTTP, keys, func(batch []*models.Mock) bool {
		aside := FilterHTTPMocks(batch)
		if len(aside) == 0 {
			return true
		}
		pool := integrations.FromPool(aside)
		if pool && h.repeatsOwn(ctx, input, mockDb, headerNoise, bodyNoise, urlNoise, autoURLDynamic, schemaNoiseStrict) {
			// The pool (last) never answers the test's own extra call.
			return false
		}
		// As the replay's matcher reads under the switch: no request noise
		// is detected, and a learned correlation is followed.
		var bound *variables.Table
		if vars != nil {
			bound = vars.Clone()
		}
		matched, mk, _, err := h.match(ctx, input, answeredOnly{MockMemDb: mockDb, mocks: aside, vars: bound}, headerNoise, bodyNoise, urlNoise, autoURLDynamic, false, schemaNoiseStrict, false, true)
		if err != nil || !matched || mk == nil || mk.Spec.HTTPReq == nil {
			if err == nil && pool {
				// The pool's recordings the call is but for its clock,
				// first recorded first (the batch's order).
				served, serr = h.claimClockMoved(ctx, input, aside, mockDb, h.newStrictNoise(headerNoise, bodyNoise, urlNoise))
				return served == nil && serr == nil
			}
			return true
		}
		for _, orig := range aside {
			if orig.Name != mk.Name {
				continue
			}
			got, claimed, err := h.claim(ctx, orig, mockDb, nil, nil)
			if err == nil && claimed {
				served = got
			}
			serr = err
			return false
		}
		return true
	})
	return served, serr
}

// claimClockMoved is the first of pool, the pool's batch in recorded order,
// that the request is but for fields that moved with its clock
// (clockMovedRecordings), claimed through the store's consume door and ready
// to serve, with an Info line naming the recording and the fields read as
// the clock (by shape, no values). One another call took first is passed
// over for the next. nil when none is left.
func (h *HTTP) claimClockMoved(ctx context.Context, input *req, pool []*models.Mock, mockDb integrations.MockMemDb, n strictNoise) (*models.Mock, error) {
	for _, c := range clockMovedRecordings(ctx, input, pool, n) {
		got, claimed, err := h.claim(ctx, c.m, mockDb, nil, nil)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
		}
		if h.Logger != nil {
			h.Logger.Info("http: answered a call in the test's window from a start-up or between-tests recording whose clock reads moved with the call's clock (unchecked, never required, once)",
				zap.String("mock", c.m.Name), zap.Strings("readAsTheClock", c.fields))
		}
		return got, nil
	}
	return nil, nil
}
