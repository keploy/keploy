package learn

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"sort"
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
//     \\ as \, and \n \r \t \b \f \" \/ as a space, a separator), again
//     while that changes anything, up to four levels (unescaped);
//   - case-folded: the decoded form in lower case;
//   - quoted-printable: its soft line breaks taken out and its "=XX"
//     escapes decoded, where they decode to text (qpDecoded), and that in
//     lower case;
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

// b64Run is a run of the base64 alphabets, standard or URL-safe, with its
// padding (base32's too: up to six '=').
var b64Run = regexp.MustCompile(`[A-Za-z0-9+/_-]+={0,6}`)

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
func b64Text(s string, clock map[string]bool) string {
	var sb strings.Builder
	seen := map[string]bool{}
	level := []string{s}
	for depth := 0; depth < maxDecodeLevels && len(level) > 0; depth++ {
		var next []string
		try := func(c string) bool {
			if len(c) < minEncodedRun || seen[c] || allIn(tokensBy(c, isWord), clock) {
				return false
			}
			seen[c] = true
			t, ok := decodeText(c)
			if !ok {
				return false
			}
			raw := t
			t = unescaped(raw)
			if q := qpForm(raw); q != t {
				t += "\n" + q
			}
			sb.WriteString(t)
			sb.WriteByte('\n')
			next = append(next, t)
			return true
		}
		tryRun := func(run string) bool {
			whole := try(run)
			for _, part := range strings.FieldsFunc(run, func(r rune) bool { return r == '/' || r == '.' }) {
				try(part)
			}
			if h, ok := strings.CutPrefix(run, "0x"); ok {
				try(h)
			} else if h, ok := strings.CutPrefix(run, "0X"); ok {
				try(h)
			}
			if whole {
				return true
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
		for _, text := range level {
			texts := []string{text}
			j, joins := joinWrapped(text)
			if j != text {
				texts = append(texts, j)
			}
			for ti, tx := range texts {
				for _, ix := range b64Run.FindAllStringIndex(tx, -1) {
					if tryRun(tx[ix[0]:ix[1]]) || ti == 0 {
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
				for _, part := range strings.FieldsFunc(tx, func(r rune) bool { return r == '.' }) {
					if b64Run.FindString(part) == part {
						try(part)
					}
				}
			}
		}
		level = next
	}
	return sb.String()
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
func joinWrapped(s string) (string, []int) {
	var b strings.Builder
	var joins []int
	line, glued := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		if b64Byte(c) {
			b.WriteByte(c)
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
			b.WriteByte(c)
			line, glued = 0, 0
			i++
			continue
		}
		if line >= 16 && (line%4 == 0 || glued >= 16 && glued%4 == 0) && j < len(s) && b64Byte(s[j]) {
			line, glued = 0, 0
			i = j
			joins = append(joins, b.Len())
			continue
		}
		b.WriteString(s[i:j])
		line, glued = 0, 0
		i = j
	}
	return b.String(), joins
}

// qpDecoded is s read as quoted-printable (RFC 2045: a mail body, a MIME
// part): its soft line breaks ("=\r\n", "=\n") taken out, and each run of
// "=XX" escapes (upper-case hex) that decodes to text decoded, but to no '%'
// or '\' (which the readings would take for escapes keploy cannot read).
// A value with no such escape is itself.
func qpDecoded(s string) string {
	if !strings.Contains(s, "=") {
		return s
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "=\r\n", ""), "=\n", "")
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		var bytes []byte
		for j+2 < len(s) && s[j] == '=' && upperHex(s[j+1]) && upperHex(s[j+2]) {
			v, _ := hex.DecodeString(s[j+1 : j+3])
			bytes = append(bytes, v[0])
			j += 3
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		if isText(bytes) && !strings.ContainsAny(string(bytes), "%\\") {
			b.Write(bytes)
		} else {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// qpForm is s read as quoted-printable (qpDecoded) after its escapes
// (unescaped), with its soft line breaks taken out first, in their escaped
// spellings too ("=\\r\\n" of a mail in a JSON string), which unescaping
// would turn into separators.
func qpForm(s string) string {
	if strings.Contains(s, "=") {
		s = qpSoftBreaks.ReplaceAllString(s, "")
	}
	return qpDecoded(unescaped(s))
}

// qpSoftBreaks are a soft line break in any spelling: raw, or escaped once
// or more ("=\\r\\n", "=\\\\r\\\\n" of a mail in a JSON string in a JSON string).
var qpSoftBreaks = regexp.MustCompile(`=(?:\r?\n|(?:\\+r)?\\+n)`)

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
	if hexRun.MatchString(c) && len(c)%2 == 0 {
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
	if !b32Run.MatchString(s) && !b32Lower.MatchString(s) {
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

var (
	hexRun   = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	b32Run   = regexp.MustCompile(`^[A-Z2-7]+=*$`)
	b32Lower = regexp.MustCompile(`^[a-z2-7]+=*$`)
)

// b64Decode decodes s in any base64 variant to text: valid UTF-8 with no
// control characters but whitespace, its padding read leniently. Binary (a
// signature, a hash) is no text, and nothing is read in it.
func b64Decode(s string) (string, bool) {
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

// unparsed says what of a drifted value, read in form (recorded, live),
// the declared readings cannot read whole, where it drifted: "" when
// nothing. clock are the value's clock words: a clock read the lead moved
// ("41+00:00" of an aware datetime) is read, whatever runs it makes.
func unparsed(rec, live string, clock clockSet) string {
	if rec == live {
		return ""
	}
	if strings.ContainsAny(rec, `%\`) || strings.ContainsAny(live, `%\`) {
		return "it holds a '%' or '\\' that is no escape keploy reads"
	}
	if !sameStrings(nonASCIIRuns(rec), nonASCIIRuns(live)) {
		return "it changed in text of a script keploy does not split into words"
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
	for _, r := range b64Run.FindAllString(s, -1) {
		out[r] = true
	}
	return out
}

// b64DecodeAny reports whether s decodes in any base64 variant, to text or
// not.
func b64DecodeAny(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// formsOf are the declared forms of a drifted value, recorded and live,
// beside the value as read raw: decoded (percent and backslash escapes),
// case-folded, and the text its encoded runs decode to (b64Text), itself
// case-folded; and that text, recorded and live, on its own (encoded).
func formsOf(rec, live, decRec, decLive string, clock clockSet) (forms [][2]string, encoded [2]string) {
	forms = [][2]string{{decRec, decLive}}
	fold := func(f [2]string) {
		if lr, ll := strings.ToLower(f[0]), strings.ToLower(f[1]); lr != f[0] || ll != f[1] {
			forms = append(forms, [2]string{lr, ll})
		}
	}
	fold(forms[0])
	if qr, ql := qpForm(rec), qpForm(live); qr != decRec || ql != decLive {
		q := [2]string{qr, ql}
		forms = append(forms, q)
		fold(q)
	}
	if br, bl := b64Text(decRec, clock.rec), b64Text(decLive, clock.live); br != "" || bl != "" {
		encoded = [2]string{br, bl}
		forms = append(forms, encoded)
		fold(encoded)
	}
	return forms, encoded
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
func textForms(text string) []string {
	out := []string{text}
	dec := unescaped(text)
	if dec != text {
		out = append(out, dec)
	}
	if low := strings.ToLower(dec); low != dec {
		out = append(out, low)
	}
	if q := qpForm(text); q != dec {
		out = append(out, q)
		if low := strings.ToLower(q); low != q {
			out = append(out, low)
		}
	}
	if b := b64Text(dec, nil); b != "" {
		out = append(out, b)
		if low := strings.ToLower(b); low != b {
			out = append(out, low)
		}
	}
	return out
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
func unaccounted(dc *DriftCache, v *valueDrift, at time.Time, served []Earlier, raw []Raw, fol *follows) string {
	if v.unparsed != "" {
		return v.unparsed
	}
	if echoed := dc.echoedIn(v, at, served, raw); len(echoed) > 0 {
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
			// test of the set: the set's start-up's. The test case's own
			// request (Earlier.Given) is the test's earliest place. Another
			// exchange of the start-up (Earlier.StartUp) is an earlier place
			// of a call made at the start-up too, when no test runs.
			where := "earlier in the test"
			switch {
			case e.Run:
				where = "in the test set's start-up"
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
			if held.of(j).raw[changedWord{i, t}] {
				note := x.atNote(at)
				place := x.mock + "'s payload"
				if f := dc.fieldOf(x, dc.searched(x, keep), t); f != "" {
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
				if f := dc.fieldOf(*x.live, dc.searched(*x.live, keep), t); f != "" {
					place = x.mock + " at " + f + ", as record's full round gave it"
				}
				why := word(t, recorded) + " appears in an earlier recording, in " + place + x.atNote(at) + ", where the recording did not give the application that word: keploy cannot tell whether it came from there; " + notClock(v) + ". " + correction(x.mock, false)
				if fol.excusedLive(x, v, i, t, at, why, place) {
					continue
				}
				return why
			}
		}
		return ""
	})
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

// wordsIn are the changed words a payload holds, as recorded (raw) and as
// F gave it (live: its ServedLive view).
type wordsIn struct{ raw, live map[changedWord]bool }

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
	read := make([]*wordsIn, len(raw))
	return heldWords{of: func(j int) wordsIn {
		if read[j] == nil {
			x := raw[j]
			h := wordsIn{raw: in(dc.searchedTokens(x, keep))}
			if x.Live() {
				h.live = in(dc.searchedTokens(*x.live, keep))
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
				if set.moved && allClock(t, set.recorded, v.clock) || !set.moved && clockWord(t, set.recorded, v, v.clock) {
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
