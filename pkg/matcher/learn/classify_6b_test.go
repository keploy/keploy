package learn

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
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

// unescapedByPattern is how unescaped read a text with regular expressions
// before it scanned by hand: the oracle its scan must agree with on every
// text.
func unescapedByPattern(s string) string {
	for i := 0; i < 4; i++ {
		d := percentEscapePattern.ReplaceAllStringFunc(s, func(m string) string {
			b, err := hex.DecodeString(m[1:])
			if err != nil {
				return m
			}
			return string(b)
		})
		d = escapedSurrogatesPattern.ReplaceAllStringFunc(d, func(m string) string {
			hi, _ := strconv.ParseUint(m[2:6], 16, 16)
			lo, _ := strconv.ParseUint(m[8:12], 16, 16)
			return string(utf16.DecodeRune(rune(hi), rune(lo)))
		})
		d = backslashEscapePattern.ReplaceAllStringFunc(d, func(m string) string {
			switch {
			case m == `\\`:
				return `\`
			case m[1] == 'u':
				if r, err := strconv.ParseUint(m[2:], 16, 16); err == nil {
					return string(rune(r))
				}
			}
			return " "
		})
		if d == s {
			break
		}
		s = d
	}
	return s
}

var (
	escapedSurrogatesPattern = regexp.MustCompile(`\\u[dD][89abAB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}`)
	percentEscapePattern     = regexp.MustCompile(`%[0-9a-fA-F]{2}`)
	backslashEscapePattern   = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|[nrtbf"\\/])`)
)

// unescapedCases are texts where a hand scan most easily parts from the
// patterns ('#' is a backslash, esc): an escape cut short at the end, a
// stray '%' or '#' right before an escape, escapes next to each other, a
// surrogate pair half there or the wrong way round, a high surrogate whose
// third digit is a capital letter, two halves with no backslash between
// them, an escape that decodes to another (read on the next pass, at most
// four), and bytes that are not UTF-8.
var unescapedCases = []string{
	"", "plain text", "%", "%4", "%41", "%%41", "%4%41", "%gg%41", "%2F%2f", "a%2Fb%2", "%E2%82%AC",
	"%252F", "%25252541", "%2525252525252541", "%5Cn", "%5Cu0041", "%5Cud83d%5Cude00", "%5C%5C",
	"#", "a#", "#u", "#u12", "#u123g", "#u1234", "#u12345", "#U0041", "#x", "#n#r#t#b#f#\"#/",
	"##", "###", "####n", "########n", "#u005C#u006E", "#u002541", "#u00e9#u00E9",
	"#uD83D#uDE00", "#ud83d#ude00x", "#ud83d#ud83d#ude00", "#ud83d", "#ude00#ud83d", "#ud83d#u0041",
	"#ud83d##ude00", "#udbff#udfff", "#ud800#udc00", "#udc00", "#uD7FF#uDC00", "#uDC00#uDC00",
	"#udA00#udC00", "#uDBff#uDfff", "#ud800xudc00", "%5Cud8000udC00", "#ud8000#udc00",
	"\xff%41\xe2#u0041\x80", "\xe2\x82#n", "%C3#u00a9%A9", "x%5C%75%30%30%34%31",
	`{"a":"b#"c#"","u":"https:#/#/h#/uploads%2Fid%3Fq%3D1"}`,
}

// TestUnescapedReadsWhatThePatternsRead pins the hand scan to the patterns
// it replaced, on the texts where they most easily part.
func TestUnescapedReadsWhatThePatternsRead(t *testing.T) {
	for _, c := range unescapedCases {
		c = esc(c)
		if got, want := unescaped(c), unescapedByPattern(c); got != want {
			t.Errorf("unescaped(%q) = %q, the patterns read %q", c, got, want)
		}
	}
}

// FuzzUnescaped holds the hand scan to the patterns on any text, seeded
// with the cases above.
func FuzzUnescaped(f *testing.F) {
	for _, c := range unescapedCases {
		f.Add(esc(c))
	}
	f.Fuzz(func(t *testing.T, c string) {
		if got, want := unescaped(c), unescapedByPattern(c); got != want {
			t.Fatalf("unescaped(%q) = %q, the patterns read %q", c, got, want)
		}
	})
}

// BenchmarkUnescaped is a 4 KB JSON text form, with no escape and with a
// few, read by the scan and by the patterns.
func BenchmarkUnescaped(b *testing.B) {
	plain := strings.Repeat(`{"_id":"6543210fedcba98765432100","name":"job-1","state":"running","n":1},`, 55)
	escaped := strings.Repeat(esc(`{"path":"uploads%2F6543210f","note":"a#"b#"#n","e":"caf#u00e9"},`), 55)
	for _, tc := range []struct {
		name string
		text string
	}{{"none", plain}, {"some", escaped}} {
		b.Run(tc.name+"/scan", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				unescaped(tc.text)
			}
		})
		b.Run(tc.name+"/pattern", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				unescapedByPattern(tc.text)
			}
		})
	}
}
