package learn

import (
	"sort"
	"strings"
)

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
func ContainsToken(s, v string) bool { return tokenIndex(s, v) >= 0 }

// tokenIndex is where s first holds v as a token (ContainsToken), -1 where
// it does not.
var tokenIndex = func(s, v string) int {
	if v == "" {
		return -1
	}
	for i := 0; i <= len(s)-len(v); {
		j := strings.Index(s[i:], v)
		if j < 0 {
			return -1
		}
		if tokenAt(s, v, i+j) {
			return i + j
		}
		i += j + 1
	}
	return -1
}

// ReplaceToken is s with each occurrence of v as a token (ContainsToken)
// replaced by with, and whether there was one: "USER:42" with 42 as 7 is
// "USER:7", "order-42x" is unchanged.
func ReplaceToken(s, v, with string) (string, bool) {
	if v == "" {
		return s, false
	}
	var b strings.Builder
	found := false
	i := 0
	for i <= len(s)-len(v) {
		j := strings.Index(s[i:], v)
		if j < 0 {
			break
		}
		at := i + j
		if !tokenAt(s, v, at) {
			b.WriteString(s[i : at+1])
			i = at + 1
			continue
		}
		b.WriteString(s[i:at])
		b.WriteString(with)
		found = true
		i = at + len(v)
	}
	if !found {
		return s, false
	}
	b.WriteString(s[i:])
	return b.String(), true
}

// ReplaceTokens is s with each key of with that stands as a token (tokenAt)
// replaced by its value, read once over s: what a replacement puts in is not
// read again, and where two keys begin at one place the longer is taken.
// missing are the keys that stand nowhere in s.
func ReplaceTokens(s string, with map[string]string) (out string, missing []string) {
	var b strings.Builder
	found := map[string]bool{}
	for i := 0; i < len(s); {
		key := ""
		for k := range with {
			if k != "" && len(k) > len(key) && strings.HasPrefix(s[i:], k) && tokenAt(s, k, i) {
				key = k
			}
		}
		if key == "" {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(with[key])
		found[key] = true
		i += len(key)
	}
	for k := range with {
		if !found[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return b.String(), missing
}
