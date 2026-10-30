package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// The dataflow guard for a call made outside every test's window that no
// mock answered: before the set's first test, or between two tests. An
// answer that reaches a test is that test's: the application got no answer
// this run, so what could reach the next test is the answer the recording
// gave it: the call's closest recording (UnmatchedCall.ClosestMock). It
// reaches the test when the word rule keploy learn reads a value's source by
// (learn.DriftCache.WordFrom) says a word of what changed in the test this
// run, in its response or in a request it sent that no mock answered, may
// have come from that recording, and the test was not given the word
// otherwise (its request, its own recordings' answers lack it). The call is
// then the test's miss, not background work, and the word and its place are
// named (UnmatchedCall.ReachesTest). A word the test did not change ("ok",
// "false" at their own places in its response) is no word of what changed:
// a test that returned and sent what it recorded is judged by its own
// answers alone.
//
// Limits, as stated: a test that read the answer only through the
// application's state (a cache it filled at start-up) and returned and sent
// what it recorded shows no change, and the call stays background work; a
// stream test's response is not read, only the requests of its own misses;
// the response is compared raw, so a change in a field the user marked noise
// counts as changed; a changed word that a start-up answer also holds charges
// the test. When keploy cannot tell (no closest recording, one not in the
// set's store, or one whose payload cannot be read back) the call stays
// background work and its reason says keploy could not tell
// (backgroundWhy).

// flowGuard reads, for a set, whether a call made outside every test's
// window reached the test read next (reaches). The zero of it, and nil,
// reads none as reaching, and says it could not tell.
type flowGuard struct {
	// mocks are the set's stored recordings, by name.
	mocks map[string]*models.Mock
	// owned are, by test name, the names of the recordings each test owns.
	owned map[string][]string
	// cases are the set's test cases, by name.
	cases map[string]*models.TestCase
	// dc reads words as keploy learn does, once per text, for one test:
	// dcTest's. It is dropped as the guard reads the next test (cache), so
	// it holds what one test's reads hold, never a whole set's.
	dc     *learn.DriftCache
	dcTest string
}

// newFlowGuard is the guard for a set of test cases cases, whose tests own
// the recordings owned lists, and whose stored recordings are stored.
func newFlowGuard(cases []*models.TestCase, owned map[string][]models.MockEntry, stored ...[]*models.Mock) *flowGuard {
	g := &flowGuard{mocks: map[string]*models.Mock{}, owned: map[string][]string{}, cases: map[string]*models.TestCase{}}
	for _, list := range stored {
		for _, m := range list {
			if m != nil && m.Name != "" {
				g.mocks[m.Name] = m
			}
		}
	}
	for test, entries := range owned {
		for _, e := range entries {
			if e.Name != "" {
				g.owned[test] = append(g.owned[test], e.Name)
			}
		}
	}
	for _, tc := range cases {
		if tc != nil && tc.Name != "" {
			g.cases[tc.Name] = tc
		}
	}
	return g
}

// drift is a value of a test that changed this run, as the guard reads it:
// where it stands, as recorded and as it is now.
type drift struct {
	place          string
	recorded, live string
}

// drifts are the values of test that changed this run: its response (live,
// the one it got: *models.HTTPResp or *models.GrpcResp; nil when it got
// none) and each request it sent that no mock answered (own, the read's
// calls of test's own), against their closest recordings.
func (g *flowGuard) drifts(test string, live any, own []models.UnmatchedCall) []drift {
	if g == nil || test == "" {
		return nil
	}
	var out []drift
	if tc := g.cases[test]; tc != nil {
		if rec, now, ok := responseTexts(tc, live); ok && rec != now {
			out = append(out, drift{"this test's response", rec, now})
		}
	}
	for _, c := range own {
		if c.ClosestMockReq != "" && c.ReceivedReq != "" && c.ClosestMockReq != c.ReceivedReq {
			out = append(out, drift{"the request this test sent that " + c.ClosestMock + " recorded", c.ClosestMockReq, c.ReceivedReq})
		}
	}
	return out
}

// cache is the word cache of test's reads: the one the guard holds when it
// is test's, a new one otherwise, dropping the previous test's.
func (g *flowGuard) cache(test string) *learn.DriftCache {
	if g.dc == nil || g.dcTest != test {
		g.dc, g.dcTest = learn.NewDriftCache(), test
	}
	return g.dc
}

// reaches says why c, a call made outside every test's window (where says
// where) that no mock answered, is test's miss: a word of what changed in
// test this run (drifts) that the word rule says may have come from c's
// closest recording, and where it stands. Both are empty when none did.
// unknown, when why is empty, says why keploy cannot tell: no guard ran, no
// recording is closest to c, or its closest recording is not in the set's
// store or cannot be read back.
func (g *flowGuard) reaches(ctx context.Context, c models.UnmatchedCall, test, where string, drifts []drift) (why, unknown string) {
	switch {
	case g == nil:
		return "", "the dataflow guard did not run for this set"
	case c.ClosestMock == "":
		return "", "no recording is closest to it"
	case test == "":
		return "", "it reached keploy only at the set's end, after every test was read"
	}
	closest, tc := g.mocks[c.ClosestMock], g.cases[test]
	switch {
	case closest == nil:
		return "", "its closest recording " + c.ClosestMock + " is not in the set's stored recordings"
	case tc == nil:
		return "", "the test read after it, " + test + ", is not in the set"
	case len(drifts) == 0:
		// The test returned and sent what it recorded: it is judged by its
		// own answers alone.
		return "", ""
	}
	whole, err := closest.WithPayload(ctx)
	if err != nil || whole == nil {
		return "", "its closest recording " + closest.Name + " could not be read back"
	}
	// What the test was given otherwise did not come from this call.
	given := []string{jsonText(testSide(tc, false))}
	for _, name := range g.owned[test] {
		if m := g.mocks[name]; m != nil && name != closest.Name {
			if own, ok := sideOf(ctx, m, true); ok {
				given = append(given, jsonText(own))
			}
		}
	}
	dc := g.cache(test)
	for _, d := range drifts {
		w, err := dc.WordFrom(d.recorded, d.live, whole, given...)
		// WordFrom fails only on an answer the agent parked on disk
		// (HasSpilledResponse). The agent sets that on mocks it decodes
		// for itself, never on the set's stored recordings that whole is
		// read from, so this branch does not run today. It stays so that
		// such an answer is told as unreadable, not as reaching no test.
		if err != nil {
			return "", "its closest recording " + closest.Name + "'s answer could not be read back"
		}
		if w != "" {
			return fmt.Sprintf("%s; its closest recording %s holds %q, which %s changed this run and nothing else gave the test", where, closest.Name, w, d.place), ""
		}
	}
	return "", ""
}

// responseTexts are tc's recorded response and live, the one it got this
// run, as JSON texts without the instants they were taken at; ok is false
// when live is none keploy reads.
func responseTexts(tc *models.TestCase, live any) (recorded, now string, ok bool) {
	switch x := live.(type) {
	case *models.HTTPResp:
		if x == nil {
			return "", "", false
		}
		rec, got := tc.HTTPResp, *x
		rec.Timestamp, got.Timestamp = time.Time{}, time.Time{}
		return jsonText(rec), jsonText(got), true
	case *models.GrpcResp:
		if x == nil {
			return "", "", false
		}
		return jsonText(tc.GrpcResp), jsonText(*x), true
	}
	return "", "", false
}

// jsonText is v as JSON, "" when it cannot be.
func jsonText(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// sideOf is the decoded JSON of m's recorded answer (answer) or request,
// read back first when its payload is on disk; ok is false when it cannot be
// read. A recording keploy reads only as one whole has neither side.
func sideOf(ctx context.Context, m *models.Mock, answer bool) (any, bool) {
	whole, err := m.WithPayload(ctx)
	if err != nil || whole == nil {
		return nil, false
	}
	sp := whole.Spec
	var parts []any
	if answer {
		parts = []any{sp.GenericResponses, sp.HTTPResp, sp.MongoResponses, sp.PostgresResponsesV2, sp.GRPCResp, sp.MySQLResponses, sp.HTTP2Resp}
	} else {
		parts = []any{sp.GenericRequests, sp.HTTPReq, sp.MongoRequests, sp.PostgresRequestsV2, sp.GRPCReq, sp.MySQLRequests, sp.HTTP2Req}
	}
	return decoded(parts)
}

// testSide is the decoded JSON of tc's recorded response (response) or
// request, without the instants it was recorded at.
func testSide(tc *models.TestCase, response bool) any {
	var parts []any
	if response {
		resp := tc.HTTPResp
		resp.Timestamp = time.Time{}
		parts = []any{resp, tc.GrpcResp}
	} else {
		req, grpc := tc.HTTPReq, tc.GrpcReq
		req.Timestamp, grpc.Timestamp = time.Time{}, time.Time{}
		parts = []any{req, grpc}
	}
	v, _ := decoded(parts)
	return v
}

// decoded is v as JSON decodes it, numbers kept as written.
func decoded(v any) (any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	if err := d.Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}
