package models

import (
	"fmt"
	"sync"
	"time"
)

const (
	// tlsHandshakeEntryTTL bounds how long an unconsumed handshake entry is kept.
	tlsHandshakeEntryTTL = 30 * time.Second
	// tlsHandshakeMaxQueuePerKey bounds queue growth for a single key.
	tlsHandshakeMaxQueuePerKey = 128
)

// TLSHandshakeEntry holds the raw MySQL handshake packets captured by the
// relay path (plaintext phase before TLS) so the post-TLS auth consumer
// can merge them into a single combined config mock.
type TLSHandshakeEntry struct {
	ReqPackets   [][]byte  // e.g. [SSLRequest raw bytes]
	RespPackets  [][]byte  // e.g. [HandshakeV10 raw bytes]
	ReqTimestamp time.Time // timestamp from the start of the relay handshake
}

// TLSHandshakeStore is a keyed store of handshake entries. Each key
// identifies a unique connection (e.g. "conn:<srcPort>:<dstPort>" or
// a port-only fallback "port:<dstPort>"). The relay path pushes entries
// when it finishes TLSOnly handshake capture; the post-TLS path pops
// them to merge with auth exchange data.
type TLSHandshakeStore struct {
	mu   sync.Mutex
	cond *sync.Cond
	m    map[string][]timedTLSHandshakeEntry
}

type timedTLSHandshakeEntry struct {
	entry    TLSHandshakeEntry
	pushedAt time.Time
}

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{m: make(map[string][]timedTLSHandshakeEntry)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// HandshakeStoreKey builds a store key from a ConnKey (connection-level
// identifier) and a destination port fallback.
// When ConnKey is set, the key is connection-specific, eliminating FIFO
// ordering issues across concurrent connections to the same port.
func HandshakeStoreKey(connKey string, dstPort uint16) string {
	if connKey != "" {
		return "conn:" + connKey
	}
	return fmt.Sprintf("port:%d", dstPort)
}

// Push adds a handshake entry for the given key.
func (s *TLSHandshakeStore) Push(key string, entry TLSHandshakeEntry) {
	s.mu.Lock()
	s.pruneExpiredLocked(time.Now())
	q := s.m[key]
	if len(q) >= tlsHandshakeMaxQueuePerKey {
		q = q[1:]
	}
	s.m[key] = append(s.m[key], timedTLSHandshakeEntry{
		entry:    entry,
		pushedAt: time.Now(),
	})
	s.cond.Broadcast()
	s.mu.Unlock()
}

// PopWait pops the oldest handshake entry for the given key, waiting up
// to timeout for one to appear. Returns false if no entry arrived in time.
func (s *TLSHandshakeStore) PopWait(key string, timeout time.Duration) (TLSHandshakeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now())

	// Fast path: already available.
	if q := s.m[key]; len(q) > 0 {
		entry := q[0].entry
		s.m[key] = q[1:]
		if len(s.m[key]) == 0 {
			delete(s.m, key)
		}
		return entry, true
	}

	if timeout <= 0 {
		return TLSHandshakeEntry{}, false
	}

	deadline := time.Now().Add(timeout)
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		timedOut = true
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()

	for {
		s.pruneExpiredLocked(time.Now())
		if q := s.m[key]; len(q) > 0 {
			if q[0].pushedAt.After(deadline) {
				s.m[key] = q[1:]
				if len(s.m[key]) == 0 {
					delete(s.m, key)
				}
				if timedOut || time.Now().After(deadline) {
					return TLSHandshakeEntry{}, false
				}
				continue
			}
			entry := q[0].entry
			s.m[key] = q[1:]
			if len(s.m[key]) == 0 {
				delete(s.m, key)
			}
			return entry, true
		}
		if timedOut || time.Now().After(deadline) {
			return TLSHandshakeEntry{}, false
		}
		s.cond.Wait()
	}
}

func (s *TLSHandshakeStore) pruneExpiredLocked(now time.Time) {
	cutoff := now.Add(-tlsHandshakeEntryTTL)
	for key, q := range s.m {
		trim := 0
		for trim < len(q) && q[trim].pushedAt.Before(cutoff) {
			trim++
		}
		if trim > 0 {
			q = q[trim:]
		}
		if len(q) == 0 {
			delete(s.m, key)
			continue
		}
		s.m[key] = q
	}
}
