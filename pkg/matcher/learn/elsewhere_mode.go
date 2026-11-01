package learn

import "strings"

// Mode Elsewhere (the timer rule): a call that landed in another
// window than its recording's may teach its recording Clock words only, for
// learning only. Every earlier place of the run is in scope, for sources and
// for conflicts. The call's result is kept only where every changed word is
// a Clock or noise keploy.yml declares; else it learns nothing, and every
// word prints one refused line naming the word that stopped it and the
// edit. It never learns noise, a Value or a fresh word.

// elsewhere guards ds, the call's words decided with every earlier place in
// scope (fields[i] is the field of ds[i]): the Clocks where every other word is declared noise, else every
// word refused naming the first word that stopped it.
func elsewhere(in Input, ds []Decision, fields []string) []Decision {
	var kept []Decision
	guard := -1
	for i, d := range ds {
		field := fields[i]
		switch {
		case d.Outcome == OutcomeClock:
			kept = append(kept, d)
		case in.Declared != nil && in.Declared(field, d.At.Word):
		default:
			if guard < 0 {
				guard = i
			}
		}
	}
	if guard < 0 {
		return kept
	}
	g := ds[guard]
	gField := fields[guard]
	stop := "round F's call landed in " + in.Landed.String() + ", which teaches clock reads only; " +
		wordWhere(in.Test, in.Mock, gField, g.At.Word) + " is " + outcomeWords(g)
	edit := Edit{Kind: EditNoiseAtWord, Mock: in.Mock, Field: gField, Word: g.At.Word}
	if g.At.Word < 0 {
		edit = Edit{Kind: EditNoiseAtField, Mock: in.Mock, Field: gField, Word: -1}
	}
	out := make([]Decision, 0, len(ds))
	for i, d := range ds {
		kind := RefusalLandedElsewhere
		if d.Refusal != nil {
			kind = d.Refusal.Kind
		}
		rf := &Refusal{Kind: kind, Test: in.Test, Mock: in.Mock, Field: fields[i], Word: d.At.Word,
			Recorded: d.Recorded, Full: d.Full, Why: "learned nothing: " + stop, Edits: []Edit{edit}}
		if d.Refusal != nil {
			rf.Places = d.Refusal.Places
		}
		hideSecrets(rf)
		out = append(out, Decision{At: d.At, Recorded: d.Recorded, Full: d.Full, Outcome: OutcomeRefused, Refusal: rf, Line: RefusalLine(*rf)})
	}
	return out
}

// outcomeWords is what d is, for the timer rule's line.
func outcomeWords(d Decision) string {
	switch d.Outcome {
	case OutcomeValue:
		return "a value an earlier place sent (" + strings.TrimSpace(d.From.Mock+" "+d.From.Path) + ")"
	case OutcomeFresh:
		why := "a value made anew each run"
		if len(d.Echo) > 0 {
			var at []string
			for _, a := range d.Echo {
				at = append(at, a.String())
			}
			why += ", which the call's recorded answer echoes at " + strings.Join(at, ", ")
		}
		return why + " (noise at it in keploy.yml lets its clock reads learn)"
	}
	if d.Refusal != nil {
		return "refused (" + string(d.Refusal.Kind) + "): " + d.Refusal.Why
	}
	return "no clock read"
}
