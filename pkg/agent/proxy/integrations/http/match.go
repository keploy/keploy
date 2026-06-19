package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// flakyHeaders lists HTTP header keys (lowercased) that are known to change
// on every request due to cryptographic signatures, timestamps, credential
// rotation, or per-request identifiers. These are automatically treated as
// noise during mock matching so that replayed requests can find the correct
// recorded mock even though these artifacts differ. Users who need strict
// header matching can disable this with --disableAutoHeaderNoise.
//
// No single public library maintains such a list. Most recording/replay
// tools (VCR, WireMock, Hoverfly) avoid the problem by not matching on
// headers at all by default. Since Keploy does match on header keys, we
// maintain this list covering the most common sources of non-determinism.
//
// Categories:
//   - Cloud auth/signing:  AWS SigV4, GCP OAuth, Azure HMAC/Bearer
//   - Tracing/correlation: W3C Trace Context, B3, Datadog, X-Request-Id
//   - Webhook signatures:  Stripe, GitHub, Slack, Twilio, Shopify
//   - SDK metadata:        per-call invocation IDs and attempt counters
var flakyHeaders = []string{
	// ── AWS SigV4 & SDK ──────────────────────────────────────────────
	"authorization",         // signature changes every request (all cloud providers)
	"x-amz-date",            // signing timestamp (yyyymmddThhmmssZ)
	"x-amz-security-token",  // STS/IRSA session token — may appear or disappear
	"x-amz-content-sha256",  // payload hash
	"x-amz-credential",      // credential scope string
	"x-amz-signature",       // explicit signature value (SigV4 query-string variant)
	"x-amz-signedheaders",   // list of signed headers (varies with SDK)
	"x-amz-expires",         // pre-signed URL expiry seconds
	"x-amz-user-agent",      // SDK metadata
	"x-amzn-trace-id",       // AWS X-Ray trace propagation
	"amz-sdk-invocation-id", // unique per-call UUID from AWS SDK
	"amz-sdk-request",       // attempt counter (attempt=1; max=3)
	"date",                  // SigV4 fallback when X-Amz-Date absent. Globally ignored (Date is dynamic for all HTTP); disable per-test via DisableAutoHeaderNoise.

	// ── GCP ──────────────────────────────────────────────────────────
	"x-goog-api-client",     // SDK metadata (version, runtime info)
	"x-goog-request-params", // routing parameters, may change with resource

	// ── Azure ────────────────────────────────────────────────────────
	"x-ms-date",                     // signing timestamp
	"x-ms-client-request-id",        // client-generated UUID per call
	"x-ms-content-sha256",           // body hash for HMAC auth
	"x-ms-return-client-request-id", // echo control flag

	// ── W3C Trace Context / OpenTelemetry ────────────────────────────
	"traceparent", // unique trace-id + span-id per request
	"tracestate",  // vendor-specific trace context

	// ── Zipkin B3 propagation ────────────────────────────────────────
	"x-b3-traceid",
	"x-b3-spanid",
	"x-b3-parentspanid",
	"x-b3-sampled",
	"b3", // single-header compact format

	// ── Datadog ──────────────────────────────────────────────────────
	"x-datadog-trace-id",
	"x-datadog-parent-id",
	"x-datadog-sampling-priority",
	"x-datadog-origin",

	// ── Generic request/correlation IDs ──────────────────────────────
	"x-request-id",     // Nginx, Envoy, HAProxy, AWS ALB, Heroku
	"x-correlation-id", // cross-service correlation
	"request-id",       // ASP.NET Core and others

	// ── Webhook signatures (request-side, inbound webhooks) ──────────
	"stripe-signature",
	"x-hub-signature-256", // GitHub
	"x-hub-signature",     // GitHub (legacy SHA-1)
	"x-twilio-signature",
	"x-shopify-hmac-sha256",
	"x-slack-signature",
	"x-slack-request-timestamp",
	"webhook-signature", // Standard Webhooks spec
	"webhook-timestamp", // Standard Webhooks spec
	"webhook-id",        // Standard Webhooks spec

	// ── Idempotency / CSRF ───────────────────────────────────────────
	"idempotency-key",
	"x-idempotency-key",
	"x-csrf-token",
	"x-xsrf-token",

	// ── GCP trace (legacy) ───────────────────────────────────────────
	"x-cloud-trace-context",
}

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
}

// match returns (matched, mock, diag, err). diag is non-nil only when
// matched is false and no error occurred. userBodyNoise carries the user's
// test.globalNoise body bucket (root-relative dotted paths, lowercased) so
// manual noise config participates in mock matching with the same vocabulary
// as response assertions.
func (h *HTTP) match(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise map[string][]string, userBodyNoise map[string][]string, urlNoise []string, autoURLDynamic bool, schemaNoiseDetection bool, schemaNoiseStrict bool) (bool, *models.Mock, *matchDiag, error) {

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

		// Matching process
		// Pass 1: exact URL + configured url-noise only. Deterministic and
		// genuinely-distinct calls match here and are never relaxed.
		schemaMatched, err := h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise, urlNoise, false)
		if err != nil {
			return false, nil, nil, err
		}
		// Pass 2 (zero-config DEFAULT): only when nothing matched the URL exactly
		// or under url-noise, retry allowing auto-detected dynamic id segments
		// (numeric/uuid/hex/long token) to vary — so a non-deterministic path id
		// doesn't 502 with no config. Disable via OutgoingOptions.DisableAutoURLDynamic.
		if len(schemaMatched) == 0 && autoURLDynamic {
			schemaMatched, err = h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise, urlNoise, true)
			if err != nil {
				return false, nil, nil, err
			}
			if len(schemaMatched) > 0 {
				h.Logger.Debug("http url: matched via auto-detected dynamic path segment(s) after no exact/url-noise match",
					zap.Int("candidates", len(schemaMatched)))
			}
		}

		if len(schemaMatched) == 0 {
			return false, nil, &matchDiag{phase: models.MatchPhaseSchema, candidates: len(unfilteredMocks)}, nil
		}

		h.Logger.Debug("http mock schema match results",
			zap.Int("schema_matched", len(schemaMatched)),
			zap.Int("total_http_mocks", len(unfilteredMocks)))

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			h.Logger.Debug("exact body match found", zap.String("mock name", bestMatch.Name))
			// Exact (byte-equal) body — nothing drifted, so no noise to detect.
			if !h.updateMock(ctx, bestMatch, mockDb, nil) {
				continue
			}
			return true, bestMatch, nil, nil
		}

		shortListed := schemaMatched
		// Schema match for JSON bodies
		if pkg.IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, schemaMatched, input.body)
			if err != nil {
				return false, nil, nil, err
			}

			// Strict enforcement (replay path): for any candidate that already
			// carries learned req_body_noise, every request-body field must
			// match except those learned-noise paths and the user's configured
			// body noise. A drift on a non-noise field rejects that candidate,
			// so a changed-but-unmarked field fails the test instead of being
			// silently served. Candidates with no learned noise keep the
			// lenient schema/key behaviour.
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
				return false, nil, &matchDiag{phase: phase, candidates: len(unfilteredMocks), schemaMatched: schemaMatched}, nil
			}

			if len(bodyMatched) == 1 {
				h.Logger.Debug("body match found", zap.String("mock name", bodyMatched[0].Name))
				detected, _ := noiseEngine.Detect(bodyMatched[0], input.body, userBodyNoise)
				if !h.updateMock(ctx, bodyMatched[0], mockDb, detected) {
					continue
				}
				return true, bodyMatched[0], nil, nil
			}

			// More than one match, perform fuzzy match
			shortListed = bodyMatched
		}

		h.Logger.Debug("Performing fuzzy match for req buffer")
		// Perform fuzzy match on the request
		isMatched, bestMatch := h.PerformFuzzyMatch(shortListed, input.raw)
		if isMatched {
			h.Logger.Debug("fuzzy match found a matching mock", zap.String("mock name", bestMatch.Name))
			detected, _ := noiseEngine.Detect(bestMatch, input.body, userBodyNoise)
			if !h.updateMock(ctx, bestMatch, mockDb, detected) {
				continue
			}
			return true, bestMatch, nil, nil
		}
		return false, nil, &matchDiag{phase: models.MatchPhaseExhausted, candidates: len(unfilteredMocks), schemaMatched: shortListed}, nil
	}
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
		const ph = "{{keploy.urlnoise}}"
		np, rp := mockPath, reqPath
		for _, pat := range urlNoise {
			re, cerr := regexp.Compile(pat)
			if cerr != nil {
				h.Logger.Debug("skipping invalid url-noise regex", zap.String("pattern", pat), zap.Error(cerr))
				continue
			}
			np = re.ReplaceAllString(np, ph)
			rp = re.ReplaceAllString(rp, ph)
		}
		if np == rp {
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
		if looksDynamicSegment(ms[i]) && looksDynamicSegment(rs[i]) {
			differed = true
			continue
		}
		return false // a non-id segment differs -> genuinely different path
	}
	return differed
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
	if len(s) == 0 {
		return false
	}
	if reSegAllDigits.MatchString(s) || reSegUUID.MatchString(s) || reSegLongHex.MatchString(s) {
		return true
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
		return hasDigit && hasAlpha
	}
	return false
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

func (h *HTTP) MapsHaveSameKeys(map1 map[string]string, map2 map[string][]string) bool {
	// Helper function to check if a header should be ignored
	shouldIgnoreHeader := func(key string) bool {
		lkey := strings.ToLower(key)
		return strings.HasPrefix(lkey, "keploy")
	}

	// Count non-ignored keys in map1
	map1Count := 0
	for key := range map1 {
		if !shouldIgnoreHeader(key) {
			map1Count++
		}
	}

	// Count non-ignored keys in map2
	map2Count := 0
	for key := range map2 {
		if !shouldIgnoreHeader(key) {
			map2Count++
		}
	}

	// Check if counts match
	if map1Count != map2Count {
		return false
	}

	// Check if all non-ignored keys in map1 exist in map2
	for key := range map1 {
		if shouldIgnoreHeader(key) {
			continue
		}
		if _, exists := map2[key]; !exists {
			return false
		}
	}

	// Check if all non-ignored keys in map2 exist in map1
	for key := range map2 {
		if shouldIgnoreHeader(key) {
			continue
		}
		if _, exists := map1[key]; !exists {
			return false
		}
	}

	return true
}

// SchemaMatch match the schema of the request with the mocks
func (h *HTTP) SchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock, headerNoise map[string][]string, urlNoise []string, autoDynamic bool) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

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

		// Query parameter match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query()) {
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

	lifetime := updatedMock.TestModeInfo.Lifetime
	rawConfig := false
	if updatedMock.Spec.Metadata != nil {
		rawConfig = updatedMock.Spec.Metadata["type"] == "config"
	}
	isSessionOrConnection := lifetime == models.LifetimeSession ||
		lifetime == models.LifetimeConnection ||
		(lifetime == models.LifetimePerTest && rawConfig)

	if isSessionOrConnection {
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

// filterStrictNoiseMatches enforces strict request-body matching on the
// replay path. A candidate mock is dropped only when it already carries
// learned req_body_noise AND a field OUTSIDE that learned-noise set drifted
// from the recorded body — i.e. an unmarked field changed. Candidates with no
// learned noise are kept unchanged (lenient schema/key behaviour), so this
// never tightens matching for mocks the auto-replay never learned noise for.
//
// It delegates to schemanoise.Engine.StrictAllows: a candidate with no learned
// noise is always kept (lenient), and a candidate WITH learned noise is dropped
// when a field outside that learned set drifted. The JSON/form comparison and
// known-noise merge are owned by the shared engine + httpNoiseAdapter.
func (h *HTTP) filterStrictNoiseMatches(eng *schemanoise.Engine, candidates []*models.Mock, reqBody []byte, userBodyNoise map[string][]string) []*models.Mock {
	var kept []*models.Mock
	for _, m := range candidates {
		if eng.StrictAllows(m, reqBody, userBodyNoise) {
			kept = append(kept, m)
			continue
		}
		h.Logger.Debug("strict req-body match rejected mock: non-noise field drift",
			zap.String("mock name", m.Name))
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

	// Pick the candidate to diff against. Preference order:
	//  1. a schema-match survivor (method+path+keys already matched — the
	//     interesting drift is in query values, headers, or the body)
	//  2. the lowest-Levenshtein "METHOD path" mock from the pool.
	closestMock := pickClosestCandidate(request, schemaSurvivors, httpMocks)
	if closestMock == nil || closestMock.Spec.HTTPReq == nil {
		return mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
			WithDestination(dest).
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
