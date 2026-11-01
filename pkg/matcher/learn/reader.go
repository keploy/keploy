package learn

import (
	"math"
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
//     (classify.go): a longest common subsequence, ties broken each way.
//     The reader keeps a word only where both ways pair it with the same
//     word of the other run (keptWords), so every kept word has one
//     partner and the words between two kept words are one stretch.
//   - A stretch of equal counts pairs its words by position. One of unequal
//     counts pairs nothing: it binds only as a whole token both runs hold
//     there, or is fresh or refused (the caller decides). A stretch only
//     one run has ("a,b" to "a,b,c") is a changed shape.
//   - A whole token is main's idByte token (classify.go), or a base64 run
//     (readings.go's nextB64Run) outside the text's instants: a zone's sign
//     glued to an instant's seconds ("13+05" of "…16:20:13+05:30") is the
//     instant's, never a run. The reader lists the whole tokens around each
//     changed word (and around a stretch) in both runs; one binds as one
//     value only where one place holds it whole in each run (the index's
//     holdsToken).
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

// wordStretch is the moved words between two kept words: the word index
// range [from, to) of each run (empty in a run that has none there), and
// the whole tokens that hold every moved word of it in both runs.
type wordStretch struct {
	Shape     stretchShape
	Rec, Live [2]int
	Tokens    []wholeToken
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
// their words, the stretches, every changed word in order, the instants
// those words are part of, and whether any stretch changed shape.
type textReading struct {
	At                  variables.Address
	Off                 [2]int
	Rec, Live           string
	RecWords, LiveWords [][2]int
	Stretches           []wordStretch
	Words               []readWord
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
// skips has. Beyond maxMovedCells every word between the common start and
// end has moved, as there. In order.
func keptWords(rec, live []string) [][2]int {
	var kept [][2]int
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
	if len(r) > 0 && len(l) > 0 && len(r)*len(l) <= maxMovedCells {
		w := len(l) + 1
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
	return kept
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
	prev := [2]int{-1, -1}
	for _, k := range append(keptWords(rw, lw), [2]int{len(rw), len(lw)}) {
		st := wordStretch{Rec: [2]int{prev[0] + 1, k[0]}, Live: [2]int{prev[1] + 1, k[1]}}
		prev = k
		nr, nl := st.Rec[1]-st.Rec[0], st.Live[1]-st.Live[0]
		if nr == 0 && nl == 0 {
			continue
		}
		switch {
		case nr == nl:
			st.Shape = stretchPaired
		case nr == 0 || nl == 0:
			st.Shape = stretchShapeChanged
			rd.ChangedShape = true
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
	rd.readInstants(rSpans, lSpans)
	return rd
}

// bytesOf is [start, end) of the words [from, to) of spans.
func bytesOf(spans [][2]int, r [2]int) [2]int { return [2]int{spans[r[0]][0], spans[r[1]-1][1]} }

// wholeTokenSpans are a text's whole tokens of each kind, [start, end) each, in
// order.
type wholeTokenSpans struct{ id, b64 [][2]int }

// tokenSpans are s's whole tokens: its idByte runs, and its base64 runs
// (nextB64Run) outside its instants (spans).
func tokenSpans(s string, spans []instant) wholeTokenSpans {
	var t wholeTokenSpans
	for i := 0; i < len(s); {
		if !idByte(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && idByte(s[j]) {
			j++
		}
		t.id = append(t.id, [2]int{i, j})
		i = j
	}
	from := 0
	gap := func(to int) {
		for at := from; at < to; {
			start, end, ok := nextB64Run(s[:to], at)
			if !ok {
				return
			}
			t.b64 = append(t.b64, [2]int{start, end})
			at = end
		}
	}
	for _, in := range spans {
		gap(in.start)
		from = in.end
	}
	gap(len(s))
	return t
}

// around is the span of spans that holds [a[0], a[1]) whole, ok false where
// none does.
func around(spans [][2]int, a [2]int) ([2]int, bool) {
	for _, sp := range spans {
		if sp[0] <= a[0] && a[1] <= sp[1] {
			return sp, true
		}
		if sp[0] >= a[1] {
			break
		}
	}
	return [2]int{}, false
}

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
		ra, ok := around(rs, r)
		if !ok {
			continue
		}
		la, ok := around(ls, l)
		if !ok || longer && ra == r && la == l {
			continue
		}
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
