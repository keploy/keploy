package mocknoise

import (
	"encoding/json"
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

// AppRandomClass reports whether v looks app-minted — a value the app
// generates fresh on every run, so it differs between record and replay — and
// names its class: "uuid", "hex" (16+ hex chars with a letter) or "nonce"
// (20+ mixed-entropy url-safe chars). Low-entropy values (small ints, enum
// strings, words) are never app-random: they collide across unrelated fields.
func AppRandomClass(v string) (string, bool) {
	return appRandomClass(v)
}

// appRandomClass reports whether v looks like a per-request application-minted
// random value and names its class. Conservative by design — it accepts only
// a UUID, a long hex token that actually contains hex letters (so a long
// decimal id or timestamp is NOT mistaken for random), or a long mixed
// letters+digits nonce; it rejects short strings, pure integers, and all-alpha
// words (names, enum labels). A false positive here would wrongly wildcard a
// field that must match exactly.
//
// It runs on every candidate word of every mock a replay stages, so it checks
// bytes instead of matching regular expressions.
func appRandomClass(v string) (string, bool) {
	s := strings.TrimSpace(v)
	switch {
	case isUUID(s):
		return "uuid", true
	case len(s) >= 16 && all(s, isHex) && hasHexLetter(s):
		return "hex", true
	case len(s) >= 20 && all(s, isValueByte) && hasMixedEntropy(s):
		return "nonce", true
	}
	return "", false
}

// isUUID reports whether s is 8-4-4-4-12 hex digits.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
		} else if !isHex(s[i]) {
			return false
		}
	}
	return true
}

func all(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isValueByte reports whether c can be part of an app-random value: every
// class AppRandomClass accepts is made of [0-9A-Za-z_-] alone, so a scanner
// looking for such values can split text at any other byte.
func isValueByte(c byte) bool { return valueBytes[c] }

// minValueLen is the length of the shortest app-random value (a 16-char hex
// token).
const minValueLen = 16

// ScanWords calls visit with the bounds of every maximal run of value bytes
// (isValueByte) in s at least minValueLen long: every place an app-random
// value can sit whole. One pass, no allocation.
func ScanWords(s string, visit func(start, end int)) {
	start := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && valueBytes[s[i]] {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= minValueLen {
			visit(start, i)
		}
		start = -1
	}
}

// ReplaceWords returns s with every word (see ScanWords) that lookup knows
// replaced by what it returns; s itself when none is. Whole words only: a
// value that is part of a longer run of value bytes ("order-<uuid>") is
// another word, and is left alone — everywhere the same way, so the mock side
// and the test side of a replay never disagree about it.
func ReplaceWords(s string, lookup func(word string) (string, bool)) string {
	var b []byte
	last := 0
	ScanWords(s, func(start, end int) {
		to, ok := lookup(s[start:end])
		if !ok || to == s[start:end] {
			return
		}
		if b == nil {
			b = make([]byte, 0, len(s))
		}
		b = append(b, s[last:start]...)
		b = append(b, to...)
		last = end
	})
	if b == nil {
		return s
	}
	return string(append(b, s[last:]...))
}

var valueBytes = func() (t [256]bool) {
	for c := 0; c < 256; c++ {
		b := byte(c)
		t[c] = (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '-'
	}
	return t
}()

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
