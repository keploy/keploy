package learn

import "strings"

// isWord reports whether c continues a token: an ASCII letter or digit.
func isWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tokenAt reports whether v occurs in s at i as a token: with no letter or
// digit right before or right after it.
func tokenAt(s, v string, i int) bool {
	if i > 0 && isWord(s[i-1]) && isWord(v[0]) {
		return false
	}
	if j := i + len(v); j < len(s) && isWord(s[j]) && isWord(v[len(v)-1]) {
		return false
	}
	return true
}

// ContainsToken reports whether s holds v as a token: "/orders/42" holds
// "42", "order-42x" does not.
func ContainsToken(s, v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i <= len(s)-len(v); {
		j := strings.Index(s[i:], v)
		if j < 0 {
			return false
		}
		if tokenAt(s, v, i+j) {
			return true
		}
		i += j + 1
	}
	return false
}
