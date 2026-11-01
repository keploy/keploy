package learn

import (
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
		if !okR || !okL {
			return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, "one run's value here is no instant")}
		}
		return []Decision{dc.instant(path, at, -1, rec.Value, live.Value, r, l, nil, nil)}
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
			out = append(out, dc.cannotCheck(path, at, -1, rec, live, "a source of part of a stretch whose word counts differ"))
			return out
		default:
			out = append(out, dc.word(path, at, rec, live, w))
		}
	}
	if len(out) == 0 && rec != live {
		out = append(out, dc.cannotCheck(path, at, -1, rec, live, "only what separates its words changed"))
	}
	return out
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
			if p.given || countsByText(dc.x, p, w) || (dc.allScope || !p.otherTest) && heldWhole(dc.x, p, w, rec) {
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
			"the value "+p.name+" sent in an earlier test or between tests; keploy binds only within a test and the start-up", names(outside),
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

// textInstant decides an instant the changed words of a text are part of.
func (dc *decider) textInstant(path string, at variables.Address, rec, live string, w readWord) Decision {
	iw := w.Instant
	if iw.RecAt[0] < 0 || iw.LiveAt[0] < 0 || iw.Rec == nil || iw.Live == nil {
		return dc.cannotCheck(path, at, -1, rec, live, "keploy cannot read this instant in both runs")
	}
	r, l := rec[iw.RecAt[0]:iw.RecAt[1]], live[iw.LiveAt[0]:iw.LiveAt[1]]
	ri := typedInstant{at: time.Unix(0, iw.Rec.key.at).UTC(), res: iw.Rec.res, names: namesMoment}
	li := typedInstant{at: time.Unix(0, iw.Live.key.at).UTC(), res: iw.Live.res, names: namesMoment}
	return dc.instant(path, at, w.RecIdx, r, l, ri, li, iw.Rec, iw.Live)
}

// instant decides one changed instant: where no earlier place holds either
// run's value (by its text or by moment) steps 0-4 and 6 have nothing to
// read, and step 5 makes it a Clock where its call's window holds it; else
// refused. One an earlier place holds is refused until the copy steps are
// built: keploy's limit, never a guess.
func (dc *decider) instant(path string, at variables.Address, n int, r, l string, ri, li typedInstant, rm, lm *moment) Decision {
	for _, p := range dc.ps {
		hr, okR := dc.x.holdsWord(p.rec, r)
		hl, okL := dc.x.holdsWord(p.live, l)
		if !okR || !okL {
			return dc.cannotCheck(path, at, n, r, l, "the run's values past the call's budget may hold this instant")
		}
		if !hr && !hl && rm != nil && lm != nil {
			hr, _, _ = dc.x.holdsMoment(p.rec, *rm)
			hl, _, _ = dc.x.holdsMoment(p.live, *lm)
		}
		if hr || hl {
			return dc.cannotCheck(path, at, n, r, l, "an earlier place ("+p.name+") holds this instant, which keploy cannot check yet")
		}
	}
	var zone *time.Location
	if dc.in.Drifts != nil {
		zone = dc.in.Drifts.zone
	}
	off, cutWhy, ok := clockInWindow(path, ri, li, dc.in.Window, zone)
	if !ok {
		why := "a time its call's window does not hold in both runs, that no answer gave and no request of this scope sent"
		if cutWhy != "" {
			why += " (" + cutWhy + ")"
		}
		return dc.refuse(path, at, n, r, l, RefusalNoClockRead, why, nil, dc.wordNoise(path, n), Edit{Kind: EditReRecord})
	}
	if rm != nil {
		lay := models.ClockOffset{Layout: clockTextLayout(r)}.MeetLayout(clockTextLayout(l))
		if lay == "" || lay == models.LayoutApart {
			return dc.cannotCheck(path, at, n, r, l, "its two runs write it in different layouts")
		}
		off.Layout = lay
	}
	d := dc.learned(path, at, n, r, l, OutcomeClock, "a read of the application's clock its call's window holds in both runs", nil)
	d.Clock = &off
	return d
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
		from = "another window"
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

// lastSegment is path's last key, a given's name.
func lastSegment(path string) string {
	if i := strings.LastIndexAny(path, ".]"); i >= 0 && i+1 < len(path) {
		return path[i+1:]
	}
	return path
}
