package memoryguard

import (
	"sync"
	"sync/atomic"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// Recording is paused for one or more independent reasons, and
// IsRecordingPaused, the one flag every consumer reads, is their OR: the
// memory guard's own (the agent is short of memory), the debug knob's
// (ForcePauseEnv), a test's (SetRecordingPausedForTest), and an external one
// (SetExternalPause: the agent's control plane asked for it, for example while
// the application's rollout is in progress).
//
// Each owner sets and clears only its own reason. So the guard, which clears
// its reason whenever memory recovers, cannot end a pause the control plane
// asked for, and the control plane's resume cannot end a pause the agent needs
// to stay under its memory limit. What a pause does (the mock managers' spans,
// the pressure hooks, each PauseTally's count) happens on the edges of the OR
// alone: a reason set while another already holds the pause, or cleared while
// another still does, changes none of it.
type pauseReason uint32

const (
	reasonMemory pauseReason = 1 << iota
	reasonForced
	reasonExternal
	reasonTest
)

var (
	// pauseApplyMu makes one change of a reason, and what it tells the mock
	// managers and the pressure hooks, one step: two reasons changing at once
	// cannot then tell them of the pause's end and its start in the wrong
	// order. Taken before pauseEdgeMu, and never while it is held.
	pauseApplyMu sync.Mutex
	// pauseReasons is the reasons in force. Under pauseEdgeMu, with the flag
	// and the request count read at its change (pause_requests.go).
	pauseReasons pauseReason
	// externalPauseReason is the free text the external pause was asked with
	// (ExternalPaused). Under pauseEdgeMu.
	externalPauseReason string
	// externalPaused mirrors reasonExternal for ExternalPausedNow, which the
	// proxy reads once per accepted connection.
	externalPaused atomic.Bool
	// otherPaused mirrors every other reason, for PausedForMemory.
	otherPaused atomic.Bool
)

// setReasonLocked sets or clears r and moves the flag and each PauseTally to
// the OR of the reasons. It reports the flag as it now is. pauseEdgeMu is
// held.
func setReasonLocked(r pauseReason, on bool) (paused bool) {
	if on {
		pauseReasons |= r
	} else {
		pauseReasons &^= r
	}
	paused = pauseReasons != 0
	externalPaused.Store(pauseReasons&reasonExternal != 0)
	otherPaused.Store(pauseReasons&^reasonExternal != 0)
	// The flag and the request count read at its change move together, so
	// each open PauseTally sees the pauses in the order the ingress does. A
	// change that leaves the OR as it was is no edge (notePauseEdgeLocked).
	recordingPaused.Store(paused)
	notePauseEdgeLocked(paused)
	return paused
}

// causesOf is the reasons rs as the mock managers' reports name them
// (syncMock.PauseCause).
func causesOf(rs pauseReason) syncMock.PauseCause {
	var c syncMock.PauseCause
	if rs&(reasonMemory|reasonForced|reasonTest) != 0 {
		c |= syncMock.PauseCauseMemory
	}
	if rs&reasonExternal != 0 {
		c |= syncMock.PauseCauseExternal
	}
	return c
}

// applyPauseReason sets or clears one reason for a pause and tells the mock
// managers and the pressure hooks what recording now is: paused while any
// reason holds. text is the external reason's free text.
//
// A reason set is told to them even when recording was paused already: the
// memory guard re-applies its pause on every tick it lasts, and each tick lets
// go of what the managers have buffered since (SetMemoryPressure), as it
// always did, and a clear that leaves recording unpaused is told to them
// however often it comes, as it always was. A reason cleared while another
// still holds the pause is not told: nothing changed for them, and telling
// them "paused" again would only drop mocks for a reason that is not theirs.
func applyPauseReason(r pauseReason, on bool, text string) {
	pauseApplyMu.Lock()
	defer pauseApplyMu.Unlock()

	pauseEdgeMu.Lock()
	if r == reasonExternal {
		if on {
			externalPauseReason = text
		} else {
			externalPauseReason = ""
		}
	}
	if on {
		// A cause that begins is noted before the flag is set, so that
		// whatever then reads the flag, or a manager's span, finds its cause.
		syncMock.NotePauseCauses(causesOf(pauseReasons | r))
	}
	paused := setReasonLocked(r, on)
	after := causesOf(pauseReasons)
	pauseEdgeMu.Unlock()

	// Skipped only for a reason cleared while another still holds the pause.
	if on || !paused {
		// Global manager: the buffering manager in single-app mode.
		if mgr := syncMock.Get(); mgr != nil {
			mgr.SetMemoryPressure(paused)
		}
		// Fan out to registered managers (multi-app: one per app). The global
		// trigger stays global; only the action reaches every live buffer.
		pressureHookMu.RLock()
		hooks := make([]func(paused bool), 0, len(pressureHooks))
		for _, fn := range pressureHooks {
			hooks = append(hooks, fn)
		}
		pressureHookMu.RUnlock()
		for _, fn := range hooks {
			fn(paused)
		}
	}
	// A cause that ended is noted once the managers have closed their spans,
	// so its span covers theirs.
	syncMock.NotePauseCauses(after)
}

// SetExternalPause pauses (true) or resumes (false) recording for a reason
// outside the agent: its control plane's, for example while the application's
// rollout is in progress. reason is free text for the logs ("rollout").
//
// It is one reason for a pause among several (IsRecordingPaused is their OR):
// memory recovering does not end an external pause, and ending an external
// pause does not end one the memory guard holds. While it is on, every
// consumer of IsRecordingPaused behaves as under a memory pause: the ingress
// records no test case, the mock managers drop the mocks requested during it
// and the recording leaves out every test case whose window overlaps it.
// Idempotent; a repeated pause replaces the reason text. It is process
// memory: an agent that restarts starts unpaused.
func SetExternalPause(paused bool, reason string) {
	applyPauseReason(reasonExternal, paused, reason)
}

// ExternalPaused reports whether an external pause is on (SetExternalPause),
// and the reason it was asked with.
func ExternalPaused() (paused bool, reason string) {
	pauseEdgeMu.Lock()
	defer pauseEdgeMu.Unlock()
	return pauseReasons&reasonExternal != 0, externalPauseReason
}

// ExternalPausedNow is ExternalPaused without the reason, at the cost of one
// atomic load: what a connection's handler reads to decide whether the
// connection is passed through unread.
func ExternalPausedNow() bool {
	return externalPaused.Load()
}

// PauseCause names why recording is paused now, for a log line or a report:
// "" when it is not, "memory_pressure", "control_plane_pause", or both joined
// by "+". The debug knob's pause and a test's read as memory pressure, which
// they stand in for.
func PauseCause() string {
	pauseEdgeMu.Lock()
	c := causesOf(pauseReasons)
	pauseEdgeMu.Unlock()
	if c == 0 {
		return ""
	}
	return c.String()
}

// PausedForMemory reports whether recording is paused for a reason other
// than the external one: the agent is short of memory (or the debug knob, or
// a test, stands in for that).
func PausedForMemory() bool {
	return otherPaused.Load()
}
