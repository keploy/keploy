package learn

import (
	"slices"

	"go.keploy.io/server/v3/pkg/models"
)

// A source keploy.yml's mock.rules declares for a field of a call
// (Input.Source) settles what learning could not: a rule of kind minted
// says the application makes the field's whole value anew at the call (an
// id it mints). The field is decided whole, by the rule's checks below. A
// piece of its value (a word of it, or a text a step opens in it) that
// learning bound to an earlier place stays bound there beside it, so a
// minted "<copied>-<random>" still checks its copied part at every replay
// (an adapter that cannot follow that place leaves the piece to the rule);
// every other decision of a piece (refused, read as the clock, or made
// anew) leaves it to the rule, so a piece of a new id that stands, by
// chance, in an earlier place's text does not set the call aside.
// Learning's decision of the whole value comes first: where it bound the
// value to an earlier place, or read it as a clock, the rule does not act
// and the field is decided as learning decided it. Else the field is
// decided made anew whole, bound at the call, so long as:
//   - keploy read every earlier place (no refusal of the field, or of any
//     word of it, is unread), and could check the whole value (no refusal
//     of it is keploy's limit);
//   - the value is a string or a number read as written in both runs (no
//     step opens it);
//   - both runs' values keep the rule's form (models.MintedForm.Keeps:
//     Source.Minted, of every value the set's recordings hold at the
//     rule's places, with both runs' values: its prefix cut to what they
//     share, its lengths widened to theirs): else it is refused as of
//     another shape;
//   - no earlier place holds the whole value: neither one the recording
//     gave the application before the call holding the recorded value,
//     nor one round F gave it holding round F's value, in the call's scope
//     or outside it. Such a value is copied, so it is a variable's use, not
//     a minted one: refused, naming the places.
//
// Every refusal of a field a rule declares names the rule in its line's
// tail ("rule":"<id>").

// SourceKind is what a declared source says of a field's value.
type SourceKind uint8

// The kinds of source keploy reads.
const (
	// SourceMinted: the application makes the field's whole value anew at
	// the call (keploy.yml's kind: minted).
	SourceMinted SourceKind = iota + 1
	// SourceClock: the application reads the field's value from its clock
	// for the call, within Source.Clock of the call's span (keploy.yml's
	// kind: clock; see declaredClock).
	SourceClock
)

// Source is what keploy.yml's rules declare of a drifted field of the call
// (Input.Source): its Kind, the id of the rule that declares it (Rule),
// which every line it decides names, and, of a clock, the range its rule
// declares (Clock: Min, Max and Res; every other field unset), and, of a
// minted field, the form of the values the set's recordings hold at the
// rule's places (Minted).
type Source struct {
	Kind   SourceKind
	Rule   string
	Clock  models.ClockOffset
	Minted models.MintedForm
}

// Minted is what a minted rule decided of its field (Decision.Minted): the
// rule's id and the form its value keeps (models.MintedForm), which the
// replay checks at the bind place.
type Minted struct {
	Rule string
	Form models.MintedForm
}

// source is the source keploy.yml declares for the field at path, ok false
// where it declares none keploy knows.
func (dc *decider) source(path string) (Source, bool) {
	if dc.in.Source == nil {
		return Source{}, false
	}
	s, ok := dc.in.Source(path)
	if !ok || s.Kind != SourceMinted && s.Kind != SourceClock {
		return Source{}, false
	}
	return s, true
}

// minted decides the drifted leaf at path, which src declares minted, from
// ds, its words as learning decided them (see above): the field whole,
// then each piece learning bound to an earlier place.
func (dc *decider) minted(path string, src Source, ds []Decision) []Decision {
	rec, live := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	var bound []Decision
	for _, d := range ds {
		switch {
		case d.Refusal != nil && d.Refusal.Kind == RefusalUnread:
			return ds
		case d.Recorded != rec.Value:
			// A piece of the value (a word of it, or a text a step opens
			// in it): one bound to an earlier place stays its check; the
			// rule decides the rest with the value whole.
			if d.Outcome == OutcomeValue {
				bound = append(bound, d)
			}
		case d.Outcome == OutcomeValue || d.Outcome == OutcomeClock || d.Refusal != nil && d.Refusal.Kind == RefusalCannotCheck:
			return ds
		}
	}
	at := dc.in.AddressOf(rec, false)
	r, f := rec.Value, live.Value
	why := "keploy.yml's rule " + src.Rule + " declares it minted"
	if rec.Type != live.Type || rec.Type != String && rec.Type != Number || len(at.Steps) > 0 {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalNotMinted,
			why+", and keploy reads a minted value as one string or number in each run, as written: this one is not", nil, dc.fieldNoise(path))}
	}
	form := src.Minted.With(r, f)
	if form.Check() != nil || !form.Keeps(r) || !form.Keeps(f) {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalChangedShape,
			why+", and a run's value does not keep the rule's form ("+form.Where()+"): minted value of another shape", nil, dc.fieldNoise(path))}
	}
	if held := dc.mintedHeld(r, f); len(held) > 0 {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalNotMinted,
			why+", yet an earlier place holds its whole value in a run: the application copied it from there, so it is a variable's use, not a value made at this call", held, dc.fieldNoise(path))}
	}
	d := dc.learned(path, at, -1, r, f, OutcomeFresh, why+": no earlier place holds its whole value in either run; bound at its first call, and a replay checks it is new and keeps its shape", nil)
	d.Minted = &Minted{Rule: src.Rule, Form: form}
	d.AnsweredAfter = dc.answeredAfter(r)
	return append([]Decision{d}, bound...)
}

// mintedHeld names the earlier places that hold a minted field's whole
// value: the recorded value r at a place the recording gave the
// application before the call (not answered or asked after it: placedFor),
// round F's value f at one round F gave it before the call reached keploy,
// in its view of it or in any of the views it served. A place round F
// served with no view stated (fMay) may hold it, and is named (fail
// closed).
func (dc *decider) mintedHeld(r, f string) []string {
	var out []string
	for _, p := range dc.ps {
		inR := !placedFor(p, r).recAfter && holdsWhole(p.t.rec, r)
		inF := p.fGave && (p.fMay || holdsWhole(p.t.live, f) || slices.ContainsFunc(p.t.views, func(t *readerText) bool { return holdsWhole(t, f) }))
		if inR || inF {
			out = append(out, p.name)
		}
	}
	return out
}

// holdsWhole reports whether t holds v anywhere, as written or in a form
// keploy reads it in (holdsWord). A text keploy could not read holds
// nothing here: the call's index is then unread, and decide refuses every
// word of the call (fail closed).
func holdsWhole(t *readerText, v string) bool {
	if held, _ := t.contains(v); held {
		return true
	}
	held, _ := t.holdsWord(v)
	return held
}

// ruled names the rule that declares each refused field's source in its
// refusal (Refusal.Rule), however it was refused; fields[i] is the field of
// out[i] wherever out[i] is refused.
func (dc *decider) ruled(out []Decision, fields []string) {
	for i, d := range out {
		if src, ok := dc.source(fields[i]); ok && d.Refusal != nil {
			rf := *d.Refusal
			rf.Rule = src.Rule
			out[i].Refusal, out[i].Line = &rf, RefusalLine(rf)
		}
	}
}
