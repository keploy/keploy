package learn

import (
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
