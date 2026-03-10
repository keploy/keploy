package models

import (
	"sync"
	"time"
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
	m    map[uint16][]TLSHandshakeEntry
}

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{m: make(map[uint16][]TLSHandshakeEntry)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Push adds a handshake entry to the FIFO queue for the given port.
func (s *TLSHandshakeStore) Push(port uint16, entry TLSHandshakeEntry) {
	s.mu.Lock()
	s.m[port] = append(s.m[port], entry)
	s.mu.Unlock()
	s.cond.Broadcast()
}

// PopWait pops the oldest handshake entry for the given port, waiting up
// to timeout for one to appear. Returns false if no entry arrived in time.
func (s *TLSHandshakeStore) PopWait(port uint16, timeout time.Duration) (TLSHandshakeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Fast path: already available.
	if q := s.m[port]; len(q) > 0 {
		entry := q[0]
		s.m[port] = q[1:]
		return entry, true
	}

	if timeout <= 0 {
		return TLSHandshakeEntry{}, false
	}

	// Wait with timeout using a timer that wakes the cond.
	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		s.cond.Broadcast()
		close(done)
	})
	defer timer.Stop()

	for {
		s.cond.Wait()
		if q := s.m[port]; len(q) > 0 {
			entry := q[0]
			s.m[port] = q[1:]
			return entry, true
		}
		// Check if timer fired.
		select {
		case <-done:
			return TLSHandshakeEntry{}, false
		default:
		}
	}
}
