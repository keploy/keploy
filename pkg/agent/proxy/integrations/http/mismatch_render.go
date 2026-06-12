package http

import (
	"bytes"
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

// Rendering bounds for the side-by-side whole-mock diff. These cap a single
// unmatched request so a large payload can't bloat the report yaml or the CLI
// view.
const (
	renderMaxValueLen = 100  // longest rendered header value
	renderMaxBodyLen  = 2000 // body bytes shown (pretty-printed)
	renderMaxHeaders  = 12   // headers shown per side
)

// valueRedacted replaces sensitive/obfuscated values in the rendered requests.
const valueRedacted = "(redacted)"

// sensitiveHeaderTokens marks header names whose values must never be printed
// into a report (they commonly carry credentials).
var sensitiveHeaderTokens = []string{"auth", "key", "token", "secret", "cookie", "signature", "credential"}

func isSensitiveHeader(name string) bool {
	ln := strings.ToLower(name)
	for _, t := range sensitiveHeaderTokens {
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

// renderRequestPreview is the shared renderer behind both sides of the diff.
func renderRequestPreview(method, path, rawQuery string, header map[string]string, body string, noise []string) string {
	nc := util.NewNoiseChecker(noise)
	isObfuscated := func(v string) bool { return v != "" && nc != nil && nc.IsNoisy(v) }

	var b strings.Builder
	target := path
	if rawQuery != "" {
		// Canonicalize so semantically equal queries render identically on
		// both sides regardless of original parameter order.
		if vals, err := url.ParseQuery(rawQuery); err == nil {
			target += "?" + vals.Encode()
		} else {
			target += "?" + rawQuery
		}
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
	shown := 0
	for _, k := range keys {
		if shown >= renderMaxHeaders {
			fmt.Fprintf(&b, "\n(+%d more headers)", len(keys)-shown)
			break
		}
		v := header[k]
		if isSensitiveHeader(k) || isObfuscated(v) {
			v = valueRedacted
		}
		fmt.Fprintf(&b, "\n%s: %s", textproto.CanonicalMIMEHeaderKey(k), truncateForDisplay(v, renderMaxValueLen))
		shown++
	}
	if body != "" {
		fmt.Fprintf(&b, "\n\n%s", truncateForDisplay(prettyJSONBody(body), renderMaxBodyLen))
	}
	return b.String()
}

// prettyJSONBody indents a JSON body one field per line so line-level diffs
// align to fields; non-JSON bodies are returned unchanged.
func prettyJSONBody(body string) string {
	if !pkg.IsJSON([]byte(body)) {
		return body
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(body), "", "  "); err != nil {
		return body
	}
	return buf.String()
}

// truncateForDisplay rune-safely truncates s to at most max characters,
// appending an ellipsis when something was cut.
func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
