package learn

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// qpSoftBreaks is the pattern qpForm took soft line breaks out by before it
// read them byte by byte (noSoftBreaks).
var qpSoftBreaks = regexp.MustCompile(`=(?:\r?\n|(?:\\+r)?\\+n)`)

// patternNoSoftBreaks is noSoftBreaks as qpForm took them out by the
// pattern.
func patternNoSoftBreaks(s string) string {
	if strings.Contains(s, "=") {
		return qpSoftBreaks.ReplaceAllString(s, "")
	}
	return s
}

// softBreakBytes are the bytes a soft line break is written in, and one of
// none.
const softBreakBytes = "=\\rn\r\nx"

// softBreakCases are each spelling of a soft line break, escaped up to three
// times, beside another, at a text's ends, and broken off.
var softBreakCases = []string{
	"", "=", "a=b", "==", "=\n", "=\r\n", "=\rx", "=\r", "=\r\r\n", "==\n", "=\n=\n", "a=\r\nb=\\nc",
	`=\n`, `=\r\n`, `=\\r\\n`, `=\\\r\\\n`, `=\\r\n`, `=\r\\n`, `=\r`, `=\r\`, `=\r\x`, `=\`, `=\\`, `=\\\`,
	`=r\n`, `=\rn`, `=\r\r\n`, `=\\\n`, `x=\\\nY`, `=\n\n`, `==\r\n`, `=\r\n=\n`,
	"eA==", `{"blob":"WyIyMDI2LTEwLTI0Il0="}`, "Caf=C3=A9 =\r\nau lait", `{"mail":"Caf=C3=A9 =\r\nau=\\r\\nlait"}`,
}

// noSoftBreaks takes out of a text what the pattern took out (qpSoftBreaks),
// of the hand cases, of every text of up to six of softBreakBytes, and of
// 4,000 random texts of seven to sixty of them; qpForm reads a text
// without them in each spelling; and noSoftBreaks allocates nothing for a
// text it leaves as it is, such as a base64 text's padding.
func TestSoftBreaksAreTakenOutAsThePatternTookThem(t *testing.T) {
	taken := 0
	check := func(s string) {
		t.Helper()
		want := patternNoSoftBreaks(s)
		if got := noSoftBreaks(s); got != want {
			t.Fatalf("noSoftBreaks(%q) = %q: the pattern gives %q", s, got, want)
		}
		if want != s {
			taken++
		}
	}
	for _, s := range softBreakCases {
		check(s)
	}
	var walk func(b []byte)
	walk = func(b []byte) {
		check(string(b))
		if len(b) == 6 {
			return
		}
		for i := range len(softBreakBytes) {
			walk(append(b, softBreakBytes[i]))
		}
	}
	walk(nil)
	r := rand.New(rand.NewPCG(5, 23))
	for range 4000 {
		b := make([]byte, 7+r.IntN(54))
		for i := range b {
			b[i] = softBreakBytes[r.IntN(len(softBreakBytes))]
		}
		check(string(b))
	}
	if taken < 1000 {
		t.Errorf("the pattern took a soft line break out of %d texts: want 1000 or more", taken)
	}
	// qpForm takes them out before it reads the escapes, which would turn
	// an escaped one into separators the quoted-printable read keeps.
	for _, s := range []string{"Caf=C3=A9 =\r\nau lait", `Caf=C3=A9 =\r\nau lait`, `Caf=C3=A9 =\\r\\nau lait`} {
		if got := qpForm(s); got != "Café au lait" {
			t.Errorf("qpForm(%q) = %q: want %q", s, got, "Café au lait")
		}
	}
	for _, s := range []string{"eA==", `{"blob":"WyIyMDI2LTEwLTI0Il0="}`, "a=b=c"} {
		if n := testing.AllocsPerRun(10, func() { noSoftBreaks(s) }); n != 0 {
			t.Errorf("noSoftBreaks(%q), which holds no soft line break, allocated %v times: want none", s, n)
		}
	}
}

// FuzzSoftBreaksAreTakenOutAsThePatternTookThem holds noSoftBreaks to the
// pattern it replaces (qpSoftBreaks) on any text.
func FuzzSoftBreaksAreTakenOutAsThePatternTookThem(f *testing.F) {
	for _, s := range softBreakCases {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := noSoftBreaks(s), patternNoSoftBreaks(s); got != want {
			t.Fatalf("noSoftBreaks(%q) = %q: the pattern gives %q", s, got, want)
		}
	})
}
