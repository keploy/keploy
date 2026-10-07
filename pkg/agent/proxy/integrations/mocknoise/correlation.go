package mocknoise

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// CorrelationsFromMock detects request→response value echoes on an HTTP mock: a
// scalar value in the recorded REQUEST body that looks application-minted and
// random (a UUID, long hex token, or high-entropy nonce) AND appears verbatim
// in the recorded RESPONSE body. Each such value becomes a FieldCorrelation so
// replay can bind the live request value and render it into the response,
// instead of replaying the stale recorded value (a mismatch) or masking it as
// noise (which hides regressions).
//
// Detection is STATIC — it reads only the recording — because an echoed value
// breaks request matching on the first replay (the live value differs), so
// there is no matched candidate to diff against; the recording is the only
// reliable source. It is deliberately conservative (see appRandomClass): a
// false positive would wildcard a field that must match exactly and collapse
// distinct calls. Scope (v1): JSON request + response BODIES; headers, URL
// query and non-JSON bodies are a documented follow-up.
func CorrelationsFromMock(m *models.Mock) []models.FieldCorrelation {
	if m == nil || m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
		return nil
	}
	reqFlat := flattenJSONBody(m.Spec.HTTPReq.Body)
	respFlat := flattenJSONBody(m.Spec.HTTPResp.Body)
	if len(reqFlat) == 0 || len(respFlat) == 0 {
		return nil
	}

	// Index response scalar values → the body paths that carry them.
	respByValue := make(map[string][]string, len(respFlat))
	for path, vals := range respFlat {
		for _, v := range vals {
			respByValue[v] = append(respByValue[v], "body."+path)
		}
	}

	var out []models.FieldCorrelation
	seen := make(map[string]struct{})
	for path, vals := range reqFlat {
		for _, v := range vals {
			class, ok := appRandomClass(v)
			if !ok {
				continue
			}
			respPaths, echoed := respByValue[v]
			if !echoed {
				continue
			}
			reqPath := "body." + path
			if _, dup := seen[reqPath]; dup {
				continue
			}
			seen[reqPath] = struct{}{}
			rp := append([]string(nil), respPaths...)
			sort.Strings(rp)
			out = append(out, models.FieldCorrelation{
				RequestPath:   reqPath,
				ResponsePaths: rp,
				RecordedValue: v,
				ValueClass:    class,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
	return out
}

// flattenJSONBody parses a JSON body and returns matcher.Flatten's path→values
// map; nil for an empty or non-JSON body.
func flattenJSONBody(body string) map[string][]string {
	b := strings.TrimSpace(body)
	if b == "" {
		return nil
	}
	var j interface{}
	if err := json.Unmarshal([]byte(b), &j); err != nil {
		return nil
	}
	return matcher.Flatten(j)
}

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRe   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	nonceRe = regexp.MustCompile(`^[0-9A-Za-z_-]{20,}$`)
)

// appRandomClass reports whether v looks like a per-request application-minted
// random value and names its class. Conservative by design — it accepts only
// a UUID, a long hex token that actually contains hex letters (so a long
// decimal id or timestamp is NOT mistaken for random), or a long mixed
// letters+digits nonce; it rejects short strings, pure integers, and all-alpha
// words (names, enum labels). A false positive here would wrongly wildcard a
// field that must match exactly.
func appRandomClass(v string) (string, bool) {
	s := strings.TrimSpace(v)
	switch {
	case uuidRe.MatchString(s):
		return "uuid", true
	case hexRe.MatchString(s) && hasHexLetter(s):
		return "hex", true
	case nonceRe.MatchString(s) && hasMixedEntropy(s):
		return "nonce", true
	}
	return "", false
}

// hasHexLetter reports whether s contains at least one a-f/A-F hex letter, so a
// long run of decimal digits (an id or epoch) is not classified as a hex token.
func hasHexLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			return true
		}
	}
	return false
}

// hasMixedEntropy requires both a letter and a digit, filtering out long
// all-alpha words and long all-digit values that are not high-entropy nonces.
func hasMixedEntropy(s string) bool {
	var hasAlpha, hasDigit bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasAlpha = true
		}
	}
	return hasAlpha && hasDigit
}

// MaterializeCorrelations computes and stores request→response echo
// correlations on a mock at replay-ingest — once, before the mock enters the
// concurrent runtime pool — so the honor-on-replay matcher has them without a
// per-match recompute or a shared-pointer mutation race. No-op when the mock
// already carries correlations (a learn pass or disk already set them) or has
// none to find. Detection is static (reads only the recording), so this is the
// natural place to run it. Unconditional by design: it only populates an
// in-memory field; the replay gate controls whether honor acts on it.
func MaterializeCorrelations(m *models.Mock) {
	if m == nil || len(m.Spec.Correlations) > 0 {
		return
	}
	if c := CorrelationsFromMock(m); len(c) > 0 {
		m.Spec.Correlations = c
	}
}
