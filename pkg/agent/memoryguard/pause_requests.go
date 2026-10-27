package memoryguard

import (
	"sync"
	"sync/atomic"
)

// ingressRequests counts every request the ingress has read (IngressRequest),
// over the agent's life.
var ingressRequests atomic.Uint64

// pauseEdgeMu orders each change of the pause flag (applyPausedState) with the
// count of ingress requests read at it, and with each open PauseTally. pauseOn
// is the flag as the last change left it; tallies are the open tallies.
var (
	pauseEdgeMu sync.Mutex
	pauseOn     bool
	tallies     = map[*PauseTally]struct{}{}
)

// PauseTally counts, for one recording session, the ingress requests that came
// while the memory guard had paused recording, by the order of events and
// apart from any capture decision: it reads the count of ingress requests at
// each pause's start and at its end, a real pause and the forced one alike. A
// request the ingress counts after a pause started and before it ended came
// during that pause, whatever its capture then did with it. A pause costs those
// requests and the few in flight at its two edges (a request counted just
// before a pause starts may read it), and no more: a recording that leaves out
// more than that, on a connection that stays uncaptured after a pause, does so
// against this count. It counts the requests of the ingress that counts them
// as it reads them (IngressRequest: HTTP/1), not gRPC calls.
type PauseTally struct {
	// Under pauseEdgeMu.
	start       uint64 // the count as the tally opened
	openAt      uint64 // the count as the pause in progress began, or as the tally opened during it
	during      uint64 // requests during the tally's pauses that have ended
	sawPause    bool
	beforeFirst uint64 // requests from the tally's open to its first pause's start
	closed      bool
	end         uint64 // the count as the tally closed
}

// PauseRequests is what a PauseTally has counted.
type PauseRequests struct {
	// Requests is the ingress requests read since the tally opened.
	Requests uint64
	// DuringPauses is those of them that came while recording was paused:
	// between a pause's start and its end, the pause still on counted to now.
	DuringPauses uint64
	// BeforeFirstPause is those of them that came before the first pause
	// after the tally opened: all of them when there was none, none when a
	// pause was on as it opened.
	BeforeFirstPause uint64
}

// OpenPauseTally starts counting, from now, the ingress requests that come
// during the guard's pauses. The caller must Close it; until then each pause's
// start and end reach it.
func OpenPauseTally() *PauseTally {
	pauseEdgeMu.Lock()
	defer pauseEdgeMu.Unlock()
	n := ingressRequests.Load()
	t := &PauseTally{start: n, openAt: n, sawPause: pauseOn}
	tallies[t] = struct{}{}
	return t
}

// Read returns what the tally has counted so far. Nil-safe.
func (t *PauseTally) Read() PauseRequests {
	if t == nil {
		return PauseRequests{}
	}
	pauseEdgeMu.Lock()
	defer pauseEdgeMu.Unlock()
	if t.closed {
		return PauseRequests{Requests: t.end - t.start, DuringPauses: t.during, BeforeFirstPause: t.beforeFirst}
	}
	n := ingressRequests.Load()
	out := PauseRequests{Requests: n - t.start, DuringPauses: t.during, BeforeFirstPause: t.beforeFirst}
	if pauseOn {
		out.DuringPauses += n - t.openAt
	}
	if !t.sawPause {
		out.BeforeFirstPause = n - t.start
	}
	return out
}

// Close stops the tally counting; what it counted until then stays readable,
// as if the pause still on, if one is, ended as it closed. Idempotent and
// nil-safe.
func (t *PauseTally) Close() {
	if t == nil {
		return
	}
	pauseEdgeMu.Lock()
	defer pauseEdgeMu.Unlock()
	if t.closed {
		return
	}
	n := ingressRequests.Load()
	if pauseOn {
		t.during += n - t.openAt
	}
	if !t.sawPause {
		t.sawPause = true
		t.beforeFirst = n - t.start
	}
	t.end = n
	t.closed = true
	delete(tallies, t)
}

// notePauseEdgeLocked is applyPausedState's half that reads the count of
// ingress requests as the pause starts or ends, after the flag is set: a
// request counted after it reads the flag as it now is. A call that does not
// change the flag (the guard re-applies a pause on each tick it lasts) is no
// edge. pauseEdgeMu is held.
func notePauseEdgeLocked(paused bool) {
	if paused == pauseOn {
		return
	}
	pauseOn = paused
	n := ingressRequests.Load()
	for t := range tallies {
		if paused {
			t.openAt = n
			if !t.sawPause {
				t.sawPause = true
				t.beforeFirst = n - t.start
			}
			continue
		}
		t.during += n - t.openAt
	}
}
