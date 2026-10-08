package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// flakyHeaders is retained for this package's tests, which enumerate the list.
// models.FlakyHeaders is the definition; production code here reaches it via
// flakyHeaderNoise().
var flakyHeaders = models.FlakyHeaders

type req struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
	raw    []byte
}

// matchDiag captures where the match cascade stopped when no mock matched, so
// the mismatch report can tell the user WHICH stage ruled everything out and
// diff the live request against the candidates that were still alive there —
// instead of the canned "headers or body differ".
type matchDiag struct {
	phase         string         // models.MatchPhase* constant
	candidates    int            // HTTP mocks considered
	schemaMatched []*models.Mock // candidates alive after schema match (nil if none)
	// pool is the FULL set match() walked for this attempt (per-test mocks in
	// window + session mocks, filtered to HTTP) — the literal "compared set".
	// len(pool) == candidates by construction.
	//
	// It exists so the destination-scope diagnostic can answer "did anything
	// we compared even target this upstream?" on EVERY miss, including the
	// schema-survivor paths where buildHTTPMismatchReport deliberately does
	// not reload the pool (it diffs against the survivors instead). Without
	// it, an out-of-scope call whose method+path happens to schema-match an
	// application mock — the shared paths, /health, /metrics, /oauth/token,
	// on a different host — was the ONE case the diagnostic could not see,
	// and precisely the case where a closest-mock diff against another
	// upstream is most confusing.
	//
	// Carrying the slice out costs one pointer copy on the miss path and
	// changes nothing about matching: it is a reference to a set match() has
	// already finished with, attached to a struct only ever built on the
	// no-match return.
	pool []*models.Mock
}

// match returns (matched, mock, diag, err). diag is non-nil only when
// matched is false and no error occurred. userBodyNoise carries the user's
// test.globalNoise body bucket (root-relative dotted paths, lowercased) so
// manual noise config participates in mock matching with the same vocabulary
// as response assertions.
func (h *HTTP) match(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise map[string][]string, userBodyNoise map[string][]string, urlNoise []string, autoURLDynamic bool, schemaNoiseDetection bool, schemaNoiseStrict bool, statefulMocks bool, mockCorrelation bool) (bool, *models.Mock, *matchDiag, error) {

	// Shared schema-noise engine for this match. HTTP is a full client of the
	// same engine Pulsar (and any future parser) uses — httpNoiseAdapter owns
	// only the HTTP-specific bits (body extraction, JSON/form diff).
	noiseEngine := schemanoise.New(httpNoiseAdapter{}, schemaNoiseDetection, schemaNoiseStrict)

	for {
		if ctx.Err() != nil {
			return false, nil, nil, ctx.Err()
		}

		// Fetch HTTP mocks from BOTH pools:
		//   - Per-test mocks (Lifetime=PerTest): test-specific app
		//     HTTP calls, window-filtered at ingest, consumed on
		//     match. These are the MORE SPECIFIC match for the
		//     current request — they were recorded for exactly this
		//     test's behaviour.
		//   - Session mocks (Lifetime=Session): auth/SigV4/SQS
		//     handshake, reusable across every test, not window-
		//     filtered, not consumed.
		//
		// Ordering: per-test FIRST. If a per-test mock matches, it
		// wins over any session mock that "sort of" matches at the
		// same score — a session mock should only be chosen when no
		// per-test mock matches at all. This prevents the subtle
		// regression where a per-test mock for the current test gets
		// passed over in favour of a session mock that tied on
		// schema but wasn't recorded for this test.
		//
		// Pre-unification this parser only consumed session mocks
		// (via GetUnFilteredMocks returning the full unfiltered pool
		// that included untagged HTTP as session-by-kind). Post-
		// Phase-3 tag-only routing, per-test HTTP mocks land in the
		// per-test pool — they need to be reachable here.
		perTestMocks, err := mockDb.GetPerTestMocksInWindow()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get per-test mocks")
			return false, nil, nil, errors.New("error while matching the request with the mocks")
		}
		sessionMocks, err := mockDb.GetSessionMocks()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get session mocks")
			return false, nil, nil, errors.New("error while matching the request with the mocks")
		}
		combined := make([]*models.Mock, 0, len(perTestMocks)+len(sessionMocks))
		combined = append(combined, perTestMocks...)
		combined = append(combined, sessionMocks...)
		unfilteredMocks := FilterHTTPMocks(combined)

		// Log all mock names in a single line for better readability
		mockNames := make([]string, len(unfilteredMocks))
		for i, mock := range unfilteredMocks {
			mockNames[i] = mock.Name
		}
		h.Logger.Debug("mocks under consideration for match function", zap.Strings("mock names", mockNames))

		h.Logger.Debug(fmt.Sprintf("Length of unfilteredMocks:%v", len(unfilteredMocks)))

		if len(unfilteredMocks) == 0 {
			return false, nil, &matchDiag{phase: models.MatchPhaseNoMocks}, nil
		}

		// Value rebinding (see rebind): a mock whose request carries a value
		// this request's live value is bound to is matched as a copy carrying
		// the live value. The pool itself is unchanged, so diagnostics keep
		// naming recorded mocks. rb is nil — nothing below changes — unless the
		// replay asked for rebinding and the staged set can be followed.
		//
		// It stays out of strict mode altogether. Strict tolerates only
		// configured or learned noise, and an id that is followed is neither:
		// a strict run is, to the byte, what it is without rebinding.
		var rb *rebind
		if !schemaNoiseStrict {
			rb = newRebind(mockDb, input, func(in *req, m *models.Mock) bool {
				return h.sameRequest(ctx, in, m, headerNoise, urlNoise, noiseEngine.KnownNoise(m, userBodyNoise))
			})
		}
		candidates := rb.candidates(unfilteredMocks)
		// learn is the request-body noise to record on a matched mock. It is
		// what a replay without rebinding records, always: the pooled
		// recording's body against the body as the app sent it — never a copy,
		// never a body with ids put back. A set whose noise was learned with
		// ids followed must still replay strictly where they are not (an older
		// keploy, another consumer of the mocks, test.disableMockRebinding).
		//
		// lenient says the pass is one of the lenient ones. The exact and the
		// correlation pass have noise to learn only when they matched a copy:
		// without rebinding that request would have differed from the
		// recording in its ids, and been matched leniently.
		//
		// Nor is anything learned when the recording echoes the request's
		// value back (its correlations match the request): without
		// rebinding the correlation pass matches such a request, and learns
		// nothing.
		learn := func(m *models.Mock, lenient bool) map[string][]string {
			orig, viaCopy := rb.original(m)
			if !lenient && !viaCopy {
				return nil
			}
			if viaCopy {
				if correlated, _, _ := h.correlationMatch(input.body, []*models.Mock{orig}, mockCorrelation); correlated {
					return nil
				}
			}
			detected, _ := noiseEngine.Detect(orig, input.body, userBodyNoise)
			return detected
		}

		// Matching process
		// Pass 1: exact URL + configured url-noise only. Deterministic and
		// genuinely-distinct calls match here and are never relaxed.
		schemaMatched, err := h.SchemaMatch(ctx, input, candidates, headerNoise, urlNoise, false)
		if err != nil {
			return false, nil, nil, err
		}
		// Pass 2 (zero-config DEFAULT): only when nothing matched the URL exactly
		// or under url-noise, retry allowing auto-detected dynamic id segments
		// (numeric/uuid/hex/long token) to vary — so a non-deterministic path id
		// doesn't 502 with no config. Disable via OutgoingOptions.DisableAutoURLDynamic.
		if len(schemaMatched) == 0 && autoURLDynamic {
			schemaMatched, err = h.SchemaMatch(ctx, input, candidates, headerNoise, urlNoise, true)
			if err != nil {
				return false, nil, nil, err
			}
			if len(schemaMatched) > 0 {
				h.Logger.Debug("http url: matched via auto-detected dynamic path segment(s) after no exact/url-noise match",
					zap.Int("candidates", len(schemaMatched)))
			}
		}

		if len(schemaMatched) == 0 {
			return false, nil, &matchDiag{phase: models.MatchPhaseSchema, candidates: len(unfilteredMocks), pool: unfilteredMocks}, nil
		}
		schemaMatched = rb.inOrder(schemaMatched)

		h.Logger.Debug("http mock schema match results",
			zap.Int("schema_matched", len(schemaMatched)),
			zap.Int("total_http_mocks", len(unfilteredMocks)))

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			// Stateful dependency: when several recorded responses share this
			// exact request (a counter, a created-then-read row), serve them in
			// record order via a per-request cursor instead of always the first,
			// then saturate on the last. No-op for a single recording.
			bestMatch, commitCursor := h.cursorPick(bestMatch, schemaMatched, mockDb, statefulMocks)
			h.Logger.Debug("exact body match found", zap.String("mock name", bestMatch.Name))
			// Exact (byte-equal) body — nothing drifted, so no noise to detect,
			// unless what matched is a copy (see learn).
			served, claimed, err := h.claim(ctx, bestMatch, mockDb, learn(bestMatch, false), nil, rb)
			if err != nil {
				return false, nil, nil, err
			}
			if !claimed {
				continue
			}
			// Advance the cursor only now the response is actually served, so a
			// failed claim + retry does not skip a recorded response.
			if commitCursor != nil {
				commitCursor()
			}
			return true, served, nil, nil
		}

		// App-random → dependency-echo correlation: a mock whose request carries
		// an app-minted random value the dependency reflects back won't byte-match
		// (the live value differs). Select it by matching every non-correlated
		// field, capture the live value, and render it into the served response —
		// run BEFORE the lenient key-schema / fuzzy passes so a correlated mock is
		// not served stale or rejected by strict noise.
		if okC, bestC, bindings := h.correlationMatch(input.body, schemaMatched, mockCorrelation); okC {
			h.Logger.Debug("correlation match found", zap.String("mock name", bestC.Name))
			// A create whose answer echoes its id is correlated, so it is
			// matched here and not by the template pass below. When it is
			// made with an id of this run it takes its place in its stateful
			// group all the same (see the template pass).
			// What is learned is decided by what matched, before the copy
			// made for the cursor stands in for it.
			learned := learn(bestC, false)
			var commitCursor func()
			if i := slices.Index(schemaMatched, bestC); i >= 0 {
				if c, sent := rb.asSent(schemaMatched, i); c != nil {
					bestC, commitCursor = h.cursorPick(c, sent, mockDb, statefulMocks)
				}
			}
			served, claimed, err := h.claim(ctx, bestC, mockDb, learned, bindings, rb)
			if err != nil {
				return false, nil, nil, err
			}
			if !claimed {
				continue
			}
			if commitCursor != nil {
				commitCursor()
			}
			return true, served, nil, nil
		}

		// A create with the id the app minted this run: the request is a
		// recording's but for the ids that recording introduced (see
		// rebind.templateMatch). Run before the lenient passes, which would
		// have to tell two such recordings apart by how alike two random ids
		// happen to look — and cannot see a top-level JSON array at all.
		if tm, sent := rb.templateMatch(schemaMatched); tm != nil {
			// The same create recorded more than once (answered 201, then
			// 409) is a stateful group like any other: this request takes its
			// place in it, so that the next identical one gets the next
			// recording.
			tm, commitCursor := h.cursorPick(tm, sent, mockDb, statefulMocks)
			h.Logger.Debug("template match found", zap.String("mock name", tm.Name))
			served, claimed, err := h.claim(ctx, tm, mockDb, learn(tm, true), nil, rb)
			if err != nil {
				return false, nil, nil, err
			}
			if !claimed {
				continue
			}
			if commitCursor != nil {
				commitCursor()
			}
			return true, served, nil, nil
		}

		// A request that is a recorded one with this run's ids in place, but
		// for request-body noise (a read-back that stamps the time): matched
		// to that recording here. The exact pass needs the bodies byte for
		// byte, and the lenient passes below choose as they do without
		// rebinding, so neither would find it but by chance.
		if fit := rb.fitting(schemaMatched); fit != nil {
			h.Logger.Debug("rebound match found", zap.String("mock name", fit.Name))
			served, claimed, err := h.claim(ctx, fit, mockDb, learn(fit, true), nil, rb)
			if err != nil {
				return false, nil, nil, err
			}
			if !claimed {
				continue
			}
			return true, served, nil, nil
		}

		// The lenient passes choose among the recordings as they are, in the
		// pool's order, exactly as they do without rebinding: from what the
		// schema passes find among the recordings themselves. A copy carrying
		// this run's ids would turn the choice — it reads as a few edits
		// closer to the request — and would narrow it: an id in a URL path
		// matches its own recording's URL on the exact pass, where without
		// rebinding the dynamic-segment pass takes every recording of that
		// path.
		if rb != nil {
			asMain, err := h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise, urlNoise, false)
			if err != nil {
				return false, nil, nil, err
			}
			if len(asMain) == 0 && autoURLDynamic {
				if asMain, err = h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise, urlNoise, true); err != nil {
					return false, nil, nil, err
				}
			}
			if len(asMain) == 0 {
				return false, nil, &matchDiag{phase: models.MatchPhaseSchema, candidates: len(unfilteredMocks), pool: unfilteredMocks}, nil
			}
			schemaMatched = asMain
		}
		shortListed := schemaMatched
		// Schema match for JSON bodies
		if pkg.IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, schemaMatched, input.body)
			if err != nil {
				return false, nil, nil, err
			}

			// Strict enforcement (replay path): every candidate's request-body
			// fields must match the recorded body except the user's configured
			// body noise and any learned req_body_noise paths. A drift on a
			// non-noise field rejects that candidate, so a changed-but-unmarked
			// field fails the test instead of being silently served. This applies
			// even to candidates with no learned noise — strict mode value-checks
			// the whole body, not just the lenient key/schema match above.
			beforeStrict := len(bodyMatched)
			if schemaNoiseStrict {
				bodyMatched = h.filterStrictNoiseMatches(noiseEngine, bodyMatched, input.body, userBodyNoise)
			}

			if len(bodyMatched) == 0 {
				h.Logger.Debug("No mock found with body schema match")
				phase := models.MatchPhaseBody
				if beforeStrict > 0 {
					phase = models.MatchPhaseStrict
				}
				return false, nil, &matchDiag{phase: phase, candidates: len(unfilteredMocks), schemaMatched: rb.originals(schemaMatched), pool: unfilteredMocks}, nil
			}

			if len(bodyMatched) == 1 {
				h.Logger.Debug("body match found", zap.String("mock name", bodyMatched[0].Name))
				served, claimed, err := h.claim(ctx, bodyMatched[0], mockDb, learn(bodyMatched[0], true), nil, rb)
				if err != nil {
					return false, nil, nil, err
				}
				if !claimed {
					continue
				}
				return true, served, nil, nil
			}

			// More than one match, perform fuzzy match
			shortListed = bodyMatched
		}

		h.Logger.Debug("Performing fuzzy match for req buffer")
		// Perform fuzzy match on the request
		isMatched, bestMatch := h.PerformFuzzyMatch(shortListed, input.raw)
		if isMatched {
			h.Logger.Debug("fuzzy match found a matching mock", zap.String("mock name", bestMatch.Name))
			served, claimed, err := h.claim(ctx, bestMatch, mockDb, learn(bestMatch, true), nil, rb)
			if err != nil {
				return false, nil, nil, err
			}
			if !claimed {
				continue
			}
			return true, served, nil, nil
		}
		return false, nil, &matchDiag{phase: models.MatchPhaseExhausted, candidates: len(unfilteredMocks), schemaMatched: rb.originals(shortListed), pool: unfilteredMocks}, nil
	}
}

// cursorPick advances a stateful dependency through its recorded responses. When
// stateful mocks are enabled and bestMatch is a cursor-consumption mock
// (ConsumeCursorSaturate), it gathers every schema-matched mock carrying the
// SAME request (method + URL + body), orders them by record time, and returns
// the cursor-th one so repeated identical requests get served 1,2,3,… and then
// saturate on the last. It returns a commit closure the caller must call once
// the response is actually served (after a successful claim), so a failed/
// retried match does not skip a recording; commit is nil when no cursoring
// applies. Returns bestMatch unchanged (commit nil) when stateful mocks are off,
// the store exposes no cursor, or there is only one recorded response for the
// request (the common case, where cursor == reuse — byte-identical to legacy).
//
// Scope: the group is gathered by BYTE-EXACT request body, which covers stateful
// dependencies whose repeated requests are identical (a counter, a created-then-
// read row). A stateful request that carries a noisy/rotating field in its body
// (so the recordings are NOT byte-equal) is not grouped here and replays as the
// first recording; that case is handled by request→response correlation (masked
// grouping), tracked separately — see correlation-auto-replay. This is a known,
// documented limit, not a silent catch-all.
func (h *HTTP) cursorPick(bestMatch *models.Mock, schemaMatched []*models.Mock, mockDb integrations.MockMemDb, statefulMocks bool) (*models.Mock, func()) {
	if !statefulMocks || bestMatch == nil || bestMatch.TestModeInfo.Consume != models.ConsumeCursorSaturate {
		return bestMatch, nil
	}
	if bestMatch.Spec.HTTPReq == nil {
		return bestMatch, nil
	}
	cs, ok := mockDb.(integrations.MockCursor)
	if !ok {
		return bestMatch, nil
	}
	wantMethod := bestMatch.Spec.HTTPReq.Method
	wantURL := bestMatch.Spec.HTTPReq.URL
	wantBody := bestMatch.Spec.HTTPReq.Body
	group := make([]*models.Mock, 0, 4)
	for _, m := range schemaMatched {
		if m == nil || m.Spec.HTTPReq == nil {
			continue
		}
		// Same request = same ConsumeCursorSaturate class, method, URL and body.
		// Method+URL matter because the auto-dynamic URL pass can schema-match
		// recordings of DIFFERENT endpoints that share a body; grouping those
		// would cursor across unrelated resources.
		if m.TestModeInfo.Consume == models.ConsumeCursorSaturate &&
			m.Spec.HTTPReq.Method == wantMethod &&
			m.Spec.HTTPReq.URL == wantURL &&
			m.Spec.HTTPReq.Body == wantBody {
			group = append(group, m)
		}
	}
	if len(group) <= 1 {
		return bestMatch, nil // single recording: nothing to advance through
	}
	// Record order: request timestamp is stable across replay; SortOrder is
	// mutated on every match so it cannot be used here. ID then Name break ties.
	sort.SliceStable(group, func(i, j int) bool {
		ti, tj := group[i].Spec.ReqTimestampMock, group[j].Spec.ReqTimestampMock
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if group[i].TestModeInfo.ID != group[j].TestModeInfo.ID {
			return group[i].TestModeInfo.ID < group[j].TestModeInfo.ID
		}
		return group[i].Name < group[j].Name
	})
	key := cursorKey(string(wantMethod), wantURL, wantBody)
	idx := cs.MockCursorIndex(key, len(group))
	if idx < 0 || idx >= len(group) {
		return bestMatch, nil
	}
	n := len(group)
	return group[idx], func() { cs.AdvanceMockCursor(key, idx, n) }
}

// cursorKey builds a stable per-request key so repeated identical requests share
// one cursor. Method + URL + a hash of the request body is enough: the group is
// gathered by the same method/URL/body, so the key must agree with it.
func cursorKey(method, url, body string) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(url)
	b.WriteByte('\x00')
	sum := sha256.Sum256([]byte(body))
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.String()
}

// FilterHTTPMocks Filter mocks to only HTTP mocks
func FilterHTTPMocks(mocks []*models.Mock) []*models.Mock {
	var httpMocks []*models.Mock
	for _, mock := range mocks {
		if mock.Kind != models.Kind(models.HTTP) {
			continue
		}
		httpMocks = append(httpMocks, mock)
	}
	return httpMocks
}

// MatchBodyType Body type match check (content type matching)
func (h *HTTP) MatchBodyType(mockBody string, reqBody []byte) bool {
	if mockBody == "" && string(reqBody) == "" {
		return true
	}
	mockBodyType := pkg.GuessContentType([]byte(mockBody))
	reqBodyType := pkg.GuessContentType(reqBody)
	h.Logger.Debug("mock body type", zap.Any("mock body type", mockBodyType), zap.Any("req body type", reqBodyType))
	return mockBodyType == reqBodyType
}

func (h *HTTP) MatchURLPath(mockURL, reqPath string, urlNoise []string, autoDynamic bool) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	h.Logger.Debug("parsed URL", zap.Any("parsed URL", parsedURL.Path), zap.Any("req path", reqPath))
	mockPath := parsedURL.Path
	if mockPath == reqPath {
		return true
	}
	// URL-path noise (test.globalNoise.url): the only request field keploy
	// matched by EXACT value with no noise hook — so a non-deterministic path
	// segment (an id / uuid / timestamp / object key that changes every run,
	// e.g. S3 PutObject receipts/<user>/<uuid>.txt) rejected the recorded mock
	// and produced a "no matching mock -> 502" on replay. Bring the URL path in
	// line with the key+noise model used for headers/body: replace every
	// configured noise pattern with a placeholder in BOTH the mock path and the
	// live request path, then compare. A variable segment thus matches while the
	// rest of the path stays strict. No url noise configured => exact match as
	// before (fully backward compatible).
	//
	// SCOPE PATTERNS TO THEIR PATH CONTEXT. A pattern is applied as a substring
	// replace over the whole path, so a BARE value pattern over-matches: "[0-9]+"
	// wildcards EVERY numeric run — a /users/55 id, a sibling /orders/100 id, and
	// even the "1" inside /v1 — which can collapse distinct calls onto one mock.
	// Anchor it to the surrounding path instead:
	//   "/users/[0-9]+"  -> wildcards only the user-id segment; /orders/100 and
	//                       /v1 stay strict, so a different order or version still
	//                       does NOT match.
	// UUIDs/hashes are specific enough to use unanchored. Whole-path substring
	// replacement is deliberate — it is what lets a partial-segment key like
	// "<uuid>.txt" match; the trade-off is that bare value patterns need
	// anchoring (see TestMatchURLPath_NumericIDScoping).
	if len(urlNoise) > 0 {
		noiseRes := compileURLNoise(h.Logger, urlNoise)
		if maskURLNoise(mockPath, noiseRes) == maskURLNoise(reqPath, noiseRes) {
			return true
		}
	}
	// Auto-detected dynamic segments — the zero-config DEFAULT. Used only as a
	// fallback (autoDynamic is set on the second matching pass, after an exact +
	// url-noise pass found nothing), so deterministic and genuinely-distinct calls
	// are never relaxed. A differing segment is wildcarded only when it looks like
	// a machine id on BOTH sides (see looksDynamicSegment) and every other segment
	// is identical — so a non-deterministic id (numeric/uuid/hash/long token)
	// matches without any config, while a different resource still does not.
	// Disable via OutgoingOptions.DisableAutoURLDynamic. The url-noise config above
	// covers corner cases the heuristic intentionally leaves alone (e.g. a
	// word-like variable slug).
	if autoDynamic && pathMatchesModuloDynamicSegments(mockPath, reqPath) {
		return true
	}
	return false
}

// pathMatchesModuloDynamicSegments reports whether mockPath and reqPath are
// identical except for one or more segments that look like machine-generated ids
// on both sides. Same segment count is required, and every non-id segment must be
// exactly equal, so it relaxes only the id-shaped positions.
func pathMatchesModuloDynamicSegments(mockPath, reqPath string) bool {
	ms := strings.Split(mockPath, "/")
	rs := strings.Split(reqPath, "/")
	if len(ms) != len(rs) {
		return false
	}
	differed := false
	for i := range ms {
		if ms[i] == rs[i] {
			continue
		}
		// Relax a differing segment only when BOTH sides are the SAME dynamic
		// type-class — so /users/123 matches /users/456 (digits) but NOT
		// /users/<uuid>: a segment that changed TYPE is a different resource, not
		// the same id drifting. Was "both look dynamic (any shape)", which let a
		// numeric id match a uuid/hash and collapse distinct calls onto one mock.
		//
		// The class is derived from each value's characters, so it is a proxy for
		// "same id type", not a guarantee: a short (~16-char) hex-alphabet id that
		// is coincidentally all-decimal on one side can class differently (digits
		// vs hex) and false-reject. That is the loud, safe direction (a replay
		// "mock missed" you can see), traded against the silent false-accept — the
		// wrong mock served — that the old any-shape relax allowed. Longer
		// hashes/uuids/nanoids are not realistically affected.
		if c := urlSegmentTypeClass(ms[i]); c != "" && c == urlSegmentTypeClass(rs[i]) {
			differed = true
			continue
		}
		return false // a non-id segment, or a type change, -> genuinely different path
	}
	return differed
}

// urlNoisePlaceholder is what a url-noise match is replaced with on BOTH the
// recorded and the live side before they are compared, so a covered substring
// stops distinguishing the two.
const urlNoisePlaceholder = "{{keploy.urlnoise}}"

// urlNoiseCache memoizes compiled url-noise patterns. MatchURLPath and
// QueryParamsMatch both run once per CANDIDATE MOCK per proxied request, so
// compiling the configured patterns inline cost O(mocks x patterns)
// regexp.Compile calls on every request — measured at ~12us and ~18KB of
// garbage per QueryParamsMatch call with five patterns configured, against
// ~240ns with none. The patterns come from config (test.globalNoise.url) and
// are fixed for the process lifetime, so compile each one once and share it.
// An invalid pattern is cached as a nil entry: it is logged once and never
// re-attempted.
var urlNoiseCache sync.Map // pattern string -> *regexp.Regexp (nil = invalid)

// compileURLNoise returns the compiled form of the configured url-noise
// patterns, skipping (and logging once) any that do not compile.
func compileURLNoise(logger *zap.Logger, urlNoise []string) []*regexp.Regexp {
	if len(urlNoise) == 0 {
		return nil
	}
	res := make([]*regexp.Regexp, 0, len(urlNoise))
	for _, pat := range urlNoise {
		if cached, ok := urlNoiseCache.Load(pat); ok {
			if re, _ := cached.(*regexp.Regexp); re != nil {
				res = append(res, re)
			}
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			logger.Debug("skipping invalid url-noise regex", zap.String("pattern", pat), zap.Error(err))
			urlNoiseCache.Store(pat, (*regexp.Regexp)(nil))
			continue
		}
		urlNoiseCache.Store(pat, re)
		res = append(res, re)
	}
	return res
}

// maskURLNoise replaces every url-noise match in s with the placeholder.
func maskURLNoise(s string, noiseRes []*regexp.Regexp) string {
	for _, re := range noiseRes {
		s = re.ReplaceAllString(s, urlNoisePlaceholder)
	}
	return s
}

var (
	reSegAllDigits = regexp.MustCompile(`^[0-9]+$`)
	reSegUUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reSegLongHex   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

// looksDynamicSegment reports whether a single URL path segment looks like a
// machine-generated identifier that legitimately varies between record and
// replay. Kept CONSERVATIVE (precision over recall): it covers the unambiguous
// shapes — all-digit ids/timestamps, UUIDs, long hex hashes / Mongo ObjectIds,
// and LONG (>=16) tokens that mix letters and digits (base62/ULID-ish ids and
// composite keys like "amit_1781794443438_47ona3" or "<uuid>.txt"). It does NOT
// match plain alphabetic segments ("users", "profile"), short composite tokens
// ("v1alpha1", "oauth2"), or word-like slugs — all ambiguous with static path
// components and left to explicit url-noise config (test.globalNoise.url).
func looksDynamicSegment(s string) bool {
	return urlSegmentTypeClass(s) != ""
}

// urlSegmentTypeClass classifies a dynamic-looking URL path segment into a stable
// type-class, or "" when the segment is not dynamic-looking. The classes mirror
// looksDynamicSegment's recognized shapes so a LEARNED-dynamic segment can be
// matched on TYPE (its value ignored, its class required to match) instead of the
// value being re-guessed per request. Returning "" for every non-dynamic shape
// keeps looksDynamicSegment byte-for-byte equivalent. See looksDynamicSegment for
// why each shape is (or isn't) treated as dynamic.
func urlSegmentTypeClass(s string) string {
	switch {
	case s == "":
		return ""
	case reSegAllDigits.MatchString(s):
		return "digits"
	case reSegUUID.MatchString(s):
		return "uuid"
	case reSegLongHex.MatchString(s):
		return "hex"
	}
	if len(s) >= 16 {
		hasDigit, hasAlpha := false, false
		for _, r := range s {
			switch {
			case r >= '0' && r <= '9':
				hasDigit = true
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
				hasAlpha = true
			}
		}
		if hasDigit && hasAlpha {
			return "token"
		}
	}
	return ""
}

// relaxed header key matcher (presence-only)
func (h *HTTP) HeadersContainKeys(expected map[string]string, actual http.Header, headerNoise map[string][]string) bool {
	shouldIgnore := func(k string) bool {
		lk := strings.ToLower(k)
		// Ignore keploy headers
		if strings.HasPrefix(lk, "keploy") {
			return true
		}
		// Ignore headers that are in noise configuration
		if headerNoise != nil {
			if _, exists := headerNoise[lk]; exists {
				return true
			}
		}
		return false
	}

	// Build a case-insensitive set of actual header keys
	actualKeys := make(map[string]struct{}, len(actual))
	for k := range actual {
		actualKeys[strings.ToLower(k)] = struct{}{}
	}

	// Ensure every non-ignored expected key exists in the request
	for k := range expected {
		if shouldIgnore(k) {
			h.Logger.Debug("header key is ignored", zap.String("header key", k))
			continue
		}
		if _, ok := actualKeys[strings.ToLower(k)]; !ok {
			return false
		}
	}
	return true
}

// queryValueTypeClass classifies a query-param VALUE into the dynamic
// type-class it belongs to ("digits" / "uuid" / "hex" / "token"), or "" when it
// is not machine-generated enough to treat as dynamic. It mirrors the path
// classifier urlSegmentTypeClass but is deliberately STRICTER about bare
// integers: in a path, position gives a number its meaning (/users/55 is
// unmistakably an id), whereas a bare small integer in a query is overwhelmingly
// a page / limit / offset / count, and collapsing ?page=2 onto ?page=3 would
// re-open the very "wrong recorded response" bug this gate exists to close. So a
// digit run counts as dynamic ("digits") only when it is LONG (epoch
// seconds/millis, snowflake ids and the like); a short one is static ("").
//
// The class is what makes the auto-dynamic query relaxation TYPE-AWARE: a
// differing value is tolerated only when both sides share the same class, so a
// rotating ?id=<uuid> no longer collapses onto a numeric ?id=<10-digit> (a
// different value space, hence a different resource).
const minDynamicQueryDigits = 10

func queryValueTypeClass(s string) string {
	if reSegAllDigits.MatchString(s) {
		if len(s) >= minDynamicQueryDigits {
			return "digits"
		}
		return "" // short bare integer: page / limit / offset — never dynamic
	}
	return urlSegmentTypeClass(s)
}

// looksDynamicQueryValue reports whether a query-param VALUE looks like a
// machine-generated token that legitimately varies between record and replay.
// It is the boolean view of queryValueTypeClass and stays byte-for-byte
// equivalent to "class != \"\"".
func looksDynamicQueryValue(s string) bool {
	return queryValueTypeClass(s) != ""
}

// maskAndSort applies url noise to every member and sorts the result, so two
// value sets can be compared order-independently.
func maskAndSort(vals []string, noiseRes []*regexp.Regexp) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = maskURLNoise(v, noiseRes)
	}
	sort.Strings(out)
	return out
}

// QueryParamsMatch reports whether the recorded query params (mockParams) and the
// live request's query (reqQuery) match on BOTH key set AND value.
//
// MapsHaveSameKeys — the previous gate here — compared only key presence/counts,
// never VALUES: a mock recorded for /search?id=A would satisfy a live request for
// /search?id=B, so a distinct query could be served the wrong recorded response.
// QueryParamsMatch keeps the exact same bidirectional key-set contract (ignoring
// keploy-prefixed keys) and additionally compares each shared key's value.
//
// Values are compared in the recorded form: pkg.URLParams stores a recorded param
// as ", "-joined (map[string]string), so the live side is joined the same way
// (strings.Join(reqQuery[key], ", ")). Note this join is lossy — ?q=a,%20b (one
// value) and ?q=a&q=b (two values) both record as "a, b" and therefore compare
// equal. That ambiguity predates this gate (it is inherent to pkg.URLParams) and
// only bounds how strict the value check can be; it never makes a genuinely
// different value match.
//
// urlNoise is applied to BOTH sides before comparison using the same
// compile-and-replace-with-placeholder approach MatchURLPath uses, so a param
// value covered by url noise still matches while a genuinely different value does
// not. The patterns are compiled lazily and memoized (see compileURLNoise): the
// exact fast path settles every param of a request that did not drift, so a
// configured noise list costs nothing until a value actually differs.
//
// A REPEATED param (same key more than once) is compared ORDER-INDEPENDENTLY:
// the recorded ", "-joined value is split back into its members, each side is
// url-noise-masked per member and sorted before comparison. This
// mirrors pkg.CompareMultiValueHeaders so repeated query params and repeated
// headers agree — a reorder of the same values (e.g. ?tag=a&tag=b vs
// ?tag=b&tag=a, which are the same request per HTTP) still matches, while a
// genuinely different value does not. Single-value params (the common case) take
// the exact fast path and are unaffected.
//
// autoDynamic mirrors MatchURLPath's auto-detected dynamic path segments and is
// the zero-config DEFAULT for values, set only on SchemaMatch's SECOND pass —
// after an exact + url-noise pass over every mock found nothing, i.e. when the
// alternative is a "no matching mock" 502. Without it a rotating value that
// needs no config in the path (/items/<uuid>) would hard-fail in the query
// (?id=<uuid>), which is the same non-deterministic-id 502 that pass 2 exists to
// prevent. A differing member is tolerated only when it looks machine-generated
// AND shares the same dynamic type-class on BOTH sides (see
// queryValueTypeClass), so deterministic queries, genuinely-distinct values, and
// cross-type drift (a uuid vs a number) are never relaxed. Disable via
// OutgoingOptions.DisableAutoURLDynamic.
func (h *HTTP) QueryParamsMatch(mockParams map[string]string, reqQuery url.Values, urlNoise []string, autoDynamic bool) bool {
	shouldIgnore := func(key string) bool {
		return strings.HasPrefix(strings.ToLower(key), "keploy")
	}

	// Enforce the same bidirectional key-set contract as MapsHaveSameKeys.
	mockCount := 0
	for key := range mockParams {
		if !shouldIgnore(key) {
			mockCount++
		}
	}
	reqCount := 0
	for key := range reqQuery {
		if !shouldIgnore(key) {
			reqCount++
		}
	}
	if mockCount != reqCount {
		return false
	}
	for key := range mockParams {
		if shouldIgnore(key) {
			continue
		}
		if _, exists := reqQuery[key]; !exists {
			return false
		}
	}
	for key := range reqQuery {
		if shouldIgnore(key) {
			continue
		}
		if _, exists := mockParams[key]; !exists {
			return false
		}
	}

	// Value compare for each shared, non-keploy key.
	var (
		noiseRes      []*regexp.Regexp
		noiseCompiled bool
	)
	for key, recorded := range mockParams {
		if shouldIgnore(key) {
			continue
		}
		actual := strings.Join(reqQuery[key], ", ")
		if recorded == actual {
			continue // exact match — covers every single-value param
		}
		// Something drifted on this key: compile the noise patterns now (once
		// per call, memoized process-wide) rather than on every candidate mock.
		if !noiseCompiled {
			noiseRes, noiseCompiled = compileURLNoise(h.Logger, urlNoise), true
		}
		// Values differ as-joined. Compare order-independently: split the
		// recorded value back into members, mask each side with url noise, and
		// sort before comparing (mirrors CompareMultiValueHeaders). A reorder
		// of the same values matches; a real difference still fails.
		recVals := strings.Split(recorded, ", ")
		liveVals := reqQuery[key]
		if len(recVals) != len(liveVals) {
			return false
		}
		rv, av := maskAndSort(recVals, noiseRes), maskAndSort(liveVals, noiseRes)
		for i := range rv {
			if rv[i] == av[i] {
				continue
			}
			// Fallback only (pass 2): tolerate a member that looks
			// machine-generated on BOTH sides AND shares the SAME dynamic
			// type-class — so a rotating ?id=<uuid> matches another uuid but NOT a
			// numeric ?id=<10-digit>: a value that changed TYPE is a different
			// resource, not the same id drifting (mirrors MatchURLPath's
			// type-aware path relax). Was "both look dynamic (any shape)", which
			// let a uuid collapse onto a long int / hash and serve the wrong
			// recorded response. Sorting can misalign two multi-value sets that
			// each carry a dynamic member, so this is a best-effort relaxation for
			// repeated params; the single-value case (len 1, the one that matters)
			// is exact.
			//
			// Same proxy-not-guarantee trade-off as the path classifier: a
			// ~16-char hex id coincidentally all-decimal on one side can class
			// differently and false-reject (~1/900, negligible for longer ids) —
			// the loud, safe direction versus the silent wrong-mock this replaces.
			if c := queryValueTypeClass(rv[i]); autoDynamic && c != "" && c == queryValueTypeClass(av[i]) {
				h.Logger.Debug("http query: value treated as auto-detected dynamic",
					zap.String("param", key),
					zap.String("mock value", rv[i]),
					zap.String("request value", av[i]),
					zap.String("type class", c))
				continue
			}
			return false
		}
	}

	return true
}

func (h *HTTP) SchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock, headerNoise map[string][]string, urlNoise []string, autoDynamic bool) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	// Parse the live query once, not once per candidate mock: url.Query()
	// re-parses and re-allocates the whole query string on every call and this
	// loop runs over the full mock set on every proxied request.
	reqQuery := input.url.Query()

	for _, mock := range unfilteredMocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Content type check — only enforced when both the request and
		// the mock specify a Content-Type. Compares only the media type
		// (ignoring parameters like charset) so that trivial differences
		// such as "application/json" vs "application/json;charset=UTF-8"
		// don't prevent a match. If either side omits Content-Type,
		// matching falls through to the other criteria.
		inputCTValues := input.header.Values("Content-Type")
		mockCT := mock.Spec.HTTPReq.Header["Content-Type"]
		if len(inputCTValues) > 0 && mockCT != "" {
			mockMediaTypes := parseMediaTypes(mockCT)
			inputMediaTypes := parseMediaTypes(strings.Join(inputCTValues, ","))
			if len(mockMediaTypes) == 0 || len(inputMediaTypes) == 0 || !mediaTypesOverlap(mockMediaTypes, inputMediaTypes) {
				h.Logger.Debug("The content type of mock and request aren't the same",
					zap.String("mock name", mock.Name),
					zap.Strings("input media types", inputMediaTypes),
					zap.Strings("mock media types", mockMediaTypes))
				continue
			}
		}
		// Body type check
		if !h.MatchBodyType(mock.Spec.HTTPReq.Body, input.body) {
			h.Logger.Debug("The body of mock and request aren't of same type", zap.String("mock name", mock.Name))
			continue
		}

		// URL path match
		if !h.MatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path, urlNoise, autoDynamic) {
			h.Logger.Debug("The url path of mock and request aren't the same", zap.String("mock name", mock.Name), zap.Any("input url", input.url.Path), zap.Any("mock url", mock.Spec.HTTPReq.URL))
			continue
		}

		// HTTP method match
		if mock.Spec.HTTPReq.Method != models.Method(input.method) {
			h.Logger.Debug("The method of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		// Header key match (presence-only; extra request headers allowed)
		if !h.HeadersContainKeys(mock.Spec.HTTPReq.Header, input.header, headerNoise) {
			h.Logger.Debug("headers missing required keys for mock name",
				zap.String("mock name", mock.Name),
				zap.Any("expected header keys", mock.Spec.HTTPReq.Header),
				zap.Any("input header", input.header))
			continue
		}

		// Query parameter match (key set AND value; url noise aware)
		if !h.QueryParamsMatch(mock.Spec.HTTPReq.URLParams, reqQuery, urlNoise, autoDynamic) {
			h.Logger.Debug("The query params of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

// ExactBodyMatch performs exact body matching with noise awareness.
// First pass: fast string equality.
// Second pass: noise-aware comparison that skips obfuscated fields identified
// by Mock.Noise patterns. Three body shapes are handled in the second pass:
//  1. A mock body that is itself entirely a noise value — auto-match.
//  2. application/x-www-form-urlencoded — per-segment noise check (see
//     formBodiesMatchModuloNoise). Lets a noise regex of the shape
//     ^<key>=[^&]+$ wildcard a rotating field (IRSA WebIdentityToken=…,
//     OAuth client_assertion=…, etc.) without depending on the SDK to
//     produce byte-identical request bodies at replay.
//  3. JSON — field-by-field comparison via JSONBodyMatchScore.
func (h *HTTP) ExactBodyMatch(body []byte, schemaMatched []*models.Mock) (bool, *models.Mock) {
	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(schemaMatched))
	for i, mock := range schemaMatched {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for exact body match", zap.Strings("mock names", mockNames), zap.String("req body", string(body)))

	// First pass: exact string match (fastest path)
	for _, mock := range schemaMatched {
		if mock.Spec.HTTPReq.Body == string(body) {
			h.Logger.Debug("http mock matched",
				zap.String("mock", mock.Name),
				zap.Float64("match_percentage", 100.0),
				zap.String("match_type", "exact_body"))
			return true, mock
		}
	}

	// Second pass: noise-aware match for mocks with obfuscated values.
	// Pre-compute request-side properties once so we don't re-derive them per
	// candidate mock: JSON unmarshaling for the JSON-noise path, and the
	// form-encoded heuristic (which itself runs url.ParseQuery) for the
	// form-body path.
	reqBodyStr := string(body)
	isReqJSON := pkg.IsJSON(body)
	var reqData interface{}
	if isReqJSON {
		if err := json.Unmarshal(body, &reqData); err != nil {
			isReqJSON = false
		}
	}
	reqIsForm := !isReqJSON && looksLikeFormEncoded(reqBodyStr)

	for _, mock := range schemaMatched {
		nc := util.NewNoiseChecker(mock.Noise)
		if nc == nil {
			continue // no noise patterns → already checked in first pass
		}

		mockBody := mock.Spec.HTTPReq.Body

		// If the entire body is a single noisy value, auto-match
		// (schema match already filtered by URL, method, headers)
		if nc.IsNoisy(mockBody) {
			h.Logger.Debug("http mock matched",
				zap.String("mock", mock.Name),
				zap.Float64("match_percentage", 100.0),
				zap.Int("noisy_fields_skipped", 1),
				zap.String("match_type", "exact_body_fully_noisy"))
			return true, mock
		}

		// Form-encoded noise-aware comparison. Each "key=value" segment is
		// tested against the mock's noise patterns; a match wildcards that
		// segment. The remaining keys must be the same set on both sides
		// and each non-wildcarded value(s) must be byte-equal.
		if reqIsForm && looksLikeFormEncoded(mockBody) {
			if formBodiesMatchModuloNoise(mockBody, reqBodyStr, nc) {
				h.Logger.Debug("http mock matched",
					zap.String("mock", mock.Name),
					zap.Float64("match_percentage", 100.0),
					zap.String("match_type", "exact_body_form_noise_aware"))
				return true, mock
			}
			// Mock body is form-encoded — JSON path can't fire, skip to
			// the next mock.
			continue
		}

		// JSON-level comparison skipping noisy fields
		if !isReqJSON || !pkg.IsJSON([]byte(mockBody)) {
			continue
		}

		var mockData interface{}
		if err := json.Unmarshal([]byte(mockBody), &mockData); err != nil {
			continue
		}

		matched, total, noisy := util.JSONBodyMatchScore(mockData, reqData, nc)

		pct := 100.0
		if total > 0 {
			pct = float64(matched) / float64(total) * 100
		}
		h.Logger.Debug("http mock match score (noise-aware)",
			zap.String("mock", mock.Name),
			zap.Int("matched_fields", matched),
			zap.Int("total_fields", total),
			zap.Int("noisy_fields_skipped", noisy),
			zap.Float64("match_percentage", pct))

		if matched == total {
			// Verify the request has no extra non-noisy keys beyond
			// what the mock defines — otherwise this isn't truly exact.
			if !util.HasExtraNonNoisyKeys(mockData, reqData, nc) {
				return true, mock
			}
		}
	}

	return false, nil
}

func (h *HTTP) bodyMatch(mockBody, reqBody []byte) (bool, error) {

	var mockData map[string]any
	var reqData map[string]any
	err := json.Unmarshal(mockBody, &mockData)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to unmarshal the mock request body", zap.String("Req", string(mockBody)))
		return false, err
	}
	err = json.Unmarshal(reqBody, &reqData)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to unmarshal the request body", zap.String("Req", string(reqBody)))
		return false, err
	}

	for key := range mockData {
		_, exists := reqData[key]
		if !exists {
			return false, nil
		}
	}
	return true, nil
}

// PerformBodyMatch Perform body match for JSON data
func (h *HTTP) PerformBodyMatch(ctx context.Context, schemaMatched []*models.Mock, reqBody []byte) ([]*models.Mock, error) {
	h.Logger.Debug("Performing schema match for body")

	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(schemaMatched))
	for i, mock := range schemaMatched {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for PerformBodyMatch function", zap.Strings("mock names", mockNames))

	var bodyMatched []*models.Mock
	for _, mock := range schemaMatched {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		ok, err := h.bodyMatch([]byte(mock.Spec.HTTPReq.Body), reqBody)
		if err != nil {
			h.Logger.Error("failed to do schema matching on request body", zap.Error(err))
			break
		}

		if ok {
			bodyMatched = append(bodyMatched, mock)
			h.Logger.Debug("found a mock with body schema match", zap.String("mock name", mock.Name))
		}
	}

	h.Logger.Debug("http mock body key match results",
		zap.Int("body_key_matched", len(bodyMatched)),
		zap.Int("schema_matched", len(schemaMatched)))

	return bodyMatched, nil
}

// findStringMatch returns the index of the closest mock string and the
// Levenshtein distance so callers don't need to recompute it.
func (h *HTTP) findStringMatch(req string, mockStrings []string) (int, int) {
	minDist := int(^uint(0) >> 1)
	bestMatch := -1
	for idx, mock := range mockStrings {
		if !util.IsASCII(mock) {
			continue
		}
		dist := levenshtein.ComputeDistance(req, mock)
		if dist == 0 {
			return idx, 0
		}
		if dist < minDist {
			minDist = dist
			bestMatch = idx
		}
	}
	return bestMatch, minDist
}

// jaccardBestMatch finds the mock body with the highest Jaccard similarity
// to reqBuff. mockBodies are pre-decoded/stripped byte slices so the caller
// controls noise handling. Returns the best index and similarity score.
func (h *HTTP) jaccardBestMatch(mockBodies [][]byte, reqBuff []byte) (int, float64) {
	mxSim := -1.0
	mxIdx := -1
	k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
	reqShingles := util.CreateShingles(reqBuff, k)
	for idx, body := range mockBodies {
		mockShingles := util.CreateShingles(body, k)
		similarity := util.JaccardSimilarity(mockShingles, reqShingles)
		if similarity > mxSim {
			mxSim = similarity
			mxIdx = idx
		}
	}
	return mxIdx, mxSim
}

// findBinaryMatch decodes mock bodies and delegates to jaccardBestMatch.
func (h *HTTP) findBinaryMatch(mocks []*models.Mock, reqBuff []byte) int {
	bodies := make([][]byte, len(mocks))
	for i, mock := range mocks {
		bodies[i], _ = decode(mock.Spec.HTTPReq.Body)
	}
	idx, _ := h.jaccardBestMatch(bodies, reqBuff)
	return idx
}

// PerformFuzzyMatch performs fuzzy matching on the request body.
// Noisy (obfuscated) values are stripped from mock bodies before computing
// similarity so that redacted padding doesn't skew the score.
func (h *HTTP) PerformFuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(tcsMocks))
	for i, mock := range tcsMocks {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for performfuzzyMatch function", zap.Strings("mock names", mockNames))

	encodedReq := encode(reqBuff)
	for _, mock := range tcsMocks {
		encodedMock, _ := decode(mock.Spec.HTTPReq.Body)
		if string(encodedMock) == string(reqBuff) || mock.Spec.HTTPReq.Body == encodedReq {
			h.Logger.Debug("http mock matched",
				zap.String("mock", mock.Name),
				zap.Float64("match_percentage", 100.0),
				zap.String("match_type", "fuzzy_exact"))
			return true, mock
		}
	}

	// Build mock body strings, stripping noisy values for fair comparison
	mockStrings := make([]string, len(tcsMocks))
	for i := range tcsMocks {
		nc := util.NewNoiseChecker(tcsMocks[i].Noise)
		mockStrings[i] = util.StripNoisyJSON(tcsMocks[i].Spec.HTTPReq.Body, nc)
	}

	// String-based fuzzy matching (Levenshtein distance)
	reqStr := string(reqBuff)
	if util.IsASCII(reqStr) {
		idx, dist := h.findStringMatch(reqStr, mockStrings)
		if idx != -1 {
			maxLen := len(reqStr)
			if len(mockStrings[idx]) > maxLen {
				maxLen = len(mockStrings[idx])
			}
			pct := 0.0
			if maxLen > 0 {
				pct = (1.0 - float64(dist)/float64(maxLen)) * 100
			}
			h.Logger.Debug("http mock matched",
				zap.String("mock", tcsMocks[idx].Name),
				zap.Float64("match_percentage", pct),
				zap.String("match_type", "fuzzy_levenshtein"))
			return true, tcsMocks[idx]
		}
	}

	// Binary fuzzy matching (Jaccard similarity) with stripped mock bodies
	mockBodies := make([][]byte, len(mockStrings))
	for i := range mockStrings {
		mockBodies[i] = []byte(mockStrings[i])
	}
	mxIdx, mxSim := h.jaccardBestMatch(mockBodies, reqBuff)
	if mxIdx != -1 {
		h.Logger.Debug("http mock matched",
			zap.String("mock", tcsMocks[mxIdx].Name),
			zap.Float64("match_percentage", mxSim*100),
			zap.String("match_type", "fuzzy_jaccard"))
		return true, tcsMocks[mxIdx]
	}
	return false, nil
}

// parseMediaTypes splits a (possibly comma-joined) Content-Type header
// value into individual media types using mime.ParseMediaType. Malformed
// entries are skipped so that a single non-conformant value (e.g. a
// trailing semicolon or vendor quirk) does not prevent matching on the
// remaining valid types.
func parseMediaTypes(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		ct := strings.TrimSpace(part)
		if ct == "" {
			continue
		}
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt == "" {
			continue
		}
		out = append(out, mt)
	}
	return out
}

// mediaTypesOverlap returns true if any media type in a matches any in b
// (case-insensitive).
func mediaTypesOverlap(a, b []string) bool {
	for _, ma := range a {
		for _, mb := range b {
			if strings.EqualFold(ma, mb) {
				return true
			}
		}
	}
	return false
}

// looksLikeFormEncoded heuristically classifies body as application/
// x-www-form-urlencoded payload. Used by the noise-aware ExactBodyMatch
// pass to decide whether to take the form-segment path. Heuristic mirrors
// enterprise/pkg/secret's same-named helper: must contain '=', must not
// start with a JSON or XML marker, and must parse via url.ParseQuery.
func looksLikeFormEncoded(body string) bool {
	if !strings.Contains(body, "=") {
		return false
	}
	trimmed := strings.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '<' || trimmed[0] == '{' || trimmed[0] == '[') {
		return false
	}
	_, err := url.ParseQuery(body)
	return err == nil
}

// valueHasUnixTimestamp reports whether s contains a decimal run that looks
// like a modern unix timestamp — 10 digits in [1_500_000_000, 2_500_000_000]
// (seconds, 2017-07-14 → 2049-03-22) or 13 digits in
// [1_500_000_000_000, 2_500_000_000_000] (milliseconds, same range). Used
// by formBodiesMatchModuloNoise to wildcard form segments whose value
// embeds a record-time timestamp the recorder didn't tag as noise.
//
// The 10/13-digit gate is deliberate: 11- and 12-digit runs (e.g. 12-digit
// AWS account IDs) are not plausible unix timestamps in either unit and
// must not be wildcarded.
func valueHasUnixTimestamp(s string) bool {
	const (
		secMin uint64 = 1_500_000_000
		secMax uint64 = 2_500_000_000
		msMin  uint64 = 1_500_000_000_000
		msMax  uint64 = 2_500_000_000_000
	)
	n := len(s)
	for i := 0; i < n; {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		j := i
		for j < n && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		runLen := j - i
		if runLen == 10 || runLen == 13 {
			var v uint64
			for k := i; k < j; k++ {
				v = v*10 + uint64(s[k]-'0')
			}
			if runLen == 10 && v >= secMin && v <= secMax {
				return true
			}
			if runLen == 13 && v >= msMin && v <= msMax {
				return true
			}
		}
		i = j
	}
	return false
}

// formBodiesMatchModuloNoise compares two form-encoded bodies treating any
// individual "key=value" segment that matches a Mock.Noise regex as a
// wildcard. Noise is evaluated *per occurrence*, not per key: a body like
// `a=NOISY&a=2` keeps the second 'a' occurrence as a non-noisy, must-match
// value even though the first occurrence is wildcarded.
//
// Match semantics: for every key, collect the ordered list of non-noisy
// values on each side. Both sides must declare the same key set (modulo
// keys whose every occurrence is noisy and absent on the other side), and
// the surviving non-noisy values for each key must match byte-for-byte in
// order.
//
// Splitting is done on raw bytes (Split on '&', IndexByte on '=') rather
// than via url.ParseQuery so the segment passed to nc.IsNoisy carries the
// same URL-encoded form the obfuscator's formKeyNoiseRegex anchored on
// (^<raw_key>=[^&]+$).
func formBodiesMatchModuloNoise(mockBody, reqBody string, nc *util.NoiseChecker) bool {
	// nonNoisyByKey returns, per key, the ordered list of values whose
	// "key=value" segment does NOT match any noise pattern. Keys whose
	// every occurrence is noisy still appear in the map (with a nil/empty
	// slice) so the cross-side presence check treats them as "declared
	// but fully wildcarded" rather than missing.
	nonNoisyByKey := func(body string) map[string][]string {
		out := make(map[string][]string)
		if body == "" {
			return out
		}
		for _, seg := range strings.Split(body, "&") {
			if seg == "" {
				continue
			}
			eqIdx := strings.IndexByte(seg, '=')
			var key, val string
			if eqIdx < 0 {
				key = seg
			} else {
				key = seg[:eqIdx]
				val = seg[eqIdx+1:]
			}
			if _, exists := out[key]; !exists {
				out[key] = nil
			}
			if nc.IsNoisy(key + "=" + val) {
				continue
			}
			// Stop-gap until the recorder emits explicit noise patterns
			// for rotating-timestamp keys (botocore RoleSessionName,
			// OAuth nonces, …): wildcard any segment whose value
			// embeds a modern unix timestamp. valueHasUnixTimestamp's
			// digit-width gate (10 or 13 only) keeps 12-digit AWS
			// account IDs out of scope.
			if valueHasUnixTimestamp(val) {
				continue
			}
			out[key] = append(out[key], val)
		}
		return out
	}
	sliceEqual := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	mockKV := nonNoisyByKey(mockBody)
	reqKV := nonNoisyByKey(reqBody)

	// Cross-check both directions: same declared key set, same non-noisy
	// values per key in order. A key that is fully wildcarded on the mock
	// side (every occurrence noisy → entry present, slice empty) still
	// requires the request to declare the key; the inverse holds too. This
	// prevents a permissive mock from silently absorbing requests that
	// omit or add a non-noisy key.
	for k, mv := range mockKV {
		rv, ok := reqKV[k]
		if !ok {
			return false
		}
		if !sliceEqual(mv, rv) {
			return false
		}
	}
	for k := range reqKV {
		if _, ok := mockKV[k]; !ok {
			return false
		}
	}
	return true
}

// sameRequest reports whether in is the request the pooled mock m recorded, as
// the matcher's exact passes read one: it passes the exact URL pass
// (SchemaMatch with no auto-detected dynamic segment, so configured url noise
// and the method, header and query rules are the matcher's own), and its body
// is the recorded one — byte for byte, or, both being JSON documents, but for
// the fields bodyNoise sets aside (mocknoise.SameJSONBut).
func (h *HTTP) sameRequest(ctx context.Context, in *req, m *models.Mock, headerNoise map[string][]string, urlNoise []string, bodyNoise map[string][]string) bool {
	if ms, err := h.SchemaMatch(ctx, in, []*models.Mock{m}, headerNoise, urlNoise, false); err != nil || len(ms) == 0 {
		return false
	}
	recorded := m.Spec.HTTPReq.Body
	return recorded == string(in.body) || mocknoise.SameJSONBut(recorded, string(in.body), bodyNoise)
}

// claim takes the matched mock m for this request: it loads m's response
// (Mock.WithResponse), then records the match (updateMock), which consumes a
// per-test mock. Loading comes first so that a response that cannot be loaded
// leaves the mock unconsumed rather than reported as served. It returns the
// mock to serve, m or a copy of it with the response loaded, and claimed=false
// when another connection took m first.
//
// m may be a rebound copy (see rebind): the POOLED original is what is loaded
// and consumed. Whatever pass matched it, rebinding acts only when the live
// request is that recording's own with this run's ids in place (rebind.fits):
// the ids the recording introduced are then bound for the rest of the test
// set, and the answer names this run's ids. Any other request is answered as
// recorded, with nothing bound.
func (h *HTTP) claim(ctx context.Context, m *models.Mock, mockDb integrations.MockMemDb, detectedNoise map[string][]string, bindings map[string]string, rb *rebind) (served *models.Mock, claimed bool, err error) {
	m, _ = rb.original(m)
	served, err = m.WithResponse()
	if err != nil {
		return nil, false, fmt.Errorf("http: %w", err)
	}
	// A request that fits claims its creator (rebind.commit). The claim is
	// refused when another request claimed the creator, or bound one of its
	// ids, since this one was matched: requests that race for one creator
	// (identical creates on several connections).
	//
	// A recording the pool shares (a session mock) is claimed before it is
	// used: refused, it is not used — no noise saved on it, not counted as
	// used — and the request is matched again, against the next creator. A
	// per-test recording is consumed first, which only one request can do;
	// a claim refused after that answers the request as recorded, from the
	// recording it holds.
	fits := rb.fits(m)
	shared := reusable(m)
	if fits && shared && !rb.commit(m) {
		return nil, false, nil
	}
	if !h.updateMock(ctx, m, mockDb, detectedNoise) {
		return nil, false, nil
	}
	if fits && !shared && !rb.commit(m) {
		fits = false
	}
	// Honor request→response correlations: render the live values captured from
	// this request into the served response (on a copy; never the pooled mock).
	served = renderCorrelations(served, bindings)
	if fits {
		served = rb.render(served, m)
	} else if disowned := rb.disowns(m); len(disowned) > 0 {
		h.Logger.Warn("an id this run was following is a constant after all: the app made the call that introduced it again, exactly as recorded. What was bound to it was a call about something else; answers that do not name that id keep the recorded one from here on",
			zap.String("mock", m.Name), zap.Strings("recorded_ids", disowned))
	} else if recorded, live, ok := rb.other(m); ok && len(bindings) == 0 {
		// Once per recording: an app that makes more of a call than was
		// recorded would say it on every one.
		if rb.binder.FirstNote("other:" + m.Name) {
			h.Logger.Warn("answered a dependency call as recorded although it names another id: where the recorded call names an id this run made anew, this call carries neither that id nor the recorded one",
				zap.String("mock", m.Name), zap.String("recorded_id", recorded), zap.String("this_runs_id", live),
				zap.String("what_it_means", "the call asks about something the recording does not know by that id — another entity, a second id for the same one, or one more call than was recorded. The answer names the recorded id"))
		}
	} else if rb.unfollowed(m) {
		// Why an id is not followed, for whoever looks: the replay CLI points
		// here when a test fails over one.
		h.Logger.Debug("answered a dependency call as recorded, without following ids: it is not the recorded call with this run's ids in place of the recorded ones (a field that is not request-body noise differs, or an id does not line up)",
			zap.String("mock", m.Name))
	}
	return served, true, nil
}

// updateMock processes the matched mock based on its Lifetime.
// Per-test mocks are CONSUMED on match (DeleteFilteredMock); session /
// connection mocks are RETAINED and updated in place (UpdateUnFilteredMock).
// See the MySQL equivalent in replayer/match.go for the pre- vs post-
// Phase-2 routing rationale.
//
// Concurrency: matchedMock is a pointer handed out of the shared mock
// pool. Two requests matching the same session-lifetime mock receive
// the SAME pointer; mutating matchedMock.TestModeInfo in place races
// with the other goroutine's read of that same struct (proxy-stress-
// test surfaced this under -race as "DATA RACE during replay" on
// match.go:723-725). We build a fresh copy, mutate the copy, and pass
// (old=matchedMock, new=&updatedMock) to the mock DB — which already
// takes treesMu internally to swap the pointer atomically.
func (h *HTTP) updateMock(_ context.Context, matchedMock *models.Mock, mockDb integrations.MockMemDb, detectedNoise map[string][]string) bool {
	updatedMock := *matchedMock
	updatedMock.TestModeInfo.IsFiltered = false
	updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()

	// Attach any request-body noise detected this match onto a FRESH map on the
	// copy (never the shared pooled mock's map — see the concurrency note above).
	// flagMockAsUsed reads updatedMock.Spec.ReqBodyNoise to carry it out on the
	// MockState; mockdb.UpdateMocks persists it. Stored on the kind-agnostic
	// MockSpec.ReqBodyNoise, same as every other parser.
	if len(detectedNoise) > 0 {
		updatedMock.Spec.ReqBodyNoise = mergeReqBodyNoise(updatedMock.Spec.ReqBodyNoise, detectedNoise)
	}

	if reusable(matchedMock) {
		return mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock)
	}
	// Per-test: consume via DeleteFilteredMock, with fallback to
	// UpdateUnFilteredMock for mocks staged into the session pool
	// during the initial pre-first-test window.
	//
	// DeleteFilteredMock keys the tree lookup on TestModeInfo, so the
	// delete-key mock MUST keep the original (unmutated) TestModeInfo —
	// we pass a copy that retains it but carries the detected noise on a
	// fresh MockSpec.ReqBodyNoise map, so flagMockAsUsed reports the noise on
	// the consumed per-test mock (it would otherwise be lost: the original
	// matchedMock has no noise, and updatedMock's mutated TestModeInfo wouldn't
	// match the tree node).
	deleteMock := *matchedMock
	if len(detectedNoise) > 0 {
		deleteMock.Spec.ReqBodyNoise = mergeReqBodyNoise(deleteMock.Spec.ReqBodyNoise, detectedNoise)
	}
	if mockDb.DeleteFilteredMock(deleteMock) {
		return true
	}
	return mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock)
}

// reusable reports whether a matched mock stays in the pool for every request
// that matches it (session and connection mocks, and per-test config mocks),
// rather than being consumed by the one it answers.
func reusable(m *models.Mock) bool {
	lifetime := m.TestModeInfo.Lifetime
	rawConfig := m.Spec.Metadata != nil && m.Spec.Metadata["type"] == "config"
	return lifetime == models.LifetimeSession ||
		lifetime == models.LifetimeConnection ||
		(lifetime == models.LifetimePerTest && rawConfig)
}

// filterStrictNoiseMatches enforces strict request-body matching on the
// replay path. Under strict enforcement EVERY candidate is value-compared
// against its recorded body — not just those carrying learned req_body_noise.
// A candidate is dropped when a field OUTSIDE the known-noise set (configured
// global/user body noise ∪ anything the mock already learned) drifted from the
// recorded body, i.e. an unmarked value or type changed. So a mock with no
// learned noise no longer passes by default: only fields explicitly marked as
// noise may differ.
//
// It delegates to schemanoise.Engine.StrictReject. The JSON/form comparison and
// known-noise merge are owned by the shared engine + httpNoiseAdapter; the
// returned drift names the offending field path(s) for the rejection log.
func (h *HTTP) filterStrictNoiseMatches(eng *schemanoise.Engine, candidates []*models.Mock, reqBody []byte, userBodyNoise map[string][]string) []*models.Mock {
	var kept []*models.Mock
	for _, m := range candidates {
		allowed, drift := eng.StrictReject(m, reqBody, userBodyNoise)
		if allowed {
			kept = append(kept, m)
			continue
		}
		h.Logger.Debug("strict req-body match rejected mock: non-noise field drift",
			zap.String("mock name", m.Name), zap.Any("drift", drift))
	}
	return kept
}

// mergeNoiseMaps combines two noise maps into a fresh map; entries in a win
// on key collision. Either input may be nil. The result never aliases an
// input map, so callers may hold or extend it without mutating shared state.
func mergeNoiseMaps(a, b map[string][]string) map[string][]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string][]string, len(a)+len(b))
	for k, v := range b {
		out[k] = v
	}
	for k, v := range a {
		out[k] = v
	}
	return out
}

// stripBodyPrefix returns a copy of the noise map with the leading "body."
// trimmed from each key, so the keys align with the matcher's root-relative
// path convention used by ChangedJSONFieldPaths.
func stripBodyPrefix(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[strings.TrimPrefix(k, "body.")] = v
	}
	return out
}

// isFormURLEncoded reports whether the request Content-Type is
// application/x-www-form-urlencoded.
func isFormURLEncoded(header map[string]string) bool {
	for k, v := range header {
		if strings.EqualFold(k, "Content-Type") &&
			strings.Contains(strings.ToLower(v), "application/x-www-form-urlencoded") {
			return true
		}
	}
	return false
}

// formReqBodyNoise diffs two form-encoded bodies key-by-key and returns the
// drifted/removed keys as body.<key> field-path noise. Keys already known or
// covered by the obfuscator value-regexes are skipped.
func formReqBodyNoise(mockBody, reqBody string, known map[string][]string, isObfuscated func(string) bool) map[string][]string {
	// Split on raw bytes (Split on '&', IndexByte on '=') rather than via
	// url.ParseQuery so the "key=value" segment handed to isObfuscated carries
	// the same URL-encoded form the obfuscator's formKeyNoiseRegex anchored on
	// (^<raw_key>=[^&]+$) — exactly as formBodiesMatchModuloNoise does. Passing
	// only the decoded value here would never match a key-anchored regex, so
	// obfuscated form fields would be wrongly re-flagged as schema noise.
	rawValuesByKey := func(body string) map[string][]string {
		out := map[string][]string{}
		for _, seg := range strings.Split(body, "&") {
			if seg == "" {
				continue
			}
			key, val := seg, ""
			if i := strings.IndexByte(seg, '='); i >= 0 {
				key, val = seg[:i], seg[i+1:]
			}
			out[key] = append(out[key], val)
		}
		return out
	}
	mockVals := rawValuesByKey(mockBody)
	reqVals := rawValuesByKey(reqBody)

	out := map[string][]string{}
	for rawKey, mv := range mockVals {
		key := "body." + rawKey
		if _, ok := known[key]; ok {
			continue
		}
		// Obfuscator exclusion is per-occurrence on the full raw key=value
		// segment, matching how Mock.Noise is evaluated for form bodies.
		// isObfuscated may be nil (no value-regex noise on the mock).
		obfuscated := false
		if isObfuscated != nil {
			for _, v := range mv {
				if isObfuscated(rawKey + "=" + v) {
					obfuscated = true
					break
				}
			}
		}
		if obfuscated {
			continue
		}
		rv, ok := reqVals[rawKey]
		if !ok {
			out[key] = []string{} // key dropped on replay
			continue
		}
		// Compare occurrences element-wise (order-sensitive) rather than
		// joining: a join is lossy when values embed the separator or the
		// repeated-key cardinality differs (["a","bc"] vs ["a,bc"]), which
		// would miss or falsely report drift. Mirrors formBodiesMatchModuloNoise.
		drifted := len(rv) != len(mv)
		for i := 0; !drifted && i < len(mv); i++ {
			if rv[i] != mv[i] {
				drifted = true
			}
		}
		if drifted {
			out[key] = []string{} // value drifted
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeReqBodyNoise returns a fresh map combining existing and newly-detected
// request-body noise. Existing entries win on key collision (noise is
// monotonic — once a field is flagged it stays flagged), and every slice is
// copied so the result shares no backing storage with its inputs. It delegates
// to the shared schema-noise engine so HTTP, Pulsar and the on-disk persistence
// all merge learned noise through one implementation.
func mergeReqBodyNoise(existing, detected map[string][]string) map[string][]string {
	return schemanoise.MergeLearned(existing, detected)
}

// buildHTTPMismatchReport finds the closest HTTP mock to the given request
// and returns a structured field-level diff report. liveBody is the live
// request body (may be nil for body-less requests); headerNoise/userBodyNoise
// are the same noise sets the matcher used, so the report never flags a field
// the matcher would have ignored. diag (from match()) tells the builder how
// far the cascade got and which candidates were still alive, so the diff is
// computed against a candidate the matcher actually considered close — not
// just the lowest-Levenshtein path. If httpMocks is nil, it fetches from
// mockDb; otherwise it uses the pre-fetched slice to avoid a redundant read.
func (h *HTTP) buildHTTPMismatchReport(request *http.Request, liveBody []byte, mockDb integrations.MockMemDb, httpMocks []*models.Mock, headerNoise, userBodyNoise map[string][]string, diag *matchDiag) *models.MockMismatchReport {
	// Defensive: this diagnostic builder and its callees (pickClosestCandidate,
	// renderLiveRequest) — plus decode.go's 502 error line — dereference
	// request.URL throughout. http.ReadRequest always sets a non-nil URL on the
	// live path, but normalize here so a hand-built request (tests / future
	// callers) can never panic the agent on the error path.
	if request.URL == nil {
		request.URL = &url.URL{}
	}
	actualKey := request.Method + " " + request.URL.Path
	// Destination identifies WHICH upstream this missed call targeted; the same
	// method+path can hit several hosts. Host header first, URL authority next.
	dest := request.Host
	if dest == "" {
		dest = request.URL.Host
	}
	if httpMocks == nil && (diag == nil || len(diag.schemaMatched) == 0) {
		// Mirror match()'s pool-merging + ordering strategy so
		// mismatch diagnostics see the same candidate set in the
		// same order the matcher saw. Per-test FIRST, session second.
		perTestMocks, err := mockDb.GetPerTestMocksInWindow()
		if err != nil {
			return mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
				WithDestination(dest).
				WithNextSteps("Failed to read mock database. Check logs for errors and retry.").Build()
		}
		sessionMocks, err := mockDb.GetSessionMocks()
		if err != nil {
			return mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
				WithDestination(dest).
				WithNextSteps("Failed to read mock database. Check logs for errors and retry.").Build()
		}
		mocks := make([]*models.Mock, 0, len(perTestMocks)+len(sessionMocks))
		mocks = append(mocks, perTestMocks...)
		mocks = append(mocks, sessionMocks...)
		httpMocks = FilterHTTPMocks(mocks)
	}

	phase := models.MatchPhaseExhausted
	candidateCount := len(httpMocks)
	var schemaSurvivors []*models.Mock
	if diag != nil {
		if diag.phase != "" {
			phase = diag.phase
		}
		if diag.candidates > 0 {
			candidateCount = diag.candidates
		}
		schemaSurvivors = diag.schemaMatched
	}

	if candidateCount == 0 && len(schemaSurvivors) == 0 {
		return mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
			WithDestination(dest).
			WithPhase(models.MatchPhaseNoMocks, 0).Build()
	}

	// Does anything the matcher just compared against even target this
	// upstream? Answered from the compared set and nothing else — see
	// comparedDestinations for why the wider "was it ever recorded?" question
	// is not asked.
	//
	// diag.pool is the set match() literally walked, so it is preferred over
	// httpMocks: on the schema-survivor path httpMocks is nil (the pool is
	// deliberately not reloaded, the diff comes from the survivors), and on
	// the other path httpMocks is a fresh re-read that a concurrent
	// consumption on another connection can have shrunk since the match. Only
	// the caller-supplied / re-read pool is used when the diag carries none —
	// a hand-built diag in a test, or diag == nil.
	comparedPool := httpMocks
	if diag != nil && diag.pool != nil {
		comparedPool = diag.pool
	}
	comparedDests := comparedDestinations(comparedPool, candidateCount)

	// Pick the candidate to diff against. Preference order:
	//  1. a schema-match survivor (method+path+keys already matched — the
	//     interesting drift is in query values, headers, or the body)
	//  2. the lowest-Levenshtein "METHOD path" mock from the pool.
	closestMock := pickClosestCandidate(request, schemaSurvivors, httpMocks)
	if closestMock == nil || closestMock.Spec.HTTPReq == nil {
		return mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
			WithDestination(dest).
			WithComparedDestinations(comparedDests).
			WithPhase(phase, candidateCount).Build()
	}

	mockReq := closestMock.Spec.HTTPReq
	var fieldDiffs []models.MockFieldDiff

	// method / path pseudo-fields
	if string(mockReq.Method) != request.Method {
		fieldDiffs = append(fieldDiffs, models.MockFieldDiff{
			Path: "method", Kind: models.DiffKindValueChanged,
			Expected: string(mockReq.Method), Actual: request.Method,
		})
	}
	mockPath := mockReq.URL
	var mockQuery url.Values
	if parsed, err := url.Parse(mockReq.URL); err == nil {
		mockPath = parsed.Path
		mockQuery = parsed.Query()
	}
	if mockPath != request.URL.Path {
		fieldDiffs = append(fieldDiffs, models.MockFieldDiff{
			Path: "path", Kind: models.DiffKindValueChanged,
			Expected: mockPath, Actual: request.URL.Path,
		})
	}

	// Recorded URLParams take precedence over the parsed URL query (some
	// recorders store params only there); fall back to the parsed query.
	recordedQuery := map[string][]string{}
	if len(mockReq.URLParams) > 0 {
		for k, v := range mockReq.URLParams {
			recordedQuery[k] = []string{v}
		}
	} else {
		recordedQuery = mockQuery
	}
	fieldDiffs = append(fieldDiffs, mismatch.QueryParamDiffs(recordedQuery, request.URL.Query())...)
	fieldDiffs = append(fieldDiffs, mismatch.HeaderKeyDiffs(mockReq.Header, request.Header, headerNoise)...)

	// Body diffs: JSON bodies get field-level diffs excluding everything the
	// matcher itself ignores (learned req_body_noise + user body noise).
	if len(liveBody) > 0 && pkg.IsJSON([]byte(mockReq.Body)) && pkg.IsJSON(liveBody) {
		ignore := mergeNoiseMaps(stripBodyPrefix(closestMock.Spec.ReqBodyNoise), userBodyNoise)
		fieldDiffs = append(fieldDiffs, mismatch.JSONBodyDiffs(mockReq.Body, string(liveBody), ignore)...)
	}

	// Redact secret / obfuscated values out of the structured diffs before they
	// are attached — they (and the Diff string derived from them) are persisted
	// into the report YAML and the platform API.
	redactFieldDiffs(fieldDiffs, closestMock.Noise)

	b := mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
		WithDestination(dest).
		WithComparedDestinations(comparedDests).
		WithPhase(phase, candidateCount).
		WithClosest(closestMock.Name, fieldDiffs).
		// Whole-request renders for the CLI side-by-side diff. Mock.Noise is
		// passed so obfuscated values stay redacted on both sides.
		WithRenderedRequests(
			renderMockRequest(mockReq, closestMock.Noise),
			renderLiveRequest(request, liveBody, closestMock.Noise),
		)
	if len(fieldDiffs) == 0 {
		// Nothing structurally differs vs the closest candidate, yet the
		// matcher rejected everything — point the user at the phase instead
		// of the misleading legacy "headers or body differ".
		b = b.WithDiff(fmt.Sprintf("closest mock %q has no field-level differences; match stopped at phase %q", closestMock.Name, phase))
	}
	return b.Build()
}

// comparedDestinations returns the upstream authority of every mock in the
// pool this report was built against, or nil when the "does anything here
// target the live upstream?" question cannot be answered from it — nil means
// undecidable, and an undecidable check leaves today's message exactly as it
// was (mismatch.WithComparedDestinations carries the full WHY).
//
// The claim it feeds is strictly LOCAL: "no mock in the compared set targets
// this host". It is deliberately NOT the stronger "this host was never
// recorded". That stronger claim needs global, consumption-immune knowledge
// of the whole test set, and keploy does not have it here: HTTP per-test
// mocks are consumed on match (updateMock -> DeleteFilteredMock) and the
// agent then strips every consumed mock from the pool it sends for all later
// tests (pkg/service/agent/agent.go filterOutDeleted over TotalConsumedMocks).
// Once a host's last mock has been served it is gone from every pool while
// having been recorded all along — so an absence read off any pool, however
// carefully assembled, eventually accuses the application's own upstream. The
// weaker claim costs a sentence of precision and is always true.
//
// nil (undecidable) when:
//   - the pool is empty — there is nothing to have compared against. In
//     production this is the diag-less path (a caller that supplied no
//     matchDiag) and the mockDb-read-failed path; on the schema-survivor path
//     the builder does NOT reload the pool, but matchDiag.pool carries the
//     compared set out of match() so that path is decidable too;
//   - the pool holds fewer mocks than the matcher reported comparing, so what
//     is in hand is a subset of the compared set and "none of them" would
//     overreach. matchDiag.pool never trips this (len(pool) == candidates by
//     construction); it fires for a caller that hands over a partial pool —
//     a test, or a future parser reusing this builder — and for the re-read
//     path if a concurrent consumption shrank the pool between the match and
//     the report. Kept as the defence for those callers, not as a live
//     production branch.
//   - any mock in it carries no readable destination — that mock could be the
//     one that targeted the live call, so the set proves nothing.
//
// This runs only on the miss path, where a report is already being rendered,
// so the walk never touches the matching hot path.
func comparedDestinations(pool []*models.Mock, candidateCount int) []string {
	if len(pool) == 0 || len(pool) < candidateCount {
		return nil
	}
	// Sized for distinct UPSTREAMS, not for mocks: a recorded test set has
	// single-digit distinct hosts behind hundreds of mocks, so len(pool)
	// would over-allocate by two orders of magnitude on every miss.
	const typicalDistinctHosts = 8
	seen := make(map[string]struct{}, typicalDistinctHosts)
	dests := make([]string, 0, typicalDistinctHosts)
	for _, mock := range pool {
		dest, ok := mock.RecordedDestination()
		if !ok {
			return nil
		}
		if _, dup := seen[dest]; dup {
			continue
		}
		seen[dest] = struct{}{}
		dests = append(dests, dest)
	}
	return dests
}

// pickClosestCandidate prefers a schema-match survivor (already same
// method/path/keys) and falls back to the lowest-Levenshtein "METHOD path"
// candidate across the pool, same-method candidates first.
func pickClosestCandidate(request *http.Request, schemaSurvivors, pool []*models.Mock) *models.Mock {
	if len(schemaSurvivors) > 0 {
		for _, m := range schemaSurvivors {
			if m != nil && m.Spec.HTTPReq != nil {
				return m
			}
		}
	}
	actualKey := request.Method + " " + request.URL.Path
	bestDist := -1
	var closest *models.Mock
	for pass := 0; pass < 2; pass++ {
		for _, mock := range pool {
			if mock == nil || mock.Spec.HTTPReq == nil {
				continue
			}
			if pass == 0 && string(mock.Spec.HTTPReq.Method) != request.Method {
				continue
			}
			// Parse mock URL to extract just the path (mocks store full URL strings)
			mockPath := mock.Spec.HTTPReq.URL
			if parsed, err := url.Parse(mock.Spec.HTTPReq.URL); err == nil {
				mockPath = parsed.Path
			}
			mockKey := string(mock.Spec.HTTPReq.Method) + " " + mockPath
			dist := levenshtein.ComputeDistance(actualKey, mockKey)
			if bestDist < 0 || dist < bestDist {
				bestDist = dist
				closest = mock
			}
		}
		if closest != nil {
			break
		}
	}
	return closest
}

// The former isTelemetryEgress / telemetryEgressPaths legacy bypass ({/v1/traces,
// /ingest} POST) was removed: both paths are built-in telemetry defaults now, so
// models.ResolvePassThrough handles them — and, unlike the legacy check, it
// honours a user mode:"off" override instead of unconditionally re-skipping.
