package mysql

import "sync/atomic"

// EncodedPayload holds the wire bytes replay encoded a recorded result set to,
// so each recorded answer is encoded once and not again on every serve. It
// holds the payload without the 4-byte header of its first packet: the
// header's length and sequence id come from the PacketBundle that serves it.
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
// payload: b, or the one another serve stored first. A nil b is not cached.
func (c *EncodedPayload) Store(b []byte) []byte {
	if b == nil {
		return nil
	}
	if c.p.CompareAndSwap(nil, &b) {
		return b
	}
	return *c.p.Load()
}

// WireCache returns the cache of r's encoded payload.
func (r *TextResultSet) WireCache() *EncodedPayload { return &r.wire }

// WireCache returns the cache of r's encoded payload.
func (r *BinaryProtocolResultSet) WireCache() *EncodedPayload { return &r.wire }
