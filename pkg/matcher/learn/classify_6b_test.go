package learn

import (
	"strings"
	"testing"
)

// esc writes a JSON escape with '#' for its backslash, so that the escapes
// below stay escapes in this file whatever tool wrote it.
func esc(s string) string { return strings.ReplaceAll(s, "#", "\x5c") }

// A "#uXXXX" escape is four hex digits, so 16 bits, up to U+FFFF, and an
// escaped surrogate pair is the character beyond it that the pair names, up
// to U+10FFFF (CodeQL go/incorrect-integer-conversion: each parse is bounded
// to 16 bits).
func TestUnescapedReadsEveryFourDigitEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{esc("#u0041"), "A"},
		{esc("#u0000"), "\x00"},
		{esc("#uffff"), string(rune(0xFFFF))},
		{esc("#uFFFF"), string(rune(0xFFFF))},
		{esc("#ud83d#ude00"), string(rune(0x1F600))},
		{esc("#uD83D#uDE00"), string(rune(0x1F600))},
		{esc("#ud800#udc00"), string(rune(0x10000))},
		{esc("#udbff#udfff"), string(rune(0x10FFFF))},
		// A lone surrogate names no character: the replacement character.
		{esc("#ud800"), string(rune(0xFFFD))},
		{esc("a#u00e9b"), "a" + string(rune(0xE9)) + "b"},
	} {
		if !strings.Contains(tc.in, "\x5cu") {
			t.Fatalf("the row %q holds no escape", tc.in)
		}
		if got := unescaped(tc.in); got != tc.want {
			t.Errorf("unescaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
