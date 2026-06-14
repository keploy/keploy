package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
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
	return renderRequestPreview(string(mockReq.Method), mockURLPath(mockReq.URL), urlParamsQuery(mockReq.URLParams), header, mockReq.Body, mockNoise)
}

// renderLiveRequest renders the live request for the RIGHT side, in the exact
// same format as renderMockRequest so the two line up in the diff.
func renderLiveRequest(request *http.Request, body []byte, mockNoise []string) string {
	if request == nil {
		return ""
	}
	header := make(map[string]string, len(request.Header))
	for k := range request.Header {
		header[k] = strings.Join(request.Header.Values(k), ", ")
	}
	return renderRequestPreview(request.Method, request.URL.Path, request.URL.RawQuery, header, string(body), mockNoise)
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
// Sensitive header/query/body values (by key name or by the mock's noise
// patterns) are redacted before they ever land in a report, and every value
// and the body are length-capped.
func renderRequestPreview(method, path, rawQuery string, header map[string]string, body string, noise []string) string {
	nc := util.NewNoiseChecker(noise)
	isObfuscated := func(v string) bool { return v != "" && nc != nil && nc.IsNoisy(v) }

	var b strings.Builder
	target := path
	if rawQuery != "" {
		// Canonicalize so semantically equal queries render identically on
		// both sides, and redact sensitive / obfuscated parameter values so
		// credentials in the query string never reach the report.
		target += "?" + redactQuery(rawQuery, nc)
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
		if isSensitiveHeader(k) || isObfuscated(v) {
			v = valueRedacted
		} else {
			v = truncateValue(v, maxRenderValueLen)
		}
		fmt.Fprintf(&b, "\n%s: %s", textproto.CanonicalMIMEHeaderKey(k), v)
	}
	if body != "" {
		fmt.Fprintf(&b, "\n\n%s", truncateValue(redactBody(body, nc), maxRenderBodyLen))
	}
	return b.String()
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
// key looks sensitive or whose value matches the mock's noise patterns.
func redactQuery(rawQuery string, nc *util.NoiseChecker) string {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	for k, vs := range vals {
		for i, v := range vs {
			if isSensitiveHeader(k) || (nc != nil && v != "" && nc.IsNoisy(v)) {
				vs[i] = valueRedacted
			} else {
				vs[i] = truncateValue(v, maxRenderValueLen)
			}
		}
	}
	return vals.Encode()
}

// redactBody redacts secret / obfuscated fields in a request body before it is
// rendered into a report. JSON bodies are walked field-by-field — a value is
// redacted when its key looks sensitive or the value matches the mock's noise
// patterns — and re-indented one field per line so line diffs align. Non-JSON
// bodies are redacted wholesale when they match a noise pattern, else returned
// as-is (the caller length-caps them).
func redactBody(body string, nc *util.NoiseChecker) string {
	if !pkg.IsJSON([]byte(body)) {
		if nc != nil && nc.IsNoisy(body) {
			return valueRedacted
		}
		return body
	}
	var v interface{}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return body
	}
	redactJSONValue(v, nc)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return body
	}
	return string(out)
}

func redactJSONValue(v interface{}, nc *util.NoiseChecker) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if isSensitiveHeader(k) || (nc != nil && s != "" && nc.IsNoisy(s)) {
					t[k] = valueRedacted
					continue
				}
			}
			redactJSONValue(val, nc)
		}
	case []interface{}:
		for _, val := range t {
			redactJSONValue(val, nc)
		}
	}
}
