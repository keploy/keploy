package cbshim

// attach_state.go — per-PID attach-state tracking, modeled on
// enterprise's tlsAttachState (pkg/agent/proxy/tls_attach.go).
//
// The state machine matters because libpq / libcrypto inside a wheel
// often aren't mapped at process startup — psycopg2-binary, for
// instance, dlopen()s its bundled libssl + libpq the first time the
// app calls psycopg2.connect(), which can happen seconds after main
// has already attached uprobes to whatever system libcrypto loaded
// alongside Python's ssl module. A one-shot AttachToPID at app launch
// would silently miss the wheel.
//
// The fix is a rescan loop: AttachToPID is cheap when nothing new has
// appeared (every (pid, libcrypto path) attach is idempotent —
// duplicate attaches are no-ops), so the watcher can poll /proc/<pid>/
// maps every few seconds without paying meaningful overhead. The
// state struct tracks per-PID retry budget and gives tests a way to
// await transitions deterministically without polling.

import (
	"sync"
	"time"
)

// attachState carries per-PID retry / progress state. Mirrors the
// fields enterprise's tlsAttachState uses:
//
//   - attempts: how many failed AttachToPID calls have happened.
//     Used to bound retries when a PID never produces a usable
//     libcrypto.
//   - pending: an AttachToPID call is in-flight; the rescan loop
//     skips this PID until pending clears.
//   - done: all expected libraries have been attached and the PID
//     no longer needs scanning.
//   - firstSuccess: timestamp of the first successful uprobe attach.
//     The rescan loop keeps polling after this — see comment above
//     about lazily-loaded wheel libraries — but tests use it as a
//     marker that at least one attach has happened.
//   - changedCh: closed-and-replaced on every state transition.
//     Tests await deterministic transitions through waitForChange;
//     production never reads it, so signalChange is a no-op until
//     changedCh has been lazily allocated by a waitForChange caller.
//     This keeps the rescan hot-path allocation-free.
type attachState struct {
	mu           sync.Mutex
	attempts     int
	pending      bool
	done         bool
	firstSuccess time.Time
	changedCh    chan struct{}
}

// signalChange notifies any goroutine blocked in waitForChange that a
// state field has been mutated. Caller must hold s.mu.
//
// No-op until a test has primed changedCh via waitForChange, so the
// production rescan loop pays nothing per transition.
func (s *attachState) signalChange() {
	if s.changedCh == nil {
		return
	}
	close(s.changedCh)
	s.changedCh = make(chan struct{})
}

// waitForChange returns a channel that is closed on the next state
// mutation. Caller MUST acquire s.mu, snapshot the channel via this
// method, release s.mu, then select on the channel — racing the lock
// and the channel observation is what guarantees no transition is
// missed.
//
// Lazily allocates changedCh on first call so production never pays
// the allocation cost.
func (s *attachState) waitForChange() <-chan struct{} {
	if s.changedCh == nil {
		s.changedCh = make(chan struct{})
	}
	return s.changedCh
}
