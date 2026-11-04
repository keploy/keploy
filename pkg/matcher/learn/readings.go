package learn

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The declared readings of the learn step. A drifted value, and every value
// it is compared with, is read in each of these forms, and each form into
// words in each of `readings`:
//
//   - raw: the value as it is;
//   - decoded: each %XX escape decoded on its own, and each backslash escape
//     read (\uXXXX and an escaped surrogate pair as the character it names,
//     \\ as \, \/ as /, and \n \r \t \b \f \" as a space, a separator),
//     again while that changes anything, up to four levels (unescaped);
//   - case-folded: the decoded form in lower case;
//   - quoted-printable: its soft line breaks taken out and its "=XX"
//     escapes decoded, where they decode to text (qpDecoded), and that in
//     lower case. What a drifted value is checked for (unparsed, below)
//     reads it in this form only where it and the value it is compared
//     with read as quoted-printable text: as a mail that says it is; or
//     whole, and its escapes show it or a soft line break splits it
//     (qpFormRead, formsOf);
//   - encoded: each run of eight characters or more that decodes to text:
//     of the standard or URL-safe base64 alphabet, padded or not, whole,
//     joined across the line breaks of wrapped base64 (indented or escaped
//     too), between '/' and '.', or its longest parts of sixteen or more
//     between groups of '-' and '_' where the run is no text whole (a
//     signed message's "<base64>--<digest>"); of hex,
//     with or without "0x"; or of base32 in one case, padded or not. The
//     text is read decoded, and its own runs again, up to maxDecodeLevels
//     (a JWT's claims, a base64 JSON state, a JWT inside one); and
//     case-folded (b64Text).
//
// A payload is read in the same forms (textForms). What these readings
// cannot read whole is not learned as noise: a value whose drift sits in a
// stray '%' or '\', in a run of the standard base64 alphabet that decodes
// as neither base64 nor base32, or in text of a script with no spaces
// between its words, is
// refused (unparsed), as is a value with a changed word that appears
// elsewhere in the test without having drifted with it (unaccounted). An
// encoding these readings do not declare (base58 or base36, a compressed or
// an encrypted value) is not read: a value that flowed into one reads as a
// fresh token, and is learned as noise when no word of it appears earlier.
// Telling the two apart takes where the value came from, not its shape.
const maxDecodeLevels = 4

// nextB64Run is the first run of the base64 alphabets in s at or after
// from, standard or URL-safe, with its padding (base32's too: up to six
// '='), as s[start:end]; ok is false where s has none there. It reads each
// byte it passes once, and the runs it gives one after another, each from
// the last one's end, are what the pattern [A-Za-z0-9+/_-]+={0,6} finds in
// s, leftmost first and none overlapping: a staging reads every payload's
// runs, and a regexp scan of them was about a sixth of its CPU.
func nextB64Run(s string, from int) (start, end int, ok bool) {
	i := from
	for i < len(s) && !isB64Byte(s[i]) {
		i++
	}
	if i >= len(s) {
		return 0, 0, false
	}
	start = i
	for i < len(s) && isB64Byte(s[i]) {
		i++
	}
	for pad := 0; pad < 6 && i < len(s) && s[i] == '='; pad++ {
		i++
	}
	return start, i, true
}

// isB64Run says whether s is one run of the base64 alphabets whole, with
// its padding (nextB64Run).
func isB64Run(s string) bool {
	start, end, ok := nextB64Run(s, 0)
	return ok && start == 0 && end == len(s)
}

// isB64Byte says whether c is of the base64 alphabets, standard or
// URL-safe.
func isB64Byte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '_' || c == '-'
}

// b64Text is the text the encoded runs of s decode to, each on a line of
// its own, decoded again while that finds more, up to maxDecodeLevels
// (base64 inside base64, a JWT inside a base64 session): base64 runs whole,
// joined across the line breaks of wrapped base64 (joinWrapped), between
// '/' (a path's segments, standard base64's own '/' aside) or '.' (a
// JWT's), and, where a run is no text whole, its longest parts between
// groups of '-' and '_' that are, of minGluedRun characters or more
// ("<base64>--<digest>" of a signed message, gluedSpans); hex
// runs, with or without "0x"; and base32 runs, padded or not; wherever they
// decode to text. Each text is read for escapes too (unescaped). clock are
// the value's clock words: a part of a clock read ("456Z" of its
// milliseconds) is read as the clock it is, not decoded.
func b64Text(s string, clock map[string]bool) string { return b64Read(s, clock, false) }

// b64Read is b64Text. A payload is mostly runs with nothing to split, and
// reading them was half of the CPU of learning a big one, so it passes by
// three reads whose answer it knows: the one part of a run with no '/' is
// the run (no run holds a '.': nextB64Run ends one there), its glued spans
// are none where it has no group of '-' or '_', and no word is in an empty
// clock. And it keeps, of the runs it tried, only those that decoded
// (seen): a run that did not decodes to nothing whenever it is tried again
// (decodeText and the clock's words read c alone), and keeping each was
// about a third of b64Read's CPU on a payload of words that are no text
// (the bound row's). everySplit makes those reads all the same and keeps
// every run tried: only the row that holds b64Text to them sets it.
func b64Read(s string, clock map[string]bool, everySplit bool) string {
	r := newB64Reader(s, clock, everySplit)
	for depth := 0; depth < maxDecodeLevels && len(r.level) > 0; depth++ {
		r.next()
	}
	return r.sb.String()
}

// b64Reader reads the encoded runs of a text one level at a time (b64Read):
// level are the texts the last level decoded to (at first the text itself),
// whose runs the next level decodes; sb holds every text decoded so far, each
// on a line of its own; and a run all of whose words are in clock is a part
// of a clock read, not decoded. Where keepRuns is set, runs are the runs it
// decoded whole where they stand (b64Pair's, for the value's plain runs:
// plainRunsOf), and offs where in sb each text of level stands (-1: the
// text it was given). texts are the texts of the runs it decoded, by run.
type b64Reader struct {
	sb         strings.Builder
	seen       map[string]bool
	texts      map[string]string
	level      []string
	clock      map[string]bool
	everySplit bool
	keepRuns   bool
	runs       []decodedRun
	offs       []int
}

// decodedRun is a run of the base64 alphabets (nextB64Run) a b64Reader
// decoded whole, or whole past its "0x" (or "0X") in any encoding it reads
// (hex, base64), where it stands: at [st, en) of the text the reader was
// given (at -1) or of its sb from at. esc is, for a run's text in a raw
// value (rawImages), how it writes the bytes it escapes, and escs each
// escape in it, in order.
type decodedRun struct {
	run        string
	at, st, en int
	esc        map[byte]string
	escs       []rawEscape
}

// rawEscape is one byte of a run's text in a raw value (rawImages) that the
// text writes by an escape: the byte (c), its spelling (sp, "%3D"), where
// that stands in the raw value ([st, en)), and which copy of c in the run
// it is (n, from 0).
type rawEscape struct {
	c      byte
	sp     string
	st, en int
	n      int
}

func newB64Reader(s string, clock map[string]bool, everySplit bool) *b64Reader {
	return &b64Reader{seen: map[string]bool{}, texts: map[string]string{}, level: []string{s}, clock: clock, everySplit: everySplit}
}

// next decodes the runs of the texts of rd's level, writes each text it
// finds to sb, and makes those texts rd's level (none where it finds none).
func (rd *b64Reader) next() {
	sb, seen, clock, everySplit := &rd.sb, rd.seen, rd.clock, rd.everySplit
	var next []string
	var nextOffs []int
	try := func(c string) bool {
		if len(c) < minEncodedRun || seen[c] || (everySplit || len(clock) > 0) && allIn(tokensBy(c, isWord), clock) {
			return false
		}
		if everySplit {
			seen[c] = true
		}
		t, ok := decodeText(c)
		if !ok {
			return false
		}
		seen[c] = true
		raw := t
		rd.texts[c] = raw
		t = unescaped(raw)
		if q := qpForm(raw); q != t {
			t += "\n" + q
		}
		if rd.keepRuns {
			nextOffs = append(nextOffs, sb.Len())
		}
		sb.WriteString(t)
		sb.WriteByte('\n')
		next = append(next, t)
		return true
	}
	// hexWhole: the last run tryRun read decoded whole past its "0x" (or
	// "0X"), in any encoding (set on every read).
	hexWhole := false
	tryRun := func(run string) bool {
		slash, glued := runSeparators(run)
		whole := try(run)
		if everySplit || slash {
			// A part of a run decoded whole, here or in a text read
			// before (texts), that decodes to a piece of the run's text
			// (one that starts a group of four: "b2s" of "b2s/IHll")
			// holds nothing that text does not: it is not read again, so
			// a '/' the encoder happened to write (a byte of the text, or
			// its length, moving one into the run) adds no copy of the
			// text's words to one side's form.
			wholeText, had := rd.texts[run]
			for _, part := range strings.FieldsFunc(run, func(r rune) bool { return r == '/' || r == '.' }) {
				if pt, ok := decodeText(part); had && ok && strings.Contains(wholeText, pt) {
					continue
				}
				try(part)
			}
		}
		h, prefixed := strings.CutPrefix(run, "0x")
		if !prefixed {
			h, prefixed = strings.CutPrefix(run, "0X")
		}
		hexWhole = prefixed && try(h)
		if whole {
			return true
		}
		if !everySplit && !glued {
			return false
		}
		// Its longest parts between groups of '-' and '_' that are text,
		// none inside another that was.
		var read [][2]int
		for _, sp := range gluedSpans(run) {
			inside := false
			for _, r := range read {
				if sp[0] >= r[0] && sp[1] <= r[1] {
					inside = true
					break
				}
			}
			if !inside && try(run[sp[0]:sp[1]]) {
				read = append(read, sp)
			}
		}
		return false
	}
	for li, text := range rd.level {
		texts := []string{text}
		j, joins := joinWrapped(text)
		if j != text {
			texts = append(texts, j)
		}
		for ti, tx := range texts {
			for st, en, ok := nextB64Run(tx, 0); ok; st, en, ok = nextB64Run(tx, en) {
				ix := [2]int{st, en}
				whole := tryRun(tx[ix[0]:ix[1]])
				// A run decoded whole where it stands in the level's text,
				// or whole past its "0x": its text stands for it.
				if ti == 0 && rd.keepRuns && (whole || hexWhole) {
					rd.runs = append(rd.runs, decodedRun{run: strings.Clone(tx[ix[0]:ix[1]]), at: rd.offs[li], st: ix[0], en: ix[1]})
				}
				if whole || ti == 0 {
					continue
				}
				// A run that joined lines and is no text: its lines up
				// to a join may be (the last line of the base64 joined
				// to the text on the next line), longest first; and so
				// may its lines from its first join (the line before
				// was no part of it: a header ending in an id).
				starts := []int{ix[0]}
				for _, c := range joins {
					if c > ix[0] && c < ix[1] {
						starts = append(starts, c)
						break
					}
				}
			retry:
				for _, st := range starts {
					if st != ix[0] && try(tx[st:ix[1]]) {
						break
					}
					for k := len(joins) - 1; k >= 0; k-- {
						if c := joins[k]; c > st && c < ix[1] && try(tx[st:c]) {
							break retry
						}
					}
				}
			}
			for part := range strings.SplitSeq(tx, ".") {
				if isB64Run(part) {
					try(part)
				}
			}
		}
	}
	rd.level, rd.offs = next, nextOffs
}

// b64Pair is b64Text of a drifted value's two sides, recorded and live
// (clock their clock words), read one level at a time: after each level,
// the text each side has decoded so far is read for clock words as the
// value's own are, in the value's window (clockOf, of a recorded text and a
// live one: newDrift's), before the runs of that level's texts are decoded
// in turn, and a run all of whose words are those of a clock read found
// there is not decoded either. So a clock read written in the text an
// encoded run decodes to ("tok <base64 of 'lead <stamp> created'> ok"),
// where the two sides hold each of the value's instants at the same depth,
// is read as the clock it is, and its digits ("3505371Z" of the stamp's
// fraction) are not decoded as data, which would decide by chance between
// a token a payload holds, text of a script keploy reads only whole and
// nothing. A word found here is a read the value's window (or the lead)
// holds: that text is the start of the encoded form the caller reads for
// clock words itself. Limit: where the sides hold an instant at different
// depths (one side's text encoded once more), it pairs with the other's
// only once both are decoded, so a run of a read decoded before that, on
// either side and of any layout whose pairing it shifts, is decoded as it
// was before (fail closed: its words are data), and the caller's read of
// the whole may pair a layout otherwise (an earlier layout taking the
// bytes of a later one's). The sets in clock are not changed: the caller
// reads the clock words of the decoded text itself. runs are the runs
// each side (recorded [0], live [1]) decoded whole, at any level, where
// they stand (plainRunsOf).
func b64Pair(rec, live string, clock clockSet, clockOf func(rec, live string) clockSet) (string, string, [2][]decodedRun) {
	r, l := newB64Reader(rec, clock.rec, false), newB64Reader(live, clock.live, false)
	for _, rd := range []*b64Reader{r, l} {
		rd.keepRuns, rd.offs = true, []int{-1}
	}
	for depth := 0; depth < maxDecodeLevels && (len(r.level) > 0 || len(l.level) > 0); depth++ {
		r.next()
		l.next()
		// Where neither side decoded more, nothing new is read.
		if len(r.level) > 0 || len(l.level) > 0 {
			c := clockOf(r.sb.String(), l.sb.String())
			r.clock, l.clock = union(r.clock, c.rec), union(l.clock, c.live)
		}
	}
	return r.sb.String(), l.sb.String(), [2][]decodedRun{r.runs, l.runs}
}

// union is a new set of the words of a and of b.
func union(a, b map[string]bool) map[string]bool {
	u := make(map[string]bool, len(a)+len(b))
	maps.Copy(u, a)
	maps.Copy(u, b)
	return u
}

// runSeparators says whether run holds a '/' (slash: it is read by its
// parts between '/' too) and a '-' or '_' (glued: by its parts between
// groups of them, where it is no text whole), read in one pass of its
// bytes. b64Read asks both of every run of a payload, mostly words of a few
// bytes, and asking with strings.Contains and strings.IndexByte, three
// calls each, was a quarter of what trying such a run cost.
func runSeparators(run string) (slash, glued bool) {
	for i := 0; i < len(run); i++ {
		switch run[i] {
		case '/':
			slash = true
		case '-', '_':
			glued = true
		}
	}
	return slash, glued
}

// maxGluedGroups bounds the groups of '-' and '_' a run is cut at every
// pair of (gluedSpans): a run with more is cut at fewer pairs, in linear
// time (gluedSpans).
const maxGluedGroups = 12

// maxGluedDoubles bounds the double separators a run over maxGluedGroups
// is cut at: its first and last few.
const maxGluedDoubles = 4

// gluedTextEnds are, of a run of base64 (either alphabet) cut at groups of
// '-' and '_' (starts: where a part begins after one; ends: where one
// begins), the ends of its longest prefixes (from 0) and the starts of its
// longest suffixes (to its end) at a group that decode to text: up to three
// prefixes, longest first, and for each of the four offsets modulo four its
// longest suffix. A part that holds both alphabets' own characters ('+' or
// '/' and '-' or '_') decodes in neither and is passed over. Each is found
// in linear time, a decoding of the run for the prefixes and one for each
// offset for the suffixes: a prefix decodes to the run's own decoding cut
// short, and a suffix starting at the same offset modulo four to that
// offset's decoding cut short.
func gluedTextEnds(run string, starts, ends []int) (prefixEnds, suffixStarts []int) {
	// mixedTo[b]: run[:b] holds both alphabets; mixedFrom[a]: run[a:] does.
	mixedTo, mixedFrom := make([]bool, len(run)+1), make([]bool, len(run)+1)
	std, url := false, false
	for i := 0; i < len(run); i++ {
		std, url = std || run[i] == '+' || run[i] == '/', url || run[i] == '-' || run[i] == '_'
		mixedTo[i+1] = std && url
	}
	std, url = false, false
	for i := len(run) - 1; i >= 0; i-- {
		std, url = std || run[i] == '+' || run[i] == '/', url || run[i] == '-' || run[i] == '_'
		mixedFrom[i] = std && url
	}
	dec := lenientB64(run)
	textLen, starts0 := textPrefix(dec)
	for g := len(ends) - 1; g >= 0 && len(prefixEnds) < 3; g-- {
		b := ends[g]
		if nb := b * 6 / 8; b >= minGluedRun && b%4 != 1 && !mixedTo[b] && nb <= textLen && starts0[nb] {
			prefixEnds = append(prefixEnds, b)
		}
	}
	for k := 0; k < 4 && k < len(run); k++ {
		d := lenientB64(run[k:])
		from, isStart := textSuffix(d)
		for _, a := range starts {
			if a < k || (a-k)%4 != 0 || len(run)-a < minGluedRun || (len(run)-a)%4 == 1 || mixedFrom[a] {
				continue
			}
			if off := (a - k) / 4 * 3; off >= from && off < len(d) && isStart[off] {
				suffixStarts = append(suffixStarts, a)
				break
			}
		}
	}
	sort.Ints(suffixStarts)
	return prefixEnds, suffixStarts
}

// lenientB64 decodes s as base64 of either alphabet ('+' or '-', '/' or
// '_'), unpadded, up to its first other character.
func lenientB64(s string) []byte {
	out := make([]byte, 0, len(s)*3/4+2)
	var acc uint32
	n := 0
	for i := 0; i < len(s); i++ {
		var v uint32
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
			v = uint32(c - 'A')
		case c >= 'a' && c <= 'z':
			v = uint32(c-'a') + 26
		case c >= '0' && c <= '9':
			v = uint32(c-'0') + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		default:
			i = len(s)
			continue
		}
		acc = acc<<6 | v
		if n++; n == 4 {
			out = append(out, byte(acc>>16), byte(acc>>8), byte(acc))
			acc, n = 0, 0
		}
	}
	switch n {
	case 2:
		out = append(out, byte(acc>>4))
	case 3:
		out = append(out, byte(acc>>10), byte(acc>>2))
	}
	return out
}

// textPrefix is how much of b from its start is text (isText), and which
// offsets up to there begin a character.
func textPrefix(b []byte) (int, []bool) {
	starts := make([]bool, len(b)+1)
	i := 0
	for i < len(b) {
		starts[i] = true
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 || textControl(r) {
			return i, starts
		}
		i += size
	}
	starts[i] = true
	return i, starts
}

// textSuffix is the offset from which b to its end is text (isText), and
// which offsets from there begin a character.
func textSuffix(b []byte) (int, []bool) {
	starts := make([]bool, len(b)+1)
	j := len(b)
	starts[j] = true
	for j > 0 {
		r, size := utf8.DecodeLastRune(b[:j])
		if r == utf8.RuneError && size <= 1 || textControl(r) {
			return j, starts
		}
		j -= size
		starts[j] = true
	}
	return 0, starts
}

// textControl reports whether r is a control character text holds none of
// (isText): one below a space but tab and line breaks, DEL, and C1.
func textControl(r rune) bool {
	return r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r >= 0x7f && r < 0xa0
}

// gluedSpans are the parts of run between groups of '-' and '_' (a signed
// message's "<base64>--<digest>", "v1_<base64>--<digest>"), as [start, end)
// offsets, of minGluedRun characters or more, longest first; the whole run
// is not one.
func gluedSpans(run string) [][2]int {
	// starts and ends of the stretches between groups.
	starts, ends := []int{0}, []int{}
	for i := 0; i < len(run); {
		if run[i] != '-' && run[i] != '_' {
			i++
			continue
		}
		j := i
		for j < len(run) && (run[j] == '-' || run[j] == '_') {
			j++
		}
		ends = append(ends, i)
		starts = append(starts, j)
		i = j
	}
	ends = append(ends, len(run))
	// A run of more groups (base64url with '-' and '_' of its own) is cut
	// between its first and last three groups, and at its first and last
	// four groups of two or more ("--" is rare inside base64url): a prefix
	// or a digest of up to three groups, or glued by a double separator.
	as, bs := starts, ends
	if len(starts) > maxGluedGroups+1 {
		// Its first and last three groups at either end of a span.
		as = append(append([]int(nil), starts[:4]...), starts[len(starts)-3:]...)
		bs = append(append([]int(nil), ends[:3]...), ends[len(ends)-4:]...)
		var doubleA, doubleB []int
		for g := 0; g+1 < len(starts); g++ {
			if starts[g+1]-ends[g] >= 2 {
				doubleA = append(doubleA, starts[g+1])
				doubleB = append(doubleB, ends[g])
			}
		}
		// The first and last few double separators: a long run of
		// base64url has some of its own.
		if len(doubleA) > 2*maxGluedDoubles {
			doubleA = append(doubleA[:maxGluedDoubles:maxGluedDoubles], doubleA[len(doubleA)-maxGluedDoubles:]...)
			doubleB = append(doubleB[:maxGluedDoubles:maxGluedDoubles], doubleB[len(doubleB)-maxGluedDoubles:]...)
		}
		as, bs = append(as, doubleA...), append(bs, doubleB...)
	}
	over := len(starts) > maxGluedGroups+1
	seen := map[[2]int]bool{}
	var out [][2]int
	add := func(a, b int) {
		if a == 0 && b == len(run) || b-a < minGluedRun || b <= a || seen[[2]int{a, b}] {
			return
		}
		seen[[2]int{a, b}] = true
		out = append(out, [2]int{a, b})
	}
	for _, a := range as {
		for _, b := range bs {
			add(a, b)
		}
	}
	if over {
		// Every stretch between two groups: a part in an alphabet with no
		// '-' or '_' (hex, base32, standard base64) is one stretch wherever
		// it sits. The stretches are disjoint, so this is linear.
		for g := range starts {
			add(starts[g], ends[g])
		}
		// The longest prefixes and suffixes at a group that decode to text
		// (gluedTextEnds), from the run's start and from after each of its
		// first three groups, and to its end and to before each of its last
		// three: base64 text of either alphabet with groups of its own, glued
		// to a blob or a signature of any length, after or before a prefix
		// or a suffix of up to three groups.
		for _, a0 := range append([]int{0}, starts[1:4]...) {
			var ss, ee []int
			for _, x := range starts {
				if x > a0 {
					ss = append(ss, x-a0)
				}
			}
			for _, x := range ends[:len(ends)-1] {
				if x > a0 {
					ee = append(ee, x-a0)
				}
			}
			pre, _ := gluedTextEnds(run[a0:], ss, ee)
			for _, b := range pre {
				add(a0, a0+b)
			}
		}
		for _, b0 := range append([]int{len(run)}, ends[len(ends)-4:len(ends)-1]...) {
			var ss, ee []int
			for _, x := range starts[1:] {
				if x < b0 {
					ss = append(ss, x)
				}
			}
			for _, x := range ends {
				if x < b0 {
					ee = append(ee, x)
				}
			}
			_, suf := gluedTextEnds(run[:b0], ss, ee)
			for _, a := range suf {
				add(a, b0)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][1]-out[i][0] > out[j][1]-out[j][0] })
	return out
}

// joinWrapped is s with the line breaks of wrapped base64 taken out (MIME's
// 76 columns, Ruby's Base64.encode64 and Python's encodebytes at 60 or 76,
// PEM's 64), with the offsets in it where it took one out: a run of line
// breaks, spaces and tabs (an escaped "\n" or "\r\n" read as separators, an
// indent, a blank line), after a line of the base64 alphabet of 16
// characters or more and a multiple of four, before more of the alphabet.
// A line starts after a key glued before the base64 by '=' ("data="), or
// counts from after a prefix glued by '-' or '_' ("v1_").
//
// Until it takes a line break out, what it has read is s's own bytes, so it
// copies s only from its first join (built): a text with none, a payload
// of words, is s itself, read with no copy. b64Read reads each text it
// decodes this way, and copying each in full was a sixth of its CPU on the
// bound row's payloads.
func joinWrapped(s string) (string, []int) {
	var b strings.Builder
	var joins []int
	built := false
	line, glued := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		if b64Byte(c) {
			if built {
				b.WriteByte(c)
			}
			line++
			glued++
			i++
			if c == '=' && i < len(s) && s[i] != '=' && b64Byte(s[i]) {
				line = 0
			}
			if (c == '-' || c == '_') && i < len(s) && s[i] != '-' && s[i] != '_' {
				glued = 0
			}
			continue
		}
		j := i + 1
		switch c {
		case '\r', '\n', ' ', '\t':
			// A line break, an escaped one read as spaces, a blank line,
			// trailing or leading spaces: a join a line before the base64
			// takes in is undone by trying the run from its first join.
			for j < len(s) && (s[j] == '\r' || s[j] == '\n' || s[j] == ' ' || s[j] == '\t') {
				j++
			}
		default:
			if built {
				b.WriteByte(c)
			}
			line, glued = 0, 0
			i++
			continue
		}
		if line >= 16 && (line%4 == 0 || glued >= 16 && glued%4 == 0) && j < len(s) && b64Byte(s[j]) {
			if !built {
				b.Grow(len(s))
				b.WriteString(s[:i])
				built = true
			}
			line, glued = 0, 0
			i = j
			joins = append(joins, b.Len())
			continue
		}
		if built {
			b.WriteString(s[i:j])
		}
		line, glued = 0, 0
		i = j
	}
	if !built {
		return s, nil
	}
	return b.String(), joins
}

// qpDecoded is s read as quoted-printable (RFC 2045: a mail body, a MIME
// part): its soft line breaks ("=\r\n", "=\n") taken out, and each run of
// "=XX" escapes (upper-case hex) that decodes to text decoded, but to no '%'
// or '\' (which the readings would take for escapes keploy cannot read).
// A value with no such escape is itself.
func qpDecoded(s string) string {
	d, _, _ := qpRead(s)
	return d
}

// qpRead is s read as quoted-printable (qpDecoded), whether it reads as
// such whole, and whether its escapes show it is.
//
// Whole: each '=' it holds starts a soft line break or a run of escapes.
// Quoted-printable writes every '=' of its text as "=3D", so a value that
// holds any other '=' ("Credential=local/..., Signature=09bb..." of an AWS
// signature) is no quoted-printable text, and an "=XX" in it only looks
// like an escape. Nor is one with a run of escapes that decodes to a '%'
// or a '\', which qpDecoded leaves as written. A run that decodes to no
// text (a Latin-1 "caf=E9") is an escape quoted-printable writes too: it
// is left as written, and the value may still read whole.
//
// Shown: a run of its escapes decodes to text that holds a character
// quoted-printable has to write as an escape (qpMustEscape), or it holds no
// escape at all once its soft line breaks are out (it reads the same either
// way). Every other escape ("=41" an 'A', "=2B" a '+', "=09" a tab) is of a
// character quoted-printable may write as itself, and one that decodes to
// no text is a byte a signature's hex spells as often, so a value whose
// escapes are all of those kinds shows nothing: a signature in upper-case
// hex whose first two digits happen to be an escape ("sha256=2BC4...",
// "v1=41F0...", "v1=AA03...") is no '+', 'A' or byte and its hex.
func qpRead(s string) (d string, whole, shown bool) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "=\r\n", ""), "=\n", "")
	if !strings.Contains(s, "=") {
		return s, true, true
	}
	var b strings.Builder
	whole = true
	for i := 0; i < len(s); {
		j := i
		var bytes []byte
		for j+2 < len(s) && s[j] == '=' && upperHex(s[j+1]) && upperHex(s[j+2]) {
			v, _ := hex.DecodeString(s[j+1 : j+3])
			bytes = append(bytes, v[0])
			j += 3
		}
		if j == i {
			if s[i] == '=' {
				whole = false
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		switch {
		case !isText(bytes):
			b.WriteString(s[i:j])
		case strings.ContainsAny(string(bytes), "%\\"):
			whole = false
			b.WriteString(s[i:j])
		default:
			b.Write(bytes)
			shown = shown || qpMustEscape(bytes)
		}
		i = j
	}
	return b.String(), whole, shown
}

// qpMustEscape reports whether text, what a run of escapes decodes to,
// holds a character quoted-printable has to write as an escape (RFC 2045):
// an '=', or a non-ASCII character.
func qpMustEscape(text []byte) bool {
	for _, r := range string(text) {
		if r == '=' || r >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// qpForm is s read as quoted-printable (qpDecoded) after its escapes
// (unescaped), with its soft line breaks taken out first (noSoftBreaks), in
// their escaped spellings too ("=\\r\\n" of a mail in a JSON string), which
// unescaping would turn into separators.
//
// It is qpFormRead's form, read without asking whether s reads as
// quoted-printable: qpForm reads every payload a read decodes (decodeForms)
// and each text of its base64 runs (b64Read), and asking the declared
// header's pattern (qpDeclared) there took a matcher's state of 37 KB from
// its pool for each of 42 bytes or more (the pattern's shortest match),
// as the soft line breaks' pattern did before noSoftBreaks.
func qpForm(s string) string {
	t, _ := qpSoftOut(s)
	q, _, _ := qpRead(unescaped(t))
	return q
}

// qpFormRead is the quoted-printable form of s (qpForm), and whether s, a
// drifted value, reads as quoted-printable text, which unparsed then checks
// in that form beside that of the value it is compared with (formsOf):
//   - a mail or a MIME part that says it is (qpDeclared), whose header
//     lines hold an '=' of their own ("charset=utf-8") that starts no
//     escape;
//   - else whole, and its escapes show it or a soft line break splits it
//     (qpRead). An escape of a character quoted-printable may write as
//     itself, or of no text, shows nothing ("v1=2B" of a signature).
//
// Either way every escape that decodes to text is decoded, as the readings
// search it: a value read as quoted-printable is read whole as such, so
// what changed behind any of its escapes ("ab=2Bcd", a '+' and a run of the
// base64 alphabet) is checked as it reads.
func qpFormRead(s string) (string, bool) {
	t, soft := qpSoftOut(s)
	u := unescaped(t)
	q, whole, shown := qpRead(u)
	return q, qpDeclared.MatchString(u) || whole && (shown || soft)
}

// qpSoftOut is s with its soft line breaks taken out (noSoftBreaks), and
// whether it held one.
func qpSoftOut(s string) (string, bool) {
	t := noSoftBreaks(s)
	return t, t != s
}

// qpDeclared is the header of a mail or a MIME part whose body is
// quoted-printable.
var qpDeclared = regexp.MustCompile(`(?i)content-transfer-encoding:[ \t]*quoted-printable`)

// noSoftBreaks is s without its soft line breaks, each a "=" before a line
// break in any spelling (softBreakAt), taken out from the left: s itself
// where it holds none. It reads s byte by byte, as the pattern
// `=(?:\r?\n|(?:\\+r)?\\+n)` read it before: that pattern's matcher took a
// state of 37 KB from its pool for each text holding a "=" (a base64
// text's padding), nearly half of what a small read of one allocated, and
// took it again each time the pool had let it go.
func noSoftBreaks(s string) string {
	var b []byte
	last := 0
	for i := strings.IndexByte(s, '='); i >= 0; {
		n := softBreakAt(s[i+1:])
		if n > 0 {
			b = append(b, s[last:i]...)
			last = i + 1 + n
		}
		j := strings.IndexByte(s[i+1+n:], '=')
		if j < 0 {
			break
		}
		i += 1 + n + j
	}
	if last == 0 {
		return s
	}
	return string(append(b, s[last:]...))
}

// softBreakAt is the bytes of the line break t starts with, t following a
// "=": a raw one ("\n", "\r\n"), or one escaped once or more ("\\n",
// "\\r\\n", "\\\\r\\\\n" of a mail in a JSON string in a JSON string); 0
// where t starts with none.
func softBreakAt(t string) int {
	switch {
	case strings.HasPrefix(t, "\n"):
		return 1
	case strings.HasPrefix(t, "\r\n"):
		return 2
	}
	k := leadingBackslashes(t)
	if k == 0 || k == len(t) {
		return 0
	}
	switch t[k] {
	case 'n':
		return k + 1
	case 'r':
		if m := leadingBackslashes(t[k+1:]); m > 0 && k+1+m < len(t) && t[k+1+m] == 'n' {
			return k + 1 + m + 1
		}
	}
	return 0
}

// leadingBackslashes is how many backslashes t starts with.
func leadingBackslashes(t string) int {
	k := 0
	for k < len(t) && t[k] == '\\' {
		k++
	}
	return k
}

func upperHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }

// b64Byte reports whether c is of a base64 alphabet, standard or URL-safe,
// or its padding.
func b64Byte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_'
}

// decodeText decodes c, a run of an encoding's alphabet, to text: hex,
// base32 or base64, the first that gives text. hex and base32 are read as
// ASCII text, and base32 in one case (the alphabet's upper case, or all
// lower case): what they carry is an id or a name in ASCII, and a run that
// decodes otherwise is a word far more often than an encoded value (its
// decode is garbage that would refuse fresh ids).
func decodeText(c string) (string, bool) {
	if isHexRun(c) && len(c)%2 == 0 {
		if b, err := hex.DecodeString(c); err == nil && isASCIIText(b) {
			return string(b), true
		}
	}
	if b, ok := b32Decode(c); ok && len(b) > 0 && isASCIIText(b) {
		return string(b), true
	}
	return b64Decode(c)
}

// b32Decode decodes s as base32 in one case, padded or not.
func b32Decode(s string) ([]byte, bool) {
	if !isB32Run(s, 'A', 'Z') && !isB32Run(s, 'a', 'z') {
		return nil, false
	}
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(strings.ToUpper(s), "="))
	return b, err == nil
}

// isASCIIText reports whether b is text (isText) all in ASCII.
func isASCIIText(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return isText(b)
}

// minEncodedRun is the shortest run read as encoded: eight characters, four
// to six bytes. A shorter run is a word (an id of four letters) far more
// often than an encoded value, and its decode is garbage that would refuse
// fresh ids; a value of three bytes or less carries no id worth following.
const minEncodedRun = 8

// minGluedRun is the shortest part of a run, at a '-' or '_', read as
// encoded where the run is no text whole: a signed message's data
// ("<base64>--<digest>") is long, and the groups of a UUID or the halves of
// "ord-<id>" are words far more often than encoded values.
const minGluedRun = 16

// isHexRun is whether s is hex digits whole, one or more, of either case
// (the pattern ^[0-9a-fA-F]+$). decodeText reads every run of a payload,
// and the pattern's match was a tenth of learning a big one.
func isHexRun(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return false
		}
	}
	return s != ""
}

// isB32Run is whether s is a run of base32's alphabet in one case, its
// letters from lo to hi ('A' to 'Z', or 'a' to 'z') and the digits 2 to 7,
// one or more, then its padding, none or more '=' (the pattern
// ^[A-Z2-7]+=*$, or ^[a-z2-7]+=*$).
func isB32Run(s string, lo, hi byte) bool {
	i := 0
	for i < len(s) && (s[i] >= lo && s[i] <= hi || s[i] >= '2' && s[i] <= '7') {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(s) && s[i] == '=' {
		i++
	}
	return i == len(s)
}

// b64Decode decodes s in any base64 variant to text: valid UTF-8 with no
// control characters but whitespace, its padding read leniently. Binary (a
// signature, a hash) is no text, and nothing is read in it.
func b64Decode(s string) (string, bool) {
	if !b64MayBeText(s) {
		return "", false
	}
	b, ok := b64DecodeAny(s)
	if !ok {
		if t := strings.TrimRight(s, "="); t != s {
			b, ok = b64DecodeAny(t)
		}
	}
	if !ok || len(b) == 0 || !isText(b) {
		return "", false
	}
	return string(b), true
}

// b64MayBeText is false where no decode of s by any of b64Variants, nor of
// s with its padding trimmed, is text (isText), told from s's first four
// characters: where each is a letter or a digit, both alphabets read them
// alike, every decode of s begins with the three bytes they make (a
// decoder skips line breaks only, and there are four characters before
// any), and isText refuses whatever begins with bytes no text begins with
// (textBegins). A payload's runs are mostly words that decode to no text
// ("w12x1000" of the bound row's), and decoding each whole, a copy each,
// was most of what reading its runs (b64Read) cost.
func b64MayBeText(s string) bool {
	if len(s) < 4 {
		return true
	}
	var v uint32
	for k := 0; k < 4; k++ {
		d, ok := b64Digit(s[k])
		if !ok {
			return true
		}
		v = v<<6 | d
	}
	first := [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
	return textBegins(first[:])
}

// b64Digit is the value of c, a letter or a digit, in both base64
// alphabets; ok is false for any other byte, which the two read apart or
// not at all.
func b64Digit(c byte) (d uint32, ok bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return uint32(c - 'A'), true
	case c >= 'a' && c <= 'z':
		return uint32(c-'a') + 26, true
	case c >= '0' && c <= '9':
		return uint32(c-'0') + 52, true
	}
	return 0, false
}

// textBegins says whether some text (isText) begins with b: each whole
// character of it valid UTF-8 and no control (textControl), and what is
// left the start of a character. A character's bytes that are invalid
// already (utf8.FullRune: no byte after them makes them valid) begin none.
func textBegins(b []byte) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			return !utf8.FullRune(b)
		}
		if textControl(r) {
			return false
		}
		b = b[n:]
	}
	return true
}

func isText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if textControl(r) {
			return false
		}
	}
	return true
}

// The reasons unparsed gives for a value whose text of a script keploy
// reads only whole (nonASCIIRuns) changed, by where that text stands: in the
// value's own text, in the text its escapes write (a percent- or
// quoted-printable-escaped name), or in the text an encoded run of it
// decodes to (b64Text: the base64 run "3505371Z" of a .NET stamp's fraction
// decodes to such text). A value all in ASCII is never said to hold such
// text itself. Each names that text as scriptWhy does ("text of a script
// keploy does not split into words"), so the three read as one refusal and
// say where the text stands.
const (
	scriptWhy        = "it changed in text of a script keploy does not split into words"
	escapedScriptWhy = "it changed in the text its escapes write, which is text of a script keploy does not split into words"
	encodedScriptWhy = "it changed in the text an encoded run of it decodes to, which is text of a script keploy does not split into words"
)

// unparsed says what of a drifted value, read in form (recorded, live),
// the declared readings cannot read whole, where it drifted: "" when
// nothing. clock are the value's clock words: a clock read the lead moved
// ("41+00:00" of an aware datetime) is read, whatever runs it makes. script
// is the reason to give where the form's text of a script keploy reads only
// whole changed (scriptWhy and its kin: where that text stands).
func unparsed(rec, live string, clock clockSet, script string) string {
	if rec == live {
		return ""
	}
	if strings.ContainsAny(rec, `%\`) || strings.ContainsAny(live, `%\`) {
		return "it holds a '%' or '\\' that is no escape keploy reads"
	}
	if !sameStrings(nonASCIIRuns(rec), nonASCIIRuns(live)) {
		return script
	}
	r, l := b64Runs(rec), b64Runs(live)
	for _, side := range []struct {
		own, other map[string]bool
		recorded   bool
	}{{r, l, true}, {l, r, false}} {
		for t := range side.own {
			// A run of the standard alphabet ('+', '=' padding, never '-'
			// or '_') that changed.
			if side.other[t] || !strings.ContainsAny(t, "+=") || strings.ContainsAny(t, "-_") {
				continue
			}
			if _, ok := b64DecodeAny(t); ok {
				continue
			}
			if _, ok := b32Decode(t); ok {
				continue
			}
			changedClock := true
			for w := range tokensBy(t, isWord) {
				if !clockSide(w, side.recorded, []clockSet{clock}) {
					changedClock = false
				}
			}
			if !changedClock {
				return "it changed in a run of the base64 alphabet that does not decode"
			}
		}
	}
	return ""
}

// b64Runs are the runs of the base64 alphabets in s, with their padding.
func b64Runs(s string) map[string]bool {
	out := map[string]bool{}
	for st, en, ok := nextB64Run(s, 0); ok; st, en, ok = nextB64Run(s, en) {
		out[s[st:en]] = true
	}
	return out
}

// b64Variants are the base64 variants b64DecodeAny tries, in its order:
// padded at even places, raw (no padding) at odd ones.
var b64Variants = [...]*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}

// b64DecodeAny reports whether s decodes in any base64 variant, to text or
// not: what the first of b64Variants that decodes it gives. It does not try
// a variant whose length rule s breaks, which would fail: a padded variant
// decodes whole quanta only (a length a multiple of four), and a raw one no
// '=' and no last quantum of one character. The decoder skips '\r' and
// '\n', so s holding either is tried in every variant. On a payload of
// words that are no text (the HTTP bound row's), the variants tried and
// failed were most of b64DecodeAny's CPU.
func b64DecodeAny(s string) ([]byte, bool) {
	lines := strings.IndexByte(s, '\n') >= 0 || strings.IndexByte(s, '\r') >= 0
	padded := strings.IndexByte(s, '=') >= 0
	for i, enc := range b64Variants {
		if raw := i%2 == 1; !lines && (!raw && len(s)%4 != 0 || raw && (padded || len(s)%4 == 1)) {
			continue
		}
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// formsOf are the declared forms of a drifted value, recorded and live,
// beside the value as read raw: decoded (percent and backslash escapes),
// case-folded; quoted-printable (qpForm, each escape that decodes to text
// decoded), where it differs, itself case-folded; and the text its encoded
// runs decode to (b64Pair: b64Text of each side, a clock read in a decoded
// text not decoded again where both sides hold the value's instants at the
// same depth), itself case-folded; and that text, recorded and live, on its
// own (encoded). The words of each form are searched for what flowed into
// the value. qp is where forms holds the quoted-printable form and its
// case-folded one (forms[qp[0]:qp[1]], empty where it holds none): right
// after the value as written. The last encN forms are that text's (none
// where there is none), in forms and in checked alike, and runs the runs
// each side decoded to it (b64Pair). clockOf reads the clock words of a
// decoded text's two sides in the value's window.
//
// checked are the forms unparsed reads, the same but for the
// quoted-printable one: that is read only where both values read as
// quoted-printable text (qpFormRead). Reading it where one does not would
// make the two values differ in that form where they did not
// ("Signature=09bb..." a tab in one run, "Signature=b17d..." as written in
// the other; "v1=2B<hex>" a '+' against "v1=41<hex>" an 'A'), in a run of
// the base64 alphabet that does not decode, which unparsed refuses. The
// search reads it in every value: a word only that reading shows
// ("=23A7K29Q" a '#' and an id) is found where it flowed.
func formsOf(rec, live, decRec, decLive string, clock clockSet, clockOf func(rec, live string) clockSet) (forms, checked [][2]string, qp [2]int, encoded [2]string, encN int, runs [2][]decodedRun) {
	with := func(fs [][2]string, f [2]string) [][2]string {
		fs = append(fs, f)
		if lr, ll := strings.ToLower(f[0]), strings.ToLower(f[1]); lr != f[0] || ll != f[1] {
			fs = append(fs, [2]string{lr, ll})
		}
		return fs
	}
	base := with(nil, [2]string{decRec, decLive})
	forms = append([][2]string(nil), base...)
	checked = append([][2]string(nil), base...)
	qp = [2]int{len(forms), len(forms)}
	qr, qpR := qpFormRead(rec)
	ql, qpL := qpFormRead(live)
	if qr != decRec || ql != decLive {
		forms = with(forms, [2]string{qr, ql})
		qp[1] = len(forms)
		if qpR && qpL {
			checked = with(checked, [2]string{qr, ql})
		}
	}
	if br, bl, rs := b64Pair(decRec, decLive, clock, clockOf); br != "" || bl != "" {
		n := len(forms)
		encoded, runs = [2]string{br, bl}, rs
		forms = with(forms, encoded)
		checked = with(checked, encoded)
		encN = len(forms) - n
	}
	return forms, checked, qp, encoded, encN, runs
}

// plainRunsOf are the words, in every reading, as written and case-folded,
// of the runs the live side of a drifted value decoded whole where they
// stand (liveRuns: b64Pair's, at any level; eLive the text they decode to),
// that stand in no live form of the value (forms) once each such run is put
// out where it stands (blankAt): words only those runs hold there. The word
// rule reads such a run as the text it decodes to, which is a form of the
// value: every word of that text that changed is searched as any word of
// the value is, and a clock read in it is read as the clock it is. So where
// record's full round served a payload before the call whose view no
// adapter stated or keploy could not read, which may hold any word
// (unseenWhy, servedWhy), the run is not a word of its own: the encoded
// text gets the answer the same text unencoded gets, whether or not such a
// payload was served before the call, which another goroutine's serve
// decides. A run decodes whole to its text, so it holds no word its text
// does not: the bytes a respelling changes (its padding or unused trailing
// bits, the other alphabet, a hex run's case or its "0x") are none. A run
// the reader decoded only in parts (between '/', '.' or groups of '-' and
// '_'), one it did not decode where it stands again (a second copy, which
// its text does not show twice), and one in a form keploy cannot place it
// in (the value's quoted-printable reading, a case-folding that moves it)
// are put out nowhere, so their words are searched there as any other
// (fail closed). In a value with escapes, a run the reader read in its
// decoded form stands in the raw form as the escaped text that decodes to
// it (rawImages), and that text is put out there too. Its words there
// ("2FIGx" of "%2FIGx") are the run's only where it pairs with the recorded
// value's run of the same rank and escapes each byte as that one does
// (pairedRuns, sameEscapes), and the value's own otherwise. Where it does
// not, each of its escapes is decided where it stands: one written as the
// recorded run writes that copy of its byte is the run's, and any other (a
// gained "%3D" of its padding) leaves a word that is the value's own even
// where another run's kept escapes spell the same word (keptEscape). An
// escape before it is no part of it, whether it stands after the run
// before it or ends that run (its padding's "%3D" right before the run): a
// word it glues to the run ("20b2s" of "%20b2s", "3DbGVh" of "Lg=%3DbGVh")
// is the run's only where the run pairs and the same escape stands before
// the recorded one, and the value's own otherwise, changed where the escape
// did. Only those two searches skip these: a run that stands in an earlier
// payload, as recorded or in a view of a serve of it, is refused or
// followed there as any word is.
func (d *valueDrift) plainRunsOf(forms [][2]string, runs [2][]decodedRun, eLive string) map[string]bool {
	liveRuns := runs[1]
	if len(liveRuns) == 0 {
		return nil
	}
	var raw, recRaw []decodedRun
	if d.decoded {
		raw, recRaw = rawImages(d.live, liveRuns), rawImages(d.rec, runs[0])
	}
	words := map[string]bool{}
	add := func(run string) {
		for _, rd := range readings {
			maps.Copy(words, tokensBy(run, rd))
			maps.Copy(words, tokensBy(strings.ToLower(run), rd))
		}
	}
	for _, r := range liveRuns {
		add(r.run)
	}
	// A run's text in the raw value holds what its escapes leave ("3D" of
	// "%3D", "2FIGx" of "%2FIGx"): its words are the run's only where the
	// run pairs with the recorded value's run of the same rank (pairedRuns)
	// and writes each byte it escapes as that run writes it (sameEscapes),
	// so no escape was gained or respelled in it; else they are the
	// value's own, as an escape a plain text gained is.
	recTops, liveTops := topRuns(runs[0]), topRuns(liveRuns)
	paired := 0
	if raw != nil && recRaw != nil {
		paired = d.pairedRuns(recTops, liveTops)
	}
	for k := 0; k < paired; k++ {
		if sameEscapes(recRaw[k].esc, raw[k].esc, recTops[k].run) {
			add(raw[k].run)
		}
	}
	// The words are a set, so each escape is decided where it stands, not
	// by its remnant's spelling: an escape of a run that pairs, written as
	// the recorded run writes that copy of its byte (keptEscape: each
	// escape of a run that kept its escapes is), is the run's and put out
	// with it; every other one (gained or respelled, or one of a run that
	// pairs with none) is left standing in the raw form (own), so its
	// remnant ("3D" of a gained "%3D") is the value's own word and searched,
	// even where a run that kept its escapes spells the same word. An escape
	// the run lost leaves no remnant, and those it kept are the recorded
	// run's.
	var own [][2]int
	for k, r := range raw {
		for _, e := range r.escs {
			if k >= paired || !keptEscape(recRaw[k], recTops[k].run, e) {
				own = append(own, [2]int{e.st, e.en})
			}
		}
	}
	decLower, eLower := strings.ToLower(d.decLive), strings.ToLower(eLive)
	for _, f := range forms {
		b, rawForm := f[1], false
		switch {
		case d.decoded && b == d.live:
			b, rawForm = blankAt(b, raw, false), true
		case b == d.decLive || b == decLower:
			b = blankAt(b, liveRuns, false)
		case b == eLive || b == eLower:
			b = blankAt(b, liveRuns, true)
		}
		for _, rd := range readings {
			rb := b
			if rawForm {
				rb = standing(b, d.live, own, rd)
			}
			for w := range tokensBy(rb, rd) {
				delete(words, w)
			}
		}
	}
	// An escape right before a run glues its remnant to the run's first
	// word in the raw form ("20b2s" of "%20b2s"): that word is the runs'
	// where the run pairs with the recorded value's run of the same rank
	// and the same escape stands right before that one, so only the run
	// changed the word. So too where the escape ends the run before it (a
	// run ends after its padding: "Lg=%3D" right before the next): runs
	// pair only where the text between them is the same on both sides,
	// here none, so the recorded escape ends the recorded run before too.
	// The word holds the escape's remnant, the same on both sides, and the
	// run's own bytes, none of the run before it: an escape that run gained
	// or respelled leaves a word of its own ("3D" of "%3D%3D" against
	// "=%3D"), which is searched unless a run read above holds it, and one
	// it lost leaves none.
	for k := 0; k < paired; k++ {
		r, q := raw[k], recRaw[k]
		if e := escapeBefore(d.live, r.st); e != "" && escapeBefore(d.rec, q.st) == e {
			for _, rd := range readings {
				if w := wordAcross(d.live, r.st, rd); w != "" {
					words[w] = true
				}
			}
		}
	}
	return words
}

// topRuns are the runs of runs read in the value's decoded form itself
// (at -1), in order.
func topRuns(runs []decodedRun) []decodedRun {
	var out []decodedRun
	for _, r := range runs {
		if r.at < 0 {
			out = append(out, r)
		}
	}
	return out
}

// pairedRuns is how many of the decoded form's top-level runs, recorded
// (rec) and live, pair by rank, from the first: the text before each, from
// the run before it (or the value's start), is the same on both sides but
// for words both sides read as clock reads (sameButClock), so each is the
// other's run where it stands, whatever the clock wrote before it. The
// first that does not pair, and every run after it, pairs with none (fail
// closed: a run dropped or gained before it shifts the ranks).
func (d *valueDrift) pairedRuns(rec, live []decodedRun) int {
	pr, pl := 0, 0
	for k := 0; k < len(rec) && k < len(live); k++ {
		q, r := rec[k], live[k]
		if !d.sameButClock(d.decRec[pr:q.st], d.decLive[pl:r.st]) {
			return k
		}
		pr, pl = q.en, r.en
	}
	return min(len(rec), len(live))
}

// sameButClock says rec and live are the same text but for words (isWord)
// that are clock words of the value on each side, in the same places.
func (d *valueDrift) sameButClock(rec, live string) bool {
	if rec == live {
		return true
	}
	rs, ls := segmentsBy(rec, isWord), segmentsBy(live, isWord)
	if len(rs) != len(ls) {
		return false
	}
	for i := range rs {
		if rs[i] == ls[i] {
			continue
		}
		if !rs[i].word || !ls[i].word || !d.clock.rec[rs[i].text] || !d.clock.live[ls[i].text] {
			return false
		}
	}
	return true
}

// sameEscapes says a live run's raw text writes each byte it escapes
// (live, rawImages' esc) as the recorded run's does (rec; recRun the text
// that run decodes to): the one spelling all of its copies share, or the
// same spellings in order where a side mixes them. A byte the recorded run
// does not hold is written so where it is one spelling of the kind (the
// escape's first byte and its length, "%" and three of "%2F") by which the
// recorded run writes another byte, so the same encoder wrote both ("%2F"
// beside "%3D"); else nothing says how the recorded side writes it, and it
// is a change of its own (fail closed). A byte live writes as itself
// leaves no word of an escape, so it says nothing either way.
func sameEscapes(rec, live map[byte]string, recRun string) bool {
	for c, sp := range live {
		if strings.IndexByte(recRun, c) >= 0 {
			rs, ok := rec[c]
			if !ok {
				rs = string(c)
			}
			if rs != sp {
				return false
			}
			continue
		}
		kin := false
		for _, rs := range rec {
			kin = kin || !mixedEscape(sp) && rs[0] == sp[0] && len(rs) == len(sp)
		}
		if !kin {
			return false
		}
	}
	return true
}

// keptEscape says e, an escape of a live run's text in the raw value
// (rawImages), writes its copy of its byte as the recorded run of the same
// rank writes that copy (rec its text there, recRun what that decodes to):
// sameEscapes, read one escape at a time. That is the one spelling every
// recorded copy shares, or the spelling of the recorded copy of the same
// rank where they differ (none where the recorded run holds fewer copies,
// or writes that one as itself); for a byte the recorded run does not
// hold, an escape of the kind by which it writes another byte.
func keptEscape(rec decodedRun, recRun string, e rawEscape) bool {
	if strings.IndexByte(recRun, e.c) < 0 {
		for _, rs := range rec.esc {
			if !mixedEscape(rs) && rs[0] == e.sp[0] && len(rs) == len(e.sp) {
				return true
			}
		}
		return false
	}
	rs, ok := rec.esc[e.c]
	if !ok {
		return false
	}
	if !mixedEscape(rs) {
		return rs == e.sp
	}
	for _, q := range rec.escs {
		if q.c == e.c && q.n == e.n {
			return q.sp == e.sp
		}
	}
	return false
}

// standing is b, the raw value s with runs put out (blankAt), with each
// escape at a span of own written back where it stands, and with it the
// bytes of s its word, as word reads it, glues to after it: the word the
// escape leaves, "2FIGE" of "b2s%2FIGE+Yi" as "3D" of "=%3D%3D", is
// searched where the escape stands, even where a run that kept its escapes
// spells the same word. An escape starts with '%' or '\', which no
// reading takes for a word byte, so no word it leaves starts before it; one
// that ends in a byte that is no word byte ("\/") leaves no word.
func standing(b, s string, own [][2]int, word func(byte) bool) string {
	if len(own) == 0 {
		return b
	}
	out := []byte(b)
	for _, sp := range own {
		e := sp[1]
		for e < len(s) && word(s[e-1]) && word(s[e]) {
			e++
		}
		copy(out[sp[0]:e], s[sp[0]:e])
	}
	return string(out)
}

// mixedEscape says an esc spelling (rawImages) lists a byte's copies, in
// order, because they are not all written the same way.
func mixedEscape(sp string) bool { return sp[0] == 0 }

// escapeBefore is the escape that ends at i in s ("%20", "\\n"), "" where
// none does.
func escapeBefore(s string, i int) string {
	isHex := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	switch {
	case i >= 3 && s[i-3] == '%' && isHex(s[i-2]) && isHex(s[i-1]):
		return s[i-3 : i]
	case i >= 2 && s[i-2] == '\\' && strings.IndexByte("nrtbf", s[i-1]) >= 0:
		return s[i-2 : i]
	}
	return ""
}

// wordAcross is the word of s, as word reads it, that holds both s[i-1] and
// s[i], "" where none does.
func wordAcross(s string, i int, word func(byte) bool) string {
	if i == 0 || i >= len(s) || !word(s[i-1]) || !word(s[i]) {
		return ""
	}
	a, b := i-1, i
	for a > 0 && word(s[a-1]) {
		a--
	}
	for b < len(s) && word(s[b]) {
		b++
	}
	return s[a:b]
}

// rawImages are the top-level runs of the value's decoded form (runs at
// -1, read in unescaped(raw)), in order, each as the text of raw that
// decodes to it, where that stands in raw (unescapedAt's places of its
// ends), and how that text writes each byte it escapes somewhere (esc): the
// one spelling all its copies share ("%2F"), or, where they differ, each
// copy's in order after a 0 byte. A run's bytes are ASCII, each placed, so
// every run has such a text; the guard for one that does not (an escape's
// character split at an end) gives none at all, so no run pairs by rank
// (fail closed). It costs a pass over raw for each pass of unescaped.
func rawImages(raw string, runs []decodedRun) []decodedRun {
	dec, at := unescapedAt(raw)
	var out []decodedRun
	for _, r := range runs {
		if r.at >= 0 {
			continue
		}
		a, b := at[r.st], at[r.en]
		if a < 0 || b < 0 {
			return nil
		}
		// Each byte written whole: as itself (one byte) or by an escape
		// (more).
		var first [256]string
		var escaped, mixed [256]bool
		var copies [256]int
		var escs []rawEscape
		for j := r.st; j < r.en; j++ {
			c, sp := dec[j], raw[at[j]:at[j+1]]
			escaped[c] = escaped[c] || len(sp) > 1
			if first[c] == "" {
				first[c] = sp
			}
			mixed[c] = mixed[c] || sp != first[c]
			if len(sp) > 1 {
				escs = append(escs, rawEscape{c: c, sp: sp, st: at[j], en: at[j+1], n: copies[c]})
			}
			copies[c]++
		}
		var esc map[byte]string
		for c := range escaped {
			if !escaped[c] {
				continue
			}
			if esc == nil {
				esc = map[byte]string{}
			}
			if !mixed[c] {
				esc[byte(c)] = first[c]
				continue
			}
			sb := strings.Builder{}
			sb.WriteByte(0)
			for j := r.st; j < r.en; j++ {
				if dec[j] == byte(c) {
					sb.WriteByte(' ')
					sb.WriteString(raw[at[j]:at[j+1]])
				}
			}
			esc[byte(c)] = sb.String()
		}
		out = append(out, decodedRun{run: raw[a:b], at: -1, st: a, en: b, esc: esc, escs: escs})
	}
	return out
}

// blankAt is s, the text the runs were read in (deep: the text they decode
// to; else the value's decoded form), or that text case-folded, with each
// run that stands there where the reader read it, as written or
// case-folded, put out: a space for each of its bytes, so the others keep
// their places.
func blankAt(s string, runs []decodedRun, deep bool) string {
	var b []byte
	for _, r := range runs {
		if (r.at >= 0) != deep {
			continue
		}
		st, en := r.st, r.en
		if deep {
			st, en = r.at+r.st, r.at+r.en
		}
		if en > len(s) {
			continue
		}
		if w := s[st:en]; w != r.run && w != strings.ToLower(r.run) {
			continue
		}
		if b == nil {
			b = []byte(s)
		}
		for i := st; i < en; i++ {
			b[i] = ' '
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// allIn reports whether every word of ws is in set, and there is one.
func allIn(ws, set map[string]bool) bool {
	if len(ws) == 0 {
		return false
	}
	for w := range ws {
		if !set[w] {
			return false
		}
	}
	return true
}

// textForms are the declared forms of one text (a payload): as recorded,
// decoded, case-folded, and its base64 runs' text.
func textForms(text string) []string { return decodeForms(text).formsOf(text) }

// decoded are what textForms decodes of a text, each a scan of its own
// (unescaped, qpForm, b64Text: the learn step's dearest reads of a
// payload): its escapes read (dec, where they change it: hasDec), its
// quoted-printable form (q, where it is not dec: hasQ) and its encoded
// runs' text (b). Its other forms are their case-foldings, a pass over each
// that reads no pattern (formsOf). A set keeps a payload's (decodedOf), so
// each is decoded once for the set, not once per call.
type decoded struct {
	dec, q, b    string
	hasDec, hasQ bool
}

// decodeForms decodes text's forms (decoded).
var decodeForms = func(text string) decoded {
	var d decoded
	dec := unescaped(text)
	if dec != text {
		d.dec, d.hasDec = dec, true
	}
	if q := qpForm(text); q != dec {
		d.q, d.hasQ = q, true
	}
	d.b = b64Text(dec, nil)
	return d
}

// formsOf are textForms(text) of the text d decodes.
func (d decoded) formsOf(text string) []string {
	out := []string{text}
	dec := text
	if d.hasDec {
		dec = d.dec
		out = append(out, dec)
	}
	if low := strings.ToLower(dec); low != dec {
		out = append(out, low)
	}
	if d.hasQ {
		out = append(out, d.q)
		if low := strings.ToLower(d.q); low != d.q {
			out = append(out, low)
		}
	}
	if d.b != "" {
		out = append(out, d.b)
		if low := strings.ToLower(d.b); low != d.b {
			out = append(out, low)
		}
	}
	return out
}

// extraOf is extraOf of the text d decodes.
func (d decoded) extraOf(text string) string { return strings.Join(d.formsOf(text)[1:], "\n") }

// size is about the bytes d holds.
func (d decoded) size() int64 { return headerBytes + int64(len(d.dec)+len(d.q)+len(d.b)) }

// clone is d, each text a copy, holding no part of the text it decodes.
func (d decoded) clone() decoded {
	d.dec, d.q, d.b = strings.Clone(d.dec), strings.Clone(d.q), strings.Clone(d.b)
	return d
}

// nonASCIIRuns are the stretches of s beyond ASCII, in order: text of a
// script keploy reads only whole.
func nonASCIIRuns(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if s[i] < 0x80 {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] >= 0x80 {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// unaccounted says why a drifted value no earlier place is the source of
// (v) cannot be learned as noise either, "" when it can: what its readings
// cannot read (unparsed), or a word of what changed, in a declared form and
// reading, that is no part of a clock read the lead moved and that appears
// earlier in the test (at an earlier place, the test case's own request
// among them: in its recorded value for a word v held when recorded, its
// live value for one v holds now; in any payload served or recorded earlier,
// either way). The words of what changed are
// those v lost and gained, and every word of its runs (runsBy) in that
// reading: ids it holds in another order, or once more, are words of what
// changed too; a word at its own place on both sides is not. keploy cannot
// tell whether such a word came from there, and refuses rather than learn
// it as noise. In a test case's request body that declares no media type,
// read as text by its bytes (Seen.Undeclared), such a word glued to the
// bytes around it ("s17" of "BAs17") is refused too: keploy cannot tell a
// protobuf's printable tag and length from a word, and never reads such a
// body more loosely than the same bytes declared, which it cannot read.
//
// And an instant of what changed whose moment F saw is one an earlier place
// holds (echoedIn: a pick from served data, by the clock), while no earlier
// place holds the moment it was recorded with (else it is a variable of that
// place, appearedIn), is refused before any lead or window excuses its words:
// what F saw came from what F served, whatever moved it.
func unaccounted(dc *DriftCache, v *valueDrift, at time.Time, served []Earlier, raw []Raw, fol *follows, eh *earlierHeld) string {
	if v.unparsed != "" {
		return v.unparsed
	}
	if echoed := dc.echoedIn(v, at, served, raw, eh); len(echoed) > 0 {
		where := dc.heldAt(echoed, at, served, raw)
		if where == "" {
			where = "earlier in the test"
		}
		return pickUnaccounted(where)
	}
	keep := dc.stampsEchoed(v, served, raw)
	word := v.wordOf
	held := dc.wordsHeld(v, keep, raw)
	return v.eachChanged(func(i int, t string, recorded bool) string {
		for _, e := range served {
			// The run's exchange (Earlier.Run) is an earlier place of every
			// test of the set: the set's start-up's, or an earlier test's
			// call (not StartUp). The test case's own
			// request (Earlier.Given) is the test's earliest place. Another
			// exchange of the start-up (Earlier.StartUp) is an earlier place
			// of a call made at the start-up too, when no test runs.
			where := "earlier in the test"
			switch {
			case e.Run && e.StartUp:
				where = "in the test set's start-up"
			case e.Run:
				where = "in an earlier test of the set"
			case e.Given:
				where = "in what the test case gave the application"
			case e.StartUp:
				where = "earlier in the recording"
			}
			for _, s := range e.Seen {
				// An answer's own clock stamp is no place a value came
				// from (Seen.Stamp), unless a stamp F served holds a word
				// the value took in F (stampsEchoed).
				if stamped(s) && !keep {
					continue
				}
				// An instant its protocol types has no words: its moments
				// are the echo's and appearedIn's to read.
				if s.Leaf.Type == Instant {
					continue
				}
				ed := dc.of(s.Leaf.Value, s.Live)
				if recorded && ed.recTok[i][t] || !recorded && ed.liveTok[i][t] {
					why := word(t, recorded) + " appears " + where + ", in " + e.Mock + " at " + s.Leaf.Path + ", which did not drift with it: keploy cannot tell whether it came from there; " + notClock(v) + ". " + correction(e.Mock, false)
					if fol.excusedSeen(e, ed, v, i, t, at, why, s.Leaf.Path) {
						continue
					}
					return why
				}
				if s.Undeclared && gluedIn(ed, t, recorded) {
					return word(t, recorded) + " appears " + where + ", in " + e.Mock + " at " + s.Leaf.Path + ", glued to the bytes around it, in a body that declares no media type (no Content-Type): keploy cannot tell the words the application was given there from the bytes that frame them, so it cannot tell whether it came from there; " + notClock(v) + ". " + correction(e.Mock, false)
				}
			}
		}
		for j, x := range raw {
			if !x.earlierThan(at) {
				continue
			}
			// A protocol-phase answer's own clock reads blanked
			// (instants.go, rule 2).
			if held.of(j).raw[changedWord{i, t}] && !insideAnotherID(v, x, dc.searched(x, keep), t, recorded) {
				note := x.atNote(at)
				place := x.mock + "'s payload"
				if f, _ := dc.fieldOf(x, dc.searched(x, keep), t); f != "" {
					place = x.mock + " at " + f
				}
				place += note
				why := word(t, recorded) + " appears in an earlier recording, in " + place + ": keploy cannot tell whether it came from there; " + notClock(v) + ". " + correction(x.mock, !x.served)
				if fol.excusedRaw(x, v, i, t, at, why, place) {
					continue
				}
				return why
			}
			// A word the value holds now that stands in what F gave the
			// application in a payload it served changed (ServedLive), and
			// not in what the recording gave there: F gave it, the
			// recording did not, as at a leaf whose recorded text lacks it.
			// Under the rule a copy follows its source, a word F gave at
			// fields where the recording gave another value, which the
			// recorded value took none of the words of, is let go
			// (follows.excusedLive).
			if !recorded && x.Live() && held.of(j).live[changedWord{i, t}] {
				place := x.mock + "'s payload, as record's full round gave it"
				if f, _ := dc.fieldOf(*x.live, dc.searched(*x.live, keep), t); f != "" {
					place = x.mock + " at " + f + ", as record's full round gave it"
				}
				why := word(t, recorded) + " appears in an earlier recording, in " + place + x.atNote(at) + ", where the recording did not give the application that word: keploy cannot tell whether it came from there; " + notClock(v) + ". " + correction(x.mock, false)
				if fol.excusedLive(x, v, i, t, at, why, place) {
					continue
				}
				return why
			}
			// A word the value holds now, where F served x changed in ways
			// keploy keeps no one view of (ServedChanged): one that stands
			// in a view of a serve of it, and not in what the recording gave
			// there, or any word where a serve's view could not be read. F
			// may have given it there, and keploy cannot tell which view the
			// application read: no rule lets it go. So is any word where F
			// served x and its adapter stated no view of it (viewUnstated):
			// keploy cannot tell what F gave the application there either.
			// A word of an encoded run read as its text is (plainRuns) is
			// no word F may have given there unread, as the same text
			// unencoded holds none: only a view that holds it refuses it.
			if !recorded && (x.unseen || x.viewUnstated()) && (!v.plainRuns[t] || len(x.views) != 0) {
				if why := unseenWhy(dc, x, held.of(j).unseen[changedWord{i, t}], keep, word(t, recorded), t, at); why != "" {
					return why + "; " + notClock(v) + ". " + correction(x.mock, false)
				}
			}
		}
		return ""
	})
}

// unseenWhy says where a word t the value holds now (named), which F may
// have given the application in x, a recording it served changed in ways
// keploy keeps no one view of (Raw.ServedChanged), stands: in a view of
// one of its serves (held), or, where a serve's view could not be read, or
// x's adapter stated none (Raw.viewUnstated), in what F gave there unread.
// "" where neither: every view was read, and none holds the word.
func unseenWhy(dc *DriftCache, x Raw, held, keep bool, named, t string, at time.Time) string {
	if x.viewUnstated() {
		return named + " may stand in " + x.mock + x.atNote(at) + ", an earlier recording record's full round served, where its adapter did not say what it gave the application there: keploy cannot tell whether it came from there"
	}
	if len(x.views) == 0 {
		return named + " may stand in " + x.mock + x.atNote(at) + ", an earlier recording record's full round served changed, where keploy could not read what it gave the application: keploy cannot tell whether it came from there"
	}
	if !held {
		return ""
	}
	place := x.mock + "'s payload"
	for _, view := range x.views {
		if f, _ := dc.fieldOf(*view, dc.searched(*view, keep), t); f != "" {
			place = x.mock + " at " + f
			break
		}
	}
	return named + " appears in an earlier recording, in " + place + x.atNote(at) + ", as record's full round gave it in one of " + strconv.Itoa(len(x.views)) + " serves that gave the application different views of it, where the recording did not give the application that word: keploy cannot tell which of them the application read, nor whether it came from there"
}

// givenWhy says why a drifted value (v) that no earlier place of the call's
// own test accounts for (unaccounted) cannot be learned as noise either, ""
// when it can: a word of what it changed (eachChanged) stands in the answer
// of a recording F gave the application before the call that no earlier
// place of its own test is (another recorded test's, or one F gave earlier
// in the running set: places().words, read only where a word of what
// changed asks): a word v holds now in any of them; a word it held when
// recorded in one recorded before the call (at), which the recording gave
// the application before it too. keploy follows no value of another test's
// (only a test's own calls carry its variables), nor of a recording made
// after the call that F gave the application before it (the recording
// could not have read it there), so a value copied word for word from what
// F gave there is refused, never noise, naming where and which of the two
// it is; and so is every such word where one of them could not be read
// (unread). Stamps are searched as any text where the call's own places say
// so (stampsEchoed). Under the rule a copy follows its source (fol:
// ClassifyFollowing), a word the recording and F both gave the application
// at such a place before the call, which one of the two values lacks, is no
// word the value copied there and is let go (follows.excusedGiven): a group
// of a random id that another test's random id holds by chance.
func givenWhy(dc *DriftCache, v *valueDrift, at time.Time, served []Earlier, raw []Raw, places func() *offsetPlaces, fol *follows) string {
	keep, kept := false, false
	return v.eachChanged(func(i int, t string, recorded bool) string {
		op := places()
		if op.unread != "" {
			return op.unread
		}
		for _, g := range op.words {
			// A word the value was recorded with, in a recording made
			// after the call, came from none: the recording could not have
			// read it there. One made at the call's own instant may have
			// been made before it, the fail-closed side.
			if recorded && g.at.After(at) {
				continue
			}
			if !kept {
				keep, kept = dc.stampsEchoed(v, served, raw), true
			}
			if g.holds(i, t, keep) {
				why := v.wordOf(t, recorded) + " appears in " + g.mock + ", a recording record's full round gave the application before the call that " + givenWhose(g.at, at, g.late, g.noTime) + ", so it cannot tell whether it came from there; " + notClock(v) + ". " + correction(g.mock, false)
				if fol.excusedGiven(g, v, i, t, at, why) {
					continue
				}
				return why
			}
		}
		// A word the value holds now that F may have given the application
		// at one of them and the recording did not (servedWhy): no rule
		// lets it go. Read after every recording's own words, so that a
		// word found where the recording gave it is named there; one that
		// reached here past a recording holding it was let go there by the
		// rule a copy follows its source, which reads F's view of it too
		// (follows.excusedGiven), as a place of the call's own test lets it
		// go past that view (unaccounted). The first loop read keep for
		// every word the value holds now, wherever there is a recording.
		if recorded {
			return ""
		}
		for _, g := range op.words {
			// A word of an encoded run read as its text is (plainRuns), as
			// in unaccounted.
			if v.plainRuns[t] && (g.unstated || g.unread) {
				continue
			}
			if why := g.servedWhy(i, t, v.wordOf(t, recorded), givenWhose(g.at, at, g.late, g.noTime), keep); why != "" {
				return why + "; " + notClock(v) + ". " + correction(g.mock, false)
			}
		}
		return ""
	})
}

// givenWhose says whose a recording F gave the application before a call
// recorded at at is, that no earlier place of the call's own test is
// (givenWhy, echoedElsewhere), its request recorded at gat: another test's;
// recorded after the call, no earlier place of any test's call; recorded at
// the call's own instant, one keploy reads as given to the application
// before the call (the fail-closed side: it may have been made before it,
// or after it), yet no earlier place of the call's own test. late, an
// earlier place of the call's own test made at the call's own instant that
// is read as one made after the call too (Raw.late, classifyGiven): it may
// as well have been made after the call, so keploy reads it both ways, and
// as one made after the call follows no value of it but what record's full
// round served there.
func givenWhose(gat, at time.Time, late, noTime bool) string {
	switch {
	case late && noTime:
		return "was " + noTimeWords + ", yet may as well have been made after the call, so keploy reads it both ways: as one made after the call, keploy follows no value of it but what record's full round served there"
	case late:
		return "was " + atCallWords + ", yet may as well have been made after the call, so keploy reads it both ways: as one made after the call, keploy follows no value of it but what record's full round served there"
	case gat.Equal(at):
		return "was " + atCallWords + ", yet as no earlier place of the call's own test: keploy follows no value of it"
	case gat.After(at):
		return "was recorded after the call, so it is no earlier place of the call's own test: keploy follows no value of a recording made after the call"
	}
	return "is another test's (another recorded test's, or one of an earlier test of the set): keploy follows no value of another test's"
}

// wordOf names t, a word of v's recorded value (recorded) or of what F saw,
// by where it stands, never by its text.
func (v *valueDrift) wordOf(t string, recorded bool) string {
	if recorded {
		return "a word of its recorded value (" + partOf(v.rec, t) + ")"
	}
	return "a word of what record's full round saw in it (" + partOf(v.live, t) + ")"
}

// eachChanged hands find each word of what v changed, in each reading (i),
// as recorded or as F saw it (recorded), until find says why one cannot be
// noise, and says that ("" when it says so of none): the words it lost,
// those it gained, then every word of its runs (runsBy) as recorded and as
// F saw them. A word of a run counts unless it is a clock word itself; a
// word lost or gained, unless it only regrouped words the other side holds
// (clockWord). Each set of words is handed in the order its words stand in
// the value (inOrder), never in a set's: the first one found names the
// place and the reason.
// changedWord is a word of what changed (eachChanged): its reading and its
// text.
type changedWord struct {
	i int
	t string
}

// heldWords are which of a value's changed words each earlier payload
// holds (searchedTokens), by the payload's index, each payload's read once,
// where unaccounted first asks of it.
type heldWords struct {
	of func(j int) wordsIn
}

// wordsIn are the changed words a payload holds, as recorded (raw), as F
// gave it (live: its ServedLive view), and as any of the views F's serves
// of it gave where they differ (unseen: its ServedChanged views).
type wordsIn struct{ raw, live, unseen map[changedWord]bool }

// wordsHeld are which of v's changed words each of raw holds, searched as
// keep says (stampsEchoed). Each payload's words are read once for the
// value, not once for each of its changed words: the set keeps them within
// a budget (setDerived), and a payload let go would be read again at each
// word of each reading, its cost per call growing with the value's words.
func (dc *DriftCache) wordsHeld(v *valueDrift, keep bool, raw []Raw) heldWords {
	var words []changedWord
	seen := map[changedWord]bool{}
	v.eachChanged(func(i int, t string, _ bool) string {
		if w := (changedWord{i, t}); !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
		return ""
	})
	in := func(toks []map[string]bool) map[changedWord]bool {
		out := map[changedWord]bool{}
		for _, w := range words {
			if toks[w.i][w.t] {
				out[w] = true
			}
		}
		return out
	}
	// A recording whose filter of words, kept for the set, holds no run of
	// letters and digits of any changed word (mayHoldWords) holds none of
	// them: its words are not read. A set's words are more than its budget
	// keeps (api-server's round: 14 813 recordings, 335 MB of payloads), and
	// each call read every earlier recording's words anew, holding them
	// while it read.
	runs := make([][]string, len(words))
	for k, w := range words {
		runs[k] = wordsBy(w.t, isWord)
	}
	read := make([]*wordsIn, len(raw))
	return heldWords{of: func(j int) wordsIn {
		if read[j] == nil {
			x := raw[j]
			var h wordsIn
			if dc.mayHoldWords(x, keep, runs) {
				h.raw = in(dc.searchedTokens(x, keep))
			}
			if x.Live() {
				h.live = in(dc.searchedTokens(*x.live, keep))
			}
			if len(x.views) > 0 {
				h.unseen = map[changedWord]bool{}
				for _, view := range x.views {
					for w := range in(dc.searchedTokens(*view, keep)) {
						h.unseen[w] = true
					}
				}
			}
			read[j] = &h
		}
		return *read[j]
	}}
}

func (v *valueDrift) eachChanged(find func(i int, t string, recorded bool) string) string {
	for i := range readings {
		for _, set := range []struct {
			words           map[string]bool
			recorded, moved bool
		}{{v.lost[i], true, false}, {v.gained[i], false, false}, {v.movedRec[i], true, true}, {v.movedLive[i], false, true}} {
			for _, t := range v.inOrder(set.words, set.recorded) {
				if v.clockChanged(t, set.recorded, set.moved) {
					continue
				}
				if why := find(i, t, set.recorded); why != "" {
					return why
				}
			}
		}
	}
	return ""
}

// gluedIn reports whether a declared form of a place's value (ed), as
// recorded or as it stands now, holds t anywhere: inside a longer word too,
// glued to the bytes around it.
func gluedIn(ed *valueDrift, t string, recorded bool) bool {
	side := 1
	if recorded {
		side = 0
	}
	for _, f := range ed.forms {
		if strings.Contains(f[side], t) {
			return true
		}
	}
	return false
}

// pickUnaccounted is why a drifted value whose moment in record's full round
// stands at where (earlier in the test), and whose recorded moment no earlier
// place holds, is refused (unaccounted).
func pickUnaccounted(where string) string {
	return "the moment record's full round saw in it stands " + where + ", while no earlier place holds the moment it was recorded with: it may be a pick from what the application was given, by its clock, and keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// clockChanged reports whether t, a word of what v changed (eachChanged;
// moved: one of its runs' words, movedWords), recorded or live, changed
// only with the clock, so the word rule searches no place for it. A word
// lost or gained is one every ASCII word of which the other side lacks is a
// clock word (clockWord). A word of its runs is one every ASCII word of
// which is a clock word (allClock), since its words may all stand on the
// other side in another grouping ("abc-def", then "abc_def").
//
// In the reading from each run's test begin alone (Window.begun), it is
// every word of its runs: the id "stop-<UnixNano>" of a coarse reading
// (idByte), a test's own name read on the clock, is no word F may have
// given the application where the finer readings' "<UnixNano>" is none.
// There the clock evidence comes before the word rule's search of what F
// gave there (unseenWhy, servedWhy) for a word inside a text, as it does
// for a value that is one read whole. That reading's noise is taken only by
// sinceWord, as a checked read of one word, and only where exactly one
// instant of the text changed and every other byte of it is the same in
// both runs (clockWordIn), the word read alone with every rule of a value:
// a word whose other bytes changed too, or that holds two changed reads, is
// refused there as in the call's window. In a call's own window the word
// rule searches for such a word as it does for any other, so one F served
// unstated may hold there is refused, never learned as noise.
func (v *valueDrift) clockChanged(t string, recorded, moved bool) bool {
	if !moved {
		return clockWord(t, recorded, v, v.clock)
	}
	return v.begun || allClock(t, recorded, v.clock)
}

// allClock reports whether every ASCII word of t is a clock word of c, on
// its recorded side or its live one, and t has one.
func allClock(t string, recorded bool, c clockSet) bool {
	ws := tokensBy(t, isWord)
	if len(ws) == 0 {
		return false
	}
	for w := range ws {
		if recorded && !c.rec[w] || !recorded && !c.live[w] {
			return false
		}
	}
	return true
}
