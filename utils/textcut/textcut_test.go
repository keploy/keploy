package textcut

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestPrefixCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"within the budget", "hello", 9, "hello"},
		{"exactly the budget", "hello", 5, "hello"},
		{"ascii", "hello world", 5, "hello"},
		{"no budget", "hello", 0, ""},
		{"a budget below zero", "hello", -1, ""},
		{"empty", "", 3, ""},
		{"cut right after a character", "abनम", 5, "abन"},
		{"inside a 2-byte é", "abcdéf", 5, "abcd"},
		{"inside a 3-byte न, 1 byte in", "abनम", 3, "ab"},
		{"inside a 3-byte न, 2 bytes in", "abनम", 4, "ab"},
		{"inside a 4-byte 👍, 3 bytes in", "ok 👍!", 6, "ok "},
		{"inside the first character", "नम", 2, ""},
		// latin1 © and ° are 0xA9 and 0xB0: UTF-8 reads them as a
		// character's continuation, but they continue no character.
		{"inside a latin1 run", "x='" + strings.Repeat("\xa9\xb0", 20) + "'", 17, "x='" + strings.Repeat("\xa9\xb0", 7)},
		{"latin1 right after a character", "न\xa9\xb0\xa9", 5, "न\xa9\xb0"},
		{"a whole character before stray bytes", "ok 👍\xa9\xb0\xa9", 9, "ok 👍\xa9\xb0"},
		{"a lead byte that starts no character", "ab\xe0\xa9z", 3, "ab\xe0"},
		{"a whole U+FFFD", "ab�c", 4, "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Prefix(tc.s, tc.n))
		})
	}
}

// Valid text is cut to valid text, within the budget, and no shorter than
// the character at the cut needs: at most utf8.UTFMax-1 bytes back.
func TestPrefixNeverSplitsACharacter(t *testing.T) {
	for name, s := range map[string]string{
		"hindi (3-byte characters)":   strings.Repeat("नमस्ते दुनिया ", 6),
		"emoji (4-byte characters)":   strings.Repeat("ok 👍 ", 12),
		"accents (2-byte characters)": strings.Repeat("très bon ", 8),
	} {
		t.Run(name, func(t *testing.T) {
			for n := 0; n <= len(s)+1; n++ {
				got := Prefix(s, n)
				require.Truef(t, utf8.ValidString(got), "n %d: %q is not valid UTF-8", n, got)
				require.Truef(t, strings.HasPrefix(s, got), "n %d: %q is not the start of s", n, got)
				require.LessOrEqual(t, len(got), n)
				require.GreaterOrEqual(t, len(got), min(n, len(s))-(utf8.UTFMax-1))
			}
		})
	}
}

// Bytes that are not UTF-8 text are cut exactly at the budget, never walked
// back past the run of them.
func TestPrefixCutsBytesThatAreNotTextAtTheBudget(t *testing.T) {
	s := "INSERT INTO notes (body) VALUES ('" + strings.Repeat("\xa9\xb0", 60) + "')"
	for n := 0; n < len(s); n++ {
		require.Equalf(t, s[:n], Prefix(s, n), "n %d", n)
	}
}
