package learn

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// LearnedElsewhereReader is an optional extension of keploy's
// instrumentation: what record's full round's learn step taught recordings
// from calls made in other windows than theirs, for learning only
// (AgentClient's GetLearnedElsewhere).
type LearnedElsewhereReader interface {
	GetLearnedElsewhere(ctx context.Context) ([]models.MockState, error)
}

// ReadLearnedElsewhere is what from's learn step taught across windows
// (LearnedElsewhereReader): nil from one that cannot say, and, said in a
// warning with fields, when the read failed. None of it served a call or
// decides a verdict, so a round that cannot read it keeps what its consumed
// recordings learned.
func ReadLearnedElsewhere(ctx context.Context, from any, logger *zap.Logger, fields ...zap.Field) []models.MockState {
	r, ok := from.(LearnedElsewhereReader)
	if !ok {
		return nil
	}
	learned, err := r.GetLearnedElsewhere(ctx)
	if err != nil {
		logger.Warn("keploy could not read what record's full round learned onto recordings from calls made in other windows than theirs; it keeps what their own calls taught them, and a recording only another window's call taught compares every field",
			append(slices.Clip(fields), zap.Error(err), zap.String("next_step", "record the test set again"))...)
		return nil
	}
	return learned
}

// Taught is what record's full round wrote onto one recording at the places
// calls made in other windows than its recording's taught it, for learning
// only, as the record says it (SplitTaught).
type Taught struct {
	// Mock is the recording.
	Mock string `yaml:"mock" json:"mock"`
	// Owner is the test whose mapping lists the recording, "" for none (the
	// set's start-up and pool, or its worker scope: OwnerSaid). No call of
	// the round consumed it: what a call in another window taught a
	// recording its own pairing took is voided (OwnPairingWins).
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	// Fields are what was written at the clock reads such calls taught, each
	// as "<path> (<clock read>)".
	Fields []string `yaml:"fields" json:"fields"`
	// TaughtIn are the windows those calls came in, in time order: the test
	// open when they came, "" between tests or where keploy no longer knew
	// it, and when the last of each taught the recording.
	TaughtIn []models.TeachingWindow `yaml:"taughtIn" json:"taughtIn"`
}

// Windows is t's windows as the record says them (SayWindows).
func (t Taught) Windows() string { return SayWindows(t.TaughtIn) }

// SplitTaught splits added, what the mock store wrote by recording, each as
// "<path> (<what>)", into what kept (OwnPairingWins) taught and the rest.
// Learning across windows teaches clock reads alone, so a field is taught
// only at the path of a clock read a state of kept carries for its
// recording; every state of one recording counts (a recording taught in two
// windows). taught is by recording, sorted, with its owner (owner); own is
// added less those fields (added itself when there are none).
func SplitTaught(added map[string][]string, kept []models.MockState, owner map[string]string) (own map[string][]string, taught []Taught) {
	by, at := map[string]*Taught{}, map[string]map[string]bool{}
	for _, e := range kept {
		if e.Name == "" || len(e.Clocks) == 0 {
			continue
		}
		if by[e.Name] == nil {
			by[e.Name], at[e.Name] = &Taught{Mock: e.Name, Owner: owner[e.Name]}, map[string]bool{}
		}
		by[e.Name].TaughtIn = append(by[e.Name].TaughtIn, e.TaughtIn...)
		for path := range e.Clocks {
			at[e.Name][path] = true
		}
	}
	own = added
	for _, name := range slices.Sorted(maps.Keys(by)) {
		t, rest := by[name], []string(nil)
		for _, f := range added[name] {
			if path, _, ok := strings.Cut(f, " ("); ok && at[name][path] {
				t.Fields = append(t.Fields, f)
			} else {
				rest = append(rest, f)
			}
		}
		if len(t.Fields) == 0 {
			continue
		}
		if len(taught) == 0 {
			own = maps.Clone(added)
		}
		if own[name] = rest; len(rest) == 0 {
			delete(own, name)
		}
		sort.SliceStable(t.TaughtIn, func(i, j int) bool { return t.TaughtIn[i].At.Before(t.TaughtIn[j].At) })
		taught = append(taught, *t)
	}
	return own, taught
}

// SayLearnedElsewhere says, with fields, what calls made in other windows
// taught recordings (taught), and what a recording's own pairing voided of
// such teaching (voided, OwnPairingWins), each under the test whose
// recording it is (owner), or the set's scope that serves it (OwnerSaid:
// worker names the set's worker-scope recordings, nil for a set with none).
func SayLearnedElsewhere(logger *zap.Logger, taught []Taught, voided []models.MockState, owner map[string]string, worker func(name string) bool, fields ...zap.Field) {
	for _, t := range taught {
		logger.Info("learned fields that changed between record and replay, from calls made in other windows than their recording's, for learning only",
			append(slices.Clip(fields), zap.String("mock", t.Mock), zap.String("test", OwnerSaid(t.Mock, owner, worker)), zap.Strings("fields", t.Fields),
				zap.String("taughtIn", t.Windows()), zap.String("evidence", "a call of its shape in another window: no call of the round consumed the recording"))...)
	}
	for _, v := range voided {
		logger.Info("kept what a recording's own pairing learned and voided what a call made in another window taught it: a call of the round consumed the recording, and its own pairing wins",
			append(slices.Clip(fields), zap.String("mock", v.Name), zap.String("test", OwnerSaid(v.Name, owner, worker)), zap.Strings("voided", slices.Sorted(maps.Keys(v.Clocks))),
				zap.String("taughtIn", SayWindows(v.TaughtIn)), zap.String("evidence", "its own pairing in the round"))...)
	}
}

// OwnerSaid is, as the record says it, the test whose mapping lists the
// recording name (owner), else the set's worker scope when worker names it
// (an idle answer a poll taught across windows is one), else the set's
// start-up and pool.
func OwnerSaid(name string, owner map[string]string, worker func(name string) bool) string {
	switch {
	case owner[name] != "":
		return owner[name]
	case worker != nil && worker(name):
		return "the set's worker scope"
	}
	return "the set's start-up and pool"
}
