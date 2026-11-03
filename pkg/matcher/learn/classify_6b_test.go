package learn

import (
	"encoding/hex"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestSameEscapesTakesAByteOnlyLiveHoldsByAnEncodersOneSpelling holds
// sameEscapes to its rule for a byte only the live run holds: it is
// written so where the recorded run writes another byte by one spelling of
// that kind (its first byte and its length), and the live run writes all
// its copies the same way; a mix of spellings says nothing of how the
// recorded side's encoder writes it, even beside a recorded mix of the same
// length. A byte the recorded run holds is written as the recorded run
// writes it: as itself where it escapes it nowhere.
func TestSameEscapesTakesAByteOnlyLiveHoldsByAnEncodersOneSpelling(t *testing.T) {
	mixed := func(sps ...string) string { return "\x00 " + strings.Join(sps, " ") }
	for _, c := range []struct {
		what      string
		rec, live map[byte]string
		recRun    string
		want      bool
	}{
		{"'/' only live holds, one spelling beside the recorded \"%3D\"", map[byte]string{'=': "%3D"}, map[byte]string{'/': "%2F"}, "YWI=", true},
		{"'/' only live holds, mixed, beside the recorded \"%3D\"", map[byte]string{'=': "%3D"}, map[byte]string{'/': mixed("%2F", "%2f")}, "YWI=", false},
		{"'/' only live holds, mixed, beside a recorded mix of the same length", map[byte]string{'=': mixed("%3D", "%3d")}, map[byte]string{'/': mixed("%2F", "%2f")}, "YWI==", false},
		{"'/' only live holds, the recorded run escaping nothing", nil, map[byte]string{'/': "%2F"}, "YWI", false},
		{"'/' the recorded run holds raw, escaped by live beside the recorded \"%3D\"", map[byte]string{'=': "%3D"}, map[byte]string{'/': "%2F", '=': "%3D"}, "YW/=", false},
		{"'/' both escape alike", map[byte]string{'/': "%2F"}, map[byte]string{'/': "%2F"}, "YW/I", true},
		{"'/' respelled", map[byte]string{'/': "%2F"}, map[byte]string{'/': "%2f"}, "YW/I", false},
	} {
		if got := sameEscapes(c.rec, c.live, c.recRun); got != c.want {
			t.Errorf("%s: sameEscapes(%q, %q, %q) = %v; want %v", c.what, c.rec, c.live, c.recRun, got, c.want)
		}
	}
}

// TestRawImagesListsABytesSpellingsInOrderWhereTheyDiffer holds rawImages'
// esc to its rule: a byte all of whose copies are written one way has that
// spelling; one written two ways (its padding's '=' escaped once and raw
// once) has each copy's, in order, so a run that respells a copy is told
// from one that writes them all alike; a byte never escaped has none.
func TestRawImagesListsABytesSpellingsInOrderWhereTheyDiffer(t *testing.T) {
	for _, c := range []struct {
		raw, run string
		esc      map[byte]string
	}{
		{"tok YW%2FI%3D= ok", "YW%2FI%3D=", map[byte]string{'/': "%2F", '=': "\x00 %3D ="}},
		{"tok YW%2FI=%3D ok", "YW%2FI=%3D", map[byte]string{'/': "%2F", '=': "\x00 = %3D"}},
		{"tok YW%2FI%3D%3D ok", "YW%2FI%3D%3D", map[byte]string{'/': "%2F", '=': "%3D"}},
		{"tok YW/I%3D%3D ok", "YW/I%3D%3D", map[byte]string{'=': "%3D"}},
	} {
		dec, _ := unescapedAt(c.raw)
		st := strings.Index(dec, "YW")
		en := strings.Index(dec, " ok")
		got := rawImages(c.raw, []decodedRun{{run: dec[st:en], at: -1, st: st, en: en}})
		if len(got) != 1 || got[0].run != c.run || !maps.Equal(got[0].esc, c.esc) {
			t.Errorf("rawImages(%q) = %+v; want one run %q with esc %q", c.raw, got, c.run, c.esc)
		}
	}
}

// keptEscape reads sameEscapes one escape at a time: an escape of a live
// run is the recorded run's where it writes its copy of its byte as the
// recorded run of the same rank writes that copy, the one spelling every
// recorded copy shares or, where they differ, the copy of the same rank; and
// for a byte the recorded run does not hold, where it is of the kind (first
// byte and length) by which that run writes another byte.
func TestKeptEscapeDecidesEachEscapeByTheRecordedCopyOfItsRank(t *testing.T) {
	image := func(raw string) (decodedRun, string) {
		dec, _ := unescapedAt(raw)
		st, en := strings.Index(dec, "YW"), strings.Index(dec, " ok")
		got := rawImages(raw, []decodedRun{{run: dec[st:en], at: -1, st: st, en: en}})
		if len(got) != 1 {
			t.Fatalf("rawImages(%q) = %+v; want one run", raw, got)
		}
		return got[0], dec[st:en]
	}
	for _, c := range []struct {
		what, rec, live string
		kept            []bool
	}{
		{"one spelling, alike", "tok YW%2FI%3D%3D ok", "tok YW%2FI%3D%3D ok", []bool{true, true, true}},
		{"one spelling, respelled", "tok YW%2FI%3D%3D ok", "tok YW%2fI%3D%3D ok", []bool{false, true, true}},
		{"a byte the recorded run writes as itself, gained", "tok YW/I%3D%3D ok", "tok YW%2FI%3D%3D ok", []bool{false, true, true}},
		{"recorded copies mixed, the live ones by rank", "tok YW%2FI=%3D ok", "tok YW%2FI%3D%3D ok", []bool{true, false, true}},
		{"recorded copies mixed, alike", "tok YW%2FI%3D= ok", "tok YW%2FI%3D= ok", []bool{true, true}},
		{"a byte only live holds, of the kind of another recorded escape", "tok YWxI%3D%3D ok", "tok YW%2FI%3D%3D ok", []bool{true, true, true}},
		{"a byte only live holds, a longer escape of the same first byte", "tok YWxI%3D%3D ok", "tok YW%252FI%3D%3D ok", []bool{false, true, true}},
		{"a byte only live holds, no recorded escape", "tok YWxI== ok", "tok YW%2FI== ok", []bool{false}},
		{"a byte only live holds, an escape of another first byte as long as a recorded one", "tok YWxI%3D%3D ok", `tok YW\\/I%3D%3D ok`, []bool{false, true, true}},
	} {
		rec, recRun := image(c.rec)
		live, _ := image(c.live)
		if len(live.escs) != len(c.kept) {
			t.Fatalf("%s: %q holds %d escapes (%+v); this row lists %d", c.what, c.live, len(live.escs), live.escs, len(c.kept))
		}
		for i, e := range live.escs {
			if got := keptEscape(rec, recRun, e); got != c.kept[i] {
				t.Errorf("%s: keptEscape of %q's escape %d (%q, copy %d of %q) against %q = %v; want %v", c.what, c.live, i, e.sp, e.n, e.c, c.rec, got, c.kept[i])
			}
		}
	}
}

// An escape record's full round's value gained in its last run, a '/'
// written "%2F" whose word ("2F" and the run's bytes after it) runs to the
// value's end, is searched where it stands, even where another run, which
// kept its own "%2F", spells the same word: the word is no part of the
// runs' spelling (plainRuns). The same with a word after the last run is the
// control. The runs are base64 of "leadX0 x?abc" and "lead 0 x?abc", which
// end alike from their '/' on.
func TestAGainedEscapesWordAtTheValuesEndIsSearched(t *testing.T) {
	const kept, run, word = "bGVhZFgwIHg%2FYWJj", "bGVhZCAwIHg/YWJj", "2FYWJj"
	rec := "tok " + kept + " " + run
	live := "tok " + kept + " " + strings.ReplaceAll(run, "/", "%2F")
	if !strings.HasSuffix(live, "%"+word) || !strings.HasSuffix(kept, "%"+word) {
		t.Fatalf("live %q and the kept run %q do not both end in %q: not the layout this row reads", live, kept, word)
	}
	for _, tail := range []string{"", " ok"} {
		if d := newDrift(rec+tail, live+tail, nil, true, time.Time{}, nil); d.plainRuns[word] {
			t.Errorf("tail %q: the word %q the gained escape leaves is skipped (plainRuns); want it searched", tail, word)
		}
	}
}
