package routes

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// consumedStore is the consumed history the agent keeps for its client
// between /updatemockparams calls (models.ConsumedSync), so the client sends
// only what its tests consumed since the last call instead of the whole
// history every time.
//
// It is kept here, in the route, and the service is handed the whole map as a
// full send would have carried it: every service reads the history it is
// given exactly as before, whatever it does with it.
type consumedStore struct {
	mu     sync.Mutex
	stream string
	seq    uint64
	m      map[string]models.MockState
}

// resolve makes req's FilterParams.TotalConsumedMocks the whole history its
// ConsumedSync names. It returns the models.ConsumedSyncHeader value to
// answer the call with ("" for a call that names no stream) and the function
// that ends the call, which the caller defers: the store stays locked until
// then, so no other call changes the map while this one filters with it.
//
// ok is false for a send that builds on a state the store does not hold (an
// agent started again, or another client's stream since): nothing was
// applied, and the client must send the whole history again.
func (c *consumedStore) resolve(req *models.UpdateMockParamsReq) (held string, done func(), ok bool) {
	cs := req.ConsumedSync
	if cs == nil || cs.Stream == "" {
		return "", func() {}, true
	}
	c.mu.Lock()
	if cs.Full {
		// The send's own map, which the agent decoded for this call alone.
		c.stream, c.seq, c.m = cs.Stream, cs.Seq, req.FilterParams.TotalConsumedMocks
		return models.ConsumedSyncHeld(c.stream, c.seq), c.mu.Unlock, true
	}
	if c.stream != cs.Stream || c.seq != cs.Base {
		c.mu.Unlock()
		return "", func() {}, false
	}
	if c.m == nil && len(req.FilterParams.TotalConsumedMocks) > 0 {
		c.m = make(map[string]models.MockState, len(req.FilterParams.TotalConsumedMocks))
	}
	for name, s := range req.FilterParams.TotalConsumedMocks {
		c.m[name] = s
	}
	c.seq = cs.Seq
	// Exactly what a full send carries: an empty history is no map at all
	// (TotalConsumedMocks is omitted when empty).
	req.FilterParams.TotalConsumedMocks = nil
	if len(c.m) > 0 {
		req.FilterParams.TotalConsumedMocks = c.m
	}
	return models.ConsumedSyncHeld(c.stream, c.seq), c.mu.Unlock, true
}
