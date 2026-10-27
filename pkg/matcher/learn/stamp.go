package learn

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Stamp is what keploy's CLI knows of everything one run learns, which it
// writes into the about of each decision it adds (models.About): the round,
// when the run started, its own version, and the test open when each
// mock's decisions were learned.
type Stamp struct {
	// Round is models.AboutRoundFull or models.AboutRoundKeployTest.
	Round string
	// Run is when the run started, UTC, RFC 3339 (RunStarted).
	Run string
	// Keploy is the version keploy --version prints.
	Keploy string
	// Test is the test open when each mock's decisions were learned, by
	// mock name, as mappings.yaml names it; a mock it does not name was
	// learned between tests (models.AboutBetweenTests).
	Test map[string]string
}

var runStarted = time.Now().UTC().Format(time.RFC3339)

// RunStarted is when this keploy run started, UTC, RFC 3339: the run of
// each decision it learns (models.About.Run).
func RunStarted() string { return runStarted }

// stamp is a with s's fields for a decision learned on a mock while test
// was open.
func (s Stamp) stamp(a models.About, test string) models.About {
	a = a.Clone()
	a.Round, a.Run, a.Keploy, a.Test = s.Round, s.Run, s.Keploy, test
	return a
}

// StampStates is states with s in the about of each decision each carries
// (its learned noise, clock reads, variables and the variables it binds at
// earlier mocks), one made for a decision the learn step gave none: what
// the protocol's learn step said (why, seen, call) stays. It copies what it
// changes, so states is untouched. MergeLearned then completes the about
// of each decision it adds (models.About.Stamped).
func StampStates(states map[string]models.MockState, s Stamp) map[string]models.MockState {
	if states == nil {
		return nil
	}
	out := make(map[string]models.MockState, len(states))
	for name, st := range states {
		test := s.Test[name]
		if test == "" {
			test = models.AboutBetweenTests
		}
		if len(st.ReqBodyNoise) > 0 {
			about := make(map[string]models.About, len(st.ReqBodyNoise))
			for path := range st.ReqBodyNoise {
				about[path] = s.stamp(st.NoiseAbout[path], test)
			}
			st.NoiseAbout = about
		}
		if len(st.Clocks) > 0 {
			about := make(map[string]models.About, len(st.Clocks))
			for path := range st.Clocks {
				about[path] = s.stamp(st.ClockAbout[path], test)
			}
			st.ClockAbout = about
		}
		st.Variables = stampVariables(st.Variables, s, test)
		if len(st.Binds) > 0 {
			binds := make(map[string]models.MockVariables, len(st.Binds))
			for target, vs := range st.Binds {
				binds[target] = stampVariables(vs, s, test)
			}
			st.Binds = binds
		}
		out[name] = st
	}
	return out
}

// stampVariables is vs, on a copy, with s in each one's about.
func stampVariables(vs models.MockVariables, s Stamp, test string) models.MockVariables {
	if len(vs) == 0 {
		return vs
	}
	out := vs.Clone()
	for n, v := range out {
		var a models.About
		if v.About != nil {
			a = *v.About
		}
		a = s.stamp(a, test)
		v.About = &a
		out[n] = v
	}
	return out
}
