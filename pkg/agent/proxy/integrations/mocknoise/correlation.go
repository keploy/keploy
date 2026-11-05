package mocknoise

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
// distinct calls. Scope: JSON request and response BODIES of an HTTP mock,
// and the values a Mongo write's answer echoes (mongoCorrelations). An HTTP
// mock's headers, URL query and non-JSON bodies are not read: a value there
// is matched as recorded.
func CorrelationsFromMock(m *models.Mock) []models.FieldCorrelation {
	if m != nil && m.Kind == models.Mongo {
		return mongoCorrelations(m)
	}
	if m == nil || m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
		return nil
	}
	// An answer with no body (a 204) echoes nothing, so its request, however
	// large, is not parsed either.
	if strings.TrimSpace(m.Spec.HTTPResp.Body) == "" {
		return nil
	}
	reqFlat := flattenJSONBody(m.Spec.HTTPReq.Body)

	// The request is read first, and the answer only for the app-random values
	// the request writes. This runs for every HTTP mock at replay start: an
	// answer to a call that writes none, or whose raw text cannot hold one, is
	// never parsed, so a large answer costs a few raw scans, not a parse and a
	// flatten of the whole document.
	written := make(map[string]string) // app-random request value → its class
	for _, vals := range reqFlat {
		for _, v := range vals {
			if class, ok := appRandomClass(v); ok {
				written[v] = class
			}
		}
	}
	if len(written) == 0 || !answerMayHold(m.Spec.HTTPResp.Body, written) {
		return nil
	}

	// Index the answer's scalar values the request wrote → the body paths that
	// carry them. Other values are never looked up, so they are not kept.
	respByValue := make(map[string][]string, len(written))
	for path, vals := range flattenJSONBody(m.Spec.HTTPResp.Body) {
		for _, v := range vals {
			if _, ok := written[v]; ok {
				respByValue[v] = append(respByValue[v], "body."+path)
			}
		}
	}

	var out []models.FieldCorrelation
	seen := make(map[string]struct{})
	for path, vals := range reqFlat {
		for _, v := range vals {
			respPaths, echoed := respByValue[v] // only written values are indexed
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
				ValueClass:    written[v],
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

// answerMayHold reports, from a JSON answer's raw text and without parsing
// it, whether the answer can carry a string equal to one of the written
// values. False only when it cannot: a written value is a run of the
// characters [0-9A-Za-z_-] with at most whitespace around it (appRandomClass),
// and JSON writes each of those characters either as itself or as a \u
// escape. So an answer whose text holds no written run, and escapes none of
// those characters, holds no written value.
func answerMayHold(body string, written map[string]string) bool {
	for v := range written {
		if strings.Contains(body, strings.TrimSpace(v)) {
			return true
		}
	}
	return escapesAValueChar(body)
}

// escapesAValueChar reports whether body holds a \uXXXX escape of a character
// an app-random value is made of. Encoders write those characters as
// themselves, so this is rare; an escaped backslash before a u counts too,
// which only makes the answer be parsed.
func escapesAValueChar(body string) bool {
	for rest := body; ; {
		i := strings.Index(rest, `\u`)
		if i < 0 {
			return false
		}
		rest = rest[i+2:]
		if len(rest) < 4 {
			return false
		}
		// Not four hex digits reads as 0, which is not a value character.
		r, _ := strconv.ParseUint(rest[:4], 16, 16)
		if isValueChar(rune(r)) {
			return true
		}
	}
}

// isValueChar reports whether r is one of [0-9A-Za-z_-], the characters every
// value appRandomClass accepts is made of. answerMayHold is exact only while
// that holds and while JSON writes none of them with a short escape such as
// the one for '/'. A test parses the class regexes and fails when one can
// match any other character; two more put runes and pairs of characters into
// a value of each class and fail when appRandomClass then accepts any other.
func isValueChar(r rune) bool {
	return r == '-' || r == '_' || ('0' <= r && r <= '9') || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z')
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
// natural place to run it. It only populates an in-memory field; the replay
// gate controls whether honor acts on it.
//
// Under the proven switch nothing is detected: what a value looks like
// decides nothing there. The learn step writes an echo as a variable of its
// call instead (models.MockVariable.Echo), from what changed between the
// recording and its replay.
func MaterializeCorrelations(m *models.Mock) {
	if m == nil || len(m.Spec.Correlations) > 0 || provenswitch.On() {
		return
	}
	if c := CorrelationsFromMock(m); len(c) > 0 {
		m.Spec.Correlations = c
	}
}
