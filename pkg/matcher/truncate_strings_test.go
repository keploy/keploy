package matcher

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// The expected and actual bodies the diff table shows are cut at a byte
// budget (10000 bytes, or the shorter body's length plus 1000). A body with
// non-English text at the cut point keeps whole characters: the cut never
// leaves half of one for the table to print as broken text.
func TestTruncateStringsNeverSplitsACharacter(t *testing.T) {
	// 38 bytes of 3-byte characters and spaces.
	const hindi = "नमस्ते दुनिया "
	// n ASCII bytes, then Hindi text: the first character starts at byte n.
	body := func(n int) string { return strings.Repeat("a", n) + strings.Repeat(hindi, 40) }
	for _, tc := range []struct {
		name     string
		exp, act string
	}{
		{"both bodies over 10000 bytes", body(9999), body(9998)},
		{"expected far longer: cut to actual's length plus 1000", body(1099), strings.Repeat("b", 100)},
		{"actual far longer: cut to expected's length plus 1000", strings.Repeat("b", 100), body(1098)},
		{"lengths close, one over 10000 bytes", body(9999), strings.Repeat("c", 10600)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp, act := truncateStrings(tc.exp, tc.act)
			for _, side := range []struct{ name, in, out string }{{"expected", tc.exp, exp}, {"actual", tc.act, act}} {
				require.Truef(t, utf8.ValidString(side.out), "the %s body splits a character: %q", side.name, side.out[max(0, len(side.out)-80):])
				kept, _, _ := strings.Cut(side.out, "\n...(truncated")
				require.Truef(t, strings.HasPrefix(side.in, kept), "the %s body is not the start of the original", side.name)
			}
			require.Contains(t, exp+act, "(truncated", "the case cut neither body")
		})
	}
}
