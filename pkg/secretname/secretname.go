// Package secretname is keploy's one rule for whether a name (an
// environment variable's, a flag's, a field's, a header's) names a secret,
// keploy's sanitizer's rule for a field name (keploy's secret detector's
// context keywords and sensitive keys): its segments, split at _, -, . and
// the other non-letters and at camelCase, include a keyword (secretWords),
// or the name holds a word no other name holds (secretParts: PGPASSWORD,
// requirepass). A container's fingerprint leaves such a value out
// (containerkey), and a learned line prints it as *** (learn).
package secretname

import (
	"strings"
	"unicode"
)

// secretWords are the keywords a name's segments are compared with.
var secretWords = map[string]bool{
	"secret": true, "secrets": true, "token": true, "tokens": true, "key": true, "keys": true,
	"password": true, "passwords": true, "pass": true, "passwd": true, "pwd": true, "pw": true,
	"passphrase": true, "auth": true, "authorization": true, "credential": true, "credentials": true,
	"cred": true, "creds": true, "hmac": true, "signature": true, "sig": true, "encrypt": true,
	"cipher": true, "hash": true, "oauth": true, "apikey": true, "dsn": true, "pin": true,
	"otp": true, "cvv": true, "ssn": true, "private": true, "jaas": true, "salt": true,
	"pat": true, "jwt": true, "bearer": true, "cookie": true, "cookies": true,
}

// secretParts are words a name is a secret's for holding anywhere in it,
// its separators left out (connection_string: connectionstring).
var secretParts = []string{"password", "passwd", "pass", "secret", "token", "credential", "apikey", "passphrase", "privatekey", "privkey", "accesskey", "auth", "cookie", "connectionstring", "sessionid"}

// Name reports whether name names a secret.
func Name(name string) bool {
	joined := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
	for _, p := range secretParts {
		if strings.Contains(joined, p) {
			return true
		}
	}
	for _, seg := range nameSegments(name) {
		if secretWords[seg] {
			return true
		}
	}
	return false
}

// nameSegments splits name, lowercased, at every non-letter-or-digit and at
// each lower-to-upper change of case (clientSecret: client, secret).
func nameSegments(name string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return segs
}
