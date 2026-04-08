package util

import "strings"

// ObfuscationPrefix is the marker prepended to values that have been
// redacted by Keploy's secret-protection feature. During mock matching
// any value starting with this prefix is automatically treated as a match
// and excluded from the similarity score.
const ObfuscationPrefix = "__KEPLOY_REDACT__:"

// IsObfuscated reports whether value is a string that starts with the
// obfuscation prefix, indicating it was redacted and should be skipped
// during matching.
func IsObfuscated(value interface{}) bool {
	s, ok := value.(string)
	return ok && strings.HasPrefix(s, ObfuscationPrefix)
}

// ContainsObfuscatedValue reports whether s contains at least one
// occurrence of the obfuscation prefix anywhere in the string.
func ContainsObfuscatedValue(s string) bool {
	return strings.Contains(s, ObfuscationPrefix)
}
