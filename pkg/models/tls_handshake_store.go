package models

import (
	"sync"
	"time"
)

const (
	// tlsHandshakeEntryTTL bounds how long an unconsumed handshake entry is kept.
	tlsHandshakeEntryTTL = 30 * time.Second
	// tlsHandshakeMaxQueuePerPort bounds queue growth for a single destination port.
	tlsHandshakeMaxQueuePerPort = 128
)

// TLSHandshakeEntry holds the raw MySQL handshake packets captured by the
// relay path (plaintext phase before TLS) so the post-TLS auth consumer
// can merge them into a single combined config mock.
type TLSHandshakeEntry struct {
	ReqPackets   [][]byte  // e.g. [SSLRequest raw bytes]
	RespPackets  [][]byte  // e.g. [HandshakeV10 raw bytes]
	ReqTimestamp time.Time // timestamp from the start of the relay handshake
}

// TLSHandshakeStore is a FIFO queue of handshake entries per destination
// port. The relay path pushes entries when it finishes TLSOnly handshake
// capture; the post-TLS path pops them to merge with auth exchange data.
type TLSHandshakeStore struct {
	mu   sync.Mutex
	cond *sync.Cond
	m    map[uint16][]timedTLSHandshakeEntry
}

type timedTLSHandshakeEntry struct {
	entry    TLSHandshakeEntry
	pushedAt time.Time
}

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{m: make(map[uint16][]timedTLSHandshakeEntry)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Push adds a handshake entry to the FIFO queue for the given port.
func (s *TLSHandshakeStore) Push(port uint16, entry TLSHandshakeEntry) {
	s.mu.Lock()
	s.pruneExpiredLocked(time.Now())
	q := s.m[port]
	if len(q) >= tlsHandshakeMaxQueuePerPort {
		// Drop oldest to preserve FIFO behavior among retained entries.
		q = q[1:]
	}
	s.m[port] = append(s.m[port], timedTLSHandshakeEntry{
		entry:    entry,
		pushedAt: time.Now(),
	})
	s.cond.Broadcast()
	s.mu.Unlock()
}

// PopWait pops the oldest handshake entry for the given port, waiting up
// to timeout for one to appear. Returns false if no entry arrived in time.
func (s *TLSHandshakeStore) PopWait(port uint16, timeout time.Duration) (TLSHandshakeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now())

	// Fast path: already available.
	if q := s.m[port]; len(q) > 0 {
		entry := q[0].entry
		s.m[port] = q[1:]
		if len(s.m[port]) == 0 {
			delete(s.m, port)
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
		if q := s.m[port]; len(q) > 0 {
			// Keep timeout contract strict: only return entries that arrived before the deadline.
			if q[0].pushedAt.After(deadline) {
				return TLSHandshakeEntry{}, false
			}
			entry := q[0].entry
			s.m[port] = q[1:]
			if len(s.m[port]) == 0 {
				delete(s.m, port)
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
	for port, q := range s.m {
		trim := 0
		for trim < len(q) && q[trim].pushedAt.Before(cutoff) {
			trim++
		}
		if trim > 0 {
			q = q[trim:]
		}
		if len(q) == 0 {
			delete(s.m, port)
			continue
		}
		s.m[port] = q
	}
}
