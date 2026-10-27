package models

import (
	"regexp"
	"strings"
)

// SensitiveKeyTokens mark a header, query or body key whose value keploy
// never prints, in a mismatch report or in a learned decision's evidence
// (About.Seen): such a key commonly carries a credential.
var SensitiveKeyTokens = []string{"auth", "key", "token", "secret", "cookie", "signature", "credential", "password", "passwd", "pwd"}

// IsSensitiveKey reports whether a header, query-param or JSON body key (or
// a place naming one, "header.authorization") looks like it carries a
// credential.
func IsSensitiveKey(name string) bool {
	ln := strings.ToLower(name)
	for _, t := range SensitiveKeyTokens {
		if strings.Contains(ln, t) {
			return true
		}
	}
	return false
}

// SecretValuePatterns match credential-SHAPED values, whatever the key: the
// value-shape backstop behind IsSensitiveKey. They are prefix and scheme
// based, not raw entropy: an entropy rule would also hide UUIDs, content
// hashes and idempotency keys, often the very value that drifted. Each
// matches a credential form with effectively no legitimate non-secret
// collision.
var SecretValuePatterns = []*regexp.Regexp{
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

// LooksLikeSecretValue reports whether a single header, query-param or JSON
// scalar value is credential-shaped (SecretValuePatterns), whatever its key.
func LooksLikeSecretValue(v string) bool {
	s := strings.TrimSpace(v)
	if len(s) < 8 {
		return false
	}
	for _, re := range SecretValuePatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
