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
// incomplete mock set for its test). The mocks a held entry waits for are the
// ones the agent has sent and this recorder has not saved yet, so a healthy
// recording stays far below it however loaded its host is; only a stream that
// names mocks it never sends can reach it.
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

// heldEntry is one mapping entry waiting for its mock.
type heldEntry struct {
	test   string
	tempID string
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
// mock set.
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
	// limit is maxHeldMappingEntries, read once.
	limit int
}

func newMappingWaits() *mappingWaits {
	return &mappingWaits{
		held:  map[string][]string{},
		wake:  make(chan struct{}, 1),
		limit: maxHeldMappingEntries,
	}
}

// claim settles the entry (test, tempID) now, or holds it until its mock's
// outcome arrives. settle is called under the waits' lock, so a mock whose
// outcome is published (before its arrived call) while the entry is being
// claimed is either seen by settle or finds the entry held: never neither.
// held reports the entry was held; otherwise fate says what it settled as
// (fateUnknown: it never will, it is lost to the test's mock set).
func (w *mappingWaits) claim(test, tempID string, settle func() (models.MockEntry, mockFate)) (entry models.MockEntry, fate mockFate, held bool) {
	if w == nil {
		entry, fate = settle()
		return entry, fate, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	entry, fate = settle()
	if fate != fateUnknown || w.ended || w.nHeld >= w.limit {
		return entry, fate, false
	}
	w.held[tempID] = append(w.held[tempID], test)
	w.nHeld++
	return entry, fate, true
}

// arrived tells the waits that tempID's mock has an outcome, published before
// this call. The entries held for it are handed back to the consumer.
func (w *mappingWaits) arrived(tempID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	tests, ok := w.held[tempID]
	if ok {
		delete(w.held, tempID)
		w.nHeld -= len(tests)
		for _, tn := range tests {
			w.ready = append(w.ready, heldEntry{test: tn, tempID: tempID})
		}
	}
	w.mu.Unlock()
	if ok {
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
			w.ready = append(w.ready, heldEntry{test: tn, tempID: tempID})
		}
	}
	clear(w.held)
	w.nHeld = 0
	w.mu.Unlock()
	if moved {
		w.signal()
	}
}

func (w *mappingWaits) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// takeReady returns the entries handed back since the last call.
func (w *mappingWaits) takeReady() []heldEntry {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.ready
	w.ready = nil
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
