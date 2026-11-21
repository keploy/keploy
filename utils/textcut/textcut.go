// Package textcut cuts text to a byte budget without splitting a character.
//
// It depends on nothing but the standard library, so the lowest packages
// (pkg/models) can use it as well as the matcher, the reports and the
// proxy's parsers. It replaces hand-written copies of the cut that disagreed
// on bytes that are not UTF-8 text: some walked back without a bound, and so
// dropped a whole run of a binary or latin1 value.
package textcut

import "unicode/utf8"

// Prefix is s cut to at most n bytes without splitting a character: when
// byte n is inside a UTF-8 character, the cut moves back to that
// character's first byte, so what is kept from valid text is valid text.
//
// The cut moves back only for a character byte n is really inside, and so
// at most utf8.UTFMax-1 bytes. Bytes that are not UTF-8 text (a binary or
// latin1 value, whose bytes 0x80-0xBF UTF-8 reads as a character's
// continuation) are cut at n: never walked back past the whole run of them.
//
// s within the budget is returned whole, and n <= 0 gives "".
func Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	if utf8.RuneStart(s[n]) {
		return s[:n]
	}
	// The character byte n may be inside starts at most utf8.UTFMax-1
	// bytes before it, at the nearest byte that can start one.
	for start := n - 1; start >= 0 && start > n-utf8.UTFMax; start-- {
		if !utf8.RuneStart(s[start]) {
			continue
		}
		if r, size := utf8.DecodeRuneInString(s[start:]); (r != utf8.RuneError || size > 1) && start+size > n {
			return s[:start]
		}
		break
	}
	return s[:n]
}
