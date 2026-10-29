package record

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// maxHeldMappingEntries bounds mappingWaits: how many mapping entries may wait
// for their mock at once. An entry costs one map slot, a slice element and a
// test name the mapping already holds, about 100 bytes, so the cap holds the
// waits under 10 MB. An entry past it is not held: it is settled at once, as an
// entry still waiting at the end of the mock stream is (counted as an
// incomplete mock set for its test), and the first one is said in a Warn that
// names the cap. The mocks a held entry waits for are the ones the agent has
// sent and this recorder has not saved yet, so a healthy recording stays far
// below it however loaded its host is; only a stream that names mocks it never
// sends can reach it. A revoked test's entries are not held (revoke), so the
// tests the agent revokes over a mock it never sent do not count toward it.
//
// A package-level var, not a const, only so a test can shrink it. Never
// reassigned in production code.
var maxHeldMappingEntries = 100000

// mockFate is what is known, at one moment, of the mock a mapping entry names.
type mockFate int

const (
	// fateUnknown: the mock loop has said nothing of this mock yet.
	fateUnknown mockFate = iota
	// fateMapped: the mock is saved and belongs to the test's mock set.
	fateMapped
	// fateExcluded: the mock is async egress or one a RecordHook skipped. It
	// is never part of a test's mock set, and its absence is no loss.
	fateExcluded
	// fateLost: the recorder failed to save the mock; its test is revoked.
	fateLost
)

// lossReason is why an entry settled with no mock (fateUnknown): what its
// "Failed to correlate" line says.
type lossReason int

const (
	// reasonEnded: the mock stream ended with no mock left for the entry.
	// Either the agent never sent it, or another entry naming the same
	// tempID took it (a tempID is mapped once, and a mock the consumer took
	// is not remembered, so the two cannot be told apart here).
	reasonEnded lossReason = iota
	// reasonTaken: the mock's outcome came while the entry was held, and
	// another entry naming the same tempID, held beside it, took the mock.
	reasonTaken
	// reasonUntrackedDrop: the recorder failed to save the mock while the
	// dropped-mock set was full (maxLiveDroppedMockIDs), so the test cannot be
	// revoked over it.
	reasonUntrackedDrop
	// reasonCapFull: maxHeldMappingEntries entries were already waiting, so
	// this one was not held.
	reasonCapFull
	// reasonRevoked: the entry's test is revoked (deleted at the recording's
	// end), so its mock is no longer waited for.
	reasonRevoked
	// reasonAbandoned: the recording's teardown gave the mock loop up (its
	// drain timed out) before the mock's outcome was told.
	reasonAbandoned
	// reasonConsumerGivenUp: the mock's outcome was told, but the recording's
	// teardown gave the mapping consumer up (its drain timed out) before it
	// applied the entry.
	reasonConsumerGivenUp
)

// heldEntry is one mapping entry waiting for its mock, or handed back.
type heldEntry struct {
	test   string
	tempID string
	// why it was handed back: what its loss is if it finds no mock.
	why lossReason
}

// mappingWaits holds the mapping entries whose mock the mock loop has not
// reported yet, and hands each back to the mapping consumer when it does.
//
// Mocks and mappings reach the recorder on two streams drained by two
// goroutines, and a mapping routinely arrives before the mocks it names are
// saved: the agent sends a test's mocks before its mapping, but saving a mock
// takes a disk write, and a loaded host falls behind. So an entry whose mock has
// no outcome yet is held here, never given up on a clock. The mock loop calls
// arrived once a mock's outcome (saved, dropped or skipped) is published, and
// end once its stream is over. An entry still held at end has a mock that never
// came: that, and only that, is counted as a test recorded with an incomplete
// mock set. A teardown that gave the mock loop up (it never ends), or the
// consumer (it never applies what was handed back), takes what is left
// instead (abandon).
//
// The consumer never blocks on a held entry: it goes on draining the mapping
// stream (the agent drops mappings it cannot hand over), and applies the held
// entries as they are handed back (ready, signalled on wake).
type mappingWaits struct {
	mu sync.Mutex
	// held maps a tempID to the tests whose mapping names it and waits for it
	// (one, but a second mapping naming the same tempID is kept, not lost).
	held  map[string][]string
	nHeld int
	// ready are the entries whose mock has an outcome now (or never will:
	// those end handed back), for the consumer to settle.
	ready []heldEntry
	// wake is signalled when ready gains an entry; capacity 1, so a signal is
	// never lost and the mock loop never blocks on it.
	wake chan struct{}
	// ended is set once the mock stream is over: no outcome comes any more.
	ended bool
	// revoked are the tests revoked so far: their entries are not held. One
	// name per revoked test, as the recording's own revoke set keeps.
	revoked map[string]struct{}
	// capSaid is set once the cap has turned an entry away.
	capSaid bool
	// limit is maxHeldMappingEntries, read once.
	limit int
	// changed, when set (a test, before the waits are used), is signalled
	// after every change of held or ready; capacity 1, never blocks.
	changed chan struct{}
}

func newMappingWaits() *mappingWaits {
	return &mappingWaits{
		held:    map[string][]string{},
		wake:    make(chan struct{}, 1),
		revoked: map[string]struct{}{},
		limit:   maxHeldMappingEntries,
	}
}

// claimed is what claim made of one entry.
type claimed struct {
	entry models.MockEntry
	fate  mockFate
	// held: the entry waits for its mock; entry and fate say nothing yet.
	held bool
	// why the entry was not held, for one that settled fateUnknown.
	why lossReason
	// capFirst: the first entry the cap turned away in this recording.
	capFirst bool
}

// claim settles the entry (test, tempID) now, or holds it until its mock's
// outcome arrives. settle is called under the waits' lock, so a mock whose
// outcome is published (before its arrived call) while the entry is being
// claimed is either seen by settle or finds the entry held: never neither.
func (w *mappingWaits) claim(test, tempID string, settle func() (models.MockEntry, mockFate)) claimed {
	if w == nil {
		entry, fate := settle()
		return claimed{entry: entry, fate: fate, why: reasonEnded}
	}
	w.mu.Lock()
	var c claimed
	c.entry, c.fate = settle()
	switch {
	case c.fate != fateUnknown:
	case w.ended:
		c.why = reasonEnded
	case w.isRevoked(test):
		c.why = reasonRevoked
	case w.nHeld >= w.limit:
		c.why = reasonCapFull
		c.capFirst = !w.capSaid
		w.capSaid = true
	default:
		w.held[tempID] = append(w.held[tempID], test)
		w.nHeld++
		c.held = true
	}
	w.mu.Unlock()
	if c.held {
		w.notify()
	}
	return c
}

func (w *mappingWaits) isRevoked(test string) bool {
	_, ok := w.revoked[test]
	return ok
}

// arrived tells the waits that tempID's mock has an outcome, published before
// this call. The entries held for it are handed back to the consumer.
func (w *mappingWaits) arrived(tempID string) {
	w.handBack(tempID, reasonTaken)
}

// droppedUntracked tells the waits that tempID's mock was dropped while the
// dropped-mock set was full: there is nothing for an entry to settle on, so
// the entries held for it are handed back as lost for that reason.
func (w *mappingWaits) droppedUntracked(tempID string) {
	w.handBack(tempID, reasonUntrackedDrop)
}

func (w *mappingWaits) handBack(tempID string, why lossReason) {
	if w == nil {
		return
	}
	w.mu.Lock()
	tests, ok := w.held[tempID]
	if ok {
		delete(w.held, tempID)
		w.nHeld -= len(tests)
		for _, tn := range tests {
			w.ready = append(w.ready, heldEntry{test: tn, tempID: tempID, why: why})
		}
	}
	w.mu.Unlock()
	if ok {
		w.signal()
	}
}

// revoke tells the waits these tests are revoked: they are deleted at the
// recording's end, so nothing waits for their mocks any more. Their held
// entries are handed back (they settle as reasonRevoked, unless their mock
// came meanwhile), and a later entry of theirs is not held. It walks the held
// entries once per call; revokes are rare (a capacity drop) and come a batch
// of names per call.
func (w *mappingWaits) revoke(tests []string) {
	if w == nil || len(tests) == 0 {
		return
	}
	w.mu.Lock()
	for _, tn := range tests {
		w.revoked[tn] = struct{}{}
	}
	moved := false
	for tempID, names := range w.held {
		kept := names[:0]
		for _, tn := range names {
			if w.isRevoked(tn) {
				w.ready = append(w.ready, heldEntry{test: tn, tempID: tempID, why: reasonRevoked})
				w.nHeld--
				moved = true
				continue
			}
			kept = append(kept, tn)
		}
		if len(kept) == 0 {
			delete(w.held, tempID)
		} else {
			w.held[tempID] = kept
		}
	}
	w.mu.Unlock()
	if moved {
		w.signal()
	}
}

// end tells the waits the mock stream is over. Every entry still held is
// handed back: its mock never came. Idempotent.
func (w *mappingWaits) end() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		return
	}
	w.ended = true
	moved := w.nHeld > 0
	for tempID, tests := range w.held {
		for _, tn := range tests {
			w.ready = append(w.ready, heldEntry{test: tn, tempID: tempID, why: reasonEnded})
		}
	}
	clear(w.held)
	w.nHeld = 0
	w.mu.Unlock()
	if moved {
		w.signal()
	}
}

// abandon is the teardown's, when its drain gave the mock loop or the mapping
// consumer up: neither may ever settle what is left, so it is taken here, under
// the lock, so the consumer cannot apply one of these entries as well.
//
//   - ready: the entries handed back that the consumer has not applied (a
//     consumer wedged in a write, while the mock loop ended). Each keeps the
//     why it was handed back with, but a revoked test's says reasonRevoked.
//   - held: the entries whose mock was never told (a mock loop wedged in a
//     save or a hook). Each says reasonAbandoned.
//
// A parked consumer is woken, finds nothing, and returns once its stream is
// closed. After a clean drain both are empty: the consumer returned only once
// nothing waited.
func (w *mappingWaits) abandon() (ready, held []heldEntry) {
	if w == nil {
		return nil, nil
	}
	w.mu.Lock()
	w.ended = true
	ready = w.ready
	w.ready = nil
	for i := range ready {
		if w.isRevoked(ready[i].test) {
			ready[i].why = reasonRevoked
		}
	}
	for tempID, tests := range w.held {
		for _, tn := range tests {
			held = append(held, heldEntry{test: tn, tempID: tempID, why: reasonAbandoned})
		}
	}
	clear(w.held)
	w.nHeld = 0
	w.mu.Unlock()
	w.signal()
	return ready, held
}

func (w *mappingWaits) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
	w.notify()
}

func (w *mappingWaits) notify() {
	if w.changed == nil {
		return
	}
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// takeReady returns the entries handed back since the last call.
func (w *mappingWaits) takeReady() []heldEntry {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	r := w.ready
	w.ready = nil
	w.mu.Unlock()
	if len(r) > 0 {
		w.notify()
	}
	return r
}

// settled reports that no entry waits and none is left to apply.
func (w *mappingWaits) settled() bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nHeld == 0 && len(w.ready) == 0
}

// heldCount is how many entries wait now.
func (w *mappingWaits) heldCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nHeld
}

// wakeCh is the channel the consumer selects on; nil (never ready) with no
// waits.
func (w *mappingWaits) wakeCh() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.wake
}
