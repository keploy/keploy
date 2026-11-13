package learn

import (
	"cmp"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A rule of kind clock (Source.Kind SourceClock) says the application reads
// the field's value from its clock for the call. It settles which source a
// moment has where learning could not tell: the same second also stands in
// an answer round F gave outside the call's scope (another-scope), in an
// answer it reads as a pick, or at a place whose state in round F keploy
// cannot tell (unknown). The value is then decided a read of the clock,
// bound to no place, so long as the check a replay makes of it holds it in
// both runs: the declared range (declaredOffset) against each run's span of
// the call (Window), read as a replay reads it (CheckClockRead). It never
// widens what is accepted: a value the replay's check would refuse in
// either run is refused here (no-clock-read), naming the rule. The rule's
// check reads its value in UTC: a wall clock of no zone the application
// writes in its own zone is refused (fail closed).
//
// Learning's own facts come first: a word it bound to an earlier place, or
// read as the clock itself, keeps learning's decision, and so does every
// other refusal (what keploy could not read, its limit, a conflict, two
// slots of the test case's request). A word learning read no instant in is
// refused no-clock-read (clockOnly).

// clockSettles are the refusals a clock rule settles: those of which
// source a moment has.
var clockSettles = map[RefusalKind]bool{RefusalAnotherScope: true, RefusalPick: true, RefusalUnknown: true}

// declaredOffset is the check a clock rule's range o makes (Source.Clock),
// as learning stores a clock read: a range that holds zero is a plain read
// at o.Res, which a replay holds within one cut of the call's span on
// either side; one all on one side of zero is a read plus that offset (a
// look-back, an expiry). false for a Res of none or below, or a Max below
// its Min: no range keploy checks.
func declaredOffset(o models.ClockOffset) (models.ClockOffset, bool) {
	switch {
	case o.Res <= 0 || o.Max < o.Min:
		return models.ClockOffset{}, false
	case o.Min <= 0 && o.Max >= 0:
		return models.ClockOffset{Res: o.Res}, true
	}
	return models.ClockOffset{Min: o.Min, Max: o.Max, Res: o.Res}, true
}

// declaredClock decides the changed instant c at path, which src declares
// a read of the clock, from d, learning's decision of it (see above).
func (dc *decider) declaredClock(path string, at variables.Address, n int, c changedInstant, src Source, d Decision) Decision {
	switch {
	case d.Outcome == OutcomeValue || d.Outcome == OutcomeClock:
		return d
	case d.Refusal != nil && !clockSettles[d.Refusal.Kind]:
		return d
	}
	why := "keploy.yml's rule " + src.Rule + " declares it a read of the clock from " + src.Clock.Min.String() + " to " + src.Clock.Max.String() + " of its call's span, at " + src.Clock.Res.String()
	off, ok := declaredOffset(src.Clock)
	w := dc.in.Window
	switch {
	case !ok:
		return dc.refuse(path, at, n, c.r, c.l, RefusalNoClockRead, why+", a range keploy does not check (a resolution of none, or a max below its min)", nil, dc.fieldNoise(path))
	case w == nil:
		return dc.refuse(path, at, n, c.r, c.l, RefusalNoClockRead, why+", and keploy knows no span of the call", nil, dc.fieldNoise(path))
	case c.rm == nil || c.lm == nil:
		return dc.refuse(path, at, n, c.r, c.l, RefusalNoClockRead, why+", and keploy reads no moment in its value in a run (a number it matches by its text alone)", nil, dc.fieldNoise(path))
	}
	cd := dc.clock(path, at, n, c, off, why, false)
	if cd.Outcome != OutcomeClock {
		return cd
	}
	runs := []struct {
		name string
		leaf Leaf
		span Span
	}{{"the recorded value", dc.in.Rec.Leaves[path], w.Rec}, {"round F's value", dc.in.Live.Leaves[path], w.F}}
	for _, r := range runs {
		if held, no := CheckClockRead(path, *cd.Clock, r.leaf, r.span, true); !held {
			return dc.refuse(path, at, n, c.r, c.l, RefusalNoClockRead, why+", yet "+r.name+" is no such read in its run: "+no, nil, dc.fieldNoise(path))
		}
	}
	return cd
}

// clockOnly refuses each word of the drifted leaf at path, which src
// declares a read of the clock, that learning made anew (ds, its words as
// learning decided them): it read no instant there, so no read of the
// clock. Every other decision stands (declaredClock decided the instants).
func (dc *decider) clockOnly(path string, src Source, ds []Decision) []Decision {
	for i, d := range ds {
		if d.Outcome == OutcomeFresh {
			ds[i] = dc.refuse(path, d.At, d.At.Word, d.Recorded, d.Full, RefusalNoClockRead,
				"keploy.yml's rule "+src.Rule+" declares it a read of the clock, and keploy reads no instant in it in each run", nil, dc.fieldNoise(path))
		}
	}
	return ds
}

// DeclaredClockLearned is ln, a drifted leaf the classify path learned
// (ClassifyArrived: the HTTP adapter's learn step), as src, a clock rule
// keploy.yml declares at its field, decides it, the way Decide decides a
// changed instant a rule declares (declaredClock, then timeDefault). A
// declared clock is a time field, so it settles what learning could not
// tell of the value's source: an echo (a variable From.Echo: learning found
// the value's moment in what the application was given earlier, and cannot
// tell a copy of it, or a pick from it by the clock, from a read of the
// clock: echoProvenance), noise, and a drift refused for its source alone
// (Learned.sourceWhy). Such a value is a read of the clock when the rule's
// range holds both runs' values in the call's span (w: Rec and F), in one
// layout, as a replay checks it (CheckClockRead); else, under the time
// default (timeForm), an instant each run writes in a form (TimeForm), an
// epoch number near its call in its run (nearSpan), checked by that form
// alone, or by either of the two its runs wrote (FormF), as is a read the
// range holds. One neither check holds is refused, naming
// the rule: noise becomes unaccounted (a value the rule's own check refuses
// is never let pass unchecked), and an echo or a refused drift keeps
// learning's refusal. The caller leaves out, where the time default is off,
// a value its call's recorded answer holds (a replay would serve that
// answer's recorded moment). Every other decision stands: a value bound to an
// earlier place that holds it whole, a read of the clock or an offset from
// it learning found, an echo the answer carries, and every other refusal
// (two layouts or zone forms, a value its call's answer holds, what keploy
// could not read). A read the re-read since the test began learned in
// place of the offset rule's refusal (Learned.offsetRefused), or the T2
// time rule in place of a place no variable binds at (Learned.placeRefused),
// is decided as that refusal, or that variable: the rule, not learning, says
// how the field is checked.
func DeclaredClockLearned(ln Learned, src Source, w *Window, timeForm bool) Learned {
	switch {
	case src.Kind != SourceClock:
	case ln.offsetRefused != nil:
		// The rule decides the offset rule's refusal, which the re-read
		// since the test began learned as a read (Learned.offsetRefused).
		ln = *ln.offsetRefused
	case ln.placeRefused != nil:
		// And the variable of a place no variable binds at, which the T2
		// time rule learned as a read (Learned.placeRefused).
		ln = *ln.placeRefused
	}
	settles := ln.Kind == KindVariable && ln.From.Echo || ln.Kind == KindNoise ||
		ln.Kind == KindUnaccounted && ln.sourceWhy != "" && ln.sourceWhy == ln.Why
	if src.Kind != SourceClock || !settles {
		return ln
	}
	rec, live := ln.Recorded, ln.Live
	why := "keploy.yml's rule " + src.Rule + " declares it a read of the clock"
	var read *models.ClockOffset
	off, ok := declaredOffset(src.Clock)
	lay := models.ClockOffset{Layout: leafLayout(rec)}.MeetLayout(leafLayout(live))
	var no string // why the range check refuses it
	switch {
	case !ok:
		no = "its range is one keploy does not check (a resolution of none, or a max below its min)"
	case w == nil:
		no = "keploy knows no span of the call"
	case !isInstant(rec) || !isInstant(live):
		no = "keploy reads no moment in its value in a run"
	case lay == "" || lay == models.LayoutApart:
		no = "its two runs write it in different layouts"
	default:
		off.Layout = lay
		recHeld, recNo := CheckClockRead(rec.Path, off, rec, w.Rec, true)
		liveHeld, liveNo := CheckClockRead(live.Path, off, live, w.F, true)
		switch {
		case recHeld && liveHeld:
			off.Why = why + ", and both runs' values are such reads in the call's span"
			read = &off
		case !recHeld:
			no = "the recorded value is no such read in its run: " + recNo
		default:
			no = "round F's value is no such read in its run: " + liveNo
		}
	}
	if timeForm {
		// The time default checks a time by its form alone, a read the
		// rule's range holds too (timeDefault), and one its two runs wrote
		// in two forms by either (timeForms), never by the range.
		recForm, liveForm := TimeForm(rec), TimeForm(live)
		form, formF := timeForms(recForm, liveForm, rec, live, w, nil)
		forms := models.ClockOffset{Form: form, FormF: formF}.Forms()
		switch {
		case !isInstant(rec) || !isInstant(live): // an instant has a form
			no += "; and a run writes it in no form of an instant (" + cmp.Or(recForm, "none") + " recorded, " + cmp.Or(liveForm, "none") + " in round F)"
		case form == "":
			no += "; and, an epoch number, it names no moment near its call in both runs"
		case read != nil:
			read.Form, read.FormF = form, formF
			read.Why += "; a time, checked by its form alone (" + forms + ", keploy.yml's times setting at its default)"
		default:
			read = &models.ClockOffset{Form: form, FormF: formF, Why: why + ": a time, checked by its form alone (" + forms + ", keploy.yml's times setting at its default); its range alone would refuse it: " + no}
		}
	}
	switch {
	case read != nil:
		return Learned{Kind: KindClockRead, Recorded: rec, Live: live, Read: read, Why: read.Why, Refuted: ln.Refuted}
	case ln.Kind == KindNoise:
		return Learned{Kind: KindUnaccounted, Recorded: rec, Live: live, Why: why + ", and " + no, Refuted: ln.Refuted}
	}
	return ln
}

// isInstant reports whether l names a moment keploy reads (clockReadOf): a
// time of day, which names no day, does not.
func isInstant(l Leaf) bool {
	_, _, _, ok := clockReadOf(l)
	return ok
}
