package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// Word-level learn: of a drifted leaf, record's full round learns only the
// words that changed (models.WordsEntry), and a later replay keeps checking
// the rest.
//
// A value is read as segments, split at each run of structural characters
// (whitespace and " ' ` { } [ ] ( ) : , ; & < > | \ = / ? # +: a closed
// lexical class, the delimiters of the grammars keploy declares, applied byte
// by byte, with no threshold and no inference of meaning), and each segment
// as runs, split at every other byte that is neither a letter nor a digit
// (the other separators: - _ . @ ~ ! % * $ ^ and the like) and wherever the
// character class changes. The classes are Latin (ASCII letters and digits,
// and the letters of the Latin script), one class per other script, Common
// (ー, µ, fullwidth digits) and Inherited, which joins the class before it
// (as a combining mark joins its letter). An accepted clock read collapses
// first, on both sides, into one segment and one run, and counts as one run
// in every decision. No value's shape is read: what may change is what F saw
// change.
//
// Of each drifted value (recorded R, record's full round F):
//
//   - a value whose segments changed (another number of them, other
//     structural characters between them, its clock reads elsewhere) is
//     refused;
//   - a segment whose runs stand alike (as many, of one class each, the same
//     separators between) learns each run that changed as a slot of its own
//     (models.WordSlotRun): a later replay lets only a run of ASCII letters
//     and digits stand there, its neighbouring separators fixed, or, of a
//     run alone in its segment, any ASCII token (N1);
//   - a segment whose runs drifted, ASCII on both sides, is one token slot
//     (models.WordSlotToken): any ASCII token may stand there (N1);
//   - an accepted clock read is one instant slot (models.WordSlotInstant),
//     by its segment, its layout and its skeleton;
//   - anything else is refused.
//
// Each refusal names the part, never a value.

// structural reports whether c is one of the structural characters: the
// characters a value's segments are split at.
func structural(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v', '"', '\'', '`', '{', '}', '[', ']', '(', ')', ':', ',', ';', '&', '<', '>', '|', '\\', '=', '/', '?', '#', '+':
		return true
	}
	return false
}

// structuralSet is the structural characters, as the layout hash names them.
const structuralSet = " \t\n\r\f\v\"'`{}[]():,;&<>|\\=/?#+"

// The run classes that are no script's own name.
const (
	classLatin  = "Latin"
	classCommon = "Common"
)

// scriptNames are unicode's scripts by name, in order: a rune's class is the
// one script that holds it, found the same way on every run.
var scriptNames = func() []string {
	out := make([]string, 0, len(unicode.Scripts))
	for name := range unicode.Scripts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}()

// runeClass is the class of r within a segment: word reports whether r is a
// letter or a digit (a run's), joins whether it joins the class before it (a
// mark: Inherited), and class names its class.
func runeClass(r rune) (class string, word, joins bool) {
	if r < utf8.RuneSelf {
		if isWord(byte(r)) {
			return classLatin, true, false
		}
		return "", false, false
	}
	if unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me) || unicode.Is(unicode.Inherited, r) {
		return "", false, true
	}
	if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return "", false, false
	}
	switch {
	case unicode.Is(unicode.Latin, r):
		return classLatin, true, false
	case unicode.Is(unicode.Common, r):
		return classCommon, true, false
	}
	for _, name := range scriptNames {
		if unicode.Is(unicode.Scripts[name], r) {
			return name, true, false
		}
	}
	return "Unknown", true, false
}

// wrun is one run of a segment: [from, to) of the value, and its class.
type wrun struct {
	from, to int
	class    string
}

// wseg is one segment of a value: [from, to), and either the clock read it
// collapses (inst, the read's index; lay, its layout) or its runs (inst -1).
type wseg struct {
	from, to int
	inst     int
	lay      int
	runs     []wrun
}

// wtoks is a value read into segments and runs.
type wtoks struct {
	s    string
	segs []wseg
}

// collapsed is where an accepted clock read stands in one value: its span,
// its layout, and its index among the value's reads.
type collapsed struct {
	from, to, lay, read int
}

// tokenize reads s into segments and runs, each of spans (sorted, disjoint)
// one segment of its own.
func tokenize(s string, spans []collapsed) wtoks {
	t := wtoks{s: s}
	k := 0
	for i := 0; i < len(s); {
		if k < len(spans) && spans[k].from == i {
			t.segs = append(t.segs, wseg{from: i, to: spans[k].to, inst: spans[k].read, lay: spans[k].lay})
			i = spans[k].to
			k++
			continue
		}
		if structural(s[i]) {
			i++
			continue
		}
		limit := len(s)
		if k < len(spans) {
			limit = spans[k].from
		}
		j := i
		for j < limit && !structural(s[j]) {
			j++
		}
		t.segs = append(t.segs, wseg{from: i, to: j, inst: -1, runs: runsOf(s, i, j)})
		i = j
	}
	return t
}

// runsOf are the runs of s[a:b], a segment.
func runsOf(s string, a, b int) []wrun {
	var runs []wrun
	open := false
	for i := a; i < b; {
		r, size := utf8.DecodeRuneInString(s[i:b])
		class, word, joins := runeClass(r)
		last := len(runs) - 1
		switch {
		case joins && open:
			runs[last].to = i + size
		case word && open && runs[last].class == class:
			runs[last].to = i + size
		case word:
			runs = append(runs, wrun{from: i, to: i + size, class: class})
			open = true
		default:
			open = false
		}
		i += size
	}
	return runs
}

// text is segment i's text.
func (t wtoks) text(i int) string { return t.s[t.segs[i].from:t.segs[i].to] }

// sep is the structural text before segment i (i == len(t.segs): after the
// last).
func (t wtoks) sep(i int) string {
	from, to := 0, len(t.s)
	if i > 0 {
		from = t.segs[i-1].to
	}
	if i < len(t.segs) {
		to = t.segs[i].from
	}
	return t.s[from:to]
}

// gap is the text of segment g before its run j (j == len(runs): after its
// last run).
func (t wtoks) gap(g, j int) string {
	sg := t.segs[g]
	from, to := sg.from, sg.to
	if j > 0 {
		from = sg.runs[j-1].to
	}
	if j < len(sg.runs) {
		to = sg.runs[j].from
	}
	return t.s[from:to]
}

// runText is run j of segment g.
func (t wtoks) runText(g, j int) string {
	r := t.segs[g].runs[j]
	return t.s[r.from:r.to]
}

// sameRunLayout reports whether segment g of a and of b hold their runs
// alike: as many, of one class each, with the same separators around them.
func sameRunLayout(a, b wtoks, g int) bool {
	ra, rb := a.segs[g].runs, b.segs[g].runs
	if len(ra) != len(rb) {
		return false
	}
	for j := range ra {
		if ra[j].class != rb[j].class || a.gap(g, j) != b.gap(g, j) {
			return false
		}
	}
	return a.gap(g, len(ra)) == b.gap(g, len(rb))
}

// asciiAlnum reports whether s is a run of ASCII letters and digits, and not
// empty.
func asciiAlnum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isWord(s[i]) {
			return false
		}
	}
	return true
}

// asciiToken reports whether s is an ASCII token: not empty, all ASCII, and
// no structural character.
func asciiToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf || structural(s[i]) {
			return false
		}
	}
	return true
}

// spansOf are a value's collapsed reads: of the recorded value (rec) or the
// live one, from its accepted pairs, sorted by place.
func spansOf(pairs []clockPair, rec bool) []collapsed {
	out := make([]collapsed, 0, len(pairs))
	for i, p := range pairs {
		sp := p.l
		if rec {
			sp = p.r
		}
		out = append(out, collapsed{from: sp.from, to: sp.to, lay: sp.lay, read: i})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].from < out[j].from })
	return out
}

// wordsGrammar is what a layout hash says of the rules a value's words were
// read by: the structural characters, the declared zone names and the clock
// layouts' names. A change to any is a new version of the words format
// (models.WordsVersion), whose builds keep reading this one as it is; within
// one version, an entry whose layout no longer reads (a Go toolchain whose
// Unicode tables put a character in another script) is void and its leaf is
// compared whole: it fails closed.
var wordsGrammar = func() []byte {
	h := sha256.New()
	h.Write([]byte("keploy word-level learn 1\x00"))
	h.Write([]byte(structuralSet))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(zoneAbbrevs, ",")))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(zoneLongNames, ",")))
	h.Write([]byte{0})
	for _, lay := range clockLayouts {
		h.Write([]byte(lay.name + ","))
	}
	return h.Sum(nil)
}()

// wordsDigest is the digest guard of a recorded leaf: the first 16 hex
// digits of the sha256 of its value as the learn step reads it.
func wordsDigest(rec string) string {
	sum := sha256.Sum256([]byte(rec))
	return hex.EncodeToString(sum[:])[:16]
}

// layoutHash is the layout guard of a recorded value read as t: the first 16
// hex digits of the sha256 of its segments, runs, classes and collapsed
// clock reads, under wordsGrammar.
func layoutHash(t wtoks) string {
	h := sha256.New()
	h.Write(wordsGrammar)
	for _, sg := range t.segs {
		fmt.Fprintf(h, "s%d,%d", sg.from, sg.to)
		if sg.inst >= 0 {
			fmt.Fprintf(h, "i%s", clockLayouts[sg.lay].name)
			continue
		}
		for _, r := range sg.runs {
			fmt.Fprintf(h, "r%d,%d,%s", r.from, r.to, r.class)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// changedSpan is one slot of a drifted value as learnWords checks it: its
// place, and where it stands recorded and live.
type changedSpan struct {
	seg, run int
	kind     string
	lay      int
	r, l     [2]int
}

func (c changedSpan) recText(rec string) string   { return rec[c.r[0]:c.r[1]] }
func (c changedSpan) liveText(live string) string { return live[c.l[0]:c.l[1]] }

// Why a value's words cannot be learned: each names the part, never a value.
const (
	whyParts      = "it holds another number of parts than when it was recorded, or its clock reads stand elsewhere in it, so keploy cannot tell which of its words changed"
	whySeparators = "a character that separates its parts changed (whitespace, a quote, a bracket, or one of : , ; & < > | \\ = / ? # +), so keploy cannot tell which of its words changed"
	whyReadOrder  = "its clock reads stand in another order than when it was recorded"
	whyNotASCII   = "a word of it that changed holds more than ASCII letters and digits"
	whyScript     = "a part of it with text beyond ASCII changed how its words are split"
	whyMoved      = "a word of it that changed stands elsewhere in it on the other side: its words moved, which is no fresh value"
	whyCase       = "only the case of a word of it changed"
	whyEmpty      = "a word of it that changed is empty on one side"
	whyEscape     = "a word of it that changed stands right after a '%' or '\\', an escape keploy does not read apart"
	whyInstant    = "a word of it that changed is part of an instant that did not move as F's clock moves it, so it is no clock read keploy may learn"
	whyZone       = "a word of it that changed is part of an instant written in another zone than when it was recorded, which F's clock does not account for"
	whyGluedUUID  = "a time-ordered UUID in it that changed is glued to a letter or digit"
	whyVariant    = "a time-ordered UUID in it has a variant keploy does not read (N18)"
	whyUUIDCase   = "a time-ordered UUID in it mixes upper and lower case"
	whySkeleton   = "an instant in it is written otherwise than when it was recorded (another layout of its digits, names or zone), which F's clock does not account for"
	whyRepeat     = "the same changed word repeats in it"
	whyGlued      = "a word of it that changed is glued inside another word of it that changed"
	whySame       = "it did not change"
)

// learnWords is what record's full round learns of the words of a drifted
// value (rec, recorded; live, F's), its accepted clock reads (c.pairs) as c
// read them: the entry (models.WordsEntry), or why none can be learned. It
// is a function of its arguments alone.
func learnWords(rec, live string, c clockSet) (models.WordsEntry, string) {
	if rec == live {
		return models.WordsEntry{}, whySame
	}
	// An instant that moved as F's clock moves it but whose zone is written
	// otherwise is said first: it changed, and is no clock read, so its parts
	// would otherwise read as words that moved apart (another number of
	// parts), which says nothing of what changed.
	if len(c.zoned) > 0 {
		return models.WordsEntry{}, whyZone
	}
	for i := 1; i < len(c.pairs); i++ {
		if c.pairs[i].l.from < c.pairs[i-1].l.to {
			return models.WordsEntry{}, whyReadOrder
		}
	}
	for _, p := range c.pairs {
		if p.r.lay != uuidLayout {
			continue
		}
		if !uuidVariant(rec[p.r.from:p.r.to]) {
			return models.WordsEntry{}, whyVariant
		}
	}
	tr, tf := tokenize(rec, spansOf(c.pairs, true)), tokenize(live, spansOf(c.pairs, false))
	if len(tr.segs) != len(tf.segs) {
		return models.WordsEntry{}, whyParts
	}
	for i := range tr.segs {
		if tr.segs[i].inst != tf.segs[i].inst {
			return models.WordsEntry{}, whyParts
		}
	}
	for i := 0; i <= len(tr.segs); i++ {
		if tr.sep(i) != tf.sep(i) {
			return models.WordsEntry{}, whySeparators
		}
	}
	var slots []models.WordSlot
	var spans []changedSpan
	for g, rs := range tr.segs {
		fs := tf.segs[g]
		if rs.inst >= 0 {
			lay := clockLayouts[rs.lay]
			sk := lay.skeleton(tr.text(g))
			if sk == "" {
				return models.WordsEntry{}, whyUUIDCase
			}
			if sk != lay.skeleton(tf.text(g)) {
				return models.WordsEntry{}, whySkeleton
			}
			slots = append(slots, models.WordSlot{Seg: g, Kind: models.WordSlotInstant, Layout: lay.name, Skeleton: sk})
			spans = append(spans, changedSpan{seg: g, kind: models.WordSlotInstant, lay: rs.lay, r: [2]int{rs.from, rs.to}, l: [2]int{fs.from, fs.to}})
			continue
		}
		rt, ft := tr.text(g), tf.text(g)
		if rt == ft {
			continue
		}
		if sameRunLayout(tr, tf, g) {
			for j, rr := range rs.runs {
				fr := fs.runs[j]
				a, b := rec[rr.from:rr.to], live[fr.from:fr.to]
				if a == b {
					continue
				}
				if !asciiAlnum(a) || !asciiAlnum(b) {
					return models.WordsEntry{}, whyNotASCII
				}
				slots = append(slots, models.WordSlot{Seg: g, Run: j})
				spans = append(spans, changedSpan{seg: g, run: j, kind: models.WordSlotRun, r: [2]int{rr.from, rr.to}, l: [2]int{fr.from, fr.to}})
			}
			continue
		}
		if !asciiToken(rt) || !asciiToken(ft) {
			return models.WordsEntry{}, whyScript
		}
		slots = append(slots, models.WordSlot{Seg: g, Kind: models.WordSlotToken})
		spans = append(spans, changedSpan{seg: g, kind: models.WordSlotToken, r: [2]int{rs.from, rs.to}, l: [2]int{fs.from, fs.to}})
	}
	if len(slots) == 0 {
		return models.WordsEntry{}, whySeparators
	}
	if why := checkSlots(rec, live, tr, tf, spans, c); why != "" {
		return models.WordsEntry{}, why
	}
	return models.WordsEntry{V: models.WordsVersion, Digest: wordsDigest(rec), Layout: layoutHash(tr), Slots: models.SortWordSlots(slots)}, ""
}

// checkSlots says why the slots of a drifted value (spans; tr and tf, its
// words recorded and live) cannot be learned, "" when they can: per slot, a
// word that moved, changed only its case, is empty, stands right after an
// escape, or is part of an instant that is no accepted clock read (or of a
// time-ordered UUID glued to a word); and between the slots of the value, a
// changed word that repeats, or one glued inside another (the changed piece,
// class-bounded: gluedPieces). An instant slot is a clock read: it is
// checked with the others only as a UUID's.
func checkSlots(rec, live string, tr, tf wtoks, spans []changedSpan, c clockSet) string {
	recWords, liveWords := wordsAt(tr), wordsAt(tf)
	for _, sp := range spans {
		if sp.kind == models.WordSlotInstant {
			continue
		}
		a, b := sp.recText(rec), sp.liveText(live)
		if a == "" || b == "" {
			return whyEmpty
		}
		if strings.EqualFold(a, b) {
			return whyCase
		}
		if movedTo(liveWords, a, sp) || movedTo(recWords, b, sp) {
			return whyMoved
		}
		if afterEscape(rec, sp.r[0]) || afterEscape(live, sp.l[0]) || sp.kind == models.WordSlotToken && (strings.Contains(a, "%") || strings.Contains(b, "%")) {
			return whyEscape
		}
	}
	// Every instant of a declared layout that is no accepted read. (One that
	// moved as F's clock moves it but changed its zone never gets here:
	// learnWords refuses it first, whyZone.)
	for _, side := range []struct {
		s       string
		spanOf  func(changedSpan) [2]int
		instant []collapsed
	}{
		{rec, func(c changedSpan) [2]int { return c.r }, spansOf(c.pairs, true)},
		{live, func(c changedSpan) [2]int { return c.l }, spansOf(c.pairs, false)},
	} {
		overlaps := func(from, to int) bool {
			for _, sp := range spans {
				if sp.kind == models.WordSlotInstant {
					continue
				}
				x := side.spanOf(sp)
				if x[0] < to && from < x[1] {
					return true
				}
			}
			return false
		}
		taken := make([]bool, len(side.s))
		for _, x := range side.instant {
			for i := x.from; i < x.to; i++ {
				taken[i] = true
			}
		}
		for _, lay := range clockLayouts {
			for _, m := range clockMatches(lay, side.s, taken) {
				if _, _, ok := lay.read(side.s[m[0]:m[1]]); ok && overlaps(m[0], m[1]) {
					return whyInstant
				}
			}
		}
		// A time-ordered UUID glued to a word is no match of its layout
		// (clockMatches): the parts of it that changed are refused, not
		// learned as plain runs.
		for _, m := range clockLayouts[uuidLayout].re.FindAllStringIndex(side.s, -1) {
			glued := m[0] > 0 && isWord(side.s[m[0]-1]) || m[1] < len(side.s) && isWord(side.s[m[1]])
			if glued && overlaps(m[0], m[1]) {
				return whyGluedUUID
			}
		}
	}
	// Between the slots of the value: a changed word that repeats (the same
	// word recorded, or live, at two of its slots), and one whose changed
	// piece is glued inside another's. Two instants other than UUIDs are
	// clock reads, which may repeat (N2); a UUID repeats as any word does.
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			if clockOnly(a) || clockOnly(b) {
				continue
			}
			if a.recText(rec) == b.recText(rec) || a.liveText(live) == b.liveText(live) {
				return whyRepeat
			}
			if a.kind == models.WordSlotInstant || b.kind == models.WordSlotInstant {
				continue
			}
			pa, oka := changedPiece(a.recText(rec), a.liveText(live))
			pb, okb := changedPiece(b.recText(rec), b.liveText(live))
			if oka && okb && gluedPieces(pa, pb) {
				return whyGlued
			}
		}
	}
	return ""
}

// clockOnly reports whether a slot is an instant other than a UUID's: a
// clock read, which the checks between slots pass over.
func clockOnly(c changedSpan) bool {
	return c.kind == models.WordSlotInstant && c.lay != uuidLayout
}

// wordAt is one word of a value as checkSlots compares it: a run of a
// segment, or a segment whole, and where it is.
type wordAt struct {
	seg, run int
	whole    bool
}

// wordsAt are the words of t by their text: every run of a plain segment,
// and every plain segment whole.
func wordsAt(t wtoks) map[string][]wordAt {
	out := map[string][]wordAt{}
	for g, sg := range t.segs {
		if sg.inst >= 0 {
			continue
		}
		out[t.text(g)] = append(out[t.text(g)], wordAt{seg: g, whole: true})
		for j := range sg.runs {
			out[t.runText(g, j)] = append(out[t.runText(g, j)], wordAt{seg: g, run: j})
		}
	}
	return out
}

// movedTo reports whether words (of the other side of a slot) hold text, the
// slot's text on its own side, at another place than sp's own: the word
// moved there, or came from there.
func movedTo(words map[string][]wordAt, text string, sp changedSpan) bool {
	for _, at := range words[text] {
		if at.seg != sp.seg {
			return true
		}
		if sp.kind == models.WordSlotRun && !at.whole && at.run != sp.run {
			return true
		}
	}
	return false
}

// afterEscape reports whether the byte before s[i:] is a '%' or a '\'.
func afterEscape(s string, i int) bool {
	return i > 0 && (s[i-1] == '%' || s[i-1] == '\\')
}

// uuidVariant reports whether a time-ordered UUID's variant nibble is one
// keploy reads: 8, 9, a or b, in either case.
func uuidVariant(u string) bool {
	return len(u) > 19 && strings.IndexByte("89abAB", u[19]) >= 0
}

// --- the changed piece, and glued containment (B1) ---

// byteClass is a byte's class within a changed piece: a letter, a digit, or
// another byte.
func byteClass(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return 2
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		return 1
	}
	return 0
}

// subRuns are s's letter runs, digit runs and runs of other bytes, in order.
func subRuns(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		j := i + 1
		for j < len(s) && byteClass(s[j]) == byteClass(s[i]) {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// changedPiece is the changed piece of a changed word (r, recorded; f, live):
// the word with its unchanged sub-runs (letters, digits, separators) stripped
// from either end, at least one left on each side ("INV1042" -> "INV2077" is
// "1042" -> "2077"; "1042A" -> "2077A" is "1042" -> "2077"). A fresh hex
// changes from end to end: its piece is the whole word. ok is false when the
// two do not differ.
func changedPiece(r, f string) (run, bool) {
	if r == f || r == "" || f == "" {
		return run{}, false
	}
	rs, fs := subRuns(r), subRuns(f)
	n := len(rs)
	if len(fs) < n {
		n = len(fs)
	}
	p := 0
	for p < n-1 && rs[p] == fs[p] {
		p++
	}
	q := 0
	for q < n-p-1 && rs[len(rs)-1-q] == fs[len(fs)-1-q] {
		q++
	}
	return run{strings.Join(rs[p:len(rs)-q], ""), strings.Join(fs[p:len(fs)-q], "")}, true
}

// pieceBoundary reports whether i is a class boundary of s: either end, or
// where a letter, a digit and another byte meet.
func pieceBoundary(s string, i int) bool {
	return i == 0 || i == len(s) || byteClass(s[i-1]) != byteClass(s[i])
}

// pieceIn reports whether changed piece p lies inside changed piece c: p's
// recorded text in c's recorded one and p's live text in c's live one, each
// starting and ending at a class boundary of c, at one offset in both,
// counted from c's start or from its end.
func pieceIn(c, p run) bool {
	lr, lf := len(p.rec), len(p.live)
	if lr == 0 || lf == 0 || lr > len(c.rec) || lf > len(c.live) {
		return false
	}
	at := func(rs, fs int) bool {
		return c.rec[rs:rs+lr] == p.rec && c.live[fs:fs+lf] == p.live &&
			pieceBoundary(c.rec, rs) && pieceBoundary(c.rec, rs+lr) && pieceBoundary(c.live, fs) && pieceBoundary(c.live, fs+lf)
	}
	for o := 0; o+lr <= len(c.rec) && o+lf <= len(c.live); o++ {
		if at(o, o) {
			return true
		}
	}
	for e := 0; e+lr <= len(c.rec) && e+lf <= len(c.live); e++ {
		if at(len(c.rec)-e-lr, len(c.live)-e-lf) {
			return true
		}
	}
	return false
}

// gluedPieces reports whether either of two changed pieces lies inside the
// other (pieceIn): the two drifted values are glued, one carried inside the
// other ("ORD123" and "123", "INV1042" and "ORD1042", "p12x1042" and
// "1042").
func gluedPieces(a, b run) bool { return pieceIn(a, b) || pieceIn(b, a) }

// wordPieces are the changed pieces of a drifted value (rec, live; c, its
// clock reads as it stands): of each run that changed in a segment whose
// runs stand alike, and of each other segment that changed whole; of the
// whole value when its segments do not stand alike. A collapsed clock read
// is no piece: what drifted with F's clock is no value that flowed.
func wordPieces(rec, live string, c clockSet) []run {
	if rec == live {
		return nil
	}
	tr, tf := tokenize(rec, spansOf(c.pairs, true)), tokenize(live, spansOf(c.pairs, false))
	aligned := len(tr.segs) == len(tf.segs)
	for i := 0; aligned && i < len(tr.segs); i++ {
		aligned = tr.segs[i].inst == tf.segs[i].inst
	}
	var out []run
	add := func(r, f string) {
		if p, ok := changedPiece(r, f); ok {
			out = append(out, p)
		}
	}
	if !aligned {
		add(rec, live)
		return out
	}
	for g, rs := range tr.segs {
		if rs.inst >= 0 || tr.text(g) == tf.text(g) {
			continue
		}
		if !sameRunLayout(tr, tf, g) {
			add(tr.text(g), tf.text(g))
			continue
		}
		for j := range rs.runs {
			add(tr.runText(g, j), tf.runText(g, j))
		}
	}
	return out
}

// instantOnly reports whether every word of a drifted value that changed
// lies inside an accepted clock read other than a UUID's (c, as it stands):
// a value two places of one call may share without one carrying the other
// (createdAt = updatedAt). Such twins are never held equal (N2).
func instantOnly(rec, live string, c clockSet) bool {
	if rec == live || len(c.pairs) == 0 {
		return false
	}
	tr, tf := tokenize(rec, spansOf(c.pairs, true)), tokenize(live, spansOf(c.pairs, false))
	if len(tr.segs) != len(tf.segs) {
		return false
	}
	for i := 0; i <= len(tr.segs); i++ {
		if tr.sep(i) != tf.sep(i) {
			return false
		}
	}
	for g, sg := range tr.segs {
		if sg.inst != tf.segs[g].inst {
			return false
		}
		if sg.inst >= 0 {
			if sg.lay == uuidLayout {
				return false
			}
			continue
		}
		if tr.text(g) != tf.text(g) {
			return false
		}
	}
	return true
}

// --- skeletons ---

var (
	weekdays = map[string]bool{"Mon": true, "Tue": true, "Wed": true, "Thu": true, "Fri": true, "Sat": true, "Sun": true}
	months   = map[string]bool{"Jan": true, "Feb": true, "Mar": true, "Apr": true, "May": true, "Jun": true, "Jul": true, "Aug": true, "Sep": true, "Oct": true, "Nov": true, "Dec": true}
)

// skeleton is an instant m, a match of lay, written in classes
// (models.WordSlot.Skeleton): "d<length>" for each digit run, W for a
// weekday's name and M for a month's, D for Unix date's padded day, F for a
// fraction of a second after the seconds (present or not, on either side),
// N for a zone's name from the declared list where the layout writes one,
// and every other character as itself (T or a space, + or -, Z). A UUID's is
// its groups' lengths in its letters' case, its version and its variant
// (uuidSkeleton). "" for a UUID that mixes cases.
func (lay clockLayout) skeleton(m string) string {
	if lay.name == "uuid" {
		return uuidSkeleton(m)
	}
	na, nb := -1, -1
	if lay.nameGroup > 0 {
		if sub := lay.re.FindStringSubmatchIndex(m); sub != nil && sub[2*lay.nameGroup] >= 0 {
			na, nb = sub[2*lay.nameGroup], sub[2*lay.nameGroup+1]
		}
	}
	var b strings.Builder
	colons := 0 // digit groups joined by ':' so far: 3 at the seconds
	prevMonth := false
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	letter := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	for i := 0; i < len(m); {
		c := m[i]
		switch {
		case i == na:
			b.WriteByte('N')
			i = nb
			colons, prevMonth = 0, false
		case lay.dayPad && prevMonth && c == ' ' && i+2 < len(m) && (m[i+1] == ' ' || digit(m[i+1])) && digit(m[i+2]):
			b.WriteString(" D")
			i += 3
			colons, prevMonth = 0, false
		case digit(c):
			j := i
			for j < len(m) && digit(m[j]) {
				j++
			}
			if i > 0 && m[i-1] == ':' && colons > 0 {
				colons++
			} else {
				colons = 1
			}
			b.WriteString("d" + strconv.Itoa(j-i))
			// The seconds: of a time, its third group joined by ':'; of an
			// epoch, its ten leading digits.
			seconds := colons == 3 || lay.name == "epoch" && i == 0 && j-i == 10
			i = j
			if lay.frac && seconds {
				b.WriteByte('F')
				if i+1 < len(m) && (m[i] == '.' || m[i] == ',') && digit(m[i+1]) {
					i++
					for i < len(m) && digit(m[i]) {
						i++
					}
				}
				colons = 0
			}
			prevMonth = false
		case letter(c):
			j := i
			for j < len(m) && letter(m[j]) {
				j++
			}
			w := m[i:j]
			switch {
			case weekdays[w]:
				b.WriteByte('W')
			case months[w]:
				b.WriteByte('M')
			default:
				b.WriteString(w)
			}
			prevMonth = months[w]
			i = j
			colons = 0
		default:
			b.WriteByte(c)
			i++
			if c != ':' {
				colons = 0
			}
			if c != ' ' {
				prevMonth = false
			}
		}
	}
	return b.String()
}

// uuidSkeleton is a time-ordered UUID written in classes: its groups'
// lengths in its letters' case (x lower, X upper; a UUID of digits alone is
// lower), its version digit pinned, and its variant nibble as v (V): "x8-x4-
// 7x3-vx3-x12". "" when its letters mix cases.
func uuidSkeleton(u string) string {
	lower, upper := false, false
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c >= 'a' && c <= 'f':
			lower = true
		case c >= 'A' && c <= 'F':
			upper = true
		}
	}
	if lower && upper || len(u) != 36 {
		return ""
	}
	x, v := "x", "v"
	if upper {
		x, v = "X", "V"
	}
	return x + "8-" + x + "4-" + string(u[14]) + x + "3-" + v + x + "3-" + x + "12"
}

// --- learn: the exported reads ---

// Words is what record's full round learns of the words of drifted value
// (rec, recorded; live, F's) in the call whose clock reads were made in w
// (Tracker.CallWindow; nil: by the lead alone): the entry its leaf carries
// (models.WordsEntry), or why none can be learned, for a person to read. It
// is read once per test (the cache), and is a function of its arguments
// alone.
func (dc *DriftCache) Words(rec, live string, w *Window) (models.WordsEntry, string) {
	if dc == nil {
		dc = NewDriftCache()
	}
	v := dc.ofIn(rec, live, w)
	v.wordsOnce.Do(func() { v.words, v.wordsWhy = learnWords(rec, live, v.clock) })
	return v.words.Clone(), v.wordsWhy
}

// Tie is how two drifted values of one call stand to each other.
type Tie int

const (
	// TieNone: neither carries the other.
	TieNone Tie = iota
	// TieExempt: every word of both that changed lies inside an accepted
	// clock read other than a UUID's (createdAt = updatedAt): neither
	// carries the other, and they are never held equal (N2).
	TieExempt
	// TieWhole: one value whole, recorded, at both places: they may be one
	// value of the call.
	TieWhole
	// TiePartial: one carries the other, or a part of it, inside a longer
	// value (sources), or glued (gluedPieces): keploy cannot follow it.
	TiePartial
)

// TieOf is how drifted values a and b (each [recorded, live]) of one call,
// whose clock reads were made in w, stand to each other: the instant check
// first (TieExempt), then sources, both ways.
func (dc *DriftCache) TieOf(a, b [2]string, w *Window) Tie {
	if dc == nil {
		dc = NewDriftCache()
	}
	va, vb := dc.ofIn(a[0], a[1], w), dc.ofIn(b[0], b[1], w)
	if instantOnly(va.rec, va.live, va.clock) && instantOnly(vb.rec, vb.live, vb.clock) {
		return TieExempt
	}
	found, whole := sources(va, vb)
	switch {
	case whole:
		return TieWhole
	case found:
		return TiePartial
	}
	return TieNone
}

// TieClass names the word-level rule that ties drifted values a and b of one
// call, as TieOf reads them, for the report of a call refused by the tie
// (models.UnmatchedCall.Refused): models.RefusedContainment when the glued
// test (B1) alone ties them, models.RefusedTie for any other tie, "" when
// TieOf finds none or exempts them. It names a rule and decides nothing:
// TieOf decides.
func (dc *DriftCache) TieClass(a, b [2]string, w *Window) string {
	if dc == nil {
		dc = NewDriftCache()
	}
	va, vb := dc.ofIn(a[0], a[1], w), dc.ofIn(b[0], b[1], w)
	if instantOnly(va.rec, va.live, va.clock) && instantOnly(vb.rec, vb.live, vb.clock) {
		return ""
	}
	found, _, glued := sourcesBy(va, vb)
	switch {
	case !found:
		return ""
	case glued:
		return models.RefusedContainment
	}
	return models.RefusedTie
}

// RefusalClass names the word-level rule behind a reason Words gives for a
// value's words it cannot learn (why), for the report of the call it refuses
// (models.UnmatchedCall.Refused): models.RefusedTie for a changed word that
// repeats in the leaf, models.RefusedContainment for one glued inside another
// (B1); "" for every other reason.
func RefusalClass(why string) string {
	switch why {
	case whyRepeat:
		return models.RefusedTie
	case whyGlued:
		return models.RefusedContainment
	}
	return ""
}

// --- serve: WordsAdmit ---

// maxCollapses bounds the ways WordsAdmit tries to place a value's instant
// slots: beyond it, the value is not admitted.
const maxCollapses = 256

// Why an entry is void, or a value is not admitted.
const (
	whyVoidMerged  = "the learn step read this leaf in two ways, so its learned words are void and it is compared whole"
	whyGuard       = "a guard of its learned words is missing or malformed"
	whyDigest      = "its recorded value is not the one its words were learned of"
	whyLayoutGuard = "its recorded value no longer reads into the parts its words were learned of"
	whyKind        = "a slot of its learned words is of a kind or a layout this keploy does not know"
	whyRange       = "a slot of its learned words names a part or a word its recorded value does not hold"
	whyRecSkeleton = "an instant slot of its learned words is not the instant its recorded value holds there"
	whyTooMany     = "it holds too many instants of its layouts for keploy to place its learned words"
)

// hexGuard reports whether g is 16 lower-case hex digits.
func hexGuard(g string) bool {
	if len(g) != 16 {
		return false
	}
	for i := 0; i < len(g); i++ {
		if !(g[i] >= '0' && g[i] <= '9' || g[i] >= 'a' && g[i] <= 'f') {
			return false
		}
	}
	return true
}

// WordsAdmit reports whether live may stand where rec was recorded, given the
// words e learned of the leaf: both values are split again, every byte outside
// e's slots must be equal, and each slot is checked as its kind says
// (models.WordSlot). An entry that does not hold is void and admits nothing:
// this build cannot read it (models.WordsEntry.Unreadable: written by a newer
// keploy, or with no version), its digest or layout no longer matches rec, a
// guard is missing or malformed, a slot's seg or run is out of range, a kind
// or a layout is one this build does not know, or an instant's skeleton is
// not rec's match at its segment. why says, for a person to read, what did
// not hold; "" when ok.
//
// An instant slot's live instant is found as record found it: the clock
// layouts in their order, each taking the matches (clockMatches, at word
// boundaries) the earlier layouts' slots left free; it must be the slot's
// segment, hold the recorded skeleton, and read. A UUID's must hold the
// recorded version and letter case and a variant of 8, 9, a or b (either
// case).
func WordsAdmit(rec, live string, e models.WordsEntry) (ok bool, why string) {
	if why := e.Unreadable(); why != "" {
		return false, why
	}
	if e.Void() {
		return false, whyVoidMerged
	}
	if !hexGuard(e.Digest) || !hexGuard(e.Layout) {
		return false, whyGuard
	}
	if wordsDigest(rec) != e.Digest {
		return false, whyDigest
	}
	need := map[int]int{}
	for _, s := range e.Slots {
		switch s.Kind {
		case models.WordSlotRun, models.WordSlotToken:
			if s.Layout != "" || s.Skeleton != "" || s.Kind == models.WordSlotToken && s.Run != 0 {
				return false, whyGuard
			}
		case models.WordSlotInstant:
			li := layoutNamed(s.Layout)
			if li < 0 || s.Skeleton == "" {
				return false, whyKind
			}
			if s.Run != 0 {
				return false, whyGuard
			}
			need[li]++
		default:
			return false, whyKind
		}
		if s.Seg < 0 || s.Run < 0 {
			return false, whyRange
		}
	}
	// The recorded value's collapse: the one whose layout is e's.
	var tr wtoks
	found := false
	if exhausted := collapses(rec, need, func(spans []collapsed) bool {
		t := tokenize(rec, spans)
		if layoutHash(t) == e.Layout {
			tr, found = t, true
			return true
		}
		return false
	}); exhausted && !found {
		return false, whyTooMany
	}
	if !found {
		return false, whyLayoutGuard
	}
	bySeg := map[int][]models.WordSlot{}
	for _, s := range e.Slots {
		if s.Seg >= len(tr.segs) {
			return false, whyRange
		}
		sg := tr.segs[s.Seg]
		switch s.Kind {
		case models.WordSlotInstant:
			if sg.inst < 0 || clockLayouts[sg.lay].name != s.Layout {
				return false, whyRange
			}
			if clockLayouts[sg.lay].skeleton(tr.text(s.Seg)) != s.Skeleton {
				return false, whyRecSkeleton
			}
		case models.WordSlotToken:
			if sg.inst >= 0 {
				return false, whyRange
			}
		case models.WordSlotRun:
			if sg.inst >= 0 || s.Run >= len(sg.runs) {
				return false, whyRange
			}
		}
		bySeg[s.Seg] = append(bySeg[s.Seg], s)
	}
	// A segment holds one instant or token slot, or slots of its runs, each
	// once; and every collapsed instant is an instant slot's.
	for g, sg := range tr.segs {
		slots := bySeg[g]
		if sg.inst >= 0 && (len(slots) != 1 || slots[0].Kind != models.WordSlotInstant) {
			return false, whyRange
		}
		runs := map[int]bool{}
		for _, s := range slots {
			if s.Kind != models.WordSlotRun && len(slots) != 1 || s.Kind == models.WordSlotRun && runs[s.Run] {
				return false, whyRange
			}
			runs[s.Run] = true
		}
	}
	if rec == live {
		return true, ""
	}
	first := ""
	admitted := false
	exhausted := collapses(live, need, func(spans []collapsed) bool {
		why := admitWords(tr, tokenize(live, spans), bySeg)
		if why == "" {
			admitted = true
			return true
		}
		if first == "" {
			first = why
		}
		return false
	})
	switch {
	case admitted:
		return true, ""
	case exhausted:
		// Cut short: a collapse not tried might have admitted it, so no
		// one tried says why it is not.
		return false, whyTooMany
	case first != "":
		return false, first
	}
	return false, "an instant of it is not where it was recorded, in its layout"
}

// collapses calls visit with each way the instants of s can collapse for
// need (how many instant slots each layout, by index, has): the layouts in
// clockLayouts' order, each taking as many of its matches (clockMatches) as
// need says, in order, among those the earlier layouts' choices left free.
// It stops when visit returns true. exhausted reports whether it stopped at
// maxCollapses.
func collapses(s string, need map[int]int, visit func([]collapsed) bool) (exhausted bool) {
	tries := 0
	done := false
	var walk func(li int, taken []bool, chosen []collapsed)
	walk = func(li int, taken []bool, chosen []collapsed) {
		if done {
			return
		}
		if li == len(clockLayouts) {
			if tries++; tries > maxCollapses {
				exhausted, done = true, true
				return
			}
			spans := append([]collapsed(nil), chosen...)
			sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
			for i := range spans {
				spans[i].read = i
			}
			if visit(spans) {
				done = true
			}
			return
		}
		n := need[li]
		if n == 0 {
			walk(li+1, taken, chosen)
			return
		}
		ms := clockMatches(clockLayouts[li], s, taken)
		var pick func(from int, left int, cur []collapsed)
		pick = func(from, left int, cur []collapsed) {
			if done {
				return
			}
			if left == 0 {
				t := append([]bool(nil), taken...)
				for _, c := range cur[len(chosen):] {
					for i := c.from; i < c.to; i++ {
						t[i] = true
					}
				}
				walk(li+1, t, cur)
				return
			}
			for k := from; k <= len(ms)-left; k++ {
				pick(k+1, left-1, append(append([]collapsed(nil), cur...), collapsed{from: ms[k][0], to: ms[k][1], lay: li}))
			}
		}
		pick(0, n, chosen)
	}
	walk(0, make([]bool, len(s)), nil)
	return exhausted
}

// admitWords says why tl, a live value read with one collapse of its
// instants, cannot stand where tr was recorded, given the slots by segment
// (bySeg); "" when it can.
func admitWords(tr, tl wtoks, bySeg map[int][]models.WordSlot) string {
	if len(tr.segs) != len(tl.segs) {
		return "it holds another number of parts than when it was recorded"
	}
	for i := 0; i <= len(tr.segs); i++ {
		if tr.sep(i) != tl.sep(i) {
			return "a character that separates its parts is not the one recorded"
		}
	}
	for g, rs := range tr.segs {
		ls := tl.segs[g]
		slots := bySeg[g]
		if rs.inst >= 0 {
			if ls.inst < 0 || ls.lay != rs.lay {
				return "an instant of it is not where it was recorded, in its layout"
			}
			lay := clockLayouts[rs.lay]
			text := tl.text(g)
			if lay.skeleton(text) != slots[0].Skeleton {
				return "an instant of it is written otherwise than when it was recorded"
			}
			if _, _, ok := lay.read(text); !ok {
				return "an instant of it does not read as one"
			}
			if rs.lay == uuidLayout && !uuidVariant(text) {
				return "a time-ordered UUID in it has a variant keploy does not read"
			}
			continue
		}
		if ls.inst >= 0 {
			return "it holds an instant where it was recorded with none"
		}
		rt, lt := tr.text(g), tl.text(g)
		if len(slots) == 0 {
			if rt != lt {
				return "a part of it that is not learned to change changed"
			}
			continue
		}
		if slots[0].Kind == models.WordSlotToken {
			if !asciiToken(lt) {
				return "a part of it learned to change holds more than an ASCII token"
			}
			continue
		}
		if len(rs.runs) == 1 {
			// The run alone in its segment: any ASCII token, the separators
			// around it fixed (N1).
			lead, trail := tr.gap(g, 0), tr.gap(g, 1)
			if !strings.HasPrefix(lt, lead) || !strings.HasSuffix(lt, trail) || len(lt) < len(lead)+len(trail) {
				return "a separator beside a word of it learned to change is not the one recorded"
			}
			if !asciiToken(lt[len(lead) : len(lt)-len(trail)]) {
				return "a word of it learned to change holds more than an ASCII token"
			}
			continue
		}
		if len(ls.runs) != len(rs.runs) {
			return "a part of it holds another number of words than when it was recorded"
		}
		slotted := map[int]bool{}
		for _, s := range slots {
			slotted[s.Run] = true
		}
		for j := range rs.runs {
			if tr.gap(g, j) != tl.gap(g, j) {
				return "a separator beside a word of it is not the one recorded"
			}
			lw := tl.runText(g, j)
			if slotted[j] {
				if !asciiAlnum(lw) {
					return "a word of it learned to change holds more than ASCII letters and digits"
				}
				continue
			}
			if lw != tr.runText(g, j) {
				return "a word of it that is not learned to change changed"
			}
		}
		if tr.gap(g, len(rs.runs)) != tl.gap(g, len(ls.runs)) {
			return "a separator beside a word of it is not the one recorded"
		}
	}
	return ""
}

// --- WholeDrift ---

// WholeDrift reports whether a drifted leaf (rec, recorded; live, record's
// full round's) changed whole: after its accepted clock reads collapse (read
// by the full round's lead, or within w, the call's window; nil for none),
// every run of rec changed and no changed run repeats. It is judged by runs,
// never by a value's shape: a run changed when the live value holds it
// nowhere as a run (an accepted clock read always changed), and one repeats
// when two runs of either value are one text. A protocol learner that learns
// a leaf whole (as noise, or an echo) asks it in each reading it compares,
// and learns the leaf only when it is whole in all of them. why says, for a
// person to read, why it is not whole; "" when it is.
func WholeDrift(rec, live string, w *Window) (whole bool, why string) {
	if rec == live {
		return false, whySame
	}
	c := clockInBy(rec, live, w, true)
	tr, tf := tokenize(rec, spansOf(c.pairs, true)), tokenize(live, spansOf(c.pairs, false))
	recRuns, liveRuns := runTexts(tr), runTexts(tf)
	if len(recRuns) == 0 {
		return false, "it holds no word"
	}
	liveSet := map[string]bool{}
	for _, r := range liveRuns {
		if !r.instant {
			liveSet[r.text] = true
		}
	}
	for _, r := range recRuns {
		if !r.instant && liveSet[r.text] {
			return false, "a word of it did not change, so only part of it is new, and " + allOfIt
		}
	}
	for _, runs := range [][]wordRun{recRuns, liveRuns} {
		seen := map[string]bool{}
		for _, r := range runs {
			if seen[r.text] {
				return false, "a word of it that changed repeats in it"
			}
			seen[r.text] = true
		}
	}
	return true, ""
}

// wordRun is one run of a value as WholeDrift reads it: an accepted clock
// read is one.
type wordRun struct {
	text    string
	instant bool
}

// runTexts are every run of t, in order, an instant as one.
func runTexts(t wtoks) []wordRun {
	var out []wordRun
	for g, sg := range t.segs {
		if sg.inst >= 0 {
			out = append(out, wordRun{text: t.text(g), instant: true})
			continue
		}
		for j := range sg.runs {
			out = append(out, wordRun{text: t.runText(g, j)})
		}
	}
	return out
}
