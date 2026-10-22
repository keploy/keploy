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

// TokenValue is the value that, put in place of each token v of rec
// (ReplaceToken), makes rec into live, exactly: "1792678752275" of
// "e2e-role-1792678689546" and "e2e-role-1792678752275" for v
// "1792678689546". ok is false when rec holds no token v, when no one value
// does it (live differs from rec elsewhere too, or holds two values in v's
// places), or when that value is empty or v itself.
func TokenValue(rec, v, live string) (string, bool) {
	if v == "" {
		return "", false
	}
	var at []int
	for i := 0; i <= len(rec)-len(v); {
		j := strings.Index(rec[i:], v)
		if j < 0 {
			break
		}
		if tokenAt(rec, v, i+j) {
			at = append(at, i+j)
			i += j + len(v)
			continue
		}
		i += j + 1
	}
	if len(at) == 0 {
		return "", false
	}
	// Each of the len(at) places grows by the same amount.
	grow := len(live) - len(rec)
	if grow%len(at) != 0 {
		return "", false
	}
	n := len(v) + grow/len(at)
	if n <= 0 || at[0]+n > len(live) {
		return "", false
	}
	w := live[at[0] : at[0]+n]
	if w == v {
		return "", false
	}
	if with, _ := ReplaceToken(rec, v, w); with != live {
		return "", false
	}
	return w, true
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
