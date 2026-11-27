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
// slots of the test case's request), except cannot-check and no-clock-read
// refusals when declaredClockDropOK holds (the declared range holds both runs
// and the replay compares that check; dc.clock still refuses if the call's
// own recorded answer echoes the value). An incoming client test request is
// excluded before learning binds to it under the same declaredClockDropOK
// gate, just as readOverClients excludes it for plain window reads; client
// values outside the declared range (or on one-sided rules over windows that
// do not compare offsets) stay bound to the client request. A word learning
// read no instant in is refused no-clock-read (clockOnly).

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
// A cannot-check refusal (e.g. an inferred offset on a protocol that does not
// compare offsets) or a no-clock-read refusal is also settled whenever
// declaredClockDropOK holds (declaredClockHolds and declaredClockComparable),
// matching DeclaredClockLearned's declaredUnread rule on HTTP: dc.clock still
// refuses if the call's own answer echoes the moment.
func (dc *decider) declaredClock(path string, at variables.Address, n int, c changedInstant, src Source, d Decision) Decision {
	switch {
	case d.Outcome == OutcomeValue || d.Outcome == OutcomeClock:
		return d
	case d.Refusal != nil && !clockSettles[d.Refusal.Kind]:
		settleable := d.Refusal.Kind == RefusalCannotCheck || d.Refusal.Kind == RefusalNoClockRead
		if !settleable || !dc.declaredClockDropOK(path, src, c) {
			return d
		}
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

// declaredClockHolds reports whether src, a clock rule declared at path,
// holds both runs' values of c in dc.in.Window as a replay checks a clock
// read (CheckClockRead). Only a whole-field instant (no word inside a longer
// text) with matching layouts is checked: paired with declaredClockComparable
// (declaredClockDropOK), where both hold, the declared read goes before an
// incoming client test's request (instantLearned), as a plain read in the
// call's window does (readOverClients).
func (dc *decider) declaredClockHolds(path string, src Source, c changedInstant) bool {
	off, ok := declaredOffset(src.Clock)
	w := dc.in.Window
	if !ok || w == nil || c.rm == nil || c.lm == nil || (c.text && !c.word.IsZero()) {
		return false
	}
	if c.text {
		lay := models.ClockOffset{Layout: clockTextLayout(c.r)}.MeetLayout(clockTextLayout(c.l))
		if lay == "" || lay == models.LayoutApart {
			return false
		}
		off.Layout = lay
	} else {
		off.Layout = models.LayoutTyped
	}
	recOK, _ := CheckClockRead(path, off, dc.in.Rec.Leaves[path], w.Rec, true)
	if !recOK {
		return false
	}
	liveOK, _ := CheckClockRead(path, off, dc.in.Live.Leaves[path], w.F, true)
	return liveOK
}

// declaredClockComparable reports whether src's declared clock check is one
// w's replay compares: a plain read (min <= 0 <= max) or a window that
// compares offsets (w.offsets).
func declaredClockComparable(src Source, w *Window) bool {
	off, ok := declaredOffset(src.Clock)
	return ok && w != nil && (off.Plain() || w.offsets)
}

// declaredClockDropOK reports whether src, a clock rule declared at path,
// holds both runs in dc.in.Window (declaredClockHolds) and produces a check
// the call's replay compares (declaredClockComparable).
func (dc *decider) declaredClockDropOK(path string, src Source, c changedInstant) bool {
	return declaredClockComparable(src, dc.in.Window) && dc.declaredClockHolds(path, src, c)
}

// declaredClockHoldsLeaves reports whether src, a clock rule declared at
// rec.Path, holds both runs' whole-leaf values (rec, live) in w as a replay
// checks a clock read (CheckClockRead), with matching layouts. Used on the
// HTTP classify path (ClassifyArrivedSources) to check declared range holds
// and, paired with declaredClockComparable (declaredClockDropLeavesOK), drop
// incoming client test requests before classifying.
func declaredClockHoldsLeaves(rec, live Leaf, src Source, w *Window) bool {
	if src.Kind != SourceClock {
		return false
	}
	off, ok := declaredOffset(src.Clock)
	if !ok || w == nil || !isInstant(rec) || !isInstant(live) {
		return false
	}
	lay := models.ClockOffset{Layout: leafLayout(rec)}.MeetLayout(leafLayout(live))
	if lay == "" || lay == models.LayoutApart {
		return false
	}
	off.Layout = lay
	recOK, _ := CheckClockRead(rec.Path, off, rec, w.Rec, true)
	if !recOK {
		return false
	}
	liveOK, _ := CheckClockRead(live.Path, off, live, w.F, true)
	return liveOK
}

// declaredClockDropLeavesOK reports whether src, a clock rule declared at
// rec.Path, holds both runs' whole-leaf values in w (declaredClockHoldsLeaves)
// and produces a check the call's replay compares (declaredClockComparable).
// Used on the HTTP classify path to gate dropping incoming client requests
// consistently with decider.declaredClockDropOK.
func declaredClockDropLeavesOK(rec, live Leaf, src Source, w *Window) bool {
	return declaredClockComparable(src, w) && declaredClockHoldsLeaves(rec, live, src, w)
}

// clientWordIndex finds the word index in clients' leaf (mock, path) whose
// recorded text equals val, matching Decide's fromAt word lookup when a
// client request leaf changed in more than one word.
func clientWordIndex(clients []Earlier, mock, path, val string) int {
	for _, e := range clients {
		if e.Mock != mock {
			continue
		}
		for _, s := range e.Seen {
			if s.Leaf.Path != path {
				continue
			}
			for i, w := range variables.Words(s.Leaf.Value) {
				if s.Leaf.Value[w[0]:w[1]] == val {
					return i
				}
			}
		}
	}
	return -1
}

// clientAsidePlace formats a set-aside incoming client test request place
// for a learned reason line, matching Decide's "<mock> <path>[#<word>]" and
// "<mock> <path> (its JWT's <claim> claim)" forms.
func clientAsidePlace(from Place) string {
	switch {
	case from.Word != "":
		return from.Mock + " " + from.Word
	case from.Claim != "":
		if a, err := variables.ParseAddress(from.Claim); err == nil {
			if claim := ClaimName(a); claim != "" && from.Path != "" {
				return from.Mock + " " + from.Path + " (its JWT's " + claim + " claim)"
			}
		}
		return from.Mock + " " + from.Claim
	case from.Path != "":
		return from.Mock + " " + from.Path
	case from.Field != "":
		return from.Mock + " " + from.Field
	default:
		return placeAt(from)
	}
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
// clock: echoProvenance), noise, a drift refused for its source alone
// (Learned.sourceWhy), and—when the replay compares the declared check
// (declaredClockComparable: a plain read min<=0<=max, or a window that
// compares offsets)—a variable bound to an incoming client test request
// (From.Client, !From.Echo) or an unread-clock refusal (Learned.unreadWhy).
// Such a value is a read of the clock when the rule's range holds both runs'
// values in the call's span (w: Rec and F), in one layout, as a replay checks
// it (CheckClockRead); else, under the time default (timeForm), an instant
// each run writes in a form (TimeForm), an epoch number near its call in its
// run (nearSpan), checked by that form alone, or by either of the two its
// runs wrote (FormF), as is a read the range holds. Neither a client variable
// nor an unread-clock refusal is ever checked by its form alone when the
// declared range does not hold both runs; under the time default a read the
// range holds also gets its form. One neither check holds is refused, naming
// the rule: noise becomes unaccounted (a value the rule's own check refuses
// is never let pass unchecked), and an echo, client variable, or refused
// drift keeps learning's decision. The caller leaves out, where the time
// default is off, a value its call's recorded answer holds (a replay would
// serve that answer's recorded moment). Every other decision stands: a value
// bound to an earlier non-client place that holds it whole, a read of the
// clock or an offset from it learning found, an echo the answer carries, and
// every other refusal (two layouts or zone forms, a value its call's answer
// holds, what keploy could not read). A read the re-read since the test began
// learned in place of the offset rule's refusal (Learned.offsetRefused) is
// decided as that refusal: the rule, not learning, says how the field is
// checked.
func DeclaredClockLearned(ln Learned, src Source, w *Window, timeForm bool) Learned {
	if src.Kind == SourceClock && ln.offsetRefused != nil {
		// The rule decides the offset rule's refusal, which the re-read
		// since the test began learned as a read (Learned.offsetRefused).
		ln = *ln.offsetRefused
	}
	if src.Kind != SourceClock {
		return ln
	}
	rec, live := ln.Recorded, ln.Live
	off, ok := declaredOffset(src.Clock)
	canOverride := declaredClockComparable(src, w)
	clientVar := ln.Kind == KindVariable && ln.From.Client && !ln.From.Echo && canOverride
	// A declared clock rule whose check the replay compares (a plain read
	// min<=0<=max, or a window that compares offsets) settles an unread-clock
	// refusal when the declared range itself holds both runs, independently of
	// whether a client request happened to hold the second. One-sided offset
	// rules on !ComparesOffsets windows keep their unread refusal (and client
	// bindings).
	declaredUnread := ln.Kind == KindUnaccounted && ln.unreadWhy != "" && ln.unreadWhy == ln.Why && canOverride
	settles := ln.Kind == KindVariable && ln.From.Echo || clientVar || declaredUnread || ln.Kind == KindNoise ||
		ln.Kind == KindUnaccounted && ln.sourceWhy != "" && ln.sourceWhy == ln.Why
	if !settles {
		return ln
	}
	why := "keploy.yml's rule " + src.Rule + " declares it a read of the clock"
	aside := ln.clientAside
	if aside == "" && clientVar {
		aside = clientAsidePlace(ln.From)
	}
	var read *models.ClockOffset
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
			if aside != "" {
				off.Why += "; set aside " + aside
			}
			read = &off
		case !recHeld:
			no = "the recorded value is no such read in its run: " + recNo
		default:
			no = "round F's value is no such read in its run: " + liveNo
		}
	}
	if timeForm && (!clientVar && !declaredUnread || read != nil) {
		// The time default checks a time by its form alone, a read the
		// rule's range holds too (timeDefault), and one its two runs wrote
		// in two forms by either (timeForms), never by the range. For a
		// variable bound to an incoming client request (clientVar) or an
		// unread-clock refusal (declaredUnread), only a read the declared
		// range itself holds overrides (fail closed outside the declared range).
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
			formWhy := why + ": a time, checked by its form alone (" + forms + ", keploy.yml's times setting at its default); its range alone would refuse it: " + no
			if aside != "" {
				formWhy += "; set aside " + aside
			}
			read = &models.ClockOffset{Form: form, FormF: formF, Why: formWhy}
		}
	}
	switch {
	case read != nil:
		return Learned{Kind: KindClockRead, Recorded: rec, Live: live, Read: read, Why: read.Why, Refuted: ln.Refuted, clientAside: aside, clientRule: src.Rule}
	case ln.Kind == KindNoise:
		return Learned{Kind: KindUnaccounted, Recorded: rec, Live: live, Why: why + ", and " + no, Refuted: ln.Refuted}
	}
	return ln
}

// DeclaredClockStrictEchoRefusal rewrites an unread-clock refusal on a field
// that keploy.yml's clock rule src already declares when mock.times: strict is
// set and the call's recorded answer echoes the field: the rule cannot turn an
// echoed out-of-window value into a ClockRead (replay would serve the recorded
// moment verbatim), so the refusal names the rule and a working remedy instead
// of advising the user to declare the field noise. When w is nil (missing
// clock hooks or no earlier call and no test begin), or when the whole leaf is
// not an instant in either run (e.g. time of day or text with an embedded
// instant), or when the two runs use incompatible layouts, no rule edit can
// settle the field, so ln's original diagnosis is kept unchanged to match the
// time default. The mock.times default remedy is offered only when the rule is
// comparable on w and its range holds both runs (otherwise leaving mock.times
// at its default still refuses).
func DeclaredClockStrictEchoRefusal(ln Learned, src Source, w *Window) Learned {
	if src.Kind != SourceClock || ln.Kind != KindUnaccounted || !unreadHeld(ln) || w == nil {
		return ln
	}
	if !isInstant(ln.Recorded) || !isInstant(ln.Live) {
		return ln
	}
	if lay := (models.ClockOffset{Layout: leafLayout(ln.Recorded)}).MeetLayout(leafLayout(ln.Live)); lay == "" || lay == models.LayoutApart {
		return ln
	}
	var why string
	if declaredClockDropLeavesOK(ln.Recorded, ln.Live, src, w) {
		why = "keploy.yml's rule " + src.Rule + " declares it a read of the clock, but no clock read in this call's window matched it and the call's recorded answer echoes it, so under mock.times: strict keploy refuses to serve the recorded answer verbatim. Leave mock.times at its default in keploy.yml, or read the clock inside this call's window so keploy can learn it as an echo"
	} else {
		no := declaredClockRejectReason(ln.Recorded, ln.Live, src, w)
		why = "keploy.yml's rule " + src.Rule + " declares it a read of the clock, but no clock read in this call's window matched it, the call's recorded answer echoes it, and " + no + ". Adjust keploy.yml's rule " + src.Rule + " or read the clock inside this call's window so keploy can learn it as an echo"
	}
	if ln.clientAside != "" {
		why += "; set aside " + ln.clientAside
	}
	ln.Why = why
	ln.unreadWhy = why
	return ln
}

// declaredClockRejectReason explains why a declared clock rule src does not
// satisfy declaredClockDropLeavesOK on two compatible instants (rec, live) in
// non-nil w.
func declaredClockRejectReason(rec, live Leaf, src Source, w *Window) string {
	off, ok := declaredOffset(src.Clock)
	lay := models.ClockOffset{Layout: leafLayout(rec)}.MeetLayout(leafLayout(live))
	switch {
	case !ok:
		return "its range is one keploy does not check (a resolution of none, or a max below its min)"
	case !declaredClockComparable(src, w):
		return "keploy does not compare an offset rule on this window"
	default:
		off.Layout = lay
		recHeld, recNo := CheckClockRead(rec.Path, off, rec, w.Rec, true)
		if !recHeld {
			return "the recorded value is no such read in its run: " + recNo
		}
		_, liveNo := CheckClockRead(live.Path, off, live, w.F, true)
		return "round F's value is no such read in its run: " + liveNo
	}
}

// isInstant reports whether l names a moment keploy reads (clockReadOf): a
// time of day, which names no day, does not.
func isInstant(l Leaf) bool {
	_, _, _, ok := clockReadOf(l)
	return ok
}
