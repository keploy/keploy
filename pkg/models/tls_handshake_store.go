package models

import (
	"fmt"
	"strings"
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
	// last remembers, per scoped key, the most recent entry ever pushed —
	// independently of the consumable queue in m. Unlike m it is never consumed
	// by Pop, and it is pruned on its OWN, much longer schedule
	// (lastGreetingTTL) and capped at maxLastGreetings, so a long-lived
	// DaemonSet agent cannot accumulate entries without bound. It is the
	// last-resort
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
	// That reasoning is exactly why an entry may only ever be reused for the
	// SAME server: those fields are per-server, and stitching one server's
	// capability flags, version and auth plugin into another's config mock is
	// silent corruption — strictly worse than the missing mock it would replace.
	//
	// Two kinds of key are cached here, and they earn that guarantee differently:
	//
	//   - DESTINATION keys (HandshakeLastKey) name the server directly, in the
	//     address. Note they do NOT drop a synthesized address: the key also
	//     carries the app/session scope, so one app's "127.0.0.1:3306"
	//     placeholder bucket is already distinct from another's, and the
	//     degraded proxyless destination is precisely the case this fallback
	//     exists to rescue.
	//
	//   - PORT keys (HandshakeLastPortKey) do NOT name a server, and exist only
	//     because the two legs of a proxyless TLS connection disagree about the
	//     address (the decrypted leg's is often unresolvable). They earn the
	//     guarantee at RUNTIME instead: RememberLastForPort tags each entry with
	//     the SERVER that produced it — a caller-supplied identity, NOT an
	//     address — and latches the key ambiguous the moment a second, different
	//     server appears under it, after which
	//     Last refuses to serve it. This is BEST-EFFORT, not a guarantee: a
	//     server that never writes the key — because its own raw leg was the one
	//     that went missing, which is precisely when a borrower needs the
	//     fallback — cannot trip the latch, so it can still read another
	//     server's greeting. The reused fields are per-server-stable and the
	//     replayer already serves one recorded greeting to every connection, so
	//     the trade is deliberate; it is not a proof of isolation.
	//
	// Residual, shared by both and deliberately accepted: within ONE scope, two
	// different servers whose addresses BOTH had to be synthesized are
	// indistinguishable to the capture layer, so they can collapse onto one
	// destination key. Resolving that needs real destination resolution, not a
	// better key.
	//
	// A second residual, for completeness: a cached entry also carries the
	// borrowed connection's SSLRequest, and on the seq==0 path the synthetic
	// HandshakeResponse41 built from it copies CLIENT-side fields (max packet
	// size, charset, filler) that the replayer compares exactly. The identity
	// above names the SERVER; nothing names the client. Within one app and
	// driver that is a no-op, but two different clients sharing one unscoped
	// store can produce a mock that fails to match at replay. Pre-existing —
	// the shared placeholder-address bucket already had this — but the identity
	// does not remove it, so do not read it as doing so.
	last map[string]lastGreeting
	// lastPruned is when the last-greeting cache was last swept, so the sweep is
	// not repeated on every write while the map is under budget.
	lastPruned time.Time
}

// lastGreeting is a cached greeting plus the identity of the server that
// produced it, so a key that does NOT name a server (a port-scoped key) can
// still refuse to serve one server's greeting to another.
type lastGreeting struct {
	entry TLSHandshakeEntry
	// serverID identifies the SERVER whose raw leg produced this entry, as
	// supplied by the caller. It is deliberately opaque to this package: the
	// MySQL recorder passes a fingerprint of the greeting's server-stable fields
	// rather than an address, because in Kubernetes the same server answers on a
	// different pod IP after any rollout. Empty for address-keyed entries, where
	// the key already names the server.
	serverID string
	// ambiguous latches once two DIFFERENT server identities have been recorded
	// under the same key. Once latched it is never unlatched by a write, and it
	// is exempt from the TTL sweep, because a key shown not to identify a single
	// server cannot become trustworthy again — serving across servers stitches
	// the wrong capability flags / auth plugin into a connection's config mock.
	// (Tombstones are still subject to their own eviction budget, so an agent
	// that accumulates more than maxLastGreetings of them will drop the oldest.)
	ambiguous bool
	seen      time.Time
}

// hasPayload reports whether this record can actually be served. A record with a
// serverID but no response packets is an IDENTITY record: the payload aged out or
// was evicted, but the key must keep remembering which server owned it so a later
// write from a DIFFERENT server still latches. Without that the identity dies
// with the payload and the next write recreates the key clean and serving.
func (g lastGreeting) hasPayload() bool { return len(g.entry.RespPackets) > 0 }

// isIdentityOnly reports whether this record is an identity placeholder.
func (g lastGreeting) isIdentityOnly() bool {
	return !g.ambiguous && !g.hasPayload() && g.serverID != ""
}

const (
	// lastPortKeyPrefix marks keys built by HandshakeLastPortKey, which must
	// only ever be written through RememberLastForPort.
	lastPortKeyPrefix = "dstport:"
	// lastGreetingTTL bounds how long a cached greeting stays usable. It is far
	// longer than tlsHandshakeEntryTTL because this cache exists to survive a
	// connection's own entry being consumed, but it is not unbounded: a server
	// restarted hours ago may advertise different capabilities.
	lastGreetingTTL = 30 * time.Minute
	// identityTTL is how long a key remembers WHICH server owned it after its
	// payload is gone. It must outlast any rollout drain by a wide margin --
	// surviving the payload is the whole point -- but not forever, or a key
	// latched by an in-place version bump stays dead for the agent's lifetime.
	// 4x the payload TTL: a key that has seen no traffic from ANY server for two
	// hours has no live producer left to protect.
	identityTTL = 4 * lastGreetingTTL
	// maxLastGreetings caps the cache. Cardinality is distinct (scope,
	// destination) pairs, and a long-lived DaemonSet agent accumulates one per
	// app x session x destination, so without a cap this map only ever grows.
	maxLastGreetings = 512
)

type timedTLSHandshakeEntry struct {
	entry    TLSHandshakeEntry
	pushedAt time.Time
}

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{
		m:    make(map[string][]timedTLSHandshakeEntry),
		last: make(map[string]lastGreeting),
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

// HandshakeLastPortKey builds the port-scoped key for the last-greeting cache.
//
// It exists because the two legs of a proxyless TLS connection do not agree on
// an address. The raw plaintext leg is captured with the connection's real
// destination, but the DECRYPTED leg arrives from an fd-less uprobe whose
// destination the capture layer could not resolve, so it carries a synthesized
// stand-in (ConditionalDstCfg.AddrFabricated). Keyed on the address alone the
// writer and the reader therefore never meet, and the last-greeting fallback —
// the whole point of which is to rescue a connection whose own raw leg was
// lost — can never fire on that path.
//
// The port IS agreed: it is the same value both legs key their consumable
// entries under (HandshakeStoreKey's "port:%d"), recovered on the decrypted leg
// from content matching. Combining it with the caller's app/session scope keeps
// the isolation that matters: scope is what stops a shared DaemonSet agent from
// serving app A's greeting to app B, which is the cross-app contamination a
// bare "port:3306" would allow.
//
// Residual, and the same one HandshakeLastKey already documents: within ONE
// scope, two different servers on the same port collapse to this key. That is
// narrower than the cross-app case, is already indistinguishable to the
// decrypted leg (it cannot resolve its own destination), and only ever applies
// after the connection's own greeting has gone missing.
//
// Returns "" — meaning do not cache and do not read the cache — when there is
// no port to key on.
func HandshakeLastPortKey(scope string, dstPort uint16) string {
	if dstPort == 0 {
		return ""
	}
	// An empty scope is permitted, matching HandshakeLastKey: a process that was
	// never told a scope is a classic sidecar serving ONE session, so there is no
	// second app to contaminate. Scope is the CROSS-APP guard and is set by the
	// multi-app DaemonSet path; cross-SERVER safety comes from the ambiguity
	// latch instead (see RememberLastForPort), which is what makes a key that
	// does not name a server usable at all.
	return fmt.Sprintf("%s%s|%d", lastPortKeyPrefix, scope, dstPort)
}

// RememberLast records entry as the most recent greeting seen for a
// destination. A empty key is ignored, so callers may pass
// HandshakeLastKey's result unconditionally.
func (s *TLSHandshakeStore) RememberLast(key string, entry TLSHandshakeEntry) {
	if key == "" {
		return
	}
	// A port key carries no server identity of its own, so it is safe only while
	// every write tags the destination it came from (RememberLastForPort). An
	// untagged write here would blank that tag and permanently disable the
	// ambiguity latch for that key, so refuse it rather than silently weaken it.
	if strings.HasPrefix(key, lastPortKeyPrefix) {
		return
	}
	s.rememberLast(key, "", entry)
}

// IsAmbiguous reports whether a key has been latched: two different servers were
// seen under it, so nothing recorded there may be reused. Callers use this as
// POSITIVE evidence that this scope+port serves more than one server, and should
// then decline any other identity-less fallback for the same connection rather
// than quietly reaching for one.
// Do NOT compose this with Last to decide whether a cached greeting is usable:
// a latch landing between the two calls makes Last report a plain miss, and the
// caller then falls through to an unguarded fallback right after the guard
// proved reuse unsafe. Use LastForPort, which answers both under one lock.
func (s *TLSHandshakeStore) IsAmbiguous(key string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[key].ambiguous
}

// RememberLastForPort records entry under a key that does NOT name a server
// (see HandshakeLastPortKey), tagging it with the caller's opaque identity for
// the SERVER that produced it. Callers must NOT pass an address: the MySQL
// recorder passes a fingerprint of the greeting's server-stable fields, because
// in Kubernetes one server answers on a new pod IP after every rollout and an
// address-tagged latch would fire on two connections to the same server. If a
// DIFFERENT identity is later recorded under the same key
// the entry is latched ambiguous and Last stops serving it — the key has been
// proven not to identify one server, and a cross-server greeting would corrupt
// the borrower's config mock rather than merely fail to fill it.
func (s *TLSHandshakeStore) RememberLastForPort(key string, serverID string, entry TLSHandshakeEntry) {
	if key == "" || serverID == "" {
		return
	}
	s.rememberLast(key, serverID, entry)
}

func (s *TLSHandshakeStore) rememberLast(key string, serverID string, entry TLSHandshakeEntry) {
	if key == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[string]lastGreeting)
	}
	cur, ok := s.last[key]
	if ok && cur.ambiguous {
		// Latched: never serve this key again. Refresh seen so the tombstone ages
		// with USE, not with time-since-latching. It is already exempt from the
		// TTL sweep, so this is about EVICTION ORDER: the tombstone budget evicts
		// the oldest, and a frozen timestamp would make a busy key's tombstone
		// the first one dropped — after which the next single-server write
		// recreates a clean, SERVING entry for a key we proved unsafe.
		cur.seen = now
		s.last[key] = cur
		return
	}
	if ok && serverID != "" && cur.serverID != "" && cur.serverID != serverID {
		s.last[key] = lastGreeting{ambiguous: true, seen: now}
		return
	}
	s.last[key] = lastGreeting{entry: entry, serverID: serverID, seen: now}
	// Prune AFTER inserting, so a write cannot leave the map over its budget;
	// pruning first leaves it one over every time. Live entries and tombstones
	// are budgeted separately, so the map holds about 3*maxLastGreetings — a
	// latching write returns before pruning, so the tombstone class can sit one
	// over its budget until the next write.
	s.pruneLastLocked(now)
}

// pruneLastLocked drops expired entries and, if still over the cap, the oldest
// ones.
//
// THREE classes have SEPARATE budgets, so the map's designed steady state is up
// to 3*maxLastGreetings (measured: 1536):
//
//   - live      — a servable greeting.
//   - identity  — remembers WHICH server owned a key after the payload expired
//     or was evicted, so the ambiguity latch stays armed. Never servable.
//   - tombstone — a latched key, permanently refusing.
//
// The budgets are separate because the classes must not crowd each other out.
// Tombstones evicting live entries would disable the fallback store-wide,
// recreating the capture loss this cache prevents; live entries evicting
// tombstones early would let a proven-unsafe key serve again; and either
// evicting identity records would disarm the latch, which is the guard itself.
func (s *TLSHandshakeStore) pruneLastLocked(now time.Time) {
	// Fast path. This runs under s.mu, which also serialises Push/PopWait for
	// every MySQL connection, and the sweeps below are full map scans. While the
	// map is comfortably under budget there is nothing for them to find, so only
	// pay for them once it is worth checking.
	// The threshold is the SMALLEST single class budget, deliberately, even
	// though the three classes below are budgeted separately and the map's
	// designed steady state is therefore up to 3*maxLastGreetings. Raising it to
	// the sum looks like an obvious win -- the scans are skipped more often --
	// but it lets ONE class run far past its own budget while the total stays
	// under the combined threshold, which is unbounded growth of that class.
	// Measured: with the sum as the threshold, 1024 address-keyed live entries
	// all survived against a 512 live budget. Under the smallest budget no class
	// can be over while the total is under, so the early return is always safe.
	if len(s.last) <= maxLastGreetings && now.Sub(s.lastPruned) < lastGreetingTTL {
		return
	}
	s.lastPruned = now
	for k, v := range s.last {
		// Tombstones are exempt: expiring one lets a key already proven not to
		// identify a single server start serving again. They are bounded by
		// their own budget in the cap step below instead.
		if v.ambiguous {
			continue
		}
		// Identity records are exempt for the same reason, one step earlier: they
		// ARE the memory that makes the latch fire. Expiring one returns the key
		// to "never seen", and the next write from a different server recreates
		// it clean and serving.
		if v.isIdentityOnly() {
			// Exempt from the PAYLOAD TTL, but not immortal: unbounded, a key
			// latched by an in-place server upgrade never serves again for the
			// agent's lifetime, because the only other escape is the identity
			// class's own eviction budget, which a low-cardinality node never
			// reaches.
			if now.Sub(v.seen) > identityTTL {
				delete(s.last, k)
			}
			continue
		}
		if now.Sub(v.seen) > lastGreetingTTL {
			// Demote rather than delete when the key knows which server owned
			// it. Deleting outright is what let server A's entry age out and
			// server B's next write recreate the key with no latch, serving B's
			// capability flags and auth plugin to a reader that expected A.
			//
			// KNOWN TRADE, deliberate: the identity now outlives the TTL, so an
			// IN-PLACE server upgrade latches this key permanently. greetingServerIdentity
			// fingerprints ServerVersion and CapabilityFlags, so a minor-version
			// bump on the same logical server reads as a second server. Before
			// this change the key recovered after lastGreetingTTL -- but only if
			// no old-version write landed inside that window, which a real
			// rolling upgrade usually violates, so main latched permanently too
			// in the common case. identityTTL now bounds it either way; the
			// remaining exposure is a >2h gap between the two versions
			// (scale-to-zero, then redeploy). The outcome is a MISSING mock
			// (the fallback goes dead for that scope+port) rather than a WRONG
			// one, which is the direction we want to fail in -- but it does mean
			// the capture shortfall can return for a destination after a rolling
			// upgrade, so it is a latency-to-recovery regression, not a no-op.
			// The TTL rationale above is about payload staleness and still holds
			// for the payload; it does not apply to the identity.
			if v.serverID != "" {
				s.last[k] = lastGreeting{serverID: v.serverID, seen: v.seen}
				continue
			}
			delete(s.last, k)
		}
	}
	// Tombstones and live entries are capped SEPARATELY. Sharing one budget makes
	// them compete: tombstones are refreshed on every suppressed write, so on a
	// key that keeps taking traffic they stay the NEWEST entries in the map while
	// genuinely useful live greetings age past them and get evicted first. The
	// cache then fills with keys that can only ever refuse, and the fallback goes
	// dead for destinations that were never ambiguous at all.
	//
	// Identity records get a third budget for the same reason: they must outlive
	// their payloads (that is their whole purpose) but must not be able to crowd
	// out live entries.
	evictOldest := func(class func(lastGreeting) bool, budget int, demote bool) {
		for {
			n := 0
			var oldestKey string
			var oldest time.Time
			for k, v := range s.last {
				if !class(v) {
					continue
				}
				n++
				if oldestKey == "" || v.seen.Before(oldest) {
					oldestKey, oldest = k, v.seen
				}
			}
			if n <= budget || oldestKey == "" {
				return
			}
			// Evicting a live entry must not forget which server owned the key:
			// 512 unrelated address keys could otherwise evict a port key's live
			// entry and disarm its latch without any clock involved.
			if v := s.last[oldestKey]; demote && v.serverID != "" {
				s.last[oldestKey] = lastGreeting{serverID: v.serverID, seen: v.seen}
				continue
			}
			delete(s.last, oldestKey)
		}
	}
	// NOT "!ambiguous && hasPayload()": that leaves an entry which is neither
	// ambiguous, nor carrying a payload, nor carrying an identity in NO class at
	// all, so no eviction pass ever counts or removes it and the map grows
	// without bound. Measured: 2048 payload-less entries survived a 512 budget,
	// with every write past the budget doing a full scan that removes nothing.
	// Defining live as "not a tombstone and not an identity record" keeps the
	// three classes exhaustive, which is what the fast path's argument requires.
	isLive := func(g lastGreeting) bool { return !g.ambiguous && !g.isIdentityOnly() }
	isTomb := func(g lastGreeting) bool { return g.ambiguous }
	evictOldest(isLive, maxLastGreetings, true)
	evictOldest(lastGreeting.isIdentityOnly, maxLastGreetings, false)
	evictOldest(isTomb, maxLastGreetings, false)
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
	// cache is pruned on its own, much longer schedule (lastGreetingTTL) and
	// capped. Callers record into it explicitly via RememberLast (destination
	// keys) or RememberLastForPort (port keys).
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Last returns the most recent entry recorded for a destination key, without
// consuming anything. Unlike PopWait it survives consumption by other
// connections and the consumable queue's TTL prune, so it stays available as a
// stitching fallback when a connection's own raw-leg entry was lost (dropped
// capture event / >TTL delay). It has its own, longer expiry (lastGreetingTTL)
// and refuses any key latched ambiguous by RememberLastForPort. Callers must treat the result as
// belonging to ANOTHER connection to the same server: reuse the
// server-stable parts (greeting capabilities / plugin, client
// SSLRequest shape) but not per-connection metadata such as the
// request timestamp.
func (s *TLSHandshakeStore) Last(key string) (TLSHandshakeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLocked(key, time.Now())
}

// lastLocked is the shared body of Last and LastForPort. Callers hold s.mu.
func (s *TLSHandshakeStore) lastLocked(key string, now time.Time) (TLSHandshakeEntry, bool) {
	v, ok := s.last[key]
	if !ok || v.ambiguous {
		return TLSHandshakeEntry{}, false
	}
	// An identity record remembers WHICH server owned this key after its payload
	// expired or was evicted. It exists to keep the ambiguity latch armed; it is
	// never servable.
	if !v.hasPayload() {
		return TLSHandshakeEntry{}, false
	}
	if now.Sub(v.seen) > lastGreetingTTL {
		return TLSHandshakeEntry{}, false
	}
	return v.entry, true
}

// LastForPort answers "is this key latched, and if not what does it hold?" in a
// SINGLE acquisition of s.mu.
//
// Callers must not compose IsAmbiguous with Last to get this. Between the two
// calls another connection's raw leg can latch the key; Last then reports a miss
// (it re-checks ambiguous), the caller reads that as "nothing cached", and falls
// through to the unguarded shared address bucket — which is the exact outcome
// the ambiguity check exists to prevent. That interleaving is a logic race, so
// -race cannot see it; it was reproduced on iteration 127 of 20000.
func (s *TLSHandshakeStore) LastForPort(key string) (entry TLSHandshakeEntry, ok bool, ambiguous bool) {
	if key == "" {
		return TLSHandshakeEntry{}, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last[key].ambiguous {
		return TLSHandshakeEntry{}, false, true
	}
	e, found := s.lastLocked(key, time.Now())
	return e, found, false
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
