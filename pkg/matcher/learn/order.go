package learn

import (
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The order (DESIGN §3): every changed word of a call, read by the one
// reader, is decided here. A word that is not an instant is a Value bound at
// the earliest source of the call's scope or the start-up; refused where a
// place disproves it by its text or leaves it unknown, or where its only
// source lies outside that scope (naming the edit); else made anew each
// run, fresh. An instant no earlier place holds is a Clock where its call's
// window holds it. Every case this file does not decide yet is refused: it
// fails closed.

func init() { decideBy = decideOrder }

// decideOrder is Decide's order: Mode Elsewhere's guard around the words
// decided with every earlier place in scope, or the call's own order.
func decideOrder(in Input) []Decision {
	if in.Mode == ModeElsewhere {
		ds, fields := decideWords(in, true)
		return elsewhere(in, ds, fields)
	}
	ds, _ := decideWords(in, false)
	return ds
}

// decider is one call's state: its input, places, index and scope.
type decider struct {
	in       Input
	ps       []evPlace
	x        *readerIndex
	allScope bool
}

// decideWords decides every changed word of in, and the field each
// decision is of; allScope reads every earlier place of the run as the
// call's scope (Mode Elsewhere).
func decideWords(in Input, allScope bool) ([]Decision, []string) {
	ps, unread := placesOf(in)
	dc := &decider{in: in, ps: ps, x: newReaderIndex(in.Drifts), allScope: allScope}
	var out []Decision
	var fields []string // fields[i] is the field out[i] decides
	for _, path := range in.Drift {
		out = append(out, dc.leaf(path, unread)...)
		for len(fields) < len(out) {
			fields = append(fields, path)
		}
	}
	if dc.x.unread() {
		// A run past its budget: every change of the call is refused.
		for i, d := range out {
			out[i] = dc.refuse(fields[i], d.At, d.At.Word, d.Recorded, d.Full, RefusalUnread,
				"keploy could not read the run's values past their budget, which came before this call", nil,
				Edit{Kind: EditReRecord}, dc.fieldNoise(fields[i]))
		}
	}
	return out, fields
}

// leaf decides the changed words of the drifted leaf at path.
func (dc *decider) leaf(path, unread string) []Decision {
	rec, live := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	at := addressOf(dc.in, rec)
	switch {
	case unread != "":
		return []Decision{dc.refuse(path, at, -1, rec.Value, live.Value, RefusalUnread,
			"keploy could not read "+unread+", which came before this call", nil,
			Edit{Kind: EditReRecord}, dc.fieldNoise(path))}
	case rec.Unnamed || live.Unnamed:
		return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, "no path names this field alone")}
	case rec.Type == Instant || live.Type == Instant:
		r, okR := readTyped(rec)
		l, okL := readTyped(live)
		iw, _ := readTypedPair(rec, live)
		if !okR || !okL || iw == nil || iw.Rec == nil || iw.Live == nil {
			return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, "one run's value here is no instant")}
		}
		return []Decision{dc.instant(path, at, -1, changedInstant{r: rec.Value, l: live.Value, ri: r, li: l, rm: iw.Rec, lm: iw.Live})}
	}
	return dc.text(path, at, rec.Value, live.Value)
}

// text decides the changed words of one drifted text.
func (dc *decider) text(path string, at variables.Address, rec, live string) []Decision {
	rd := readTextIn(dc.in.Drifts, rec, live)
	if rd.ChangedShape {
		return []Decision{dc.refuse(path, at, -1, rec, live, RefusalChangedShape, "a word was added or dropped", nil, dc.fieldNoise(path))}
	}
	var out []Decision
	done := map[*instantWord]bool{}
	for _, w := range rd.Words {
		switch {
		case w.Instant != nil:
			if done[w.Instant] {
				continue
			}
			done[w.Instant] = true
			out = append(out, dc.textInstant(path, at, rec, live, w))
		case w.RecIdx < 0 || w.LiveIdx < 0:
			return append(out, dc.unpaired(path, at, rec, live, rd))
		default:
			out = append(out, dc.word(path, at, rec, live, w))
		}
	}
	if len(out) == 0 && rec != live {
		out = append(out, dc.cannotCheck(path, at, -1, rec, live, "only what separates its words changed"))
	}
	return out
}

// unpaired decides a text where a word was added or dropped: refused whole
// as unknown where a place keploy reads holds a word of what changed (it
// may be a source of part of it), else as a changed shape.
func (dc *decider) unpaired(path string, at variables.Address, rec, live string, rd textReading) Decision {
	var held []evPlace
	for _, p := range dc.ps {
		for _, w := range rd.Words {
			hr := w.Rec != "" && evHolds(dc.x, p.rec, w.Rec)
			hl := w.Live != "" && evHolds(dc.x, p.live, w.Live)
			if hr || hl {
				held = append(held, p)
				break
			}
		}
	}
	if len(held) == 0 {
		return dc.refuse(path, at, -1, rec, live, RefusalChangedShape, "a word was added or dropped", nil, dc.fieldNoise(path))
	}
	return dc.refuse(path, at, -1, rec, live, RefusalUnknown,
		"keploy cannot tell what round F gave at "+held[0].name+" before this call (a word was added or dropped beside one it holds)", names(held),
		Edit{Kind: EditReRecord}, dc.fieldNoise(path))
}

// evHolds reports whether text holds the word w, read or not.
func evHolds(x *readerIndex, text, w string) bool {
	h, _ := x.holdsWord(text, w)
	return h
}

// word decides one changed word that is no instant (DESIGN §3, any other
// word).
func (dc *decider) word(path string, at variables.Address, rec, live string, w readWord) Decision {
	n := w.RecIdx
	ps := append([]evPlace(nil), dc.ps...)
	cand := map[string]bool{}
	for _, p := range ps {
		if ev, _ := evidenceAt(dc.x, p, w.Rec, w.Live); ev != evNone {
			cand[p.mock] = true
		}
	}
	orderUnknown(dc.in, ps, cand)
	var src, outside, conflict, unknown []evPlace
	for _, p := range ps {
		ev, read := evidenceAt(dc.x, p, w.Rec, w.Live)
		if !read {
			unknown = append(unknown, evPlace{name: p.name, why: "past the call's budget"})
			continue
		}
		if ev == evNone || ev == evDisproven {
			if tokenShown(dc.x, p, w) {
				ev = evShown
			}
		}
		switch ev {
		case evShown:
			if dc.allScope || !p.otherTest {
				src = append(src, p)
			} else {
				outside = append(outside, p)
			}
		case evDisproven:
			if p.gave || countsByText(dc.x, p, w) || (dc.allScope || !p.otherTest) && heldWhole(dc.x, p, w, rec) {
				conflict = append(conflict, p)
			}
		case evUnknown:
			unknown = append(unknown, p)
		}
	}
	switch {
	case len(src) > 0:
		first := src[0]
		for _, p := range src[1:] {
			if p.at.Before(first.at) {
				first = p
			}
		}
		if d, ok := dc.atWord(path, at, n, w, rec, live); !ok {
			return d
		}
		d := dc.learned(path, at, n, w.Rec, w.Live, OutcomeValue, "bound to "+first.name+", the earliest place before the call that shows it in both runs", names(src))
		d.From = Place{Mock: first.mock, Side: first.side, Path: first.path, Live: w.Live}
		return d
	case len(conflict) > 0:
		p := conflict[0]
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalConflict,
			w.Rec+" stood whole at "+p.name+"; round F gave "+gaveAt(p, w.Live)+" there and the value did not follow", names(conflict),
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: lastSegment(path)})
	case len(unknown) > 0:
		p := unknown[0]
		why := p.why
		if why == "" {
			why = "both values hold it, or only part of a word"
		}
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalUnknown,
			"keploy cannot tell what round F gave at "+p.name+" before this call ("+why+")", names(unknown),
			Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
	case len(outside) > 0:
		p := outside[0]
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalAnotherScope,
			"the value "+p.name+" sent in "+scopeName(p)+"; keploy binds only within a test and the start-up", names(outside),
			Edit{Kind: EditMoveToMocks, Mock: p.mock}, Edit{Kind: EditMoveToStartup, Mock: p.mock}, dc.wordNoise(path, n))
	}
	if d, ok := dc.atWord(path, at, n, w, rec, live); !ok {
		return d
	}
	d := dc.learned(path, at, n, w.Rec, w.Live, OutcomeFresh, "no place before the call shows it; bound at its first call", nil)
	d.Echo = dc.echoes(w.Rec)
	if len(d.Echo) > 0 && !dc.in.Caps.AnswerAtWord {
		return dc.cannotCheck(path, at, -1, rec, live, "the adapter cannot answer a word inside its answer yet")
	}
	for i, a := range d.Echo {
		if a.Place.Part == 0 {
			return dc.cannotCheck(path, at, -1, rec, live, "its recorded answer echoes it at "+dc.echoPath(w.Rec, i)+", a place keploy cannot name to the adapter yet")
		}
	}
	return d
}

// atWord refuses a word inside a longer value where the adapter cannot
// bind at a word address.
func (dc *decider) atWord(path string, at variables.Address, n int, w readWord, rec, live string) (Decision, bool) {
	if dc.in.Caps.BindAtWord || (w.Rec == rec && w.Live == live) {
		return Decision{}, true
	}
	return dc.cannotCheck(path, at, -1, rec, live, "the adapter cannot bind a word inside this field yet"), false
}

// tokenShown reports whether p shows a whole token around w: one place
// holds that whole token in each run (DESIGN §1).
func tokenShown(x *readerIndex, p evPlace, w readWord) bool {
	if p.state != stateBefore || p.rec == p.live {
		return false
	}
	for _, t := range w.Tokens {
		hr, _ := x.holdsToken(p.rec, t.Kind, t.Rec)
		hl, _ := x.holdsToken(p.live, t.Kind, t.Live)
		if hr && hl {
			return true
		}
	}
	return false
}

// textInstant decides an instant the changed words of a text are part of;
// a number that does not parse as one is matched by its exact text.
func (dc *decider) textInstant(path string, at variables.Address, rec, live string, w readWord) Decision {
	iw := w.Instant
	if iw.RecAt[0] < 0 || iw.LiveAt[0] < 0 || (iw.Rec == nil || iw.Live == nil) && !iw.ByText {
		return dc.cannotCheck(path, at, -1, rec, live, "keploy cannot read this instant in both runs")
	}
	c := changedInstant{r: rec[iw.RecAt[0]:iw.RecAt[1]], l: live[iw.LiveAt[0]:iw.LiveAt[1]], byText: iw.ByText, text: true}
	if iw.Rec != nil && iw.Live != nil && !iw.ByText {
		c.rm, c.lm = iw.Rec, iw.Live
		c.ri = typedInstant{at: time.Unix(0, iw.Rec.key.at).UTC(), res: iw.Rec.res, names: namesMoment}
		c.li = typedInstant{at: time.Unix(0, iw.Live.key.at).UTC(), res: iw.Live.res, names: namesMoment}
	}
	return dc.instant(path, at, w.RecIdx, c)
}

// instantHold is what one earlier place holds of a changed instant: R's
// value in R's text (hr), F's in F's (hl), each by moment or by its exact
// text, and whether that hold is at a fine cut (fr, fl).
type instantHold struct {
	p              evPlace
	hr, hl, fr, fl bool
	// rank orders the places that name a value: by its exact text (0),
	// else by the finest cut that gives it (its unit in nanoseconds).
	rank int64
}

// changedInstant is one changed instant: its text in each run, its typed
// reading for the window, its moments (nil for a number that does not
// parse: matched by its exact text alone, byText).
type changedInstant struct {
	r, l   string
	ri, li typedInstant
	rm, lm *moment
	byText bool
	// text: an instant in a text, whose layout the Clock keeps.
	text bool
}

// holds is what text, one run's text at p, holds of w (that run's word)
// and m (its moment): a test's given by its text alone (M1), any other
// place by its text or by moment. fine: the hold is at a cut finer than a
// millisecond, or by the exact text of a number that does not parse (P1).
func (dc *decider) holds(p evPlace, text, w string, m *moment, byText bool) (held, fine, read bool) {
	h, ok := dc.x.holdsWord(text, w)
	if !ok {
		return false, false, false
	}
	// An instant of several words (a date and a time) stands in a text
	// as those words in order.
	h = h || len(variables.Words(w)) > 1 && strings.Contains(text, w)
	if m == nil || p.gave {
		return h, h && byText, true
	}
	hm, fm, ok := dc.x.holdsMoment(text, *m)
	if !ok {
		return false, false, false
	}
	if hm {
		return true, fm, true
	}
	return h, h && fineCut(m.res), true
}

// instant decides one changed instant by DESIGN §3's order: a conflict at
// a fine cut (0), a pick (1), a given's text, one templated slot of the
// test case's request (2), an answer (3), a fine copy (4), a Clock its
// call's window holds (5), a coarse copy (6), a client test's own value
// (8); else refused. A read since the test began (7) is not read here:
// such a word stays refused, failing closed.
func (dc *decider) instant(path string, at variables.Address, n int, c changedInstant) Decision {
	r, l := c.r, c.l
	var hs []instantHold
	for _, p := range dc.ps {
		hr, fr, okR := dc.holds(p, p.rec, r, c.rm, c.byText)
		hl, fl, okL := dc.holds(p, p.live, l, c.lm, c.byText)
		if !okR || !okL {
			return dc.refuse(path, at, n, r, l, RefusalUnread, "keploy could not read the run's values past their budget, which came before this call", nil, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
		}
		if hr || hl {
			hs = append(hs, instantHold{p: p, hr: hr, hl: hl, fr: fr, fl: fl, rank: min(dc.rank(p.rec, r, c.rm, hr), dc.rank(p.live, l, c.lm, hl))})
		}
	}
	// A place that holds a value by its exact text is named before one
	// that holds it by a cut, and a finer cut before a coarser one.
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].rank < hs[j].rank })
	inScope := func(p evPlace) bool { return dc.allScope || !p.otherTest || p.given }
	before := func(p evPlace) bool { return p.state == stateBefore && !p.fUnstated }
	request := func(p evPlace) bool { return p.given && !p.gave }

	// 0. Conflict: a place before in both runs that held R's value whole
	// at a fine cut (or by the text of a number that does not parse), or a
	// given that held its text, where round F gave another value and this
	// one did not follow.
	var conflict []evPlace
	for _, h := range hs {
		if before(h.p) && inScope(h.p) && h.hr && !h.hl && (h.fr || h.p.gave) {
			conflict = append(conflict, h.p)
		}
	}
	if len(conflict) > 0 {
		p := conflict[0]
		return dc.refuse(path, at, n, r, l, RefusalConflict,
			r+" stood whole at "+p.name+"; round F gave "+gaveAt(p, l)+" there and the value did not follow", names(conflict),
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: lastSegment(path)})
	}

	// 1. Pick: (i) round F's moment stands at an answer or the test case's
	// request as sent, which does not show it; (iii) the recorded moment
	// stands at an answer whose state in round F keploy cannot tell; (ii)
	// the recorded moment stands in the test case's request by a cut rule
	// that gives no moment F saw in it as sent. Never at a given (M1).
	for _, h := range hs {
		p := h.p
		if p.gave || !(p.side == Response || request(p)) {
			continue
		}
		switch {
		case before(p) && h.hl && !(h.hr && p.rec != p.live):
			why := "round F's moment stands unchanged in " + p.name
			if p.rec != p.live {
				why = "round F's moment stands in " + p.name + ", which does not show it"
			}
			return dc.refuse(path, at, n, r, l, RefusalPick, why, []string{p.name}, dc.wordNoise(path, n))
		case p.side == Response && p.state == stateUnknown && (h.hr || h.hl):
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in "+p.name+", whose state in round F keploy cannot tell ("+p.why+")", []string{p.name},
				Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
		case h.hr && dc.swappable(p, c):
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in "+p.name+", whose state in round F keploy cannot tell (a connection start-up keploy may swap)", []string{p.name},
				Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
		}
	}
	if c.rm != nil && c.lm != nil && dc.in.Drifts != nil {
		var reqs []Earlier
		for _, e := range dc.in.Earlier {
			if e.Given && !e.Gave && e.Unread == "" {
				reqs = append(reqs, e)
			}
		}
		if where, _ := dc.in.Drifts.requestHolds([]moment{*c.rm}, []moment{*c.lm}, reqs); where != "" {
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in the test case at "+where+", and no cut there gives round F's", []string{where}, dc.wordNoise(path, n))
		}
	}

	// A given's text, and 2. one templated slot of the test case's request
	// that shows it in both runs: a Value bound there; two slots, refused.
	var gave, slots []evPlace
	for _, h := range hs {
		switch {
		case !before(h.p) || !h.hr || !h.hl:
		case h.p.gave:
			gave = append(gave, h.p)
		case request(h.p) && h.p.rec != h.p.live:
			slots = append(slots, h.p)
		}
	}
	if len(gave) > 0 {
		return dc.bound(path, at, n, r, l, gave, "a value the test gave keploy, which the call sends as given in both runs")
	}
	switch len(slots) {
	case 0:
	case 1:
		return dc.bound(path, at, n, r, l, slots, "the test case's request holds it, templated, in both runs")
	default:
		return dc.refuse(path, at, n, r, l, RefusalTwoSources,
			"the same moment was sent at "+slots[0].name+" and "+slots[1].name+", two separate reads", names(slots), dc.wordNoise(path, n))
	}

	// 3. Answer: an answer of the call's scope or the start-up, given
	// before the call in both runs, holds R's value, and F's text there
	// holds F's or is not stated.
	var answers []evPlace
	for _, h := range hs {
		p := h.p
		if p.side == Response && !p.given && inScope(p) && p.state == stateBefore && h.hr && (p.fUnstated || h.hl) {
			answers = append(answers, p)
		}
	}
	if len(answers) > 0 {
		return dc.bound(path, at, n, r, l, answers, "an answer given before the call holds it: the dependency's value")
	}

	// 4. and 6. A copy: requests of the call's scope or the start-up, sent
	// before the call in both runs, holding the same moment in both runs.
	copies := func(fine bool) []evPlace {
		var out []evPlace
		for _, h := range hs {
			p := h.p
			if p.side == Request && !p.given && inScope(p) && before(p) && h.hr && h.hl && (h.fr && h.fl) == fine {
				out = append(out, p)
			}
		}
		return out
	}
	if fine := copies(true); len(fine) > 0 {
		return dc.copied(path, at, n, r, l, fine, "the same moment, at a cut finer than a millisecond, was sent before the call in both runs: one read, copied")
	}

	// 5. Clock: the call's own window holds it, as a plain read; or, where
	// no earlier send of the scope holds it as a copy (6), plus an offset.
	var zone *time.Location
	if dc.in.Drifts != nil {
		zone = dc.in.Drifts.zone
	}
	var off models.ClockOffset
	var cutWhy string
	inWindow := false
	if c.rm != nil && c.lm != nil {
		if dc.in.Window.holdsRead(*c.rm, *c.lm, zone) {
			off, inWindow = models.ClockOffset{Res: max(c.rm.res, c.lm.res)}, true
		} else if copies(false) == nil {
			off, cutWhy, inWindow = clockInWindow(path, c.ri, c.li, dc.in.Window, zone)
		}
	}
	if inWindow {
		if echo := dc.instantEchoed(c); echo != "" {
			return dc.cannotCheck(path, at, n, r, l, "keploy cannot check a clock read its own call's recorded answer holds ("+echo+"): it would serve that answer's recorded moment")
		}
		if c.text {
			lay := models.ClockOffset{Layout: clockTextLayout(r)}.MeetLayout(clockTextLayout(l))
			if lay == "" || lay == models.LayoutApart {
				return dc.cannotCheck(path, at, n, r, l, "its two runs write it in different layouts")
			}
			off.Layout = lay
		}
		d := dc.learned(path, at, n, r, l, OutcomeClock, "a read of the application's clock its call's window holds in both runs", nil)
		if off.Why == "" {
			off.Why = d.Line
		}
		d.Clock = &off
		return d
	}

	// 6. Coarse copy: as 4, a millisecond or coarser; no window condition.
	if coarse := copies(false); len(coarse) > 0 {
		return dc.copied(path, at, n, r, l, coarse, "the same moment, at a millisecond or coarser, was sent before the call in both runs and its own window does not hold it: one read, copied")
	}

	// 8. The test's own value (client tests): no earlier place holds it in
	// either run, its answer does not echo it, and its cut is not ambiguous.
	if dc.in.Drifts != nil && dc.in.Drifts.clientTests && len(hs) == 0 && cutWhy == "" && dc.instantEchoed(c) == "" {
		return dc.learned(path, at, n, r, l, OutcomeFresh, "the test's own value: no place before the call holds it; bound at its first call", nil)
	}
	why := "a time its call's window does not hold in both runs, that no answer gave and no request of this scope sent"
	if cutWhy != "" {
		why += " (" + cutWhy + ")"
	}
	var outside, sentIn []string
	for _, h := range hs {
		if h.hr && h.hl && h.p.otherTest && !dc.allScope && len(outside) < maxLinePlaces {
			outside = append(outside, h.p.name)
			sentIn = append(sentIn, h.p.name+" in "+scopeName(h.p))
		}
	}
	if len(outside) > 0 {
		why += " [but " + strings.Join(sentIn, ", ") + " did]"
	}
	return dc.refuse(path, at, n, r, l, RefusalNoClockRead, why, outside, dc.wordNoise(path, n), Edit{Kind: EditReRecord})
}

// rank is how closely text names w (its moment m): 0 by its exact text,
// else the unit of the finest cut that gives it; the most where held is
// false or no cut does.
func (dc *decider) rank(text, w string, m *moment, held bool) int64 {
	const none = int64(1) << 62
	if !held {
		return none
	}
	if h, _, _ := dc.holds(evPlace{gave: true}, text, w, nil, false); h {
		return 0
	}
	if m == nil {
		return none
	}
	spans, ok := dc.x.text(text).spansOf()
	if !ok {
		return none
	}
	best := none
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		h := sp.moment()
		if h.key == m.key {
			return 1
		}
		cuts(h, *m, func(c cutRule) bool {
			best = min(best, c.unit)
			return false
		})
	}
	return best
}

// swappable reports whether p is a connection's start-up keploy may swap
// for another whose field holding the recorded value differs (Raw's
// swapHolds): its state in round F keploy cannot tell.
func (dc *decider) swappable(p evPlace, c changedInstant) bool {
	if p.raw == 0 {
		return false
	}
	x := dc.in.Raw[p.raw-1]
	return x.swapHolds(x.payload().text, dc.in.Raw, func(own string) bool {
		h, _, _ := dc.holds(evPlace{}, own, c.r, c.rm, c.byText)
		return h
	})
}

// bound is a Value bound at the earliest of src.
func (dc *decider) bound(path string, at variables.Address, n int, r, l string, src []evPlace, rule string) Decision {
	first := src[0]
	for _, p := range src[1:] {
		if p.at.Before(first.at) {
			first = p
		}
	}
	d := dc.learned(path, at, n, r, l, OutcomeValue, "bound to "+first.name+": "+rule, names(src))
	d.From = Place{Mock: first.mock, Side: first.side, Path: first.path, Live: l}
	return d
}

// copied is a Value bound at the earliest of src, a copy's sources; a later
// one its own call learned as a read of the clock is a second, separate
// read, and the word is refused naming both (H2).
func (dc *decider) copied(path string, at variables.Address, n int, r, l string, src []evPlace, rule string) Decision {
	first := src[0]
	for _, p := range src[1:] {
		if p.at.Before(first.at) {
			first = p
		}
	}
	for _, p := range src {
		if p.clock && p.name != first.name {
			return dc.refuse(path, at, n, r, l, RefusalTwoSources,
				"the same moment was sent at "+first.name+" and "+p.name+", two separate reads", []string{first.name, p.name}, dc.wordNoise(path, n))
		}
	}
	return dc.bound(path, at, n, r, l, src, rule)
}

// instantEchoed is the first leaf of the call's recorded answer that holds
// the recorded instant, by its text or its moment; "" where none does.
func (dc *decider) instantEchoed(c changedInstant) string {
	for _, a := range dc.in.Resp {
		p := evPlace{}
		if h, _, _ := dc.holds(p, a.Value, c.r, c.rm, c.byText); h {
			return a.Path
		}
	}
	return ""
}

// echoes are the call's answer places that hold v (the recorded value).
func (dc *decider) echoes(v string) []variables.Address {
	var out []variables.Address
	for _, l := range dc.in.Resp {
		ws := variables.Words(l.Value)
		for i, w := range ws {
			if l.Value[w[0]:w[1]] == v {
				a := addressOf(dc.in, l)
				a.Word = i
				out = append(out, a)
			}
		}
	}
	return out
}

// echoPath is the path of the call's i-th answer place that holds v.
func (dc *decider) echoPath(v string, i int) string {
	for _, l := range dc.in.Resp {
		ws := variables.Words(l.Value)
		for _, w := range ws {
			if l.Value[w[0]:w[1]] == v {
				if i == 0 {
					return l.Path
				}
				i--
			}
		}
	}
	return "its answer"
}

// addressOf is l's word address, word -1 (the whole leaf).
func addressOf(in Input, l Leaf) variables.Address {
	at := variables.Address{Word: -1}
	if s, ok := VarPlace(l, in.Rec.Body != nil); ok {
		if a, err := variables.ParseAddress(s); err == nil {
			at = a
			at.Word = -1
		}
	}
	return at
}

// learned is a decided word and its line.
func (dc *decider) learned(path string, at variables.Address, n int, r, f string, o WordOutcome, rule string, places []string) Decision {
	at.Word = n
	from := ""
	if dc.allScope {
		from = dc.in.Landed.String()
	}
	return Decision{At: at, Recorded: r, Full: f, Outcome: o, Line: learnedLine(learnedWord{
		Test: dc.in.Test, Mock: dc.in.Mock, Field: path, Word: n, Recorded: r, Full: f,
		Outcome: o, Rule: rule, Places: places, From: from,
	})}
}

// refuse is a refused word and its line.
func (dc *decider) refuse(path string, at variables.Address, n int, r, f string, kind RefusalKind, why string, places []string, edits ...Edit) Decision {
	at.Word = n
	rf := &Refusal{Kind: kind, Test: dc.in.Test, Mock: dc.in.Mock, Field: path, Word: n, Recorded: r, Full: f, Why: why, Places: places, Edits: edits}
	return Decision{At: at, Recorded: r, Full: f, Outcome: OutcomeRefused, Refusal: rf, Line: RefusalLine(*rf)}
}

// cannotCheck refuses a field keploy cannot decide yet: its limit.
func (dc *decider) cannotCheck(path string, at variables.Address, n int, r, f, why string) Decision {
	return dc.refuse(path, at, n, r, f, RefusalCannotCheck, why, nil, dc.fieldNoise(path))
}

func (dc *decider) fieldNoise(path string) Edit {
	return Edit{Kind: EditNoiseAtField, Mock: dc.in.Mock, Field: path, Word: -1}
}

func (dc *decider) wordNoise(path string, n int) Edit {
	if n < 0 {
		return dc.fieldNoise(path)
	}
	return Edit{Kind: EditNoiseAtWord, Mock: dc.in.Mock, Field: path, Word: n}
}

// names are ps' line names, first three.
func names(ps []evPlace) []string {
	var out []string
	for _, p := range ps {
		if len(out) == maxLinePlaces {
			break
		}
		out = append(out, p.name)
	}
	return out
}

// gaveAt is what round F gave at p, in a line's words.
func gaveAt(p evPlace, f string) string {
	if p.rec == p.live {
		return "the same text"
	}
	if len(p.live) > 40 {
		return "a value without " + f
	}
	return p.live
}

// scopeName names p's scope outside the call for a line.
func scopeName(p evPlace) string {
	if p.scope != "" {
		return p.scope
	}
	return "an earlier test or between tests"
}

// lastSegment is path's last key, a given's name.
func lastSegment(path string) string {
	if i := strings.LastIndexAny(path, ".]"); i >= 0 && i+1 < len(path) {
		return path[i+1:]
	}
	return path
}
