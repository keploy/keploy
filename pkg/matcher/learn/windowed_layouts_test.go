package learn

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// instantPieces are what the generated texts are made of: an instant of every
// layout, at its longest and in pieces, digit runs about an epoch's lengths,
// and the separators and letters the patterns read, so instants stand next to,
// inside and across each other.
var instantPieces = []string{
	"Mon, 02 Jan 2006 15:04:05 GMT", "wed, 28 oct 2026 07:16:21 +0530", "Sat, 01 Sep 2026 00:00:00 UTC",
	"2026-10-28T07:16:21.123456789+05:30", "2026-10-28 07:16:21,5 UTC", "2026-10-28T10Z", "2026-10-28", "2026-10-28T07:16",
	"20261028T071621Z", "20261028t071621", "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"1761635781.123", "1761635781", "1761635781123", "1761635781123456", "1761635781123456789", "17616357811234567890123",
	"07:16", "07:16:21", "07:16:21.987654321", "-1abc-", "-7ABC-", "12-34", "12:34", "1T2",
	"0", "9", "12", "-", ":", ".", ",", " ", "T", "t", "Z", "z", "+", "a", "F", "x", "ſ", "Mon", "Jan", "GMT",
}

// genText is n pieces of instantPieces, each in a random case now and then.
func genText(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		p := instantPieces[r.Intn(len(instantPieces))]
		switch r.Intn(4) {
		case 0:
			p = strings.ToUpper(p)
		case 1:
			p = strings.ToLower(p)
		}
		b.WriteString(p)
	}
	return b.String()
}

// A layout read in windows finds every match the whole text's read finds,
// and only those, in the same order: instants side by side, inside each
// other, at the text's ends, and in long digit runs.
func TestALayoutReadInWindowsFindsWhatTheWholeTextHolds(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 3000; iter++ {
		s := genText(r, 1+r.Intn(60))
		for k, l := range instantRes {
			got, want := l.FindAllStringIndex(s, -1), l.re.FindAllStringIndex(s, -1)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("layout %d over %q: windows found %v, the whole text %v", k, s, got, want)
			}
		}
	}
}

// Every clock layout keploy reads has its anchors and a bounded match:
// a layout without them is read over the whole text (correct, slow), which
// this row names so it is not left that way unseen.
func TestEveryClockLayoutIsReadInWindows(t *testing.T) {
	for k, l := range instantRes {
		if l.anchors == nil || l.pad < 0 {
			t.Errorf("clock layout %d (%s) has no anchors or no bounded match (pad %d): add it to layoutAnchors", k, clockLayouts[k].re, l.pad)
		}
	}
}

// A long script that holds a few instants is read through the patterns only
// around them: Stripe.js's controller (1.7 MB) and its decoded forms held a
// miss's answer up to 15 s when each pattern read all of them. With 600 KB of
// script and three instants the patterns read no more than three windows per
// layout; the whole text, read by each, is 6 x 600 KB.
func TestALongScriptIsReadOnlyAroundItsInstants(t *testing.T) {
	scan := layoutScan
	defer func() { layoutScan = scan }()
	read := 0
	layoutScan = func(re *regexp.Regexp, s string) [][]int {
		read += len(s)
		return scan(re, s)
	}
	script := strings.Repeat(`function(e,t){var n=e.width||0,r=t.height-1;return{x:n*2,y:r/3,id:"ab12",k:[0,1,2]}};`, 2400)
	s := script + `"at":"2026-10-28T07:16:21.123Z",` + script + `"t":1761635781,` + script + `"d":"Wed, 28 Oct 2026 07:16:21 GMT"`
	spans := instantSpans(s)
	if len(spans) < 3 {
		t.Fatalf("found %d instants, want the three the script holds: %v", len(spans), spans)
	}
	pad := 0
	for _, l := range instantRes {
		pad = max(pad, l.pad)
	}
	if limit := len(instantRes) * 3 * (2*pad + 40); read > limit {
		t.Errorf("the patterns read %d bytes of a %d-byte script holding 3 instants, want at most %d (around the instants only)", read, len(s), limit)
	}
}

// maxMatchLen counts bytes, a case-folded letter at its longest form ("s"
// also matches "ſ", two bytes), and refuses an unbounded pattern or one with
// an empty-width assertion.
func TestMaxMatchLenCountsBytesAndRefusesTheUnbounded(t *testing.T) {
	for _, c := range []struct {
		re   string
		want int
	}{
		{`\d{2}:\d{2}`, 5},
		{`(?i)s`, 2},
		{`(?:ab|c)\d{1,9}`, 11},
		{`a+`, -1},
		{`\d*`, -1},
		{`\bab`, -1},
		{`^ab`, -1},
	} {
		if got := maxMatchLen(regexp.MustCompile(c.re)); got != c.want {
			t.Errorf("maxMatchLen(%s) = %d, want %d", c.re, got, c.want)
		}
	}
}

func BenchmarkInstantSpansLongScript(b *testing.B) {
	script := strings.Repeat(`function(e,t){var n=e.width||0,r=t.height-1;return{x:n*2,y:r/3,id:"ab12",k:[0,1,2]}};`, 20000)
	s := script + `"at":"2026-10-28T07:16:21.123Z"`
	b.Run("windows", func(b *testing.B) {
		b.SetBytes(int64(len(s)))
		for i := 0; i < b.N; i++ {
			for _, l := range instantRes {
				l.FindAllStringIndex(s, -1)
			}
		}
	})
	b.Run("whole", func(b *testing.B) {
		b.SetBytes(int64(len(s)))
		for i := 0; i < b.N; i++ {
			for _, l := range instantRes {
				l.re.FindAllStringIndex(s, -1)
			}
		}
	})
}
