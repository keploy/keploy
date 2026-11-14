package learn

import (
	"math"
	"slices"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/variables"
)

// This file is learning's one reader of a drifted value (the proven
// switch's: nothing outside it calls the reader until the outcomes do). It
// lists the words that changed between the recording (R) and record's full
// round (F), where each stands, and what each may be judged as:
//
//   - A word is an isWord run, an ASCII letter and digit run (tokens.go):
//     '-', '_', '+', '/', '=' and '.' always split, so the "17" of
//     "order-17", "uploads/17/a.png" and "Order+17" is a word.
//   - The words of R and F are lined up as movedWords lines them up
//     (classify.go): a longest common subsequence, ties broken each way;
//     past its bound, one in each stretch between two anchors, words each
//     run holds once and in the same order (anchorsOf). The reader keeps a word only where both ways pair it with the same
//     word of the other run (keptWords), so every kept word has one
//     partner and the words between two kept words are one stretch.
//   - A stretch of equal counts pairs its words by position. One of unequal
//     counts pairs nothing: it binds only as a whole token both runs hold
//     there, or is fresh or refused (the caller decides). A stretch only
//     one run has ("a,b" to "a,b,c") is a changed shape.
//   - A whole token is main's idByte token (classify.go), or a base64 run
//     that stands whole (eachB64Run) outside the text's instants: a
//     zone's sign glued to an instant's seconds ("13+05" of
//     "…16:20:13+05:30") is the instant's, never a run. The reader lists
//     the whole tokens around each changed word (and around a stretch) in
//     both runs; one binds as one value only where one place holds it
//     whole in each run (the index's holdsToken).
//   - Every changed word carries its bits (wordBits) and, where it is part
//     of an instant in either run, that instant read whole
//     (reader_instant.go).

// tokenKind is how a whole token around a changed word is cut.
type tokenKind uint8

const (
	// tokenID: main's idByte token, a run of word bytes, '-' and '_'.
	tokenID tokenKind = iota + 1
	// tokenB64: a run of the base64 alphabets with its padding, outside
	// the text's instants.
	tokenB64
)

// wholeToken is one whole token that holds a changed word (or a stretch)
// in both runs: its text and [start, end) in each.
type wholeToken struct {
	Kind          tokenKind
	Rec, Live     string
	RecAt, LiveAt [2]int
}

// stretchShape is how the two runs' moved words of a stretch line up.
type stretchShape uint8

const (
	// stretchPaired: equal counts, paired by position.
	stretchPaired stretchShape = iota + 1
	// stretchUnequal: both runs moved words there, in unequal counts.
	stretchUnequal
	// stretchShapeChanged: only one run has words there.
	stretchShapeChanged
)

// wordStretch is the moved words between two kept words (or the bounds of
// an aligned instant, alignedCuts): the word index range [from, to) of each
// run (empty in a run that has none there), the whole tokens that hold
// every moved word of it in both runs, and whether it holds an instant the
// alignment aligned with none of the other run (Loose): that instant stands
// in one run alone, so the text changed shape, and the stretch's words pair
// with none.
type wordStretch struct {
	Shape     stretchShape
	Rec, Live [2]int
	Tokens    []wholeToken
	Loose     bool
}

// readWord is one changed word: its address (At, readLeaf's; the zero
// Address from readText), its text in each run ("" in a run that has no
// partner for it), its index among that run's words of its reading (-1
// where none), its bytes there, its stretch, the whole tokens around it
// longer than it, its bits, and the instant it is part of (nil where none).
type readWord struct {
	At                variables.Address
	Rec, Live         string
	RecIdx, LiveIdx   int
	RecAt, LiveAt     [2]int
	Stretch           int
	Tokens            []wholeToken
	RecBits, LiveBits float64
	Instant           *instantWord
}

// textReading is what the reader reads of one drifted text: where it
// stands (At names the text whole, Word -1; Off is how many words of R's
// and F's text at those steps come before it, readLeaf's), both texts,
// their words, the stretches, every changed word in order, the separators
// that changed between paired words, the instants those words are part
// of, and whether the text changed shape: a word only one run has that no
// instant paired across the runs holds (readShape), or a changed separator
// no whole token holds.
type textReading struct {
	At                  variables.Address
	Off                 [2]int
	Rec, Live           string
	RecWords, LiveWords [][2]int
	Stretches           []wordStretch
	Words               []readWord
	Separators          []sepChange
	Instants            []*instantWord
	ChangedShape        bool
}

// textsOf are the texts of spans in s.
func textsOf(s string, spans [][2]int) []string {
	out := make([]string, len(spans))
	for i, sp := range spans {
		out[i] = s[sp[0]:sp[1]]
	}
	return out
}

// keptWords are the pairs (i, j) of rec's and live's words that stay: the
// common start and end, and between them each word movedWords keeps
// (classify.go: a longest common subsequence, ties broken each way) that
// both ways pair with the same partner. A word one way pairs with one
// partner and the other way with another has moved, as one either way
// skips has. Beyond maxMovedCells the words between the common start and
// end are cut at their anchors (anchorsOf), which stay, and each stretch
// between two anchors is read so alone; every word of a stretch still
// beyond the bound has moved, as there. In order.
func keptWords(rec, live []string) [][2]int {
	kept, _ := keptWordsIn(rec, live, true)
	return kept
}

// keptWordsIn is keptWords, cutting at anchors beyond maxMovedCells only
// where anchor is set (a stretch between two anchors is not cut again), and
// the cells of the tables it compared in.
func keptWordsIn(rec, live []string, anchor bool) (kept [][2]int, cells int) {
	p := 0
	for p < len(rec) && p < len(live) && rec[p] == live[p] {
		kept = append(kept, [2]int{p, p})
		p++
	}
	q := 0
	for q < len(rec)-p && q < len(live)-p && rec[len(rec)-1-q] == live[len(live)-1-q] {
		q++
	}
	r, l := rec[p:len(rec)-q], live[p:len(live)-q]
	switch {
	case len(r)*len(l) > maxMovedCells:
		if !anchor {
			break
		}
		from := [2]int{}
		for _, a := range append(anchorsOf(r, l), [2]int{len(r), len(l)}) {
			in, n := keptWordsIn(r[from[0]:a[0]], l[from[1]:a[1]], false)
			for _, k := range in {
				kept = append(kept, [2]int{p + from[0] + k[0], p + from[1] + k[1]})
			}
			if cells += n; a[0] < len(r) {
				kept = append(kept, [2]int{p + a[0], p + a[1]})
			}
			from = [2]int{a[0] + 1, a[1] + 1}
		}
	default:
		w := len(l) + 1
		cells = len(r) * len(l)
		lcs := make([]int32, (len(r)+1)*w)
		for i := len(r) - 1; i >= 0; i-- {
			for j := len(l) - 1; j >= 0; j-- {
				switch {
				case r[i] == l[j]:
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
					lcs[i*w+j] = lcs[(i+1)*w+j]
				default:
					lcs[i*w+j] = lcs[i*w+j+1]
				}
			}
		}
		partner := make([]int, len(r))
		for _, skipRec := range []bool{true, false} {
			i, j := 0, 0
			for i < len(r) && j < len(l) {
				switch {
				case r[i] == l[j] && lcs[i*w+j] == lcs[(i+1)*w+j+1]+1:
					if skipRec {
						partner[i] = j + 1
					} else if partner[i] == j+1 {
						kept = append(kept, [2]int{p + i, p + j})
					}
					i, j = i+1, j+1
				case lcs[(i+1)*w+j] > lcs[i*w+j+1] || lcs[(i+1)*w+j] == lcs[i*w+j+1] && skipRec:
					i++
				default:
					j++
				}
			}
		}
	}
	for k := q; k > 0; k-- {
		kept = append(kept, [2]int{len(rec) - k, len(live) - k})
	}
	return kept, cells
}

// textCut is one cut of a text's word diff (alignedCuts): R's word r and
// F's word l stay (kept), or, not kept, a bound before R's word r and F's
// word l, the edge of an aligned instant, where a stretch ends. loose: the
// stretch the cut ends holds an instant aligned with none (wordStretch).
type textCut struct {
	r, l        int
	kept, loose bool
}

// alignedCuts are the cuts of the word diff of rw and lw, R's and F's
// words (at rWords and lWords), the last one bounding the text's end, and
// tied, the pairs of R's instant and F's (of rSpans and lSpans) read as one
// instant across the runs (innerTied). Where both runs hold an instant, the
// text is diffed first as units (textUnits, keptWords): each word no
// instant holds, and each instant whole, every instant the same unit
// whatever its text, so that an instant stays with the other run's at its
// place. Each pair of instants aligned so is diffed word by word inside its
// bounds alone: the diff never pairs a word of one instant with a word of
// another, or with a word no instant holds ("07:06:07" to "07:07:09" keeps
// its "07" inside the one instant; "08:06 08:08" to "08:08 08:09" pairs each
// time with its own). Where only one run holds an instant there is none to
// align it with, and the text is diffed word by word, as where neither does.
func alignedCuts(rw, lw []string, rWords, lWords [][2]int, rSpans, lSpans []instant) (cuts []textCut, tied [][2]int) {
	if len(rSpans) == 0 || len(lSpans) == 0 {
		for _, k := range keptWords(rw, lw) {
			cuts = append(cuts, textCut{r: k[0], l: k[1], kept: true})
		}
		return append(cuts, textCut{r: len(rw), l: len(lw)}), nil
	}
	ru, lu := textUnits(rWords, rSpans), textUnits(lWords, lSpans)
	keys := func(words []string, us []textUnit) []string {
		out := make([]string, len(us))
		for i, u := range us {
			// No word holds a NUL byte: every instant is this one unit.
			if out[i] = words[u.from]; u.span >= 0 {
				out[i] = "\x00"
			}
		}
		return out
	}
	instantIn := func(us []textUnit, from, to int) bool {
		return slices.ContainsFunc(us[from:to], func(u textUnit) bool { return u.span >= 0 })
	}
	pr, pl := 0, 0
	for _, k := range append(keptWords(keys(rw, ru), keys(lw, lu)), [2]int{len(ru), len(lu)}) {
		loose := instantIn(ru, pr, k[0]) || instantIn(lu, pl, k[1])
		pr, pl = k[0]+1, k[1]+1
		if k[0] == len(ru) {
			cuts = append(cuts, textCut{r: len(rw), l: len(lw), loose: loose})
			break
		}
		r, l := ru[k[0]], lu[k[1]]
		if r.span < 0 {
			cuts = append(cuts, textCut{r: r.from, l: l.from, kept: true, loose: loose})
			continue
		}
		cuts = append(cuts, textCut{r: r.from, l: l.from, loose: loose})
		inner := keptWords(rw[r.from:r.to], lw[l.from:l.to])
		for _, p := range inner {
			cuts = append(cuts, textCut{r: r.from + p[0], l: l.from + p[1], kept: true})
		}
		cuts = append(cuts, textCut{r: r.to, l: l.to})
		if innerTied(rw[r.from:r.to], lw[l.from:l.to], inner) {
			tied = append(tied, [2]int{r.span, l.span})
		}
	}
	return cuts, tied
}

// readText reads a drifted text (rec, live) word by word (the reader's
// rules above), with no cache.
func readText(rec, live string) textReading { return readTextIn(nil, rec, live) }

// readTextIn is readText, its instants read through dc's text cache
// (spansOf; nil reads them now).
func readTextIn(dc *DriftCache, rec, live string) textReading {
	rd := textReading{Rec: rec, Live: live, RecWords: variables.Words(rec), LiveWords: variables.Words(live)}
	if rec == live {
		return rd
	}
	rw, lw := textsOf(rec, rd.RecWords), textsOf(live, rd.LiveWords)
	rSpans, lSpans := dc.spansOf(rec), dc.spansOf(live)
	rTok := tokenSpans(rec, rSpans)
	lTok := tokenSpans(live, lSpans)
	cuts, tied := alignedCuts(rw, lw, rd.RecWords, rd.LiveWords, rSpans, lSpans)
	paired := make([]int, len(rw))
	for i := range paired {
		paired[i] = -1
	}
	for _, c := range cuts {
		if c.kept {
			paired[c.r] = c.l
		}
	}
	pr, pl := 0, 0
	for _, c := range cuts {
		st := wordStretch{Rec: [2]int{pr, c.r}, Live: [2]int{pl, c.l}, Loose: c.loose}
		if pr, pl = c.r, c.l; c.kept {
			pr, pl = c.r+1, c.l+1
		}
		nr, nl := st.Rec[1]-st.Rec[0], st.Live[1]-st.Live[0]
		if nr == 0 && nl == 0 {
			continue
		}
		switch {
		case nr == nl && !st.Loose:
			st.Shape = stretchPaired
		case nr == 0 || nl == 0:
			st.Shape = stretchShapeChanged
		default:
			st.Shape = stretchUnequal
		}
		if nr > 0 && nl > 0 {
			st.Tokens = tokensAround(rec, live, rTok, lTok, bytesOf(rd.RecWords, st.Rec), bytesOf(rd.LiveWords, st.Live), false)
		}
		n := len(rd.Stretches)
		rd.Stretches = append(rd.Stretches, st)
		if st.Shape == stretchPaired {
			for d := 0; d < nr; d++ {
				i, j := st.Rec[0]+d, st.Live[0]+d
				paired[i] = j
				if rw[i] == lw[j] {
					continue
				}
				w := readWord{Rec: rw[i], Live: lw[j], RecIdx: i, LiveIdx: j, RecAt: rd.RecWords[i], LiveAt: rd.LiveWords[j], Stretch: n, RecBits: wordBits(rw[i]), LiveBits: wordBits(lw[j])}
				w.Tokens = tokensAround(rec, live, rTok, lTok, w.RecAt, w.LiveAt, true)
				rd.Words = append(rd.Words, w)
			}
			continue
		}
		for i := st.Rec[0]; i < st.Rec[1]; i++ {
			rd.Words = append(rd.Words, readWord{Rec: rw[i], RecIdx: i, LiveIdx: -1, RecAt: rd.RecWords[i], LiveAt: [2]int{-1, -1}, Stretch: n, RecBits: wordBits(rw[i])})
		}
		for j := st.Live[0]; j < st.Live[1]; j++ {
			rd.Words = append(rd.Words, readWord{Live: lw[j], RecIdx: -1, LiveIdx: j, RecAt: [2]int{-1, -1}, LiveAt: rd.LiveWords[j], Stretch: n, LiveBits: wordBits(lw[j])})
		}
	}
	rd.readSeparators(paired, rTok, lTok)
	rd.readInstants(rSpans, lSpans, tied)
	rd.readShape()
	rd.readEncoded(dc, rTok, lTok)
	return rd
}

// readShape marks the text changed in shape where a stretch holds an
// instant the alignment aligned with none (Loose), or where a stretch only
// one run has holds a word that no instant paired across the runs holds. A
// word such an instant holds is one of the instant's, which is read whole
// and judged by its moment (a minute crossed, 08:59:00 to 09:00:01, leaves
// the "00" of R's seconds kept with F's minutes inside the instant; a
// fraction dropped), not a word the text gained or lost.
func (rd *textReading) readShape() {
	for _, w := range rd.Words {
		if st := rd.Stretches[w.Stretch]; st.Loose {
			rd.ChangedShape = true
			return
		} else if st.Shape != stretchShapeChanged {
			continue
		}
		if iw := w.Instant; iw == nil || iw.RecAt[0] < 0 || iw.LiveAt[0] < 0 {
			rd.ChangedShape = true
			return
		}
	}
}

// sepChange is a separator that changed between two paired words, or
// before the first or after the last where those pair: [start, end) of
// its bytes in each run, and the whole tokens that hold it with the words
// beside it in both runs. Word by word, replay cannot give the new
// separator, so the words around it bind only as one of Tokens; with none,
// the text changed shape.
type sepChange struct {
	RecAt, LiveAt [2]int
	Tokens        []wholeToken
}

// readSeparators lists the separators that changed between paired words
// (paired: R's word index to F's, -1 where it moved), and marks the text
// changed in shape where no whole token holds one. A text with no words in
// either run changed in its separators alone.
func (rd *textReading) readSeparators(paired []int, rTok, lTok wholeTokenSpans) {
	// The text's start and end stand as one more paired word in each run.
	rw := append(append([][2]int{{0, 0}}, rd.RecWords...), [2]int{len(rd.Rec), len(rd.Rec)})
	lw := append(append([][2]int{{0, 0}}, rd.LiveWords...), [2]int{len(rd.Live), len(rd.Live)})
	pj := []int{0}
	for _, j := range paired {
		pj = append(pj, j+min(1, j+1)) // -1 stays -1
	}
	pj = append(pj, len(lw)-1)
	for i := 0; i+1 < len(rw); i++ {
		j := pj[i]
		r, l := [2]int{rw[i][1], rw[i+1][0]}, [2]int{lw[max(j, 0)][1], lw[max(j, 0)+1][0]}
		if j < 0 || pj[i+1] != j+1 || rd.Rec[r[0]:r[1]] == rd.Live[l[0]:l[1]] {
			continue
		}
		sc := sepChange{RecAt: r, LiveAt: l, Tokens: tokensAround(rd.Rec, rd.Live, rTok, lTok, [2]int{rw[i][0], rw[i+1][1]}, [2]int{lw[j][0], lw[j+1][1]}, false)}
		rd.Separators = append(rd.Separators, sc)
		rd.ChangedShape = rd.ChangedShape || len(sc.Tokens) == 0
	}
}

// bytesOf is [start, end) of the words [from, to) of spans.
func bytesOf(spans [][2]int, r [2]int) [2]int { return [2]int{spans[r[0]][0], spans[r[1]-1][1]} }

// wholeTokenSpans are a text's whole tokens of each kind, [start, end) each, in
// order.
type wholeTokenSpans struct{ id, b64 [][2]int }

// tokenSpans are s's whole tokens: its idByte runs, and its base64 runs
// that stand whole outside its instants (spans: eachB64Run).
func tokenSpans(s string, spans []instant) wholeTokenSpans {
	var t wholeTokenSpans
	off := 0
	for _, seg := range segmentsBy(s, idByte) {
		if seg.word {
			t.id = append(t.id, [2]int{off, off + len(seg.text)})
		}
		off += len(seg.text)
	}
	eachB64Run(s, spans, func(a, b int) bool {
		t.b64 = append(t.b64, [2]int{a, b})
		return true
	})
	return t
}

// spanHolding is the index of the span of spans (in order; bounds gives each
// one's [start, end)) that holds at whole, or -1.
func spanHolding[S any](spans []S, bounds func(S) (int, int), at [2]int) int {
	for k := 0; k < len(spans) && at[0] >= 0; k++ {
		if s, e := bounds(spans[k]); s <= at[0] && at[1] <= e {
			return k
		} else if s >= at[1] {
			break
		}
	}
	return -1
}

func pairBounds(p [2]int) (int, int) { return p[0], p[1] }

// tokensAround are the whole tokens that hold r (of rec) and l (of live)
// whole, each in both runs (where longer, one longer than what it holds in
// either run: a word's own text is no token around it); an idByte token
// first, then a base64 run that is not the same text.
func tokensAround(rec, live string, rt, lt wholeTokenSpans, r, l [2]int, longer bool) []wholeToken {
	var out []wholeToken
	for _, k := range []tokenKind{tokenID, tokenB64} {
		rs, ls := rt.id, lt.id
		if k == tokenB64 {
			rs, ls = rt.b64, lt.b64
		}
		ri, li := spanHolding(rs, pairBounds, r), spanHolding(ls, pairBounds, l)
		if ri < 0 || li < 0 || longer && rs[ri] == r && ls[li] == l {
			continue
		}
		ra, la := rs[ri], ls[li]
		tok := wholeToken{Kind: k, Rec: rec[ra[0]:ra[1]], Live: live[la[0]:la[1]], RecAt: ra, LiveAt: la}
		if len(out) > 0 && out[0].Rec == tok.Rec && out[0].Live == tok.Live {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// chanceBits is the fewest bits (wordBits) a word carries for its text alone
// to be evidence of where a value came from. In api-server's set of 7186
// UUIDs, 83% of the 4096 values of a v4 UUID's third group stand, so a
// fresh UUID shares a 4-hex group with the set nine times in ten; a word of
// 32 bits does so about once in four billion.
const chanceBits = 32

// wordBits is about how many bits t carries by its characters: their count
// times log2 of the size of the one class all of them fall in (digits 10,
// hex digits 16, letters 26, letters and digits 36; case is not read). A
// separator ('-', '_') adds no bits; a letter or digit beyond ASCII counts as
// one; any other byte counts 8 bits. It is learning's one definition.
func wordBits(t string) float64 {
	n, other := 0, 0
	var digits, hex, letters bool
	for i := 0; i < len(t); {
		r, w := utf8.DecodeRuneInString(t[i:])
		i += w
		switch {
		case r == '-' || r == '_':
		case r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F':
			n, hex = n+1, true
		case unicode.IsDigit(r):
			n, digits = n+1, true
		case unicode.IsLetter(r):
			n, letters = n+1, true
		default:
			other += w
		}
	}
	size := 0.0
	switch {
	case letters && digits:
		size = 36
	case letters:
		size = 26
	case hex:
		size = 16
	case digits:
		size = 10
	}
	bits := 8 * float64(other)
	if size > 0 {
		bits += float64(n) * math.Log2(size)
	}
	return bits
}
