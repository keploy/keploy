package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// valueRedacted replaces sensitive/obfuscated values in the rendered requests.
const valueRedacted = "(redacted)"

// sensitiveKeyTokens marks header / query / body key names whose values must
// never be printed into a report (they commonly carry credentials).
var sensitiveKeyTokens = []string{"auth", "key", "token", "secret", "cookie", "signature", "credential", "password", "passwd", "pwd"}

// isSensitiveHeader reports whether a header, query-param, or JSON body key
// looks like it carries a credential and should be redacted.
func isSensitiveHeader(name string) bool {
	ln := strings.ToLower(name)
	for _, t := range sensitiveKeyTokens {
		if strings.Contains(ln, t) {
			return true
		}
	}
	return false
}

// secretValuePatterns matches credential-SHAPED values, independent of the key
// name or the mock's noise config — the value-shape backstop behind the
// key-name denylist and the noise regexes. They are deliberately
// prefix/scheme-based (not raw Shannon entropy): a generic "high-entropy"
// rule would also redact UUIDs, content hashes and idempotency keys, which are
// frequently the exact field that drifted and caused the mismatch — redacting
// those would gut the diagnostic value of the diff. Each pattern matches a
// credential FORM with effectively no legitimate non-secret collision.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),              // Authorization schemes
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`), // JWT (header.payload.sig)
	regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----`),                            // PEM private key
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),                                // AWS access key id
	regexp.MustCompile(`\bgh[posru]_[A-Za-z0-9]{20,}\b`),                               // GitHub token
	regexp.MustCompile(`\bkep_[A-Za-z0-9_-]{20,}\b`),                                   // keploy PAT
	regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{16,}\b`),                        // Stripe secret key
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                             // Slack token
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),                                 // GitLab PAT
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}\b`),                                    // Google API key
}

// looksLikeSecretValue reports whether a single header / query-param /
// JSON-scalar value is credential-shaped and must be redacted even when its key
// isn't on the denylist and the mock's noise config didn't cover it.
func looksLikeSecretValue(v string) bool {
	s := strings.TrimSpace(v)
	if len(s) < 8 {
		return false
	}
	for _, re := range secretValuePatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// scrubSecretsInText replaces credential-shaped tokens embedded in free-form
// (opaque, non-JSON / non-form) body text with the redaction marker, leaving
// the surrounding structure intact so the diff stays useful.
func scrubSecretsInText(s string) string {
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, valueRedacted)
	}
	return s
}

// redactFieldDiffs redacts the Expected/Actual values of structured field diffs
// in place. Both values are redacted at a place the recording redacted
// (places: by the place, as the live value matches none of the recording's
// patterns), and a value is redacted when its path key looks sensitive, it
// matches the mock's noise patterns or it is credential-shaped. These diffs —
// and the one-line Diff string derived from them — are persisted into the
// report YAML and the platform API, so raw secrets must never reach them (the
// whole-request renders are sanitized separately).
//
// livePath is the call's escaped path, the one its "path" diff names decoded.
func redactFieldDiffs(diffs []models.MockFieldDiff, places redactedPlaces, livePath string) {
	for i := range diffs {
		places.redactDiff(&diffs[i], livePath)
		sensitive := isSensitiveHeader(diffs[i].Path)
		if sensitive || places.noisy(diffs[i].Expected) || looksLikeSecretValue(diffs[i].Expected) {
			diffs[i].Expected = valueRedacted
		}
		if sensitive || places.noisy(diffs[i].Actual) || looksLikeSecretValue(diffs[i].Actual) {
			diffs[i].Actual = valueRedacted
		}
	}
}

// renderMockRequest renders the closest mock's recorded request for the LEFT
// side of the side-by-side diff: request line, sorted headers (sensitive /
// obfuscated values redacted), blank line, JSON-pretty body. mockNoise is the
// mock's Mock.Noise so obfuscated values stay redacted.
func renderMockRequest(mockReq *models.HTTPReq, mockNoise []string) string {
	if mockReq == nil {
		return ""
	}
	header := make(map[string]string, len(mockReq.Header))
	for k, v := range mockReq.Header {
		header[k] = v
	}
	places := redactedPlacesOf(mockReq, mockNoise)
	path := mockURLPath(mockReq.URL)
	if _, err := parseRecordedURL(mockReq.URL); err == nil {
		path = places.decodedPath(places.recordedPath)
	}
	return renderRequestPreview(string(mockReq.Method), path, mockQueryString(mockReq), header, mockReq.Body, places)
}

// mockQueryString returns the mock's query string, mirroring the matcher's
// source-of-truth fallback: recorded URLParams when present, otherwise the
// query parsed from the full URL. Without the fallback, recordings that store
// the query only in URL would render a false URL difference vs the live request.
func mockQueryString(mockReq *models.HTTPReq) string {
	if len(mockReq.URLParams) > 0 {
		return urlParamsQuery(mockReq.URLParams)
	}
	if parsed, err := url.Parse(mockReq.URL); err == nil {
		return parsed.RawQuery
	}
	return ""
}

// renderLiveRequest renders the live request for the RIGHT side, in the exact
// same format as renderMockRequest so the two line up in the diff. places are
// the closest mock's (redactedPlacesOf): the live value at each is redacted.
func renderLiveRequest(request *http.Request, body []byte, places redactedPlaces) string {
	if request == nil {
		return ""
	}
	header := make(map[string]string, len(request.Header))
	for k := range request.Header {
		header[k] = strings.Join(request.Header.Values(k), ", ")
	}
	return renderRequestPreview(request.Method, places.decodedPath(request.URL.EscapedPath()), request.URL.RawQuery, header, string(body), places)
}

// mockURLPath extracts just the path from a mock's URL (mocks store full URL
// strings); falls back to the raw string when it doesn't parse.
func mockURLPath(mockURL string) string {
	if parsed, err := url.Parse(mockURL); err == nil {
		return parsed.Path
	}
	return mockURL
}

// urlParamsQuery renders a mock's recorded URLParams in canonical (sorted)
// query-string form so it lines up against the live request's query.
func urlParamsQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	vals := url.Values{}
	for k, v := range params {
		vals.Set(k, v)
	}
	return vals.Encode()
}

// Rendering bounds. The rendered requests are persisted into the report and
// held in the agent's bounded pending-error queue, so a single pathological
// value or body must not be able to bloat memory unbounded — cap them.
const (
	maxRenderValueLen = 256  // a single header / query value
	maxRenderBodyLen  = 4096 // the rendered body
)

// renderRequestPreview is the shared renderer behind both sides of the diff.
// Sensitive header/query/body values (by key name, by the place the recording
// redacted, or by the mock's noise patterns) are redacted before they ever
// land in a report, and every value and the body are length-capped.
func renderRequestPreview(method, path, rawQuery string, header map[string]string, body string, places redactedPlaces) string {

	var b strings.Builder
	target := path
	if rawQuery != "" {
		// Canonicalize so semantically equal queries render identically on
		// both sides, and redact sensitive / obfuscated parameter values so
		// credentials in the query string never reach the report.
		target += "?" + redactQuery(rawQuery, places.values, func(k string) bool { return places.query[k] })
	}
	fmt.Fprintf(&b, "%s %s", method, target)

	var keys []string
	for k := range header {
		lk := strings.ToLower(k)
		// Skip keploy-internal and per-message framing headers — they differ
		// on every request without ever influencing matching, so showing them
		// would highlight rows that cannot be the cause of the miss.
		if strings.HasPrefix(lk, "keploy") || lk == "content-length" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := header[k]
		if isSensitiveHeader(k) || places.headers[strings.ToLower(k)] || places.noisy(v) || looksLikeSecretValue(v) {
			v = valueRedacted
		} else {
			v = truncateValue(v, maxRenderValueLen)
		}
		fmt.Fprintf(&b, "\n%s: %s", textproto.CanonicalMIMEHeaderKey(k), v)
	}
	if body != "" {
		fmt.Fprintf(&b, "\n\n%s", truncateValue(redactBody(body, headerValue(header, "content-type"), places), maxRenderBodyLen))
	}
	return b.String()
}

// headerValue does a case-insensitive lookup in a header map.
func headerValue(header map[string]string, name string) string {
	for k, v := range header {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// truncateValue rune-safely caps s, appending an ellipsis when it cuts.
func truncateValue(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(truncated)"
}

// redactQuery canonicalizes a query string and redacts parameter values whose
// key looks sensitive or the recording redacted (redacted), or whose value
// matches the mock's noise patterns. On a parse failure it fails
// closed — the raw query is never emitted, since it could carry credentials.
func redactQuery(rawQuery string, nc *util.NoiseChecker, redacted func(string) bool) string {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return valueRedacted
	}
	for k, vs := range vals {
		for i, v := range vs {
			if isSensitiveHeader(k) || redacted(k) || (nc != nil && v != "" && nc.IsNoisy(v)) || looksLikeSecretValue(v) {
				vs[i] = valueRedacted
			} else {
				vs[i] = truncateValue(v, maxRenderValueLen)
			}
		}
	}
	return vals.Encode()
}

// redactBody redacts secret / obfuscated fields in a request body before it is
// rendered into a report. A body the recording redacted whole is redacted
// whole. JSON bodies are walked field-by-field — a value is redacted when its
// key looks sensitive or the recording redacted its place (regardless of its
// type), or the value matches the mock's noise patterns — and re-indented one
// field per line so line diffs align. Form-urlencoded and multipart bodies are
// redacted per field, the same way. When the recording redacted a field of
// its body, the call's body is read as the recording's was (redactFields), and
// one that does not read that way (JSON that does not parse, a multipart form
// cut short, another type) is redacted whole: the field's live value is in it,
// at a place that cannot be found. Other non-JSON bodies are redacted
// wholesale when they match a noise pattern, else returned as-is (the caller
// length-caps them).
func redactBody(body, contentType string, places redactedPlaces) string {
	if places.body {
		return valueRedacted
	}
	if len(places.fields) > 0 {
		if out, ok := places.redactFields(body, contentType); ok {
			return out
		}
		return valueRedacted
	}
	if pkg.IsJSON([]byte(body)) {
		var v interface{}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return body
		}
		redactJSONValue(v, places, "")
		out, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return body
		}
		return string(out)
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "x-www-form-urlencoded") {
		return redactQuery(body, places.values, places.field)
	}
	if strings.Contains(ct, "multipart/form-data") {
		if out, ok := places.redactMultipart(body, contentType); ok {
			return out
		}
	}
	if places.noisy(body) {
		return valueRedacted
	}
	// Opaque body (XML / SOAP / plain-text / proto / GraphQL): there's no field
	// structure to walk, so scrub credential-shaped tokens in place as a
	// backstop instead of emitting the body verbatim.
	return scrubSecretsInText(body)
}

// redactJSONValue walks a decoded JSON value in place, path its place in the
// body (redactedPlaces.fields' dialect). A sensitive key's value, and the
// value at a place the recording redacted, is redacted whatever its type
// (string, number, object, array); string scalars — in objects and arrays —
// are redacted when they match a noise pattern or are credential-shaped.
func redactJSONValue(v interface{}, places redactedPlaces, path string) {
	redactScalar := func(s string) bool { return places.noisy(s) || looksLikeSecretValue(s) }
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			cp := jsonChildPath(path, k)
			if isSensitiveHeader(k) || places.field(cp) {
				t[k] = valueRedacted
				continue
			}
			if s, ok := val.(string); ok {
				if redactScalar(s) {
					t[k] = valueRedacted
				}
				continue
			}
			redactJSONValue(val, places, cp)
		}
	case []interface{}:
		ep := jsonElemPath(path)
		for i, val := range t {
			if places.field(ep) {
				t[i] = valueRedacted
				continue
			}
			if s, ok := val.(string); ok {
				if redactScalar(s) {
					t[i] = valueRedacted
				}
				continue
			}
			redactJSONValue(val, places, ep)
		}
	}
}
