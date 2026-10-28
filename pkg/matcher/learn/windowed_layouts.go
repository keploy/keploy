package learn

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

// layoutRe is one clock layout's pattern in either case (instantRes) that a
// text is read through only in the windows of it that can hold a match.
//
// The learn step reads the instants of every payload served earlier in a test
// (instantSpans, through spansOf), and of their decoded forms (rawExtra), in
// the request's own path: a call keploy cannot answer from its recording is
// answered only once the learn step has decided it. A browser test's earlier
// payloads hold the scripts its page loaded (Stripe.js's controller is 1.7 MB,
// its decoded forms 7 MB), and the six patterns, each read over all of such a
// text, held a miss's answer 9 to 15 s on a loaded box (enterprise-ui's
// smart-set lane: three POST r.stripe.com/b beacons answered together after
// 14.3, 13.8 and 8.5 s, the agent's goroutines in instantSpans).
//
// Every match of a layout overlaps one of the spans its anchors find (a byte
// scan of the text), and spans at most pad bytes (maxMatchLen). So a match
// lies inside its anchor's span widened by pad on each side; windows that
// overlap are read as one, and windows apart hold no match between them. The
// patterns hold no empty-width assertion (maxMatchLen refuses one), so a
// window reads a match as the whole text does, and leftmost-first, window
// after window, is the whole text's order: FindAllStringIndex returns what
// the pattern's own returns, read only where it can match.
type layoutRe struct {
	re *regexp.Regexp
	// anchors are the spans of a text every match of re overlaps; nil where
	// keploy knows none for the layout (the whole text is read).
	anchors func(s string) [][2]int
	// pad is the most bytes a match of re spans; -1 where it is unbounded
	// (the whole text is read).
	pad int
}

// layoutScan reads s through re, every match: a variable so a row can count
// the bytes the patterns read.
var layoutScan = func(re *regexp.Regexp, s string) [][]int { return re.FindAllStringIndex(s, -1) }

// newLayoutRe is lay's pattern in either case, read in windows where its
// anchors are known (layoutAnchors, by the layout's own pattern).
func newLayoutRe(lay clockLayout) *layoutRe {
	re := regexp.MustCompile("(?i)" + lay.re.String())
	l := &layoutRe{re: re, pad: -1}
	if a, ok := layoutAnchors[lay.re.String()]; ok {
		l.anchors, l.pad = a, maxMatchLen(re)
	}
	return l
}

// FindAllStringIndex is the pattern's FindAllStringIndex(s, n), read only in
// the windows of s that can hold a match. n is -1 (every match), as
// instantSpans asks; any other n reads the whole text.
func (l *layoutRe) FindAllStringIndex(s string, n int) [][]int {
	if l.anchors == nil || l.pad < 0 || n >= 0 {
		if n >= 0 {
			return l.re.FindAllStringIndex(s, n)
		}
		return layoutScan(l.re, s)
	}
	var out [][]int
	lo, hi := -1, -1
	read := func() {
		if lo < 0 {
			return
		}
		for _, m := range layoutScan(l.re, s[lo:hi]) {
			out = append(out, []int{lo + m[0], lo + m[1]})
		}
	}
	for _, a := range l.anchors(s) {
		start, end := max(a[0]-l.pad, 0), min(a[1]+l.pad, len(s))
		if lo >= 0 && start <= hi {
			hi = max(hi, end)
			continue
		}
		read()
		lo, hi = start, end
	}
	read()
	return out
}

// layoutAnchors are, by a clock layout's pattern (clockLayouts), the spans of
// a text that every match of it overlaps, in the order they stand. A layout
// whose pattern is not here, or changed, is read over the whole text: never a
// wrong instant, only a slower one (a row names every layout that is).
var layoutAnchors = map[string]func(string) [][2]int{
	// "Mon, 02 Jan 2006 15:04:05 GMT" holds its time of day.
	`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4} \d{2}:\d{2}:\d{2} (?:GMT|UTC|[+-]\d{4})`: digitPairsAround(':'),
	// "2026-10-22…" starts with its date.
	`\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d{1,9})?)?|T\d{2})?(?:Z|[+-]\d{2}(?::?\d{2})?| ?(?:UTC|GMT))?`: digitPairsAround('-'),
	// "20261022T101500Z": a digit, T in either case, a digit.
	`\d{8}T\d{6}Z?`: digitTDigit,
	// A time-ordered UUID's version group: "-1xxx-" or "-7xxx-".
	`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[17][0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`: uuidVersionGroup,
	// An epoch starts with ten digits in a row.
	`\d{10}\.\d{1,9}|\d{19}|\d{16}|\d{13}|\d{10}`: digitRuns(10),
	// "15:04", its seconds and their fraction.
	`\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?`: digitPairsAround(':'),
}

func isDigitByte(c byte) bool { return '0' <= c && c <= '9' }

func isHexByte(c byte) bool {
	return isDigitByte(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// digitPairsAround finds sep with two digits on each side ("15:04",
// "10-22"): a span of the five.
func digitPairsAround(sep byte) func(string) [][2]int {
	return func(s string) [][2]int {
		var out [][2]int
		for i := 0; ; {
			j := strings.IndexByte(s[i:], sep)
			if j < 0 {
				return out
			}
			at := i + j
			if at >= 2 && at+2 < len(s) && isDigitByte(s[at-2]) && isDigitByte(s[at-1]) && isDigitByte(s[at+1]) && isDigitByte(s[at+2]) {
				out = append(out, [2]int{at - 2, at + 3})
			}
			i = at + 1
		}
	}
}

// digitTDigit finds a T, in either case, between two digits.
func digitTDigit(s string) [][2]int {
	var out [][2]int
	for i := 1; i+1 < len(s); i++ {
		if (s[i] == 'T' || s[i] == 't') && isDigitByte(s[i-1]) && isDigitByte(s[i+1]) {
			out = append(out, [2]int{i - 1, i + 2})
		}
	}
	return out
}

// uuidVersionGroup finds "-" then 1 or 7, three hex digits and "-".
func uuidVersionGroup(s string) [][2]int {
	var out [][2]int
	for i := 0; ; {
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			return out
		}
		at := i + j
		if at+5 < len(s) && (s[at+1] == '1' || s[at+1] == '7') && isHexByte(s[at+2]) && isHexByte(s[at+3]) && isHexByte(s[at+4]) && s[at+5] == '-' {
			out = append(out, [2]int{at, at + 6})
		}
		i = at + 1
	}
}

// digitRuns finds every run of at least n digits, whole.
func digitRuns(n int) func(string) [][2]int {
	return func(s string) [][2]int {
		var out [][2]int
		for i := 0; i < len(s); {
			if !isDigitByte(s[i]) {
				i++
				continue
			}
			j := i
			for j < len(s) && isDigitByte(s[j]) {
				j++
			}
			if j-i >= n {
				out = append(out, [2]int{i, j})
			}
			i = j
		}
		return out
	}
}

// maxMatchLen is the most bytes a match of re spans, -1 where it is
// unbounded or re holds an empty-width assertion (a window could then read a
// match its text's context decides).
func maxMatchLen(re *regexp.Regexp) int {
	p, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return -1
	}
	return syntaxMaxLen(p.Simplify())
}

func syntaxMaxLen(r *syntax.Regexp) int {
	switch r.Op {
	case syntax.OpEmptyMatch:
		return 0
	case syntax.OpLiteral:
		n := 0
		for _, c := range r.Rune {
			n += maxRuneLen(c, r.Flags&syntax.FoldCase != 0)
		}
		return n
	case syntax.OpCharClass:
		hi := rune(0)
		for i := 1; i < len(r.Rune); i += 2 {
			hi = max(hi, r.Rune[i])
		}
		return utf8.RuneLen(min(hi, utf8.MaxRune))
	case syntax.OpCapture:
		return syntaxMaxLen(r.Sub[0])
	case syntax.OpQuest:
		return syntaxMaxLen(r.Sub[0])
	case syntax.OpRepeat:
		if r.Max < 0 {
			return -1
		}
		n := syntaxMaxLen(r.Sub[0])
		if n < 0 {
			return -1
		}
		return n * r.Max
	case syntax.OpConcat:
		n := 0
		for _, s := range r.Sub {
			m := syntaxMaxLen(s)
			if m < 0 {
				return -1
			}
			n += m
		}
		return n
	case syntax.OpAlternate:
		n := 0
		for _, s := range r.Sub {
			m := syntaxMaxLen(s)
			if m < 0 {
				return -1
			}
			n = max(n, m)
		}
		return n
	}
	// OpStar, OpPlus, OpAnyChar*, and every empty-width assertion.
	return -1
}

// maxRuneLen is the most bytes c spans, in either case where fold: every
// rune of its case-folding orbit ("s" also matches "ſ", two bytes).
func maxRuneLen(c rune, fold bool) int {
	n := utf8.RuneLen(c)
	if !fold {
		return n
	}
	for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
		n = max(n, utf8.RuneLen(f))
	}
	return n
}
