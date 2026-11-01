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

// spanPartners pairs R's instants (rSpans) with F's (lSpans): the two a
// paired word lies in, and, in a stretch of unequal counts, the instants
// R's moved words lie in with those F's lie in, by position, where they
// are as many. An instant given two partners, or a paired word whose other
// run's word lies in none, keeps no partner (-1).
func (rd *textReading) spanPartners(rSpans, lSpans []instant) (rp, lp map[int]int) {
	rp, lp = map[int]int{}, map[int]int{}
	add := func(ri, li int) {
		if v, ok := rp[ri]; ri >= 0 && (!ok || v == li) {
			rp[ri] = li
		} else if ri >= 0 {
			rp[ri] = -1
		}
		if v, ok := lp[li]; li >= 0 && (!ok || v == ri) {
			lp[li] = ri
		} else if li >= 0 {
			lp[li] = -1
		}
	}
	for _, w := range rd.Words {
		if w.RecIdx >= 0 && w.LiveIdx >= 0 {
			add(spanAt(rSpans, w.RecAt), spanAt(lSpans, w.LiveAt))
		}
	}
	for _, st := range rd.Stretches {
		if st.Shape != stretchUnequal {
			continue
		}
		rs := spansOfWords(rSpans, rd.RecWords, st.Rec)
		ls := spansOfWords(lSpans, rd.LiveWords, st.Live)
		if len(rs) != len(ls) {
			continue
		}
		for k := range rs {
			add(rs[k], ls[k])
		}
	}
	return rp, lp
}

// spansOfWords are the instants of spans that the words [from, to) of a
// run lie in, each once, in order.
func spansOfWords(spans []instant, words [][2]int, r [2]int) []int {
	var out []int
	for i := r[0]; i < r[1]; i++ {
		if k := spanAt(spans, words[i]); k >= 0 && (len(out) == 0 || out[len(out)-1] != k) {
			out = append(out, k)
		}
	}
	return out
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
// run (spanPartners).
func (rd *textReading) readInstants(rSpans, lSpans []instant) {
	byPair := map[[2]int]*instantWord{}
	rp, lp := rd.spanPartners(rSpans, lSpans)
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
