package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// The kinds of decision an About explains, as its sum and its fixed
// sentences spell them.
const (
	AboutKindNoise    = "noise"
	AboutKindVariable = "variable"
	AboutKindClock    = "clock"
)

// AboutState is what the About beside a decision says of it, as the next
// learn step reads it (one of four).
type AboutState int

const (
	// AboutUnknown: no About. A person wrote the decision, or a keploy
	// older than the format did. Learning still changes it as it always
	// has, and never stamps it (keploy does not invent provenance).
	AboutUnknown AboutState = iota
	// AboutLearned: by learned, and its sum matches the decision as it
	// reads now. Only such an entry may be re-stamped.
	AboutLearned
	// AboutEdited: by learned, with a missing or wrong sum: keploy learned
	// it and a person edited it since. Treated as hand.
	AboutEdited
	// AboutHand: by hand, or a by keploy does not write. A learned
	// decision for the same place never changes it.
	AboutHand
)

func (s AboutState) String() string {
	switch s {
	case AboutLearned:
		return "learned"
	case AboutEdited:
		return "learned, then edited by hand"
	case AboutHand:
		return "hand"
	default:
		return "unknown origin"
	}
}

// Hand reports whether the decision is a person's, so a learned decision
// for the same place never changes it: by hand, or learned and then edited.
func (s AboutState) Hand() bool { return s == AboutEdited || s == AboutHand }

// readAbout is the state a (nil: none) says of a decision whose sum, over
// a run, sum is.
func readAbout(a *About, sum func(run string) string) AboutState {
	switch {
	case a == nil:
		return AboutUnknown
	case a.By != AboutByLearned:
		return AboutHand
	case a.Sum != "" && a.Sum == sum(a.Run):
		return AboutLearned
	default:
		return AboutEdited
	}
}

// NoiseAboutState is the state of the ignored field path of spec (its
// NoiseAbout entry).
func NoiseAboutState(spec *MockSpec, path string) AboutState {
	a, ok := spec.NoiseAbout[path]
	if !ok {
		return AboutUnknown
	}
	return readAbout(&a, func(run string) string { return NoiseSum(path, run) })
}

// ClockAboutState is the state of the clock read spec learned at path (its
// ClockAbout entry, read against spec.Clocks[path]).
func ClockAboutState(spec *MockSpec, path string) AboutState {
	a, ok := spec.ClockAbout[path]
	if !ok {
		return AboutUnknown
	}
	o := spec.Clocks[path]
	return readAbout(&a, func(run string) string { return ClockSum(path, o, run) })
}

// VariableAboutState is the state of the variable name v declares (its
// About).
func VariableAboutState(name string, v MockVariable) AboutState {
	return readAbout(v.About, func(run string) string { return VariableSum(name, v, run) })
}

// NoiseSum is the sum of the ignored field path, learned in the run that
// started at run.
func NoiseSum(path, run string) string {
	return aboutSum(AboutKindNoise, path, nil, run)
}

// VariableSum is the sum of the variable name as v declares it, learned in
// the run that started at run: over its recorded value, bind, use and
// answer places (as sets: their order is no edit), echo and clock reads.
func VariableSum(name string, v MockVariable, run string) string {
	clock := make([][]any, 0, len(v.Clock))
	for _, c := range v.Clock {
		clock = append(clock, []any{c.At.UTC().Format(time.RFC3339Nano), int64(c.Res), c.Day})
	}
	return aboutSum(AboutKindVariable, name, []any{v.Recorded, v.Bind, aboutSortedCopy(v.Use), aboutSortedCopy(v.Answer), v.Echo, clock}, run)
}

// ClockSum is the sum of the clock read o at path, learned in the run that
// started at run.
func ClockSum(path string, o ClockOffset, run string) string {
	return aboutSum(AboutKindClock, path, []any{int64(o.Min), int64(o.Max), int64(o.Res), o.Zone, o.Layout}, run)
}

// aboutSum is the first 8 hex characters of the sha256 of the JSON array
// [kind, key, fields, run]: a sum another tool can make again from the
// decision as it reads on disk.
func aboutSum(kind, key string, fields []any, run string) string {
	if fields == nil {
		fields = []any{}
	}
	b, err := json.Marshal([]any{kind, key, fields, run})
	if err != nil {
		// Strings, numbers, bools and lists of them always marshal.
		panic("models: about sum: " + err.Error())
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:8]
}

func aboutSortedCopy(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

// aboutRoundWords is round, the round that learned a decision, in words.
func aboutRoundWords(round string) string {
	switch round {
	case AboutRoundFull:
		return "record's full round"
	case AboutRoundKeployTest:
		return "a keploy test run"
	default:
		return "a later run"
	}
}

// AboutWhy is the fixed why keploy writes for a decision of kind (an echo
// variable when echo) learned in round, when the protocol's learn step gave
// none. It holds no value.
func AboutWhy(kind string, echo bool, round string) string {
	in := aboutRoundWords(round)
	switch kind {
	case AboutKindNoise:
		return "its value changed between the recording and " + in + ", so it is not compared"
	case AboutKindClock:
		return "a read of the app's clock: it moved with the clock between the recording and " + in + ", so a replay checks it is a read of the clock made in its call's window"
	case AboutKindVariable:
		if echo {
			return "the dependency answers with a value this call sends, and it changed between the recording and " + in + ", so the answer is served with the value the run sends"
		}
		return "the value changed between the recording and " + in + " and this test's calls pass it on, so each place named here holds the value the run makes, not the recorded one"
	}
	return ""
}

// AboutUndo is the fixed sentence that says how to undo the decision of
// kind at key (a path, or a variable's name) learned in round, and what the
// next learning does then.
func AboutUndo(kind, key, round string) string {
	again := "a fresh record of this test learns it again"
	if round == AboutRoundKeployTest {
		again = "the next keploy test run that detects noise learns it again"
	}
	switch kind {
	case AboutKindNoise:
		return "delete " + key + " from noise.req and from noise.about to compare it again; " + again
	case AboutKindClock:
		return "delete " + key + " from clocks to compare it as recorded again; " + again
	case AboutKindVariable:
		return "delete " + key + " here and on every call of this test that names " + key + "; those places are then compared as recorded, and " + again
	}
	return ""
}

// Stamped is a, the About the learn step gave a decision of kind at key, as
// keploy writes it on a decision it adds: by learned, the fixed why when it
// has none, the undo for its round, and the sum, over the run, of the
// decision as sum makes it.
func (a About) Stamped(kind, key string, echo bool, sum func(run string) string) About {
	a = a.Clone()
	a.By = AboutByLearned
	if a.Why == "" {
		a.Why = AboutWhy(kind, echo, a.Round)
	}
	a.Undo = AboutUndo(kind, key, a.Round)
	a.Sum = sum(a.Run)
	return a
}
