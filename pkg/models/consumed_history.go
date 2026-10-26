package models

import "sync"

// ConsumedHistory is a test set's record of the mocks its tests consumed: the
// latest MockState of each, by name. keploy test hands it to the agent with
// every mock filter (MockFilterParams.TotalConsumedMocks), and it grows with
// every test of the set.
//
// It keeps its writes in the order they were made, so a client whose agent
// already holds an earlier state of it sends only the entries written since
// (ConsumedSync), and an earlier state can be rebuilt (Mark) without a copy
// of the whole record being taken at every send. That is why its entries are
// written only through Set and never removed: a write that went around Set
// would never reach an agent that holds the history.
//
// Map hands out the live map, for reading only, on the goroutine that writes
// the history.
type ConsumedHistory struct {
	mu  sync.Mutex
	m   map[string]MockState
	log []MockState
}

// NewConsumedHistory returns an empty history with room for n entries.
func NewConsumedHistory(n int) *ConsumedHistory {
	return &ConsumedHistory{m: make(map[string]MockState, n)}
}

// ConsumedHistoryOf returns a history holding the entries of m, written in an
// unspecified order. The caller's map is not kept.
func ConsumedHistoryOf(m map[string]MockState) *ConsumedHistory {
	h := NewConsumedHistory(len(m))
	for _, s := range m {
		h.Set(s)
	}
	return h
}

// Set records s as the latest state of the mock it names.
func (h *ConsumedHistory) Set(s MockState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[s.Name] = s
	h.log = append(h.log, s)
}

// Get returns the latest state of the mock named name.
func (h *ConsumedHistory) Get(name string) (MockState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.m[name]
	return s, ok
}

// Len is the number of mocks the history names.
func (h *ConsumedHistory) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m)
}

// Map is the history's live map, for reading. Nil for a nil history.
func (h *ConsumedHistory) Map() map[string]MockState {
	if h == nil {
		return nil
	}
	return h.m
}

// Clone returns a new history holding the same entries.
func (h *ConsumedHistory) Clone() *ConsumedHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := NewConsumedHistory(len(h.m))
	for _, s := range h.m {
		c.m[s.Name] = s
		c.log = append(c.log, s)
	}
	return c
}

// Pos is the number of writes made so far: the place a later Since starts
// from.
func (h *ConsumedHistory) Pos() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return uint64(len(h.log))
}

// Since returns the latest state of every mock written at or after pos, and
// the Pos it reaches. ok is false when pos is past the history's writes.
func (h *ConsumedHistory) Since(pos uint64) (_ map[string]MockState, end uint64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end = uint64(len(h.log))
	if pos > end {
		return nil, end, false
	}
	out := make(map[string]MockState, end-pos)
	for _, s := range h.log[pos:] {
		out[s.Name] = s
	}
	return out, end, true
}

// Mark returns the history as it is now, to be rebuilt later (State) on any
// goroutine whatever is written to the history meanwhile. It costs no copy.
func (h *ConsumedHistory) Mark() ConsumedMark {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.log)
	return ConsumedMark{log: h.log[:n:n]}
}

// ConsumedMark is a history as it was when it was marked (ConsumedHistory.Mark).
type ConsumedMark struct {
	log []MockState
}

// State returns the latest state of each mock as of the mark, in a new map.
func (k ConsumedMark) State() map[string]MockState {
	out := make(map[string]MockState, len(k.log))
	for _, s := range k.log {
		out[s.Name] = s
	}
	return out
}
