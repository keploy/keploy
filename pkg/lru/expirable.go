package lru

import (
	"sync"
	"time"
)

// Expirable is a least-recently-used cache whose entries also expire ttl after
// they were last added; reading an entry does not extend its time. A size of
// zero or less holds any number of entries, and a ttl of zero or less never
// expires them.
//
// Expirable starts no goroutine: entries whose time is up are dropped, and
// the callback told, at the next call into the cache. A cache that is no
// longer referenced is collected with its entries.
type Expirable[K comparable, V any] struct {
	mu      sync.Mutex
	size    int
	ttl     time.Duration
	s       store[K, V]
	byAge   entry[K, V] // expiry ring root: byAge.later expires first
	onEvict EvictCallback[K, V]
	now     func() time.Time
}

// NewExpirable returns an Expirable that holds at most size entries, each for
// at most ttl, and tells onEvict, when it is not nil, about each entry it drops.
func NewExpirable[K comparable, V any](size int, onEvict EvictCallback[K, V], ttl time.Duration) *Expirable[K, V] {
	c := &Expirable[K, V]{size: size, ttl: ttl, onEvict: onEvict, now: time.Now}
	c.reset()
	return c
}

func (c *Expirable[K, V]) reset() {
	c.s.init()
	c.byAge.sooner, c.byAge.later = &c.byAge, &c.byAge
}

// update takes the lock, drops the entries whose time is up and runs fn with
// the time it read and those entries, then releases the lock and tells the
// callback about every entry dropped. The lock is released even when fn
// panics.
func (c *Expirable[K, V]) update(fn func(now time.Time, gone []*entry[K, V]) []*entry[K, V]) {
	for _, e := range c.locked(fn) {
		c.onEvict(e.key, e.value)
	}
}

func (c *Expirable[K, V]) locked(fn func(now time.Time, gone []*entry[K, V]) []*entry[K, V]) []*entry[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Read the time under the lock: the expiry ring is in Add order, and the
	// sweep stops at the first live entry, so an Add that read its time before
	// waiting for the lock would put a sooner expiry behind a later one, and
	// that entry would be served past its ttl.
	now := c.now()
	var gone []*entry[K, V]
	for e := c.byAge.later; e != &c.byAge && c.ttl > 0 && now.After(e.expires); e = c.byAge.later {
		gone = c.drop(e, gone)
	}
	return fn(now, gone)
}

// drop removes e from both rings and, when there is a callback, adds it to gone.
func (c *Expirable[K, V]) drop(e *entry[K, V], gone []*entry[K, V]) []*entry[K, V] {
	c.s.remove(e)
	c.unage(e)
	if c.onEvict != nil {
		gone = append(gone, e)
	}
	return gone
}

// age puts e last on the expiry ring: with one ttl for every entry, the entry
// added last expires last.
func (c *Expirable[K, V]) age(e *entry[K, V]) {
	e.sooner, e.later = c.byAge.sooner, &c.byAge
	c.byAge.sooner.later = e
	c.byAge.sooner = e
}

func (c *Expirable[K, V]) unage(e *entry[K, V]) {
	e.sooner.later = e.later
	e.later.sooner = e.sooner
}

// Add holds value under key as the most recent entry and restarts its time.
// It reports whether the least recently used entry was evicted to make room.
func (c *Expirable[K, V]) Add(key K, value V) (evicted bool) {
	c.update(func(now time.Time, gone []*entry[K, V]) []*entry[K, V] {
		e, ok := c.s.items[key]
		if ok {
			e.value = value
			c.s.touch(e)
			c.unage(e)
		} else {
			e = c.s.push(key, value)
		}
		e.expires = now.Add(c.ttl)
		c.age(e)
		if c.size > 0 && len(c.s.items) > c.size {
			gone = c.drop(c.s.oldest(), gone)
			evicted = true
		}
		return gone
	})
	return evicted
}

// Get returns the value held under key and makes it the most recent entry.
func (c *Expirable[K, V]) Get(key K) (value V, ok bool) {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		var e *entry[K, V]
		if e, ok = c.s.items[key]; ok {
			c.s.touch(e)
			value = e.value
		}
		return gone
	})
	return value, ok
}

// Peek returns the value held under key without changing its recency.
func (c *Expirable[K, V]) Peek(key K) (value V, ok bool) {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		var e *entry[K, V]
		if e, ok = c.s.items[key]; ok {
			value = e.value
		}
		return gone
	})
	return value, ok
}

// Contains reports whether key is held, without changing its recency.
func (c *Expirable[K, V]) Contains(key K) bool {
	_, ok := c.Peek(key)
	return ok
}

// Remove drops key and reports whether it was held.
func (c *Expirable[K, V]) Remove(key K) (ok bool) {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		var e *entry[K, V]
		if e, ok = c.s.items[key]; ok {
			gone = c.drop(e, gone)
		}
		return gone
	})
	return ok
}

// Keys returns the held keys from the least to the most recently used.
func (c *Expirable[K, V]) Keys() (keys []K) {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		keys = c.s.keys()
		return gone
	})
	return keys
}

// Len returns the number of held entries.
func (c *Expirable[K, V]) Len() (n int) {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		n = len(c.s.items)
		return gone
	})
	return n
}

// Purge drops every entry.
func (c *Expirable[K, V]) Purge() {
	c.update(func(_ time.Time, gone []*entry[K, V]) []*entry[K, V] {
		if c.onEvict != nil {
			gone = append(gone, c.s.entries()...)
		}
		c.reset()
		return gone
	})
}
