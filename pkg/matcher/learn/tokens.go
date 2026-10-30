package learn

import (
	"slices"
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
func tokenIndex(s, v string) int {
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

// firstTokens is, for each of words, where texts first hold it as a token
// (tokenIndex), the texts read as one with a byte between each: the offset
// in the first text that holds it plus the lengths before that text, or the
// length of them all where none does. It reads each text once, not once per
// word, so many words in a long text take time linear in the text. A token
// can start only where tokenAt lets it: a word that begins with a letter or
// digit starts a run of letters and digits (and one made of them alone is
// that whole run), and a word that begins with another byte starts at that
// byte. So only those places are tried, with each length a word of that
// start has.
func firstTokens(texts, words []string) map[string]int {
	whole, rest := map[string]bool{}, map[string]bool{}
	empty := false
	// lens are the lengths of the words in rest by their first byte, and at
	// alnum those of the words in rest that begin with a letter or digit.
	const alnum = 256
	var lens [alnum + 1][]int
	for _, w := range words {
		switch {
		case w == "":
			empty = true
		case whole[w] || rest[w]:
		case wordsOnly(w):
			whole[w] = true
		default:
			rest[w] = true
			k := int(w[0])
			if isWord(w[0]) {
				k = alnum
			}
			if !slices.Contains(lens[k], len(w)) {
				lens[k] = append(lens[k], len(w))
			}
		}
	}
	pos := make(map[string]int, len(whole)+len(rest))
	left := len(whole) + len(rest)
	off := 0
	try := func(s string, i int, ns []int) {
		for _, n := range ns {
			if i+n > len(s) {
				continue
			}
			w := s[i : i+n]
			if _, done := pos[w]; !done && rest[w] && tokenAt(s, w, i) {
				pos[w] = off + i
				left--
			}
		}
	}
	for _, s := range texts {
		for i := 0; i < len(s) && left > 0; i++ {
			if !isWord(s[i]) {
				try(s, i, lens[s[i]])
				continue
			}
			// i starts a run of letters and digits (the loop steps past
			// each run whole).
			j := i + 1
			for j < len(s) && isWord(s[j]) {
				j++
			}
			if _, done := pos[s[i:j]]; !done && whole[s[i:j]] {
				pos[s[i:j]] = off + i
				left--
			}
			try(s, i, lens[alnum])
			i = j - 1
		}
		off += len(s) + 1
	}
	for _, set := range []map[string]bool{whole, rest} {
		for w := range set {
			if _, done := pos[w]; !done {
				pos[w] = off
			}
		}
	}
	if empty {
		// No text holds "" as a token (tokenIndex).
		pos[""] = off
	}
	return pos
}

// wordsOnly reports whether w is letters and digits alone (isWord).
func wordsOnly(w string) bool {
	for i := 0; i < len(w); i++ {
		if !isWord(w[i]) {
			return false
		}
	}
	return true
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
