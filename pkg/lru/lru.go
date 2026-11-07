// Package lru provides size-bounded caches that are safe for concurrent use and
// are built only on the standard library:
//
//   - Cache drops the least recently used entry when it is full.
//   - TwoQueue keeps the keys read more than once apart from the keys seen
//     once, so one pass over many new keys cannot flush the keys in repeated
//     use (the 2Q policy).
//   - Expirable is a Cache whose entries also expire a fixed time after they
//     were last added.
//
// The callback given to NewWithEvict or NewExpirable is told about every entry
// the cache drops: evicted for room, expired, removed or purged. Replacing the
// value of a key that is held does not drop it. The callback runs after the
// cache has released its lock, so it may call back into the cache.
package lru

import (
	"errors"
	"sync"
	"time"
)

// EvictCallback is told the key and value of an entry the cache dropped.
type EvictCallback[K comparable, V any] func(key K, value V)

// ErrSize is returned for a cache size that is zero or negative.
var ErrSize = errors.New("lru: size must be positive")

// entry is one held key. It sits on its store's recency ring and, in an
// Expirable, also on the ring that orders entries by when they expire.
type entry[K comparable, V any] struct {
	key        K
	value      V
	prev, next *entry[K, V] // recency ring
	expires    time.Time
	sooner     *entry[K, V] // expiry ring: the entry that expires before this one
	later      *entry[K, V] // expiry ring: the entry that expires after this one
}

// store is a map from key to entry plus a ring that orders the entries by
// use. It holds no lock and no bound: its owner does both.
type store[K comparable, V any] struct {
	items map[K]*entry[K, V]
	root  entry[K, V] // root.next is the most recently used entry, root.prev the least
}

func (s *store[K, V]) init() {
	s.items = make(map[K]*entry[K, V])
	s.root.prev, s.root.next = &s.root, &s.root
}

// push adds key, which the store must not hold, as the most recent entry.
func (s *store[K, V]) push(key K, value V) *entry[K, V] {
	e := &entry[K, V]{key: key, value: value}
	s.items[key] = e
	s.link(e)
	return e
}

func (s *store[K, V]) link(e *entry[K, V]) {
	e.prev, e.next = &s.root, s.root.next
	s.root.next.prev = e
	s.root.next = e
}

func (s *store[K, V]) unlink(e *entry[K, V]) {
	e.prev.next = e.next
	e.next.prev = e.prev
}

// touch makes e the most recent entry.
func (s *store[K, V]) touch(e *entry[K, V]) {
	s.unlink(e)
	s.link(e)
}

func (s *store[K, V]) remove(e *entry[K, V]) {
	delete(s.items, e.key)
	s.unlink(e)
}

// oldest returns the least recently used entry, or nil when the store is empty.
func (s *store[K, V]) oldest() *entry[K, V] {
	if s.root.prev == &s.root {
		return nil
	}
	return s.root.prev
}

// entries lists the entries from the least to the most recently used.
func (s *store[K, V]) entries() []*entry[K, V] {
	all := make([]*entry[K, V], 0, len(s.items))
	for e := s.root.prev; e != &s.root; e = e.prev {
		all = append(all, e)
	}
	return all
}

func (s *store[K, V]) keys() []K {
	keys := make([]K, 0, len(s.items))
	for e := s.root.prev; e != &s.root; e = e.prev {
		keys = append(keys, e.key)
	}
	return keys
}

// Cache is a least-recently-used cache that holds at most size entries.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	size    int
	s       store[K, V]
	onEvict EvictCallback[K, V]
}

// New returns a Cache that holds at most size entries.
func New[K comparable, V any](size int) (*Cache[K, V], error) {
	return NewWithEvict[K, V](size, nil)
}

// NewWithEvict returns a Cache that holds at most size entries and tells
// onEvict, when it is not nil, about each entry it drops.
func NewWithEvict[K comparable, V any](size int, onEvict EvictCallback[K, V]) (*Cache[K, V], error) {
	if size <= 0 {
		return nil, ErrSize
	}
	c := &Cache[K, V]{size: size, onEvict: onEvict}
	c.s.init()
	return c, nil
}

// update runs fn under the lock, then tells the callback about the entries fn
// dropped. The lock is released even when fn panics.
func (c *Cache[K, V]) update(fn func() []*entry[K, V]) {
	for _, e := range c.locked(fn) {
		c.onEvict(e.key, e.value)
	}
}

func (c *Cache[K, V]) locked(fn func() []*entry[K, V]) []*entry[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fn()
}

// drop removes e and, when there is a callback, adds it to gone.
func (c *Cache[K, V]) drop(e *entry[K, V], gone []*entry[K, V]) []*entry[K, V] {
	c.s.remove(e)
	if c.onEvict != nil {
		gone = append(gone, e)
	}
	return gone
}

// Add holds value under key as the most recent entry. It reports whether the
// least recently used entry was evicted to make room.
func (c *Cache[K, V]) Add(key K, value V) (evicted bool) {
	c.update(func() (gone []*entry[K, V]) {
		if e, ok := c.s.items[key]; ok {
			e.value = value
			c.s.touch(e)
			return nil
		}
		c.s.push(key, value)
		if len(c.s.items) > c.size {
			gone = c.drop(c.s.oldest(), gone)
			evicted = true
		}
		return gone
	})
	return evicted
}

// Get returns the value held under key and makes it the most recent entry.
func (c *Cache[K, V]) Get(key K) (value V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.s.items[key]
	if !ok {
		return value, false
	}
	c.s.touch(e)
	return e.value, true
}

// Peek returns the value held under key without changing its recency.
func (c *Cache[K, V]) Peek(key K) (value V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.s.items[key]
	if !ok {
		return value, false
	}
	return e.value, true
}

// Contains reports whether key is held, without changing its recency.
func (c *Cache[K, V]) Contains(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.s.items[key]
	return ok
}

// Remove drops key and reports whether it was held.
func (c *Cache[K, V]) Remove(key K) (ok bool) {
	c.update(func() (gone []*entry[K, V]) {
		var e *entry[K, V]
		if e, ok = c.s.items[key]; ok {
			gone = c.drop(e, gone)
		}
		return gone
	})
	return ok
}

// GetOldest returns the least recently used entry without changing its recency.
func (c *Cache[K, V]) GetOldest() (key K, value V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.s.oldest(); e != nil {
		return e.key, e.value, true
	}
	return key, value, false
}

// RemoveOldest drops the least recently used entry and returns it.
func (c *Cache[K, V]) RemoveOldest() (key K, value V, ok bool) {
	c.update(func() (gone []*entry[K, V]) {
		if e := c.s.oldest(); e != nil {
			key, value, ok = e.key, e.value, true
			gone = c.drop(e, gone)
		}
		return gone
	})
	return key, value, ok
}

// Keys returns the held keys from the least to the most recently used.
func (c *Cache[K, V]) Keys() []K {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.s.keys()
}

// Len returns the number of held entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.s.items)
}

// Purge drops every entry.
func (c *Cache[K, V]) Purge() {
	c.update(func() (gone []*entry[K, V]) {
		if c.onEvict != nil {
			gone = c.s.entries()
		}
		c.s.init()
		return gone
	})
}
