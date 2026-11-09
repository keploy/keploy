package models

import (
	"sync"
	"time"
)

// An answer's release (founder, 2026-11-05). A call is matched and consumed
// when it is made, in the test that made it. An answer recorded in a later
// window than its call (a long-running request, a stream whose pieces came
// test after test) is written to the application no earlier than the replay
// reaches that answer's recorded place among the set's windows: its write is
// held, its match never is. A replay never holds an answer past what record
// did: the place moves only on events the replayer drives (a test's window
// opens, a test ends, the set ends), never on a call the application may not
// make, and every hold ends at the set's end.
//
// The parser holds and writes; the decision, whether a recorded moment is due
// at the run's place, is registered (RegisterAnswerRelease). With none, the
// parser writes every answer at once.

// RunPlace is where a replay's run is among the recorded test windows of the
// set it replays (the agent's place facet, integrations.RunPlacer).
type RunPlace struct {
	// Windows are the set's recorded test windows, the unselected and
	// skipped ones included; nil where the replay sent none.
	Windows *WindowSchedule
	// Reached is the latest anchor the run has reached: 0 before any window
	// opened (the set's start-up), RunOpen(i) once window i opened, and
	// RunEnd(i) once its test ended, i indexing Windows. It never goes back
	// within one staging: an anchor at or before it is passed.
	Reached int
	// SetEnd is set once the run has reached the set's end, which passes
	// every anchor.
	SetEnd bool
	// Staging is the set's staging the place is of: a new set's staging
	// begins a new place, and a hold taken under an earlier one is over, as
	// its set is.
	Staging uint64
}

// RunOpen is the anchor of the open of the window at index i of a RunPlace's
// Windows.
func RunOpen(i int) int { return 2*i + 1 }

// RunEnd is the anchor of the end of the test whose window is at index i of a
// RunPlace's Windows: the replay has its answer, or gave up on it.
func RunEnd(i int) int { return 2*i + 2 }

// AnswerRelease reports whether a piece of an answer recorded at at is due at
// the run's place. It is called whenever the place changes while a piece
// waits, must be cheap, and must not block. It must be monotone in time: what
// is due at a moment is due at every earlier one, so an answer whose last
// byte is due is due whole.
type AnswerRelease func(place RunPlace, at time.Time) bool

// RegisterAnswerRelease declares, for answers of kind, when a recorded piece
// is due (AnswerRelease). Several registrations for one kind must all find a
// piece due. A nil release is ignored. Kinds nobody registers write every
// answer at once.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterAnswerRelease(kind Kind, release AnswerRelease) (unregister func()) {
	if release == nil {
		return func() {}
	}
	answerReleases.mu.Lock()
	defer answerReleases.mu.Unlock()
	answerReleases.next++
	id := answerReleases.next
	if answerReleases.byKind == nil {
		answerReleases.byKind = map[Kind][]answerReleaseEntry{}
	}
	answerReleases.byKind[kind] = append(answerReleases.byKind[kind], answerReleaseEntry{id: id, fn: release})
	return func() {
		answerReleases.mu.Lock()
		defer answerReleases.mu.Unlock()
		var kept []answerReleaseEntry
		for _, e := range answerReleases.byKind[kind] {
			if e.id != id {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(answerReleases.byKind, kind)
			return
		}
		answerReleases.byKind[kind] = kept
	}
}

// AnswerReleaseOf is the release registered for kind: every registration's
// decision AND-ed. ok is false when none is registered: the parser writes at
// once.
func AnswerReleaseOf(kind Kind) (release AnswerRelease, ok bool) {
	answerReleases.mu.Lock()
	entries := append([]answerReleaseEntry(nil), answerReleases.byKind[kind]...)
	answerReleases.mu.Unlock()
	if len(entries) == 0 {
		return nil, false
	}
	return func(place RunPlace, at time.Time) bool {
		for _, e := range entries {
			if !e.fn(place, at) {
				return false
			}
		}
		return true
	}, true
}

type answerReleaseEntry struct {
	id uint64
	fn AnswerRelease
}

var answerReleases struct {
	mu     sync.Mutex
	next   uint64
	byKind map[Kind][]answerReleaseEntry
}
