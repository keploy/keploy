package learn

import (
	"math/rand/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// b64RunPattern is the pattern the base64 runs were read with before the
// one-pass scan (nextB64Run): the rows hold the scan to what it found.
var b64RunPattern = regexp.MustCompile(`[A-Za-z0-9+/_-]+={0,6}`)

// b64RunCorpus is the texts the scan is held to the pattern on: the shapes
// the learn step decodes (JWTs, wrapped and padded base64, hex, base32,
// signed messages, paths), each edge of a run (padding past six, '=' with
// no run before it, both alphabets' extra bytes), text of other scripts and
// bytes that are no UTF-8.
var b64RunCorpus = []string{
	"", "=", "==", "a", "a=", "a======", "a=======", "a=========b", "=a", "a=b=c", "==a==",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1LTciLCJpYXQiOjE3MDAwMDAwMDB9.c2lnbmF0dXJl",
	"dGV4dCB3cmFwcGVk\n  IGFjcm9zcyBsaW5lcw==\n", `"dGV4dA==\ndGV4dA=="`, "0xDEADbeef 0X00ff",
	"MZXW6YTBOI======", "mzxw6ytboi", "c2lnbmVk--ZGlnZXN0", "a_b-c/d+e", "/v1/users/41?at=100&x=y",
	"héllo wörld ünïcode", "日本語のテキスト abc", "\xff\xfeab\xc3", "abc\x00def", "tab\there", "--__--++//",
	`{"token":"tok-0c41b2","state":"eyJ1IjoxfQ=="}`, strings.Repeat("A", 5000) + "===", strings.Repeat("ab= ", 300),
	"2026-10-31T16:20:13+05:30", "2026-10-31 16:20:13+05:30", "2026-10-31T16:20:13.305+05:30==", "sig=ab+cd 2026-10-31T16:20:13+05:30+ef+gh",
	"at+1761909613+ab", "ab+cd2026-10-31T16:20:13+05:30",
}

// b64RunAlphabet weighs the random texts toward a run's edges: both
// alphabets' extra bytes, padding, the separators the learn step cuts runs
// at, a multi-byte rune and a byte that is no UTF-8.
var b64RunAlphabet = []string{"a", "Z", "7", "+", "/", "-", "_", "=", "=", "=", " ", ".", "\n", "\\n", "é", "\xff", ":", `"`}

func b64RunTexts() []string {
	texts := append([]string(nil), b64RunCorpus...)
	r := rand.New(rand.NewPCG(7, 41))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := r.IntN(48); n > 0; n-- {
			b.WriteString(b64RunAlphabet[r.IntN(len(b64RunAlphabet))])
		}
		texts = append(texts, b.String())
	}
	return texts
}

// The one-pass scan finds the runs of the base64 alphabets the pattern
// [A-Za-z0-9+/_-]+={0,6} found, at the same spans, in the corpus and in
// 20 000 random texts weighed toward a run's edges: each place the learn
// step reads runs (b64Text's runs and its '.'-separated parts) reads the
// runs it read before; b64Runs reads those of the text outside its
// instants (instantSpans), the pattern's runs of the text with each
// instant's bytes blanked.
func TestTheBase64RunScanFindsWhatThePatternFound(t *testing.T) {
	for _, s := range b64RunTexts() {
		var got [][]int
		for st, en, ok := nextB64Run(s, 0); ok; st, en, ok = nextB64Run(s, en) {
			got = append(got, []int{st, en})
		}
		if want := b64RunPattern.FindAllStringIndex(s, -1); !reflect.DeepEqual(got, want) {
			t.Fatalf("runs of %q: the scan finds %v, the pattern %v", s, got, want)
		}
		// b64Text's parts: never empty (strings.FieldsFunc), and read as a
		// run where the pattern's leftmost match is the part whole.
		if s != "" {
			if got, want := isB64Run(s), b64RunPattern.FindString(s) == s; got != want {
				t.Fatalf("%q whole a run: the scan says %v, the pattern %v", s, got, want)
			}
		}
		blanked := []byte(s)
		for _, in := range instantSpans(s) {
			for i := in.start; i < in.end; i++ {
				blanked[i] = ' '
			}
		}
		want := map[string]bool{}
		for _, r := range b64RunPattern.FindAllString(string(blanked), -1) {
			want[r] = true
		}
		if got := b64Runs(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("b64Runs(%q) = %v, the pattern's runs %v", s, got, want)
		}
	}
}
