package http

import (
	"context"
	"net/http"
	"net/url"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The variable pass (variables.go) for another parser of HTTP mocks: the SQS
// parser, whose recordings are HTTP mocks. Under the proven switch a mock that
// declares variables (models.MockVariables) is served by the variable pass
// alone, whichever parser reads it: a parser that compared it as recorded
// missed every call whose variable took a new value (a false red), and served
// one it fit without binding its variables or rendering its answers.

// VariableScope is the variables' scopes a match of a call checks against. A
// parser reads it before it fetches its pool (ReadVariableScope): a test that
// begins after the read makes the match's binds stale, and the call is
// checked again (matchCascade).
type VariableScope struct{ s varScope }

// ReadVariableScope reads mockDb's variables' scopes.
func ReadVariableScope(mockDb integrations.MockMemDb) VariableScope {
	return VariableScope{s: readVarScope(mockDb)}
}

// Moved reports whether a test began since the scope was read: the call is
// checked again against it.
func (s VariableScope) Moved() bool { return s.s.moved() }

// VariableCall is a live call, as a parser read it.
type VariableCall struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

// VariableOutcome is how the variable pass left a call.
type VariableOutcome int

const (
	// VariableNone: no recording that declares variables fit the call. The
	// caller matches it against Rest, as it would have.
	VariableNone VariableOutcome = iota
	// VariableServed: Served answers the call.
	VariableServed
	// VariableExactOnly: a recording fit the call but its variables did not
	// hold (Problem). Only an exact recording of this very call among Rest
	// may still answer it, never a lenient pass; else it is a miss.
	VariableExactOnly
	// VariableMiss: the call is a miss (Problem).
	VariableMiss
	// VariableRetry: the call must be checked again (a test began, another
	// connection bound a variable or took the mock first).
	VariableRetry
)

// VariableResult is what the variable pass made of a call.
type VariableResult struct {
	Outcome VariableOutcome
	// Served is the answer (VariableServed): its answer places hold their
	// variables' values, its binds are committed, and consume consumed it.
	Served *models.Mock
	// Rest is the pool's mocks that declare no variables (VariableNone,
	// VariableExactOnly).
	Rest []*models.Mock
	// Problem says why the call was not served (VariableExactOnly,
	// VariableMiss).
	Problem string
}

// PinLiveDestination is ctx with the call's destination read once, as the
// HTTP decoder reads a request's (decodeHTTP): a read is a use of the
// connection's container in the scope open now (containerkey), so a parser
// pins it once per request, before it matches, under the proven switch;
// read again at each comparison, a test that began mid-match had this
// connection counted in it, and a check compared its mocks against two
// destinations. With the switch off ctx is returned as it is.
func PinLiveDestination(ctx context.Context, call VariableCall) context.Context {
	if !provenswitch.On() {
		return ctx
	}
	return withLiveDestination(ctx, liveDestination(ctx, call.req()))
}

func (c VariableCall) req() *req {
	return &req{method: c.Method, url: c.URL, header: c.Header, body: c.Body, raw: c.Body}
}

// VariableMatch is the variable pass for a call of another parser, over pool
// (in its order), with scope read before pool was fetched, and the run's
// request noise (NewRequestNoise): what of the call may differ from its
// recording beside the variables' places, the mock's learned noise and the
// obfuscator's values, as for an HTTP call. consume takes the answering mock
// from mockDb as the parser consumes its recordings (false: another
// connection took it first). ctx has the call's destination pinned
// (PinLiveDestination). With the switch off nothing here runs: the outcome is
// VariableNone and Rest is pool.
func (h *HTTP) VariableMatch(ctx context.Context, scope VariableScope, call VariableCall, noise RequestNoise, pool []*models.Mock, mockDb integrations.MockMemDb, consume func(*models.Mock) bool) (VariableResult, error) {
	if !provenswitch.On() {
		return VariableResult{Outcome: VariableNone, Rest: pool}, nil
	}
	withVars, rest := splitVariableMocks(pool)
	if len(withVars) == 0 {
		return VariableResult{Outcome: VariableNone, Rest: rest}, nil
	}
	hits, problem, err := h.variablePass(ctx, call.req(), withVars, scope.s, noise.Header, noise.Body, noise.URL)
	if err != nil {
		return VariableResult{}, err
	}
	if len(hits) == 0 {
		if problem != nil {
			return VariableResult{Outcome: VariableExactOnly, Rest: rest, Problem: problem.diff()}, nil
		}
		return VariableResult{Outcome: VariableNone, Rest: rest}, nil
	}
	hit, commitCursor, _ := h.pickHit(hits, nil, mockDb, false, nil)
	served, problem, retry, err := h.serveVariablesBy(hit, consume)
	switch {
	case err != nil:
		return VariableResult{}, err
	case retry:
		return VariableResult{Outcome: VariableRetry}, nil
	case problem != nil:
		if scope.Moved() {
			return VariableResult{Outcome: VariableRetry}, nil
		}
		return VariableResult{Outcome: VariableMiss, Problem: problem.diff()}, nil
	}
	if commitCursor != nil {
		commitCursor()
	}
	return VariableResult{Outcome: VariableServed, Served: served}, nil
}

// StrictFits are the recordings of pool that are this very call under the
// proven switch (S1), in pool's order, as the HTTP parser's strict pass reads
// them (strictCompare): every request field outside the run's request noise
// (NewRequestNoise), a SigV4 signature by its grammar (region, service and
// signed headers compared; access key id, date and signature not). For
// another parser of HTTP mocks, whose recordings are held to the same rule
// (the SQS parser's exact pass), which serves one with its own consume. ctx
// has the call's destination pinned (PinLiveDestination).
func (h *HTTP) StrictFits(ctx context.Context, call VariableCall, noise RequestNoise, pool []*models.Mock) ([]*models.Mock, error) {
	input := call.req()
	fits, _, err := strictCompare(ctx, input, liveDestination(ctx, input), pool, h.newStrictNoise(noise.Header, noise.Body, noise.URL))
	return fits, err
}
