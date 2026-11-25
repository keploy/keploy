package mismatch

import (
	"regexp"
	"strings"
)

// ValueRedacted replaces sensitive or obfuscated values in mismatch reports
// and rendered requests.
const ValueRedacted = "(redacted)"

// sensitiveKeyTokens marks header / query / body key names whose values must
// never be printed into a report (they commonly carry credentials).
var sensitiveKeyTokens = []string{"auth", "key", "token", "secret", "cookie", "signature", "credential", "password", "passwd", "pwd"}

// IsSensitiveKey reports whether a header, query-param, or JSON body key/path
// looks like it carries a credential and should be redacted.
func IsSensitiveKey(name string) bool {
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

// LooksLikeSecretValue reports whether a single header / query-param /
// JSON-scalar value is credential-shaped and must be redacted even when its key
// isn't on the denylist and the mock's noise config didn't cover it.
func LooksLikeSecretValue(v string) bool {
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

// ScrubSecretsInText replaces credential-shaped tokens embedded in free-form
// (opaque, non-JSON / non-form) body text with ValueRedacted, leaving the
// surrounding structure intact so the diff stays useful.
func ScrubSecretsInText(s string) string {
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, ValueRedacted)
	}
	return s
}

// SplitTypedDiffValue splits a matcher type_changed diff side ("<type>: <value>")
// into its JSON type prefix and bare value. ok is false for container kinds
// ("object", "array") or any value without a known scalar type prefix.
func SplitTypedDiffValue(v string) (prefix, raw string, ok bool) {
	for _, p := range []string{"string: ", "number: ", "boolean: ", "null: "} {
		if raw, found := strings.CutPrefix(v, p); found {
			return p, raw, true
		}
	}
	return "", "", false
}

// ValueRedactor returns a pre-cut scalar redaction predicate combining an
// optional mock-noise matcher (isNoisy, e.g. NoiseChecker.IsNoisy) with
// LooksLikeSecretValue. Pass it as redactValue to
// JSONBodyDiffsWithTypeStrictRedacted / matcher.JSONFieldDiffsWithTypeStrictRedacted
// so placeholders and credential-shaped values of any length are redacted
// before truncation.
func ValueRedactor(isNoisy func(string) bool) func(string) bool {
	if isNoisy == nil {
		return LooksLikeSecretValue
	}
	return func(raw string) bool {
		return isNoisy(raw) || LooksLikeSecretValue(raw)
	}
}

// RedactDiffValue redacts a single Expected or Actual field-diff side. Empty
// values stay empty so absent-side semantics (missing_in_live / missing_in_mock)
// are preserved. For type_changed diffs rendered as "<type>: <value>", it
// inspects the bare value so anchored obfuscator patterns (^...$) and
// credential-shape backstops match cleanly without touching the type prefix.
func RedactDiffValue(v string, isTypeChanged, sensitive bool, shouldRedact func(string) bool) string {
	if v == "" {
		return ""
	}
	if sensitive {
		return ValueRedacted
	}
	if isTypeChanged {
		if prefix, raw, ok := SplitTypedDiffValue(v); ok {
			if shouldRedact != nil && shouldRedact(raw) {
				return prefix + ValueRedacted
			}
			return v
		}
	}
	if shouldRedact != nil && shouldRedact(v) {
		return ValueRedacted
	}
	return v
}
