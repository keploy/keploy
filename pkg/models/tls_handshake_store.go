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
	// last remembers, per DESTINATION-scoped key, the most recent entry ever pushed —
	// independently of the consumable queue in m. Only DESTINATION keys are
	// cached here (see HandshakeLastKey) because it is never TTL/size-pruned and
	// the only reader queries it by destination. Unlike m it is never
	// consumed by Pop and never TTL-pruned: it is the last-resort
	// fallback for a consumer whose OWN raw-leg entry was genuinely
	// lost (e.g. the plaintext MySQL greeting+SSLRequest capture event
	// dropped by a full ringbuf under load). A MySQL server's greeting
	// is reusable across connections for stitching purposes: the
	// capability flags, protocol version and auth plugin are per-server
	// (stable), and the only per-connection field — the auth-plugin-data
	// salt — is not verified anywhere on the record or replay path (the
	// replayer explicitly skips AuthResponse comparison because it is
	// salt-dependent; see mysql/replayer/match.go matchHanshakeResponse41).
	//
	// That reasoning is exactly why the key must name the SERVER. Those fields
	// are per-server, so an entry may only be reused for the same server. A
	// port-only key does not identify one: under a DaemonSet a single agent
	// serves every pod on the node, so app A talking to MySQL-A:3306 and app B
	// talking to MySQL-B:3306 share "port:3306", and B would stitch A's
	// capability flags, server version and auth plugin into its own config mock.
	// Keying by destination address confines reuse to one server, and
	// HandshakeLastKey returns "" for an address the capture layer merely
	// synthesized, which disables the cache rather than letting every
	// unresolved destination collapse onto one placeholder key.
	last map[string]TLSHandshakeEntry
}

type timedTLSHandshakeEntry struct {
	entry    TLSHandshakeEntry
	pushedAt time.Time
}

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{
		m:    make(map[string][]timedTLSHandshakeEntry),
		last: make(map[string]TLSHandshakeEntry),
	}
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

// HandshakeLastKey builds the key for the last-greeting cache.
//
// The cache lets a connection whose own raw leg was dropped borrow a greeting
// captured for the same destination. That is only sound between connections to
// the SAME server, so the key must name one. A destination address alone does
// not: in a long-lived proxyless/DaemonSet agent a single store is shared across
// every app on the node, and an unresolved destination is reported as a
// placeholder (see ConditionalDstCfg.AddrFabricated), so app A talking to
// MySQL-A and app B talking to MySQL-B both present as 127.0.0.1:3306. Keyed on
// that alone, B would stitch A's capability flags, server version and auth
// plugin into its own config mock.
//
// The key therefore combines the caller's app/session scope with the address.
// Scope is the same isolation PassThroughScope already applies to a shared
// recorder: the enterprise DaemonSet gate sets it per app/session, and the
// classic sidecar leaves it empty because a process serves one session and the
// address alone already isolates.
//
// Returns "" — meaning do not cache and do not read the cache — when there is no
// address to key on at all.
//
// Residual, deliberately not solved here: within ONE scope, two DIFFERENT
// servers whose addresses were both fabricated collapse to the same key (a
// multi-destination JVM whose fds are unresolvable). That is narrower than the
// cross-app case and needs real destination resolution, not a better key.
func HandshakeLastKey(scope string, dst *ConditionalDstCfg) string {
	if dst == nil || dst.Addr == "" {
		return ""
	}
	return "dst:" + scope + "|" + dst.Addr
}

// RememberLast records entry as the most recent greeting seen for a
// destination. A empty key is ignored, so callers may pass
// HandshakeLastKey's result unconditionally.
func (s *TLSHandshakeStore) RememberLast(key string, entry TLSHandshakeEntry) {
	if key == "" {
		return
	}
	s.mu.Lock()
	if s.last == nil {
		s.last = make(map[string]TLSHandshakeEntry)
	}
	s.last[key] = entry
	s.mu.Unlock()
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
	// The last-greeting cache is NOT populated from the queue key. A queue key
	// is a port or a connection, neither of which identifies a server, and this
	// cache is never TTL/size-pruned. Callers record into it explicitly via
	// RememberLast with a destination-scoped key.
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Last returns the most recent entry recorded for a destination key, without
// consuming anything. Unlike PopWait it survives consumption by other
// connections and the TTL prune, so it stays available as a stitching
// fallback when a connection's own raw-leg entry was lost (dropped
// capture event / >TTL delay). Callers must treat the result as
// belonging to ANOTHER connection to the same server: reuse the
// server-stable parts (greeting capabilities / plugin, client
// SSLRequest shape) but not per-connection metadata such as the
// request timestamp.
func (s *TLSHandshakeStore) Last(key string) (TLSHandshakeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.last[key]
	return entry, ok
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
				return TLSHandshakeEntry{}, false
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
