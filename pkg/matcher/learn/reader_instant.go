package learn

import (
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The reader's instants. An instant is judged whole, never word by
// word:
//   - every typed datetime (an Instant leaf: a BSON date), whole
//     (readTypedPair);
//   - every token that parses in main's layouts (clockLayouts, read by
//     instantSpans), zoned text included ("+05:30": the zone is the
//     instant's, never a base64 run's);
//   - every number of 10, 13, 16 or 19 digits in either run: one readEpoch
//     takes (2000-2100) by its moment, one it does not by its exact text.
//
// A place holds an instant by moment (byMoment): main's cuts, one rule
// (truncate or round, at one unit) naming its moment. A cut finer than one
// millisecond is fine; a millisecond or coarser is coarse (fineCut). Two
// reads that share a fine cut are a copy; two that share a coarse cut may be
// two reads, one database round trip often fitting in a millisecond.

// instantWord is one instant the changed words of a value are part of: its
// moment in each run (nil where that run's text there reads as none, or
// names none: a time-ordered UUID's time groups), its bytes in each run
// ([-1, -1] where none), the layout each run's text matched (clockLayouts;
// -1 where none), whether its protocol types it (Typed), whether it is a
// number of an epoch's length in either run (Number), and whether such a
// number is to be matched by its exact text (ByText: it does not parse in a
// run that has it). Encoded: it stands inside a whole token's decoded form
// (a base64 cursor glued after "cursor="), where no address names it: its
// bytes are [-1, -1] in both runs and the caller refuses it (readEncoded).
type instantWord struct {
	Rec, Live                      *moment
	RecAt, LiveAt                  [2]int
	RecLayout, LiveLayout          int
	Typed, Number, ByText, Encoded bool
}

// fineCut reports whether res, the cut of an instant, is fine: finer than
// one millisecond.
func fineCut(res time.Duration) bool { return res > 0 && res < time.Millisecond }

// fine reports whether both runs' moments are at a fine cut.
func (iw *instantWord) fine() bool {
	return iw != nil && iw.Rec != nil && iw.Live != nil && fineCut(iw.Rec.res) && fineCut(iw.Live.res)
}

// epochDigits reports whether s is a number of an epoch's length: 10, 13,
// 16 or 19 digits.
func epochDigits(s string) bool {
	switch len(s) {
	case 10, 13, 16, 19:
		return isDigitsOnly(s)
	}
	return false
}

// spanAt is the index of the instant of spans that holds at whole, -1
// where none does (or at is none).
func spanAt(spans []instant, at [2]int) int {
	return spanHolding(spans, func(sp instant) (int, int) { return sp.start, sp.end }, at)
}

// textUnit is one unit of a run's text in the alignment of its instants
// (alignedCuts): the run's words [from, to), and the instant of the run's
// spans that holds them whole (span; -1 for a word no instant holds, a unit
// of its own).
type textUnit struct{ from, to, span int }

// textUnits are a run's words (in order) as units: each instant of spans
// (disjoint, in order) one unit of the words it holds whole, each other word
// one unit.
func textUnits(words [][2]int, spans []instant) []textUnit {
	var out []textUnit
	k := 0
	for i, w := range words {
		var in bool
		k, in = instantNext(spans, w, k)
		if in && len(out) > 0 && out[len(out)-1].span == k {
			out[len(out)-1].to = i + 1
			continue
		}
		u := textUnit{from: i, to: i + 1, span: -1}
		if in {
			u.span = k
		}
		out = append(out, u)
	}
	return out
}

// instantNext is k moved past the instants of spans (disjoint, in order) that
// end at or before w begins, and whether the instant it then names holds w
// whole: from k on, the first that may. Read over a run's words in order,
// it walks spans once.
func instantNext(spans []instant, w [2]int, k int) (int, bool) {
	for k < len(spans) && spans[k].end <= w[0] {
		k++
	}
	return k, k < len(spans) && spans[k].start <= w[0] && w[1] <= spans[k].end
}

// innerTied reports whether two aligned instants, of R's words rw and F's
// lw, are one instant read whole across the runs: they hold as many words
// (one shape in both runs, whatever stayed: 07:06:07 to 07:07:09 keeps R's
// seconds with F's minute), or a stretch of their word diff (kept, inner
// indices) holds words of both runs, a word that changed or moved. Where
// neither holds, one run's instant gained or lost words and nothing else
// ("10:50" to "10:50:13"): its words are a change of the text's shape.
func innerTied(rw, lw []string, kept [][2]int) bool {
	if len(rw) == len(lw) {
		return true
	}
	pr, pl := 0, 0
	for _, k := range append(kept[:len(kept):len(kept)], [2]int{len(rw), len(lw)}) {
		if k[0] > pr && k[1] > pl {
			return true
		}
		pr, pl = k[0]+1, k[1]+1
	}
	return false
}

// partner is m[k] where k's partner names k back (the other map), -1
// otherwise.
func partner(m, back map[int]int, k int) int {
	if v, ok := m[k]; ok && v >= 0 && back[v] == k {
		return v
	}
	return -1
}

// readInstants sets each changed word's instant: the instant of rSpans and
// of lSpans (instantSpans of each run) that holds it, one instantWord per
// pair of them, and a number of an epoch's length in either run, parsed or
// not. A word only one run has takes its instant's partner in the other
// run: tied, the instants the alignment paired (alignedCuts, innerTied), one
// partner each at most; an instant it left alone has none.
func (rd *textReading) readInstants(rSpans, lSpans []instant, tied [][2]int) {
	byPair := map[[2]int]*instantWord{}
	rp, lp := map[int]int{}, map[int]int{}
	for _, t := range tied {
		rp[t[0]], lp[t[1]] = t[1], t[0]
	}
	for k := range rd.Words {
		w := &rd.Words[k]
		ri, li := spanAt(rSpans, w.RecAt), spanAt(lSpans, w.LiveAt)
		if w.LiveIdx < 0 && ri >= 0 {
			li = partner(rp, lp, ri)
		}
		if w.RecIdx < 0 && li >= 0 {
			ri = partner(lp, rp, li)
		}
		num := epochDigits(w.Rec) || epochDigits(w.Live)
		if ri < 0 && li < 0 && !num {
			continue
		}
		key := [2]int{ri, li}
		if ri < 0 && li < 0 {
			key = [2]int{-2 - k, -2 - k}
		}
		iw := byPair[key]
		if iw == nil {
			iw = &instantWord{RecAt: w.RecAt, LiveAt: w.LiveAt, RecLayout: -1, LiveLayout: -1}
			if ri >= 0 {
				iw.RecAt, iw.RecLayout = [2]int{rSpans[ri].start, rSpans[ri].end}, rSpans[ri].layout
				if rSpans[ri].keyed {
					m := rSpans[ri].moment()
					iw.Rec = &m
				}
			}
			if li >= 0 {
				iw.LiveAt, iw.LiveLayout = [2]int{lSpans[li].start, lSpans[li].end}, lSpans[li].layout
				if lSpans[li].keyed {
					m := lSpans[li].moment()
					iw.Live = &m
				}
			}
			byPair[key] = iw
			rd.Instants = append(rd.Instants, iw)
		}
		if num {
			iw.Number = true
			if ri < 0 && w.Rec != "" || li < 0 && w.Live != "" {
				iw.ByText = true
			}
		}
		w.Instant = iw
	}
}

// readEncoded gives each changed word no instant holds, whose whole token
// (rTok, lTok: an idByte token or a base64 run, around the word or, for a
// word the other run has no partner for, its stretch) holds in its other
// declared forms (rawExtra: its encoded runs' text, where main reads an
// instant) an instant that differs between the runs, that instant:
// Encoded, at no bytes, with each run's moment (nil where none differs).
// No address names an instant there, so the caller refuses the word rather
// than learn it fresh. A shape that changed is refused whole already.
func (rd *textReading) readEncoded(dc *DriftCache, rTok, lTok wholeTokenSpans) {
	byTok := map[[2]string]*instantWord{}
	for k := range rd.Words {
		w := &rd.Words[k]
		st := rd.Stretches[w.Stretch]
		if w.Instant != nil || st.Shape == stretchShapeChanged {
			continue
		}
		r, l := w.RecAt, w.LiveAt
		if r[0] < 0 {
			r = bytesOf(rd.RecWords, st.Rec)
		}
		if l[0] < 0 {
			l = bytesOf(rd.LiveWords, st.Live)
		}
		for _, tok := range tokensAround(rd.Rec, rd.Live, rTok, lTok, r, l, false) {
			key := [2]string{tok.Rec, tok.Live}
			iw, ok := byTok[key]
			if !ok {
				iw = encodedInstant(dc, key[0], key[1])
				byTok[key] = iw
				if iw != nil {
					rd.Instants = append(rd.Instants, iw)
				}
			}
			if iw != nil {
				w.Instant = iw
				break
			}
		}
	}
}

// encodedInstant is the instant rec's and live's other declared forms hold
// that differs between them (the first instant of each that the other's
// forms do not hold by its text), Encoded; nil where they hold the same
// instants.
func encodedInstant(dc *DriftCache, rec, live string) *instantWord {
	re, le := dc.rawExtra(rec), dc.rawExtra(live)
	rs, ls := dc.spansOf(re), dc.spansOf(le)
	iw := &instantWord{Encoded: true, RecAt: [2]int{-1, -1}, LiveAt: [2]int{-1, -1}, RecLayout: -1, LiveLayout: -1}
	pick := func(s string, spans []instant, other string, others []instant, at **moment, layout *int) {
		for _, sp := range spans {
			text := s[sp.start:sp.end]
			if !sp.keyed || slices.ContainsFunc(others, func(o instant) bool { return other[o.start:o.end] == text }) {
				continue
			}
			m := sp.moment()
			*at, *layout = &m, sp.layout
			return
		}
	}
	pick(re, rs, le, ls, &iw.Rec, &iw.RecLayout)
	pick(le, ls, re, rs, &iw.Live, &iw.LiveLayout)
	if iw.Rec == nil && iw.Live == nil {
		return nil
	}
	return iw
}

// readTypedPair reads a leaf its protocol types as one instant (readTyped),
// in each run, whole: ok false where neither run's leaf is one.
func readTypedPair(rec, live Leaf) (*instantWord, bool) {
	rt, rok := readTyped(rec)
	lt, lok := readTyped(live)
	if !rok && !lok {
		return nil, false
	}
	iw := &instantWord{Typed: true, RecAt: [2]int{0, len(rec.Value)}, LiveAt: [2]int{0, len(live.Value)}, RecLayout: -1, LiveLayout: -1}
	if rok {
		m := rt.moment(rt.res)
		iw.Rec = &m
	}
	if lok {
		m := lt.moment(lt.res)
		iw.Live = &m
	}
	return iw, true
}

// byMoment reports whether held, an instant a place holds, names m's moment
// by main's cuts (covers): the same moment, or one rule (truncate or round,
// at one unit, in one zone) that gives it. fine: the finest such cut is
// finer than one millisecond (fineCut); the same moment is cut at the
// coarser of the two resolutions.
func byMoment(held, m moment) (ok, fine bool) {
	if held.key == m.key {
		return true, fineCut(max(held.res, m.res))
	}
	var finest int64
	cuts(held, m, func(c cutRule) bool {
		if finest == 0 || c.unit < finest {
			finest = c.unit
		}
		return false
	})
	if finest == 0 {
		return false, false
	}
	return true, fineCut(time.Duration(finest))
}

// viewAnswerMoments are the instants of lv's answer, a view F gave the
// application of a recording (Raw.viewOf), read as the recording's answer
// is (readAnswerMoments: GivenPlaces.answer), each name a copy: kept by the
// set with the view's words (viewLists.moments), so that it reads the view
// once. Under the proven switch only: nil with it off, where nothing reads
// them.
func viewAnswerMoments(lv Raw) []givenMoment {
	if !provenswitch.On() {
		return nil
	}
	out := NewDriftCache().readAnswerMoments(lv)
	for i := range out {
		a := &out[i]
		a.mock, a.where = strings.Clone(a.mock), strings.Clone(a.where)
	}
	return out
}

// viewMoments are round F's view of the instants of m's recording, which F
// gave the application earlier in the set (GivenPlaces.fView): the instants
// of each different view the serves of it the read saw were told with
// (Order.viewsLocked, ServedMockLive), as the set keeps them
// (setPlaces.viewWords), each standing where the recording does in time,
// as its answer's are read (GivenPlaces.answer): a view is read as the
// recording's Raw (viewRaw: its at, whatever the view's own stamps say),
// and, as the recording's own answer is here, with no call's late or
// noTime. None where every serve was told as recorded (F gave the
// recording itself: answer), where m's recording holds no answer
// (answers), and where keploy cannot tell what F gave there (w.unstated,
// w.unread: its words say so, and their reader refuses); none with the
// proven switch off. unread is w.unread, or true where a view its words
// were read from cannot be read again (fail closed).
func (o Order) viewMoments(m *models.Mock, w givenWords, answers bool) (moments []givenMoment, unread bool) {
	if !provenswitch.On() || o.t == nil || !answers || w.unstated || w.unread {
		return nil, w.unread
	}
	id := MockID(m)
	o.t.mu.Lock()
	_, keys, loads, viewUnread, ok := o.viewsLocked(id)
	o.t.mu.Unlock()
	if !ok || viewUnread {
		// statedWords read the same statements: w says so.
		return nil, w.unread
	}
	for k, load := range loads {
		v, read := o.places.viewWords(m, id, keys[k], load)
		if !read {
			return nil, true
		}
		moments = append(moments, v.moments...)
	}
	return moments, false
}
