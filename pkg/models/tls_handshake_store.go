package models

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	// tlsHandshakeEntryTTL bounds how long an unconsumed handshake entry is kept.
	tlsHandshakeEntryTTL = 30 * time.Second
	// tlsHandshakeMaxQueuePerKey bounds queue growth for a single key. An entry
	// waits here until its own decrypted stream takes it, and a raw leg whose
	// decrypted stream never comes (a TLS library the uprobes cannot see, an
	// aborted handshake) leaves its entry for the whole tlsHandshakeEntryTTL.
	// At R new connections per second to one port, a full queue keeps an entry
	// for about max/R seconds, so the bound must leave a late decrypted stream
	// time to arrive: 512 is ~17s at 30 connections/s, at ~200 bytes an entry.
	tlsHandshakeMaxQueuePerKey = 512
)

// TLSHandshakeEntry holds the raw MySQL handshake packets captured by the
// relay path (plaintext phase before TLS) so the post-TLS auth consumer
// can merge them into a single combined config mock.
type TLSHandshakeEntry struct {
	ReqPackets   [][]byte  // e.g. [SSLRequest raw bytes]
	RespPackets  [][]byte  // e.g. [HandshakeV10 raw bytes]
	ReqTimestamp time.Time // timestamp from the start of the relay handshake
}

// TLSHandshakeStore is a keyed store of handshake entries. The raw (pre-TLS)
// leg of a connection pushes its greeting and SSLRequest under the
// destination's port key ("port:<dstPort>"), tagged with the connection that
// produced it (HandshakeOwner); the decrypted (post-TLS) leg of the same
// connection pops it by that owner to merge with its auth exchange.
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
	// Two kinds of key are cached here, and they earn that guarantee differently
	// (a third, the server key, is kept apart in servers):
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
	//   - SERVER keys (HandshakeServerKey), kept in servers, also name the
	//     server in the address, but hold ONLY its bare greeting: no SSLRequest,
	//     no timing, nothing any one client contributed. That is what lets them
	//     drop the app/session scope for a routable address and be shared by
	//     every app and every recording session that reaches the server, and it
	//     is enforced: RememberLast and RememberLastForPort refuse them, and
	//     RememberServerGreeting stores only greeting bytes. A namespace-local
	//     address keeps a namespace qualifier (see AddrIsNetnsLocal).
	//
	// Residual, shared by the first two and deliberately accepted: within ONE scope, two
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

	// servers remembers, per server key (HandshakeServerKey), that server's bare
	// greeting. It has its own lock: every recorded MySQL handshake refreshes
	// it, and none of them should contend with the queue's Push/PopWait (mu),
	// on which post-TLS streams wait. Refreshes of a greeting already known are
	// mostly skipped under the read lock (RememberServerGreeting).
	//
	// Staleness. A remembered greeting goes stale only when the server behind
	// the address changes: a pod IP or Service reused by another MySQL, or an
	// in-place upgrade. Either ends every connection to the old server, so the
	// first connection to the new one made while anything records is FRESH,
	// and its raw leg replaces the memo at once (a live greeting always
	// replaces). What remains is a connection made while nothing recorded, to a
	// server replaced within lastGreetingTTL of the memo's last refresh. A
	// shorter lifetime for fetched entries would narrow that window at the
	// price of one dial per lifetime for exactly the apps the memo serves
	// (pools that open no fresh connections), each an aborted handshake counted
	// against the dialling host (the node, for a DaemonSet agent), which only
	// a successful connection from that same host resets. So fetched and live
	// entries share lastGreetingTTL.
	srvMu   sync.RWMutex
	servers map[string]serverGreeting
}

// serverGreeting is one server's remembered greeting.
type serverGreeting struct {
	greeting []byte
	// identity fingerprints the greeting's server-stable fields (the caller's
	// choice; the MySQL recorder uses protocol and server version, capability
	// flags, charset and auth plugin). Empty for a fetched greeting.
	identity string
	// live: a raw leg captured it on a live connection. A fetched one only
	// fills a key that holds nothing servable.
	live bool
	seen time.Time
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
	owner    HandshakeOwner
	pushedAt time.Time
}

const (
	// maxServerGreetings caps the per-server greeting memo. Its cardinality is
	// distinct servers (plus, for namespace-local addresses, namespaces), far
	// below the per-app x session x destination cache above.
	maxServerGreetings = 512
	// serverGreetingRefresh is how stale a live greeting may get before a raw
	// leg carrying the same server identity refreshes it. Refreshing on every
	// handshake would take the memo's write lock on every recorded connection;
	// refreshing this often keeps a busy server's greeting well inside
	// lastGreetingTTL.
	serverGreetingRefresh = lastGreetingTTL / 6
)

// NewTLSHandshakeStore creates a new store.
func NewTLSHandshakeStore() *TLSHandshakeStore {
	s := &TLSHandshakeStore{
		m:       make(map[string][]timedTLSHandshakeEntry),
		last:    make(map[string]lastGreeting),
		servers: make(map[string]serverGreeting),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// HandshakeStoreKey builds a queue key. With an empty connKey it is the
// destination port's key ("port:<dstPort>"), under which a raw leg pushes its
// entry tagged with its HandshakeOwner (PushFor) and a decrypted leg pops by
// its own (PopWaitFor). A non-empty connKey gives a key of its own, which
// nothing in the recorder pushes to any more: pairing by connection happens
// through the owner on the port key, so one capture is never queued twice.
func HandshakeStoreKey(connKey string, dstPort uint16) string {
	if connKey != "" {
		return "conn:" + connKey
	}
	return fmt.Sprintf("port:%d", dstPort)
}

// HandshakeOwner identifies the connection a queued handshake entry was
// captured from, as far as the capture layer can tell, so each decrypted
// stream is stitched with its OWN connection's greeting.
//
// The two legs of one TLS connection reach the recorder separately, and a
// port's queue is shared by every connection to that port on the agent. By
// arrival order alone, a stream takes whichever entry is oldest: under
// concurrency that is often another connection's, whose salt, SSLRequest and
// timestamp then land in this connection's config mock. Its connection may be
// in another app, talking to another server.
type HandshakeOwner struct {
	// Conn names the connection's socket (OutgoingOptions.ConnKey). When both
	// the entry and the consumer have one, they pair only if equal.
	Conn string
	// Proc names the process that made the connection
	// (OutgoingOptions.ConnProc). It decides when either side lacks Conn: a
	// stream then takes only an entry from its own process.
	Proc string
}

// HandshakeOwnerOf is the owner identity a connection's options carry.
func HandshakeOwnerOf(opts OutgoingOptions) HandshakeOwner {
	return HandshakeOwner{Conn: opts.ConnKey, Proc: opts.ConnProc}
}

// Is reports whether e names the same connection as o: both know it, and agree.
func (o HandshakeOwner) Is(e HandshakeOwner) bool {
	return o.Conn != "" && o.Conn == e.Conn
}

// mayTake reports whether a consumer o may take an entry owned by e when it
// has not found its own: never another connection's (both know their
// connection, and they differ), never another process's (both know theirs, and
// they differ), and otherwise yes, as the best the capture layer can tell.
func (o HandshakeOwner) mayTake(e HandshakeOwner) bool {
	if o.Conn != "" && e.Conn != "" {
		return o.Conn == e.Conn
	}
	if o.Proc != "" && e.Proc != "" {
		return o.Proc == e.Proc
	}
	return true
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

// serverKeyPrefix marks keys built by HandshakeServerKey, which may only ever
// hold a bare server greeting (see RememberServerGreeting).
const serverKeyPrefix = "srv:"

// AddrIsNetnsLocal reports whether host (an IP literal or a name, without a
// port) can name a DIFFERENT server in every network namespace, so the same
// spelling from two pods is not evidence of one server:
//
//   - loopback (127.0.0.0/8, ::1) and the unspecified address, which connects
//     to the local host: every netns has its own;
//   - link-local unicast and multicast (169.254.0.0/16, fe80::/10), and any
//     address carrying an IPv6 zone: scoped to one link of one netns;
//   - a hostname: resolution is per pod (search domains, /etc/hosts), so
//     "mysql" in two namespaces is two services.
//
// Every other IP address, private ranges included, is taken to name the same
// destination from every namespace a single agent can see: a pod IP one pod, a
// ClusterIP one Service (whichever of its backends answers), an external
// address one host. The exception it cannot see is a network private to a
// namespace that reuses a range used elsewhere, such as a docker bridge inside
// a Docker-in-Docker pod, or overlapping secondary networks: connections there
// are treated as reaching the same server as any other with that address.
//
// An empty host counts as local: it names nothing portable.
func AddrIsNetnsLocal(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return true
	}
	return ipIsNetnsLocal(ip)
}

// ipIsNetnsLocal is AddrIsNetnsLocal for a parsed IP.
func ipIsNetnsLocal(ip netip.Addr) bool {
	if ip.Zone() != "" {
		return true
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast()
}

// HandshakeServerKey names the SERVER behind a connection's destination, for
// state that belongs to the server rather than to one connection, app or
// recording session. Today that is a bare greeting: protocol version, server
// version, capability flags and auth plugin, all properties of the server, with
// the per-connection salt never verified on the record or replay path.
//
// It differs from HandshakeLastKey on purpose. That key also carries the
// app/session scope, because what it caches includes the CLIENT's SSLRequest and
// timing, which belong to one app. A server key carries neither, so for a
// routable address it carries no scope at all: the enterprise scope is
// "<ns>/<deployment>/<test-set>", and keying server state on it re-learned the
// same server's greeting in every recording session. For a Service's ClusterIP
// the greeting is that of whichever backend answered last; a live greeting from
// any of them replaces it.
//
// A namespace-local address (AddrIsNetnsLocal) names a different server in every
// network namespace, so it is qualified by netNS, the connection's namespace
// (OutgoingOptions.NetNS). Without one it gets no key: no app/session scope
// names a namespace (every replica of a deployment shares one), so sharing by
// scope would hand one pod's loopback server's greeting to its sibling pod.
//
// Returns "", meaning do not remember, do not look up and do not share a fetch,
// when dst names no server this way: no address, a fabricated stand-in
// (ConditionalDstCfg.AddrFabricated), an address that does not parse, or a
// namespace-local address with no namespace. A port alone is never a key.
func HandshakeServerKey(netNS string, dst *ConditionalDstCfg) string {
	if dst == nil || dst.Addr == "" || dst.AddrFabricated {
		return ""
	}
	host, port, err := net.SplitHostPort(dst.Addr)
	if err != nil || host == "" || port == "" {
		return ""
	}
	ip, perr := netip.ParseAddr(host)
	if perr == nil && !ipIsNetnsLocal(ip) {
		if ip.Is4() && ip.String() == host {
			return serverKeyPrefix + dst.Addr // already canonical: "a.b.c.d:port"
		}
		return serverKeyPrefix + net.JoinHostPort(ip.Unmap().String(), port)
	}
	if netNS == "" {
		return ""
	}
	if perr == nil {
		host = ip.Unmap().String() // keeps any zone
	}
	return serverKeyPrefix + "netns=" + netNS + "|" + net.JoinHostPort(host, port)
}

// RememberServerGreeting records greeting, a raw server greeting packet the
// caller CAPTURED on a live connection, under a server key (HandshakeServerKey).
// identity fingerprints the greeting's server-stable fields.
//
// A live greeting is the server as it is now, so it replaces whatever the key
// held, a fetched greeting included. When the key already holds a live greeting
// of the same identity it is only refreshed, and at most every
// serverGreetingRefresh: the salt and connection id change on every connection,
// nothing a borrower uses does. Callers must pass only a greeting that decodes;
// this package cannot decode one. Keys that are not server keys are ignored, so
// no client data can be written where every app reads.
func (s *TLSHandshakeStore) RememberServerGreeting(key string, greeting []byte, identity string) {
	if !strings.HasPrefix(key, serverKeyPrefix) || len(greeting) == 0 {
		return
	}
	now := time.Now()
	s.srvMu.RLock()
	cur, ok := s.servers[key]
	s.srvMu.RUnlock()
	if ok && cur.live && identity != "" && cur.identity == identity && now.Sub(cur.seen) < serverGreetingRefresh {
		return
	}
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	s.putServerGreetingLocked(key, serverGreeting{
		greeting: append([]byte(nil), greeting...), // often a slice of a read buffer
		identity: identity,
		live:     true,
		seen:     now,
	}, now)
}

// RememberServerGreetingIfAbsent records a greeting the caller FETCHED from the
// server, under a server key, only when the key holds no servable greeting, and
// reports whether it wrote. The check and the write are one acquisition of the
// lock: a live greeting a raw leg records while the fetch is in flight is newer
// evidence and must not be overwritten by the fetch.
func (s *TLSHandshakeStore) RememberServerGreetingIfAbsent(key string, greeting []byte) bool {
	if !strings.HasPrefix(key, serverKeyPrefix) || len(greeting) == 0 {
		return false
	}
	now := time.Now()
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if cur, ok := s.servers[key]; ok && now.Sub(cur.seen) <= lastGreetingTTL {
		return false
	}
	s.putServerGreetingLocked(key, serverGreeting{greeting: append([]byte(nil), greeting...), seen: now}, now)
	return true
}

// ServerGreeting returns the greeting remembered under a server key, if one is
// still within lastGreetingTTL. The bytes are the store's own: callers must not
// modify them.
func (s *TLSHandshakeStore) ServerGreeting(key string) ([]byte, bool) {
	if !strings.HasPrefix(key, serverKeyPrefix) {
		return nil, false
	}
	s.srvMu.RLock()
	defer s.srvMu.RUnlock()
	cur, ok := s.servers[key]
	if !ok || time.Since(cur.seen) > lastGreetingTTL {
		return nil, false
	}
	return cur.greeting, true
}

// putServerGreetingLocked stores g under key and keeps the memo within
// maxServerGreetings: expired entries go first, then the least recently seen.
// Callers hold srvMu for writing.
func (s *TLSHandshakeStore) putServerGreetingLocked(key string, g serverGreeting, now time.Time) {
	if s.servers == nil {
		s.servers = make(map[string]serverGreeting)
	}
	s.servers[key] = g
	if len(s.servers) <= maxServerGreetings {
		return
	}
	for k, v := range s.servers {
		if now.Sub(v.seen) > lastGreetingTTL {
			delete(s.servers, k)
		}
	}
	for len(s.servers) > maxServerGreetings {
		oldestKey, oldest := "", now
		for k, v := range s.servers {
			if oldestKey == "" || v.seen.Before(oldest) {
				oldestKey, oldest = k, v.seen
			}
		}
		delete(s.servers, oldestKey)
	}
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
	// A server key is read by every app and session that reaches the server,
	// so it may hold only the server's own greeting, never an entry carrying
	// one client's SSLRequest and timing. RememberServerGreeting writes it.
	if strings.HasPrefix(key, serverKeyPrefix) {
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
	if key == "" || serverID == "" || strings.HasPrefix(key, serverKeyPrefix) {
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
	s.rememberLastLocked(key, serverID, entry, now)
}

// rememberLastLocked is the body of rememberLast. Callers hold s.mu.
func (s *TLSHandshakeStore) rememberLastLocked(key string, serverID string, entry TLSHandshakeEntry, now time.Time) {
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

// Push adds a handshake entry for the given key, with no owner: any consumer
// may take it.
func (s *TLSHandshakeStore) Push(key string, entry TLSHandshakeEntry) {
	s.PushFor(key, HandshakeOwner{}, entry)
}

// PushFor adds a handshake entry for the given key, captured from the
// connection owner names. Each capture is pushed ONCE: pairing by connection
// goes through the owner (PopWaitFor), so no copy of it can be left behind for
// another stream to take as its own.
func (s *TLSHandshakeStore) PushFor(key string, owner HandshakeOwner, entry TLSHandshakeEntry) {
	s.mu.Lock()
	s.pruneExpiredLocked(time.Now())
	q := s.m[key]
	if len(q) >= tlsHandshakeMaxQueuePerKey {
		// Make room. This MUST append to the trimmed q, not to s.m[key]:
		// appending to the untrimmed slice discarded the trim entirely and the
		// cap never applied, so a key with a producer and no matching consumer
		// grew without bound.
		q = evictForRoom(q)
	}
	s.m[key] = append(q, timedTLSHandshakeEntry{
		entry:    entry,
		owner:    owner,
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

// evictForRoom removes one entry from a full queue: the oldest entry of the
// process (HandshakeOwner.Proc) that holds the most. A process whose entries
// are never taken (its decrypted streams are not captured) is the one that
// fills a queue, and evicting plain oldest-first would push every other app's
// live entries out ahead of its own. Entries with no known process count as
// one process, so without identities this is oldest-first, as it always was.
func evictForRoom(q []timedTLSHandshakeEntry) []timedTLSHandshakeEntry {
	counts := make(map[string]int)
	for _, e := range q {
		counts[e.owner.Proc]++
	}
	top, most := "", -1
	for p, n := range counts {
		if n > most || (n == most && p < top) {
			top, most = p, n
		}
	}
	for i := range q {
		if q[i].owner.Proc == top {
			return append(append(make([]timedTLSHandshakeEntry, 0, len(q)), q[:i]...), q[i+1:]...)
		}
	}
	return q[1:]
}

// PopWait pops the oldest handshake entry for the given key, waiting up
// to timeout for one to appear. Returns false if no entry arrived in time.
func (s *TLSHandshakeStore) PopWait(key string, timeout time.Duration) (TLSHandshakeEntry, bool) {
	e, _, ok := s.PopWaitFor(key, HandshakeOwner{}, false, timeout)
	return e, ok
}

// PopWaitFor pops a handshake entry for the consumer owner, waiting up to
// timeout for one to appear, and returns the entry with the owner it was
// pushed with.
//
// The consumer's OWN entry (HandshakeOwner.Is) is taken first, wherever it sits
// in the queue. Otherwise, unless ownOnly, the oldest entry the consumer may
// take (never another known connection's, never another known process's; see
// HandshakeOwner). A consumer with no owner at all may take any entry, which is
// arrival order on the key, as Pop always was.
func (s *TLSHandshakeStore) PopWaitFor(key string, owner HandshakeOwner, ownOnly bool, timeout time.Duration) (TLSHandshakeEntry, HandshakeOwner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now())

	// Fast path: already available.
	if e, o, ok := s.takeLocked(key, owner, ownOnly, time.Time{}); ok {
		return e, o, true
	}

	if timeout <= 0 {
		return TLSHandshakeEntry{}, HandshakeOwner{}, false
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
		if e, o, ok := s.takeLocked(key, owner, ownOnly, deadline); ok {
			return e, o, true
		}
		if timedOut || time.Now().After(deadline) {
			return TLSHandshakeEntry{}, HandshakeOwner{}, false
		}
		s.cond.Wait()
	}
}

// takeLocked removes and returns the entry PopWaitFor would take from key's
// queue, if any. An entry pushed after a non-zero notAfter is not taken: it
// arrived after the waiter's deadline. Callers hold s.mu.
func (s *TLSHandshakeStore) takeLocked(key string, owner HandshakeOwner, ownOnly bool, notAfter time.Time) (TLSHandshakeEntry, HandshakeOwner, bool) {
	q := s.m[key]
	pick := -1
	for i := range q {
		if owner.Is(q[i].owner) {
			pick = i
			break
		}
	}
	if pick < 0 && !ownOnly {
		for i := range q {
			if owner.mayTake(q[i].owner) {
				pick = i
				break
			}
		}
	}
	if pick < 0 || (!notAfter.IsZero() && q[pick].pushedAt.After(notAfter)) {
		return TLSHandshakeEntry{}, HandshakeOwner{}, false
	}
	taken := q[pick]
	if len(q) == 1 {
		delete(s.m, key)
	} else {
		s.m[key] = append(append(make([]timedTLSHandshakeEntry, 0, len(q)-1), q[:pick]...), q[pick+1:]...)
	}
	return taken.entry, taken.owner, true
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
