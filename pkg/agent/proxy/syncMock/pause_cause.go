package manager

import (
	"sync"
	"time"
)

// PauseCause is why recording is paused. The pause itself is one flag
// (memoryguard.IsRecordingPaused) and one span per manager
// (SyncMockManager.pressure), whatever caused it; the cause is kept beside
// them, for the process, so that what a pause costs is reported truthfully:
// "the agent was short of memory" is false of a pause its control plane asked
// for.
type PauseCause uint8

const (
	// PauseCauseMemory is the memory guard's pause: the agent was short of
	// memory (and the debug knob that forces such a pause).
	PauseCauseMemory PauseCause = 1 << iota
	// PauseCauseExternal is a pause the agent was asked for from outside (its
	// control plane, over POST /agent/record/pause): the agent was not short
	// of anything.
	PauseCauseExternal
)

// String names the cause as the logs do.
func (c PauseCause) String() string {
	switch c {
	case PauseCauseExternal:
		return "control_plane_pause"
	case PauseCauseMemory | PauseCauseExternal:
		return "memory_pressure+control_plane_pause"
	case PauseCauseMemory:
		return "memory_pressure"
	}
	return "none"
}

// pauseCauses is the process's record of what paused recording and when:
// the causes in force now, the last ones that were, and a span per cause.
// memoryguard writes it (NotePauseCauses) around each change of its flag.
var pauseCauses struct {
	mu            sync.Mutex
	now, last     PauseCause
	closeMemory   func()
	closeExternal func()
	memory        Spans
	external      Spans
}

// NotePauseCauses records the causes for which recording is paused from now:
// none once it is not. memoryguard calls it before it sets its flag for a
// cause that begins, and after the managers were told for one that ended, so
// each cause's span here covers the managers' spans it caused. Idempotent.
func NotePauseCauses(c PauseCause) {
	now := time.Now()
	p := &pauseCauses
	p.mu.Lock()
	defer p.mu.Unlock()
	if c == p.now {
		return
	}
	if c != 0 {
		p.last = c
	}
	p.closeMemory = moveCauseSpan(&p.memory, p.closeMemory, c&PauseCauseMemory != 0, now)
	p.closeExternal = moveCauseSpan(&p.external, p.closeExternal, c&PauseCauseExternal != 0, now)
	p.now = c
}

// moveCauseSpan opens a span of spans at now when on and none is open, closes
// the open one when not, and returns the open span's closer (nil for none).
func moveCauseSpan(spans *Spans, closer func(), on bool, now time.Time) func() {
	switch {
	case on && closer == nil:
		return spans.Open(now)
	case !on && closer != nil:
		closer()
		return nil
	}
	return closer
}

// PauseCauseNow is why recording is paused now, as one cause to report:
// PauseCauseExternal only when the control plane's pause is the sole cause in
// force, PauseCauseMemory otherwise, which is also what a pause nobody noted
// the cause of (a manager told of pressure directly, a test's) is reported as.
func PauseCauseNow() PauseCause {
	p := &pauseCauses
	p.mu.Lock()
	c := p.now
	p.mu.Unlock()
	return reportedCause(c)
}

// pauseCauseLatest is PauseCauseNow, or, once recording is no longer paused,
// the cause of the last pause that was noted: a request's capture can be
// decided just after the pause that cost it has ended.
func pauseCauseLatest() PauseCause {
	p := &pauseCauses
	p.mu.Lock()
	c := p.now
	if c == 0 {
		c = p.last
	}
	p.mu.Unlock()
	return reportedCause(c)
}

// PauseCauseIn is the one cause to report for what a pause cost over
// [start, end]: PauseCauseExternal when a control-plane pause overlaps it and
// no memory pause does, PauseCauseMemory otherwise.
func PauseCauseIn(start, end time.Time) PauseCause {
	p := &pauseCauses
	ext, _ := p.external.Overlaps(start, end)
	if !ext {
		return PauseCauseMemory
	}
	if mem, _ := p.memory.Overlaps(start, end); mem {
		return PauseCauseMemory
	}
	return PauseCauseExternal
}

// reportedCause folds a set of causes into the one a report names.
func reportedCause(c PauseCause) PauseCause {
	if c == PauseCauseExternal {
		return PauseCauseExternal
	}
	return PauseCauseMemory
}

// ResetPauseCausesForTest forgets every cause and span noted. Tests only.
func ResetPauseCausesForTest() {
	p := &pauseCauses
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closeMemory != nil {
		p.closeMemory()
	}
	if p.closeExternal != nil {
		p.closeExternal()
	}
	p.closeMemory, p.closeExternal = nil, nil
	p.now, p.last = 0, 0
	p.memory.reset()
	p.external.reset()
}

// reset empties the spans. Tests only.
func (s *Spans) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed, s.open, s.recorded = nil, nil, 0
	s.checked = time.Time{}
}
