package integrations

import "go.keploy.io/server/v3/pkg/models"

// ReadThroughMatcher is an optional facet implemented by MockManager (and
// heldScope) to serve deduplicated session read-through mocks strictly by
// exact canonical match, as a fallback after a test's own pool misses.
//
// Read-through mocks are strictly read-only, reusable across tests on a cold
// replay pod, and never shadow a test's own per-test recordings.
type ReadThroughMatcher interface {
	MatchReadThrough(live *models.Mock) (*models.Mock, bool)
}

// MatchReadThrough queries db's session read-through tier for an exact match
// to live when db implements ReadThroughMatcher. Returns (nil, false) when db
// does not implement it, live is not a read-only call, or no exact entry exists.
func MatchReadThrough(db MockMemDb, live *models.Mock) (*models.Mock, bool) {
	if db == nil || live == nil {
		return nil, false
	}
	rt, ok := db.(ReadThroughMatcher)
	if !ok {
		return nil, false
	}
	return rt.MatchReadThrough(live)
}
