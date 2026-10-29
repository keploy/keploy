package learn

import (
	"math/bits"
	"sync/atomic"
)

// ONE PASS OVER A TEXT FOR ITS INSTANTS.
//
// instantSpans reads every text a set's payloads hold, at staging and in a
// call's own learn. It read them with clockLayouts' six patterns, each
// compiled case-insensitive and run over the whole text in turn: six
// regular-expression scans per text, which no literal prefix speeds up once
// the patterns fold case. On api-server's real e2e round under keploy mock
// record, that was 77% of the replay agent's CPU.
//
// scanInstants reads the text once. At each byte that can begin a layout's
// match it tries that layout there, and it reads no further than one match
// (at most 46 bytes). It finds exactly the matches each pattern's own scan
// finds, so instantSpans keeps its rules unchanged: the same matches, word
// boundaries, layout order and reads.
//
//   - Each layout's matches are found the way Go's FindAllStringIndex finds
//     them: the leftmost match at or after the previous match's end, at its
//     leftmost-first end. Each pattern is a run of fixed parts and optional
//     tails, and nothing after an optional tail can fail, so its first
//     greedy path is the leftmost-first match.
//   - Case folds as Go's (?i) does: an ASCII letter matches either case, and
//     's' also matches U+017F (ſ) and 'k' also matches U+212A (the Kelvin
//     sign), the only non-ASCII runes in an ASCII letter's simple-fold
//     orbit. \d is ASCII digits only, as in Go.
//   - A match begins only at an ASCII byte or at ſ's lead byte. Neither is
//     ever a UTF-8 continuation byte, so a decoder never reads either as
//     part of an earlier rune, and every such byte is a rune start where the
//     patterns' own scan tries it.
//
// The row TestInstantSpansIsTheSixPatternsScans holds the scan to the
// patterns: on a corpus and under fuzzing, every span equals the one that
// running clockLayouts' patterns gives. A pattern changed in clockLayouts
// without changing its matcher here fails that row.

// numLayouts is len(clockLayouts): a matcher per layout, in its order.
const numLayouts = 6

// layoutAt are clockLayouts' patterns, one matcher per layout, in the same
// order. Each returns the end of the leftmost-first match of its pattern,
// folded, that begins at s[i], or -1 if none begins there.
var layoutAt = [numLayouts]func(s string, i int) int{rfc1123At, isoAt, compactAt, uuidAt, epochAt, timeOfDayAt}

func init() {
	if len(clockLayouts) != numLayouts {
		panic("learn: clockLayouts and layoutAt differ in length")
	}
}

// layoutStarts says, for each byte, which layouts' matches can begin with
// it: bit k for layout k.
var layoutStarts = func() (t [256]uint8) {
	for c := '0'; c <= '9'; c++ {
		t[c] |= 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5
	}
	for _, c := range "abcdefABCDEF" {
		t[c] |= 1 << 3
	}
	for _, d := range rfcDays {
		t[d[0]] |= 1
		t[d[0]^0x20] |= 1
	}
	// ſ, which folds to 's': "Sat" and "Sun" begin with it.
	t[0xc5] |= 1
	return t
}()

// span is a pattern's match: s[start:end].
type span struct{ start, end int }

// The scan's work, counted where it is done, for the rows (nothing in
// product code reads them; each is one add per text):
//   - instantScanPasses: the texts scanInstants read, one pass each.
//   - instantScanSteps: the bytes those passes stepped over, counted in the
//     loop that steps.
//   - instantScanTries: the matchers tried, each at a byte its layout's
//     matches can begin with; each reads at most one match's bytes past it.
var instantScanPasses, instantScanSteps, instantScanTries atomic.Int64

// scanInstants is each layout's matches in s, in the order they stand, as
// FindAllStringIndex of the layout's pattern, folded, gives them: matches[k]
// for layout k. It reads s once.
func scanInstants(s string) (matches [numLayouts][]span) {
	var next [numLayouts]int
	var steps, tries int64
	for i := 0; i < len(s); i++ {
		steps++
		set := layoutStarts[s[i]]
		for set != 0 {
			k := bits.TrailingZeros8(set)
			set &^= 1 << k
			if i < next[k] {
				continue
			}
			tries++
			if e := layoutAt[k](s, i); e > i {
				matches[k] = append(matches[k], span{i, e})
				next[k] = e
			}
		}
	}
	instantScanPasses.Add(1)
	instantScanSteps.Add(steps)
	instantScanTries.Add(tries)
	return matches
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || c|0x20 >= 'a' && c|0x20 <= 'f' }

// digitsAt reports whether s holds n ASCII digits at i.
func digitsAt(s string, i, n int) bool {
	if i+n > len(s) {
		return false
	}
	for j := i; j < i+n; j++ {
		if !isDigit(s[j]) {
			return false
		}
	}
	return true
}

// hexAt reports whether s holds n hex digits, in either case, at i.
func hexAt(s string, i, n int) bool {
	if i+n > len(s) {
		return false
	}
	for j := i; j < i+n; j++ {
		if !isHex(s[j]) {
			return false
		}
	}
	return true
}

// digitsFrom is how many ASCII digits s holds from i, counted to at most
// max.
func digitsFrom(s string, i, max int) int {
	n := 0
	for i+n < len(s) && n < max && isDigit(s[i+n]) {
		n++
	}
	return n
}

// byteIs reports whether s holds one of set at i.
func byteIs(s string, i int, set string) bool {
	if i >= len(s) {
		return false
	}
	for j := 0; j < len(set); j++ {
		if s[i] == set[j] {
			return true
		}
	}
	return false
}

// foldAt is the end of w, ASCII, in s at i under Go's simple case folding,
// or -1: a letter in either case, 's' also as ſ (U+017F) and 'k' also as
// the Kelvin sign (U+212A); any other byte as itself.
func foldAt(s string, i int, w string) int {
	for j := 0; j < len(w); j++ {
		c := w[j]
		if i >= len(s) {
			return -1
		}
		lc := c | 0x20
		if lc < 'a' || lc > 'z' {
			if s[i] != c {
				return -1
			}
			i++
			continue
		}
		switch {
		case s[i]|0x20 == lc:
			i++
		case lc == 's' && i+1 < len(s) && s[i] == 0xc5 && s[i+1] == 0xbf:
			i += 2
		case lc == 'k' && i+2 < len(s) && s[i] == 0xe2 && s[i+1] == 0x84 && s[i+2] == 0xaa:
			i += 3
		default:
			return -1
		}
	}
	return i
}

// foldAny is the end of the first of ws whose fold stands in s at i, or -1.
func foldAny(s string, i int, ws []string) int {
	for _, w := range ws {
		if e := foldAt(s, i, w); e >= 0 {
			return e
		}
	}
	return -1
}

var (
	rfcDays   = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	rfcMonths = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	utcGMT    = []string{"UTC", "GMT"}
	gmtUTC    = []string{"GMT", "UTC"}
)

// rfc1123At is layout 0:
// (?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (?:Jan|…|Dec) \d{4} \d{2}:\d{2}:\d{2} (?:GMT|UTC|[+-]\d{4})
func rfc1123At(s string, i int) int {
	j := foldAny(s, i, rfcDays)
	if j < 0 || !byteIs(s, j, ",") || !byteIs(s, j+1, " ") || !digitsAt(s, j+2, 2) || !byteIs(s, j+4, " ") {
		return -1
	}
	if j = foldAny(s, j+5, rfcMonths); j < 0 {
		return -1
	}
	if !byteIs(s, j, " ") || !digitsAt(s, j+1, 4) || !byteIs(s, j+5, " ") ||
		!digitsAt(s, j+6, 2) || !byteIs(s, j+8, ":") || !digitsAt(s, j+9, 2) || !byteIs(s, j+11, ":") || !digitsAt(s, j+12, 2) ||
		!byteIs(s, j+14, " ") {
		return -1
	}
	j += 15
	if e := foldAny(s, j, gmtUTC); e >= 0 {
		return e
	}
	if byteIs(s, j, "+-") && digitsAt(s, j+1, 4) {
		return j + 5
	}
	return -1
}

// isoAt is layout 1:
// \d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d{1,9})?)?|T\d{2})?(?:Z|[+-]\d{2}(?::?\d{2})?| ?(?:UTC|GMT))?
func isoAt(s string, i int) int {
	if !digitsAt(s, i, 4) || !byteIs(s, i+4, "-") || !digitsAt(s, i+5, 2) || !byteIs(s, i+7, "-") || !digitsAt(s, i+8, 2) {
		return -1
	}
	j := i + 10
	// The time: to the minute, the second and a fraction; else, after a T,
	// the hour.
	switch {
	case byteIs(s, j, "Tt ") && digitsAt(s, j+1, 2) && byteIs(s, j+3, ":") && digitsAt(s, j+4, 2):
		j += 6
		if byteIs(s, j, ":") && digitsAt(s, j+1, 2) {
			j += 3
			if byteIs(s, j, ".,") && digitsAt(s, j+1, 1) {
				j += 1 + digitsFrom(s, j+1, 9)
			}
		}
	case byteIs(s, j, "Tt") && digitsAt(s, j+1, 2):
		j += 3
	}
	// The zone.
	switch {
	case byteIs(s, j, "Zz"):
		return j + 1
	case byteIs(s, j, "+-") && digitsAt(s, j+1, 2):
		j += 3
		switch {
		case byteIs(s, j, ":") && digitsAt(s, j+1, 2):
			return j + 3
		case digitsAt(s, j, 2):
			return j + 2
		}
		return j
	}
	if byteIs(s, j, " ") {
		if e := foldAny(s, j+1, utcGMT); e >= 0 {
			return e
		}
	}
	if e := foldAny(s, j, utcGMT); e >= 0 {
		return e
	}
	return j
}

// compactAt is layout 2: \d{8}T\d{6}Z?
func compactAt(s string, i int) int {
	if !digitsAt(s, i, 8) || !byteIs(s, i+8, "Tt") || !digitsAt(s, i+9, 6) {
		return -1
	}
	if byteIs(s, i+15, "Zz") {
		return i + 16
	}
	return i + 15
}

// uuidAt is layout 3:
// [0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[17][0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}
func uuidAt(s string, i int) int {
	if !hexAt(s, i, 8) || !byteIs(s, i+8, "-") || !hexAt(s, i+9, 4) || !byteIs(s, i+13, "-") ||
		!byteIs(s, i+14, "17") || !hexAt(s, i+15, 3) || !byteIs(s, i+18, "-") || !hexAt(s, i+19, 4) ||
		!byteIs(s, i+23, "-") || !hexAt(s, i+24, 12) {
		return -1
	}
	return i + 36
}

// epochAt is layout 4: \d{10}\.\d{1,9}|\d{19}|\d{16}|\d{13}|\d{10}, its
// alternatives tried in that order.
func epochAt(s string, i int) int {
	n := digitsFrom(s, i, 19)
	switch {
	case n < 10:
		return -1
	case n == 10 && byteIs(s, i+10, ".") && digitsAt(s, i+11, 1):
		return i + 11 + digitsFrom(s, i+11, 9)
	case n >= 19:
		return i + 19
	case n >= 16:
		return i + 16
	case n >= 13:
		return i + 13
	}
	return i + 10
}

// timeOfDayAt is layout 5: \d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?
func timeOfDayAt(s string, i int) int {
	if !digitsAt(s, i, 2) || !byteIs(s, i+2, ":") || !digitsAt(s, i+3, 2) {
		return -1
	}
	j := i + 5
	if byteIs(s, j, ":") && digitsAt(s, j+1, 2) {
		j += 3
		if byteIs(s, j, ".") && digitsAt(s, j+1, 1) {
			j += 1 + digitsFrom(s, j+1, 9)
		}
	}
	return j
}
