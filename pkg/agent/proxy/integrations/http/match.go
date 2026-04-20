package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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

func (h *HTTP) match(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise map[string][]string) (bool, *models.Mock, error) {
	for {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
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
			return false, nil, errors.New("error while matching the request with the mocks")
		}
		sessionMocks, err := mockDb.GetSessionMocks()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get session mocks")
			return false, nil, errors.New("error while matching the request with the mocks")
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

		// Matching process
		schemaMatched, err := h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise)
		if err != nil {
			return false, nil, err
		}

		if len(schemaMatched) == 0 {
			return false, nil, nil
		}

		h.Logger.Debug("http mock schema match results",
			zap.Int("schema_matched", len(schemaMatched)),
			zap.Int("total_http_mocks", len(unfilteredMocks)))

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			h.Logger.Debug("exact body match found", zap.String("mock name", bestMatch.Name))
			if !h.updateMock(ctx, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}

		shortListed := schemaMatched
		// Schema match for JSON bodies
		if pkg.IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, schemaMatched, input.body)
			if err != nil {
				return false, nil, err
			}

			if len(bodyMatched) == 0 {
				h.Logger.Debug("No mock found with body schema match")
				return false, nil, nil
			}

			if len(bodyMatched) == 1 {
				h.Logger.Debug("body match found", zap.String("mock name", bodyMatched[0].Name))
				if !h.updateMock(ctx, bodyMatched[0], mockDb) {
					continue
				}
				return true, bodyMatched[0], nil
			}

			// More than one match, perform fuzzy match
			shortListed = bodyMatched
		}

		h.Logger.Debug("Performing fuzzy match for req buffer")
		// Perform fuzzy match on the request
		isMatched, bestMatch := h.PerformFuzzyMatch(shortListed, input.raw)
		if isMatched {
			h.Logger.Debug("fuzzy match found a matching mock", zap.String("mock name", bestMatch.Name))
			if !h.updateMock(ctx, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}
		return false, nil, nil
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

func (h *HTTP) MatchURLPath(mockURL, reqPath string) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	h.Logger.Debug("parsed URL", zap.Any("parsed URL", parsedURL.Path), zap.Any("req path", reqPath))
	return parsedURL.Path == reqPath
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
func (h *HTTP) SchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock, headerNoise map[string][]string) ([]*models.Mock, error) {
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
		if !h.MatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path) {
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
// Second pass: noise-aware JSON comparison that skips obfuscated fields
// identified by Mock.Noise patterns.
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
	// Pre-parse request body once to avoid repeated JSON parsing per mock.
	isReqJSON := pkg.IsJSON(body)
	var reqData interface{}
	if isReqJSON {
		if err := json.Unmarshal(body, &reqData); err != nil {
			isReqJSON = false
		}
	}

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

// updateMock processes the matched mock based on its Lifetime.
// Per-test mocks are CONSUMED on match (DeleteFilteredMock); session /
// connection mocks are RETAINED and updated in place (UpdateUnFilteredMock).
// See the MySQL equivalent in replayer/match.go for the pre- vs post-
// Phase-2 routing rationale.
func (h *HTTP) updateMock(_ context.Context, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	originalMatchedMock := *matchedMock
	matchedMock.TestModeInfo.IsFiltered = false
	matchedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()

	lifetime := matchedMock.TestModeInfo.Lifetime
	rawConfig := false
	if matchedMock.Spec.Metadata != nil {
		rawConfig = matchedMock.Spec.Metadata["type"] == "config"
	}
	isSessionOrConnection := lifetime == models.LifetimeSession ||
		lifetime == models.LifetimeConnection ||
		(lifetime == models.LifetimePerTest && rawConfig)

	if isSessionOrConnection {
		return mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
	}
	// Per-test: consume via DeleteFilteredMock, with fallback to
	// UpdateUnFilteredMock for mocks staged into the session pool
	// during the initial pre-first-test window.
	if mockDb.DeleteFilteredMock(originalMatchedMock) {
		return true
	}
	return mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
}

// buildHTTPMismatchReport finds the closest HTTP mock to the given request
// and returns a human-readable diff report. If httpMocks is nil, it fetches
// from mockDb; otherwise it uses the pre-fetched slice to avoid a redundant read.
func (h *HTTP) buildHTTPMismatchReport(request *http.Request, mockDb integrations.MockMemDb, httpMocks []*models.Mock) *models.MockMismatchReport {
	if httpMocks == nil {
		// Mirror match()'s pool-merging + ordering strategy so
		// mismatch diagnostics see the same candidate set in the
		// same order the matcher saw. Per-test FIRST, session second.
		perTestMocks, err := mockDb.GetPerTestMocksInWindow()
		if err != nil {
			return &models.MockMismatchReport{
				Protocol:      "HTTP",
				ActualSummary: fmt.Sprintf("%s %s", request.Method, request.URL.Path),
				NextSteps:     "Failed to read mock database. Check logs for errors and retry.",
			}
		}
		sessionMocks, err := mockDb.GetSessionMocks()
		if err != nil {
			return &models.MockMismatchReport{
				Protocol:      "HTTP",
				ActualSummary: fmt.Sprintf("%s %s", request.Method, request.URL.Path),
				NextSteps:     "Failed to read mock database. Check logs for errors and retry.",
			}
		}
		mocks := make([]*models.Mock, 0, len(perTestMocks)+len(sessionMocks))
		mocks = append(mocks, perTestMocks...)
		mocks = append(mocks, sessionMocks...)
		if len(mocks) == 0 {
			return &models.MockMismatchReport{
				Protocol:      "HTTP",
				ActualSummary: fmt.Sprintf("%s %s", request.Method, request.URL.Path),
				NextSteps:     "No HTTP mocks available. Re-record mocks to capture this endpoint.",
			}
		}
		httpMocks = FilterHTTPMocks(mocks)
	}
	if len(httpMocks) == 0 {
		return &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: fmt.Sprintf("%s %s", request.Method, request.URL.Path),
			NextSteps:     "No HTTP mocks available. Re-record mocks to capture this endpoint.",
		}
	}

	// Find closest mock: first try same-method mocks (cheap filter), then fall back to all.
	actualKey := request.Method + " " + request.URL.Path
	bestDist := -1
	var closestMock *models.Mock
	// Two-pass: first same method only, then all if no match found
	for pass := 0; pass < 2; pass++ {
		for _, mock := range httpMocks {
			if mock.Spec.HTTPReq == nil {
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
				closestMock = mock
			}
		}
		if closestMock != nil {
			break
		}
	}
	if closestMock == nil || closestMock.Spec.HTTPReq == nil {
		return &models.MockMismatchReport{
			Protocol:      "HTTP",
			ActualSummary: actualKey,
			NextSteps:     "Re-record mocks if the API endpoint or request format has changed.",
		}
	}

	// Build diff details
	var diffs []string
	mockReq := closestMock.Spec.HTTPReq
	if string(mockReq.Method) != request.Method {
		diffs = append(diffs, fmt.Sprintf("method: %q vs %q", request.Method, mockReq.Method))
	}
	mockPath := mockReq.URL
	if parsed, err := url.Parse(mockReq.URL); err == nil {
		mockPath = parsed.Path
	}
	if mockPath != request.URL.Path {
		diffs = append(diffs, fmt.Sprintf("path: %q vs %q", request.URL.Path, mockPath))
	}

	diff := strings.Join(diffs, "; ")
	if diff == "" {
		diff = "method and path match but headers or body differ"
	}

	return &models.MockMismatchReport{
		Protocol:      "HTTP",
		ActualSummary: actualKey,
		ClosestMock:   closestMock.Name,
		Diff:          diff,
		NextSteps:     "Re-record mocks if the API endpoint or request format has changed.",
	}
}
