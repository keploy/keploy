package mysql

import (
	"sync"
	"sync/atomic"
	"weak"
)

// EncodedPayload holds the wire bytes replay encoded a recorded result set to,
// so a recorded answer served again is copied, not encoded again. It holds the
// payload without the 4-byte header of its first packet: the header's length
// and sequence id come from the PacketBundle that serves it.
//
// Every payload cached in the process counts against one byte budget
// (WireCacheBudget, 64 MiB). A payload that would take the cache past it first
// evicts the payloads cached longest ago, oldest first, until it fits; a
// payload larger than the whole budget is not cached. An evicted answer is
// encoded again at its next serve, to the same bytes, and cached again.
// Eviction counts bytes; it never reads a clock.
//
// The budget never keeps a mock alive: it holds a weak pointer to each cache
// it counts, so a result set nothing else holds is collected with its payload,
// and its bytes leave the count when eviction reaches them. Until then they
// are counted though freed, so the bytes really held stay within the budget.
//
// The cache assumes the result set does not change once it has been served.
// Replay never changes a recorded answer; code that needs a different answer
// builds a new result set, which starts with an empty cache.
//
// It is never recorded: the field that holds it is unexported, so YAML, JSON,
// gob and BSON skip it. A copy of a mock shares the cache with the original,
// since both point at the same result set.
type EncodedPayload struct {
	p atomic.Pointer[[]byte]
}

// Load returns the cached payload, or nil when none is cached. The slice is
// shared by every serve of the answer: never write to it.
func (c *EncodedPayload) Load() []byte {
	if b := c.p.Load(); b != nil {
		return *b
	}
	return nil
}

// Store caches b unless a payload is cached already, and returns the cached
// payload: b, or the one another serve stored first. A nil b is not cached,
// nor is a b larger than the whole budget (Store still returns it).
func (c *EncodedPayload) Store(b []byte) []byte {
	if b == nil {
		return nil
	}
	if !c.p.CompareAndSwap(nil, &b) {
		if cur := c.p.Load(); cur != nil {
			return *cur
		}
		// Evicted since the swap failed: serve this encode uncached.
		return b
	}
	wireCache.admit(c, &b)
	return b
}

// WireCache returns the cache of r's encoded payload.
func (r *TextResultSet) WireCache() *EncodedPayload { return &r.wire }

// WireCache returns the cache of r's encoded payload.
func (r *BinaryProtocolResultSet) WireCache() *EncodedPayload { return &r.wire }

// WireCacheUsage reports the bytes the encode cache counts against its budget,
// and the budget. Payloads of collected result sets stay counted until
// eviction reaches them, so held is at least what the cache really keeps.
func WireCacheUsage() (held, budget int64) {
	wireCache.mu.Lock()
	defer wireCache.mu.Unlock()
	return wireCache.held, wireCache.budget
}

const (
	// WireCacheBudget is the most bytes the encode cache counts at once.
	WireCacheBudget = 64 << 20
	// WireEntryCost is what the budget charges a cached payload beyond the
	// capacity of its bytes: the slice header the cache points at, the weak
	// pointer and its runtime record, and the payload's slot in the queue.
	WireEntryCost = 128
)

// wireEntry is one cached payload as the budget counts it.
type wireEntry struct {
	cache weak.Pointer[EncodedPayload]
	cost  int64
}

// wireBudget is the byte budget every EncodedPayload of the process shares.
// The queue holds one entry per cached payload, oldest first from head. A
// cache has at most one entry: Store fills only an empty cache and admit then
// queues its entry, and evictTo empties a cache only as it takes that entry
// out, under the same lock, so a cache is filled again only after its entry
// has left the queue.
type wireBudget struct {
	mu     sync.Mutex
	budget int64
	held   int64
	queue  []wireEntry
	head   int
}

var wireCache = wireBudget{budget: WireCacheBudget}

// admit counts the payload *b, which c has just cached, against the budget,
// evicting the payloads cached longest ago until it fits. A payload the budget
// cannot hold at all is taken out of c again and evicts nothing.
func (w *wireBudget) admit(c *EncodedPayload, b *[]byte) {
	cost := int64(cap(*b)) + WireEntryCost
	w.mu.Lock()
	defer w.mu.Unlock()
	if cost > w.budget {
		c.p.CompareAndSwap(b, nil)
		return
	}
	w.evictTo(w.budget - cost)
	w.queue = append(w.queue, wireEntry{cache: weak.Make(c), cost: cost})
	w.held += cost
}

// evictTo evicts the oldest payloads until at most limit bytes are counted.
// The caller holds w.mu.
func (w *wireBudget) evictTo(limit int64) {
	for w.held > limit && w.head < len(w.queue) {
		e := w.queue[w.head]
		w.queue[w.head] = wireEntry{}
		w.head++
		w.held -= e.cost
		if c := e.cache.Value(); c != nil {
			c.p.Store(nil)
		}
	}
	switch {
	case w.head == len(w.queue):
		w.queue, w.head = w.queue[:0], 0
	case w.head > len(w.queue)/2:
		n := copy(w.queue, w.queue[w.head:])
		clear(w.queue[n:])
		w.queue, w.head = w.queue[:n], 0
	}
}
