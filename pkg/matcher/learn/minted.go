package learn

import (
	"slices"

	"go.keploy.io/server/v3/pkg/variables"
)

// A source keploy.yml's mock.rules declares for a field of a call
// (Input.Source) settles what learning could not: a rule of kind minted
// says the application makes the field's whole value anew at the call (an
// id it mints). Learning's own facts come first: where it bound a word of
// the field to an earlier place, or read it as a clock, the rule does not
// act and the field is decided as learning decided it. Where learning
// refused the field's words (a piece of a new id that stands, by chance,
// in another place), or learned them made anew word by word, the field is
// decided made anew whole, bound at the call, so long as:
//   - keploy read every earlier place (no refusal of the field is unread
//     or keploy's limit);
//   - the value is a string or a number read as written in both runs (no
//     step opens it);
//   - round F's value keeps the recorded one's shape (variables.Alphabet):
//     else it is refused as of another shape;
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

// SourceMinted: the application makes the field's whole value anew at the
// call (keploy.yml's kind: minted).
const SourceMinted SourceKind = iota + 1

// Source is what keploy.yml's rules declare of a drifted field of the call
// (Input.Source): its Kind and the id of the rule that declares it (Rule),
// which every line it decides names.
type Source struct {
	Kind SourceKind
	Rule string
}

// Minted is what a minted rule decided of its field (Decision.Minted): the
// rule's id and the alphabet its value keeps (variables.MintedAlphabet),
// which the replay checks at the bind place.
type Minted struct {
	Rule     string
	Alphabet variables.Alphabet
}

// source is the source keploy.yml declares for the field at path, ok false
// where it declares none keploy knows.
func (dc *decider) source(path string) (Source, bool) {
	if dc.in.Source == nil {
		return Source{}, false
	}
	s, ok := dc.in.Source(path)
	return s, ok && s.Kind == SourceMinted
}

// minted decides the drifted leaf at path, which src declares minted, from
// ds, its words as learning decided them (see above).
func (dc *decider) minted(path string, src Source, ds []Decision) []Decision {
	for _, d := range ds {
		switch {
		case d.Outcome == OutcomeValue || d.Outcome == OutcomeClock:
			return ds
		case d.Refusal != nil && (d.Refusal.Kind == RefusalUnread || d.Refusal.Kind == RefusalCannotCheck):
			return ds
		}
	}
	rec, live := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	at := dc.in.AddressOf(rec, false)
	r, f := rec.Value, live.Value
	why := "keploy.yml's rule " + src.Rule + " declares it minted"
	if rec.Type != live.Type || rec.Type != String && rec.Type != Number || len(at.Steps) > 0 {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalNotMinted,
			why+", and keploy reads a minted value as one string or number in each run, as written: this one is not", nil, dc.fieldNoise(path))}
	}
	alphabet, ok := variables.MintedAlphabet(r, f)
	if !ok {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalChangedShape,
			why+", and round F's value has another shape than the recorded one (its length, or a byte that is no letter or digit, or its letters' alphabet): minted value of another shape", nil, dc.fieldNoise(path))}
	}
	if held := dc.mintedHeld(r, f); len(held) > 0 {
		return []Decision{dc.refuse(path, at, -1, r, f, RefusalNotMinted,
			why+", yet an earlier place holds its whole value in a run: the application copied it from there, so it is a variable's use, not a value made at this call", held, dc.fieldNoise(path))}
	}
	d := dc.learned(path, at, -1, r, f, OutcomeFresh, why+": no earlier place holds its whole value in either run; bound at its first call, and a replay checks it is new and keeps its shape", nil)
	d.Minted = &Minted{Rule: src.Rule, Alphabet: alphabet}
	d.AnsweredAfter = dc.answeredAfter(r)
	return []Decision{d}
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
