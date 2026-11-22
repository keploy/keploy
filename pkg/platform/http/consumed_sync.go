package http

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// consumedHeld is the consumed history the agent holds for this client
// (models.ConsumedSync), as the agent's answers said: history up to seq,
// under stream. The zero value holds nothing.
type consumedHeld struct {
	history *models.ConsumedHistory
	stream  string
	seq     uint64
	// narrowed says the full send it started from carried only the per-test
	// region's entries. The region can change within a set, and a mock it
	// gains has no entry there, so such a stream is never built on: the next
	// send is whole again, and to an agent that is known to keep the
	// history, never narrowed.
	narrowed bool
}

// consumedSync is the client's side of models.ConsumedSync.
type consumedSync struct {
	// mu is held from building a send that names a stream to reading the
	// agent's answer to it, so two sends never build on the same state.
	mu   sync.Mutex
	held consumedHeld
	// keeps says an answer of this agent's said it keeps the history.
	keeps bool
}

// consumedSend is how one /updatemockparams call carries the consumed history.
type consumedSend struct {
	sync *models.ConsumedSync
	// next is what the agent holds once it answers the call as asked.
	next consumedHeld
}

// request is the request body for params. With no history, or one it cannot
// build on, it carries the consumed history whole, as every client before it
// did: only the per-test region's entries when perTest (the agent's last
// answer said it reads no others, models.ConsumedScopeHeader). To an agent
// that holds an earlier state of the same history, it carries only the
// entries written since. Called with c.mu held when params has a history.
func (c *consumedSync) request(params models.MockFilterParams, perTest bool) (models.UpdateMockParamsReq, consumedSend) {
	h := params.ConsumedHistory
	params.ConsumedHistory = nil
	if h == nil || params.TotalConsumedMocks == nil {
		if perTest && params.ConsumedRegion != nil && params.TotalConsumedMocks != nil {
			params.TotalConsumedMocks = pkg.ConsumedForAgent(params.TotalConsumedMocks, params.ConsumedRegion)
		}
		return models.UpdateMockParamsReq{FilterParams: params}, consumedSend{}
	}
	held := c.held
	if held.history == h && !held.narrowed {
		if since, end, ok := h.Since(held.seq); ok {
			params.TotalConsumedMocks = since
			cs := &models.ConsumedSync{Stream: held.stream, Base: held.seq, Seq: end}
			return models.UpdateMockParamsReq{FilterParams: params, ConsumedSync: cs},
				consumedSend{sync: cs, next: consumedHeld{history: h, stream: held.stream, seq: end}}
		}
	}
	narrowed := perTest && params.ConsumedRegion != nil && !c.keeps
	if narrowed {
		params.TotalConsumedMocks = pkg.ConsumedForAgent(params.TotalConsumedMocks, params.ConsumedRegion)
	}
	req := models.UpdateMockParamsReq{FilterParams: params}
	s := consumedSend{next: consumedHeld{history: h, stream: newConsumedStream(), seq: h.Pos(), narrowed: narrowed}}
	s.sync = &models.ConsumedSync{Stream: s.next.stream, Full: true, Seq: s.next.seq}
	req.ConsumedSync = s.sync
	return req, s
}

// answered records what the agent's answer to send said it holds: held is
// its models.ConsumedSyncHeader. It reports whether the agent applied the
// filter with the history the send meant: false only for a send that built
// on a state of the history and was answered by an agent that does not keep
// one, which filtered with the entries since that state alone. Called with
// c.mu held, after the send forgot what the agent held: an answer that does
// not say it holds the send's state leaves the client believing it holds
// nothing.
func (c *consumedSync) answered(send consumedSend, held string) bool {
	if send.sync == nil {
		return true
	}
	if held == models.ConsumedSyncHeld(send.next.stream, send.next.seq) {
		c.held, c.keeps = send.next, true
		return true
	}
	c.keeps = false
	return send.sync.Full
}

// forget drops what the agent was believed to hold: a call that failed at
// any point may have left it holding anything, or nothing.
func (c *consumedSync) forget() {
	c.held = consumedHeld{}
}

// newConsumedStream names a new stream of a consumed history.
func newConsumedStream() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
