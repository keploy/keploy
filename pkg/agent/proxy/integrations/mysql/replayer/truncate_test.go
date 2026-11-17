package replayer

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// truncate cuts to a byte budget, and a statement with non-English text or an
// emoji at the cut point used to come out with half a character: invalid
// UTF-8 in the call summary, which the report YAML then writes as !!binary
// base64 and the dashboard as U+FFFD. Every cut must be valid text, within the
// budget, and no shorter than the character it backs off from requires.
func TestTruncate_NeverSplitsACharacter(t *testing.T) {
	for name, s := range map[string]string{
		"hindi (3-byte characters)":   "INSERT INTO reviews (body) VALUES ('" + strings.Repeat("नमस्ते दुनिया ", 20) + "')",
		"emoji (4-byte characters)":   "INSERT INTO chats (msg) VALUES ('" + strings.Repeat("ok 👍 ", 60) + "')",
		"accents (2-byte characters)": "SELECT id FROM reviews WHERE body = '" + strings.Repeat("très bon produit ", 20) + "'",
	} {
		t.Run(name, func(t *testing.T) {
			for maxLen := 0; maxLen <= len(s)+1; maxLen++ {
				got := truncate(s, maxLen)
				require.Truef(t, utf8.ValidString(got), "maxLen %d: %q is not valid UTF-8", maxLen, got)
				require.LessOrEqualf(t, len(got), maxLen, "maxLen %d: %q is over the budget", maxLen, got)
				if len(s) <= maxLen {
					require.Equal(t, s, got, "a string within the budget is kept whole")
					continue
				}
				kept := got
				if maxLen > 3 {
					require.Truef(t, strings.HasSuffix(got, "..."), "maxLen %d: %q does not say it was cut", maxLen, got)
					kept = strings.TrimSuffix(got, "...")
				}
				require.Truef(t, strings.HasPrefix(s, kept), "maxLen %d: %q is not the start of the statement", maxLen, kept)
				// Backing off to a character's first byte drops at most the
				// three bytes a 4-byte character can have before the cut.
				require.GreaterOrEqualf(t, len(got), maxLen-3, "maxLen %d: %q was cut shorter than needed", maxLen, got)
			}
		})
	}
}

// Bytes that are not UTF-8 text are cut near the budget, never shortened
// further. In latin1, © and ° are 0xA9 and 0xB0, which UTF-8 reads as the
// continuation bytes of a character, so an unbounded walk back to a
// character's first byte would drop the whole run.
func TestTruncate_BytesThatAreNotTextAreCutNearTheBudget(t *testing.T) {
	s := "INSERT INTO notes (body) VALUES ('" + strings.Repeat("\xa9\xb0", 60) + "')"
	for maxLen := 4; maxLen < len(s); maxLen++ {
		got := truncate(s, maxLen)
		require.LessOrEqualf(t, len(got), maxLen, "maxLen %d: %q is over the budget", maxLen, got)
		require.GreaterOrEqualf(t, len(got), maxLen-3, "maxLen %d: %q was cut shorter than needed", maxLen, got)
		require.Truef(t, strings.HasPrefix(s, strings.TrimSuffix(got, "...")), "maxLen %d: %q is not the start of the statement", maxLen, got)
	}
}

// The cuts an ASCII statement gets are unchanged, and a multi-byte character
// at the cut point is dropped whole, never split.
func TestTruncate_Cases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"ascii within the budget", "hello world", 20, "hello world"},
		{"ascii exactly the budget", "hello world", 11, "hello world"},
		{"ascii cut", "hello world", 8, "hello..."},
		{"ascii with no room for the dots", "hello", 3, "hel"},
		{"ascii with no room at all", "hello", 0, ""},
		{"2-byte é at the cut point", "abcdéfgh", 8, "abcd..."},
		{"3-byte न at the cut point", "abनमस्ते", 6, "ab..."},
		{"4-byte 👍 at the cut point", "ok 👍 fine", 8, "ok ..."},
		{"cut right after a whole character", "abनमस्ते", 8, "abन..."},
		{"no room for the dots, inside a character", "नमस्ते", 2, ""},
		{"no room for the dots, after a character", "नमस्ते", 3, "न"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, truncate(tc.s, tc.maxLen))
		})
	}
}

// The summary a COM_QUERY miss reports (cut at 160 bytes, so at byte 157
// before its "...") stays valid text when the statement has a multi-byte
// character at the cut point. The text starts 1 to 4 bytes before byte 157,
// so the cut lands at each offset inside a 3-byte (Hindi) and a 4-byte
// (emoji) character.
func TestMySQLMissSummaryNeverSplitsACharacter(t *testing.T) {
	const prefix = "SELECT id FROM reviews WHERE product_id = 42 AND body = '"
	const cutAt = 160 - len("...")
	for name, text := range map[string]string{
		"hindi": strings.Repeat("कैमरा बहुत अच्छा है ", 8),
		"emoji": strings.Repeat("😍", 40),
	} {
		for back := 1; back <= 4; back++ {
			t.Run(fmt.Sprintf("%s, %d bytes back", name, back), func(t *testing.T) {
				pad := cutAt - back - len(prefix)
				require.Positive(t, pad, "the prefix leaves no room to place the text before the cut")
				live := prefix + strings.Repeat("x", pad) + text + "' LIMIT 1"
				db := &fakeMockDb{session: []*models.Mock{nativeHandshake(),
					queryRecorded("mock-users", "SELECT name FROM users WHERE api_key = 'k1'", nil)}}
				rep, _ := missOf(t, db, models.OutgoingOptions{}, func(c mysqlClient) {
					c.send(0, append([]byte{mysql.COM_QUERY}, live...))
				})
				assert.Truef(t, utf8.ValidString(rep.ActualSummary), "the summary splits a character: %q", rep.ActualSummary)
				assert.Truef(t, strings.HasSuffix(rep.ActualSummary, "...") && strings.Contains(rep.ActualSummary, prefix),
					"the summary is not the cut statement: %q", rep.ActualSummary)
				assert.Truef(t, utf8.ValidString(reportText(rep)), "the report carries invalid UTF-8: %q", reportText(rep))
			})
		}
	}
}

// A bound value is cut at 48 bytes in the report's parameter list; an emoji
// at that point is dropped whole.
func TestFormatExecParams_NeverSplitsACharacter(t *testing.T) {
	got := formatExecParams([]mysql.Parameter{{Value: "Great product!! " + strings.Repeat("👍", 12)}})
	assert.True(t, utf8.ValidString(got), "the parameter list splits a character: %q", got)
	assert.Equal(t, "params=[Great product!! "+strings.Repeat("👍", 7)+"...]", got)
}
