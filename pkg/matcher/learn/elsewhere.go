package learn

import (
	"maps"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// What record's full round keeps of the clock reads a call made in another
// window than its recording's taught the recording, for learning only (the
// agent's LearnedElsewhere): one rule for both of keploy's full rounds, the
// one keploy mock record runs and the one keploy record runs.

// OwnPairingWins splits elsewhere, what calls made in other windows than
// their recordings' taught them, by one pairing per recording per round,
// its own one winning. A recording a call of the round consumed was paired
// by the rules that serve it (its own test's call, or the start-up and
// pool's), in its own window: stronger evidence than a call of its shape in
// another window, so what such a call taught it is voided, whichever came
// first. The agent never pairs a consumed recording across windows, so a
// teaching beside a consumption came first. consumed names the recordings
// a call of the round consumed. kept is the rest, in order; a state with
// no name or no clock read is in neither.
func OwnPairingWins(consumed func(name string) bool, elsewhere []models.MockState) (kept, voided []models.MockState) {
	for _, e := range elsewhere {
		if e.Name == "" || len(e.Clocks) == 0 {
			continue
		}
		if consumed(e.Name) {
			voided = append(voided, e)
			continue
		}
		kept = append(kept, e)
	}
	return kept, voided
}

// WithLearnedElsewhere is consumed with a state of its own for each
// recording elsewhere taught that no call of the round consumed: its clock
// reads alone, each ClockOffset as the agent sent it. Anything else a state
// of elsewhere carries (noise, variables) is never written onto a recording
// from a call made in another window, whatever the agent sent. A recording
// a call of the round consumed keeps what its own pairing learned alone
// (OwnPairingWins): what another window taught it is never merged into it.
// Two states elsewhere sends for one recording (an agent that taught a
// recording more than once a round) are merged (models.MergeClocks), never
// overwritten. consumed is not changed.
func WithLearnedElsewhere(consumed, elsewhere []models.MockState) []models.MockState {
	if len(elsewhere) == 0 {
		return consumed
	}
	out := slices.Clone(consumed)
	own := len(out)
	at := make(map[string]int, len(out))
	for i, st := range out {
		at[st.Name] = i
	}
	for _, e := range elsewhere {
		if e.Name == "" || len(e.Clocks) == 0 {
			continue
		}
		i, ok := at[e.Name]
		switch {
		case !ok:
			at[e.Name] = len(out)
			out = append(out, models.MockState{Name: e.Name, Kind: e.Kind, Clocks: maps.Clone(e.Clocks)})
		case i >= own:
			out[i].Clocks = models.MergeClocks(out[i].Clocks, e.Clocks)
		}
	}
	return out
}

// TaughtInAny reports whether any of ws is a window of a test in tests, or a
// window keploy did not know the test of ("": between tests, or a call read
// in a round that moved since): such learning is that window's, and
// whatever undoes what a window learned undoes it with any window's.
func TaughtInAny(ws []models.TeachingWindow, tests map[string]bool) bool {
	if len(tests) == 0 {
		return false
	}
	for _, w := range ws {
		if w.Test == "" || tests[w.Test] {
			return true
		}
	}
	return false
}

// SayWindows is ws as the record says them: "<test>'s window" or "between
// tests", comma-separated.
func SayWindows(ws []models.TeachingWindow) string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if w.Test == "" {
			out = append(out, "between tests")
			continue
		}
		out = append(out, w.Test+"'s window")
	}
	return strings.Join(out, ", ")
}
