package lru

import "sync"

const (
	// twoQueueRecentShare is the share of the size the keys seen once keep
	// when the cache is full: past it, room is made by evicting them first.
	twoQueueRecentShare = 0.25
	// twoQueueGhostShare is how many keys evicted from the seen-once list are
	// remembered, as a share of the size. A remembered key that is added again
	// goes straight to the frequent list.
	twoQueueGhostShare = 0.5
)

// TwoQueue is a 2Q cache that holds at most size entries. A key enters on a
// seen-once list and moves to a frequent list when it is read or added again,
// so a scan over many keys that are each seen once evicts only other
// seen-once keys and leaves the frequent ones in place.
type TwoQueue[K comparable, V any] struct {
	mu        sync.Mutex
	size      int
	recentCap int
	ghostCap  int
	recent    store[K, V]        // keys seen once
	frequent  store[K, V]        // keys seen more than once
	ghost     store[K, struct{}] // keys recently evicted from recent; no values
}

// New2Q returns a TwoQueue that holds at most size entries.
func New2Q[K comparable, V any](size int) (*TwoQueue[K, V], error) {
	if size <= 0 {
		return nil, ErrSize
	}
	q := &TwoQueue[K, V]{
		size:      size,
		recentCap: int(float64(size) * twoQueueRecentShare),
		ghostCap:  int(float64(size) * twoQueueGhostShare),
	}
	q.Purge()
	return q, nil
}

// Get returns the value held under key. A key read for the first time since
// it was added moves to the frequent list.
func (q *TwoQueue[K, V]) Get(key K) (value V, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.frequent.items[key]; ok {
		q.frequent.touch(e)
		return e.value, true
	}
	if e, ok := q.recent.items[key]; ok {
		q.recent.remove(e)
		q.frequent.push(key, e.value)
		return e.value, true
	}
	return value, false
}

// Add holds value under key. A key that is held already, or was evicted from
// the seen-once list not long ago, goes to the frequent list; any other key
// goes to the seen-once list.
func (q *TwoQueue[K, V]) Add(key K, value V) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.frequent.items[key]; ok {
		e.value = value
		q.frequent.touch(e)
		return
	}
	if e, ok := q.recent.items[key]; ok {
		q.recent.remove(e)
		q.frequent.push(key, value)
		return
	}
	if g, ok := q.ghost.items[key]; ok {
		q.makeRoom(true)
		q.ghost.remove(g)
		q.frequent.push(key, value)
		return
	}
	q.makeRoom(false)
	q.recent.push(key, value)
}

// makeRoom evicts one entry when the cache is full. It takes the oldest
// seen-once entry while that list is past its share, or at its share when
// the key being added is a new one, and remembers its key; otherwise it takes
// the oldest frequent entry.
func (q *TwoQueue[K, V]) makeRoom(fromGhost bool) {
	recent := len(q.recent.items)
	if recent+len(q.frequent.items) < q.size {
		return
	}
	if recent > 0 && (recent > q.recentCap || (recent == q.recentCap && !fromGhost)) {
		e := q.recent.oldest()
		q.recent.remove(e)
		q.ghost.push(e.key, struct{}{})
		if len(q.ghost.items) > q.ghostCap {
			q.ghost.remove(q.ghost.oldest())
		}
		return
	}
	q.frequent.remove(q.frequent.oldest())
}

// Peek returns the value held under key without moving it.
func (q *TwoQueue[K, V]) Peek(key K) (value V, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.frequent.items[key]; ok {
		return e.value, true
	}
	if e, ok := q.recent.items[key]; ok {
		return e.value, true
	}
	return value, false
}

// Contains reports whether key is held, without moving it.
func (q *TwoQueue[K, V]) Contains(key K) bool {
	_, ok := q.Peek(key)
	return ok
}

// Len returns the number of held entries.
func (q *TwoQueue[K, V]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.recent.items) + len(q.frequent.items)
}

// Purge drops every entry and forgets the remembered keys.
func (q *TwoQueue[K, V]) Purge() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recent.init()
	q.frequent.init()
	q.ghost.init()
}
