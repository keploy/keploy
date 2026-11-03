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
			case m == `\/`:
				return "/"
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
// second digit is a capital letter, two halves with no backslash between
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

// TestUnescapedAtPlacesEachPartOfTheTextWhereItStands holds unescapedAt's
// places to unescaped on the texts above and some with a JSON "\/" (a
// base64 text a JSON writer escaped), as the fuzzer does on any text of up
// to 64 bytes: what stands between any two places it gives reads as what
// stands between them in the text (from the start: the text before a
// place), the ends are the value's, and only a place inside a character an
// escape names is not given. A text with no escape costs no allocation.
func TestUnescapedAtPlacesEachPartOfTheTextWhereItStands(t *testing.T) {
	for _, c := range append(unescapedCases, "YWI#/eHk#/", "x%5C%2FYWI%5C/eHk", "#/#u00e9#/") {
		unescapedAtHolds(t, esc(c))
	}
	if got := unescaped(esc("YWI#/eHk#/")); got != "YWI/eHk/" {
		t.Errorf(`unescaped reads a JSON "\/" as %q, not "/"`, got)
	}
	// A text with no escape costs no allocation.
	if n := testing.AllocsPerRun(100, func() { _ = unescaped("tok YWI/eHk/ ok, no escape at all") }); n != 0 {
		t.Errorf("unescaped of a text with no escape allocates %.0f times; want none", n)
	}
}

// unescapedAtHolds checks unescapedAt(c) against unescaped on every two of
// its places.
func unescapedAtHolds(t *testing.T, c string) {
	t.Helper()
	got, at := unescapedAt(c)
	if got != unescaped(c) || len(at) != len(got)+1 || at[0] != 0 || at[len(got)] != len(c) {
		t.Fatalf("unescapedAt(%q) = %q, %v; want unescaped's %q, placed from 0 to %d", c, got, at, unescaped(c), len(c))
	}
	for j := range at {
		if at[j] < 0 && got[j]&0xC0 != 0x80 {
			t.Errorf("unescapedAt(%q) gives no place at %d, which splits no character (%q)", c, j, got)
		}
		for k := j; k < len(at); k++ {
			if at[j] < 0 || at[k] < 0 {
				continue
			}
			if at[k] < at[j] || unescaped(c[at[j]:at[k]]) != got[j:k] {
				t.Errorf("unescapedAt(%q): %d..%d (of %d..%d) does not read as %q", c, at[j], at[k], j, k, got[j:k])
			}
		}
	}
}

// TestUnescapedReadsEveryByteOfASurrogatePairAsThePatterns holds the
// surrogate pair check to the patterns on every byte at every position of
// four pairs (lower case, upper case, mixed, and one whose backslashes are
// percent-escaped), bare and inside text: 26,624 texts. The table holds the
// shapes someone thought of; this holds every digit and letter each place
// of a pair may take, so a check that drops or adds one fails here, not
// only under the fuzzer, which CI does not run.
func TestUnescapedReadsEveryByteOfASurrogatePairAsThePatterns(t *testing.T) {
	for _, pair := range []string{esc("#ud800#udc00"), esc("#uDBFF#uDFFF"), esc("#udA9b#uDcEf"), "%5Cud800%5Cudc00"} {
		for p := 0; p < len(pair); p++ {
			for b := 0; b < 256; b++ {
				s := pair[:p] + string([]byte{byte(b)}) + pair[p+1:]
				for _, c := range []string{s, "x" + s + "y"} {
					if got, want := unescaped(c), unescapedByPattern(c); got != want {
						t.Fatalf("unescaped(%q) = %q, the patterns read %q", c, got, want)
					}
				}
			}
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
		if len(c) <= 64 {
			unescapedAtHolds(t, c)
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
