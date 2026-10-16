// Package proxy — automatic MySQL wire-protocol detection.
//
// # Why this exists
//
// MySQL is a *server-speaks-first* protocol: the server sends its Initial
// Handshake Packet (HandshakeV10) the moment the TCP connection is
// accepted, and the client stays silent until it arrives. The proxy's
// generic dispatch path does the opposite — it blocks reading the client
// (util.ReadInitialBuf, see handleConnection) *before* dialing upstream,
// so it can sniff the protocol and pick a parser.
//
// Those two facts deadlock: the client waits for a greeting that the
// proxy has not fetched, and the proxy waits for bytes the client will
// never send. The app eventually fails with the MySQL client's classic
//
//	ERROR 2013 (HY000): Lost connection to server at
//	'handshake: reading initial communication packet'
//
// The historical fix was a hardcoded port list — [3306, 4000] plus an
// opt-in `mysqlPorts` config key — that short-circuits dispatch and
// dials upstream eagerly. That works only if the user's MySQL happens
// to sit on a port keploy already knows, which excludes ProxySQL
// (6033), MaxScale (4006), Vitess/PlanetScale (15306), StarRocks and
// Doris (9030), OceanBase (2881), and the very common 3307/3308
// multi-instance and Cloud SQL Auth Proxy layouts.
//
// # What this file does instead
//
// It replaces "is this a known MySQL port?" with "does this connection
// actually behave like MySQL?", answered differently per mode because
// the two modes have fundamentally different information available.
//
//	RECORD — a real upstream exists, so ask it. The upstream is reached
//	at once and both ends are watched: whichever speaks first decides.
//	The client first: not MySQL, and its bytes go on to the parser the
//	moment they arrive. The upstream first: run the existing
//	MySQL.MatchType content matcher over its greeting. A positive match
//	routes to the MySQL parser and *learns* the port, so every later
//	connection to it takes the zero-cost fast path. No clock decides
//	anything (probeMysqlRecord).
//
//	REPLAY — there is no upstream to greet anyone; mocks are served
//	from disk. Content detection is impossible by construction, so the
//	answer has to come from what record already observed. Every MySQL
//	mock carries Metadata["destAddr"] ("host:port"), so the port set is
//	derived from the loaded mocks (see DeriveMysqlPortsFromMocks). A
//	port the recording never saw is MySQL only by the identity of what
//	the connection reaches: the container the recording reached MySQL
//	in, or the host it reached it on with the port moved, never this
//	machine or the Docker host at the container's gateway or at an
//	address of its own the keploy CLI gave the agent
//	(recordedEndpointMoved); anything else only by mysqlPorts.
//	Nothing is judged by how long a client stays silent.
//
// Together: auto-detect at record, auto-recall at replay. `mysqlPorts`
// remains as an escape hatch but is no longer required for correctness.
//
// # Cost
//
// The first connections to a port the proxy knows nothing about are
// probed; in a record, once one of them has a verdict the port is cached,
// and later connections skip the probe. In a replay, a port the recording
// never saw is judged connection by connection and never cached
// (replayUnrecordedPort). A probed connection's client is read the
// moment it speaks, so a client-speaks-first protocol (HTTP, gRPC,
// Redis, Postgres, Mongo, Kafka) waits only for the probe's dial of its
// destination to finish, a dial generic dispatch makes anyway. That dial
// is made when the connection arrives, which is when the application's
// own connect reached the proxy (where handshakes are held, it has
// already happened and is reused). Ports on the configured/default/
// learned list skip the probe entirely.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mysqlPortRegistry caches what the proxy has learned about each
// destination port, so a port is probed at most once per session (a
// replay's port the recording never saw excepted: replayUnrecordedPort).
//
// Positives (`ports`) come from either source:
//
//   - record: a port whose upstream answered with a HandshakeV10 that
//     MySQL.MatchType accepted.
//   - replay: a port parsed out of a loaded MySQL mock's destAddr.
//
// Negatives (`notMysql`) exist because the probe is not free — it
// reads both ends of the connection on goroutines of its own, and
// reaching the upstream costs a dial where handshakes are not held.
// Without a negative cache every connection to a non-MySQL port pays
// that again. A port is cached negative once the probe reaches a
// conclusion about it:
//
//   - the client spoke first (a MySQL connection never does), or
//   - the upstream greeted with something that is not a HandshakeV10, or
//   - the upstream closed the connection without a word, which mysqld
//     never does.
//
// Negatives are advisory only: the configured mysqlPorts list and the
// built-in defaults are consulted first, so an explicit config always
// wins over a cached negative.
//
// Matching is by port rather than host:port on purpose. The recorded
// destAddr's *host* is not stable across runs — container IPs change
// between record and replay — while the port comes from the app's DSN
// and does not. This also keeps the semantics identical to the
// existing `mysqlPorts` config key, which has always been port-only. A
// port the recording never saw is judged by the hosts and container keys
// of its MySQL destinations instead (recordedEndpointMoved).
type mysqlPortRegistry struct {
	mu       sync.RWMutex
	ports    map[uint32]struct{}
	notMysql map[uint32]struct{}
	// inferred holds the endpoints served MySQL as a recorded MySQL endpoint
	// that moved (recordedEndpointMoved), and hinted those whose client ended
	// silent with nothing to tie them to the recording: each keeps its
	// warning to one line per endpoint ("host:port").
	//
	// Neither is `ports`, and Has() must keep returning false for these: each
	// connection is judged by its own destination, so no membership here can
	// turn a working dependency into a permanent MySQL hijack.
	inferred map[string]struct{}
	hinted   map[string]struct{}
	// The recording's MySQL destinations, as a moved endpoint is matched by
	// them (recordedEndpointMoved): the container keys
	// (containerkey.MetadataKey) of the mocks recorded against a container,
	// the hosts of those recorded against anything else (plainHosts), and the
	// hosts of the container ones (containerHosts), which count only where
	// the replay cannot tell containers apart. Hosts are kept as
	// containerkey.ParseAddrPort reads them (an IPv4-mapped address as the
	// IPv4 one). Swapped with fromMocks at a session's derive.
	mockKeys       map[string]struct{}
	plainHosts     map[netip.Addr]struct{}
	containerHosts map[netip.Addr]struct{}
	// fromMocks holds only the ports DeriveFromMocks read out of a recording.
	// `ports` is the union of those and anything Learn() promoted during a
	// record session, and the agent is one long-lived process that does not
	// reset this registry between sessions — so `ports` being non-empty does
	// NOT mean the recording being replayed contains MySQL. The moved-endpoint
	// judgement (replayUnrecordedPort) has to be gated on evidence from the
	// recording itself, or a port learned while recording would arm it during
	// a replay with no MySQL mocks at all, and the connection would be routed
	// to a replayer with nothing to serve.
	fromMocks map[uint32]struct{}
	// sessionStale is set when a new replay session begins and consumed by the
	// next DeriveFromMocks. See MarkSessionStale for why the swap is deferred
	// to derive time rather than done eagerly.
	sessionStale bool
	derived      bool
	// derivedCh is closed the first time DeriveFromMocks runs, so a
	// replay connection that arrives before mocks are stored can wait
	// on it instead of guessing. Never closed twice (guarded by mu +
	// the derived flag), and never replaced.
	derivedCh chan struct{}
}

func newMysqlPortRegistry() *mysqlPortRegistry {
	return &mysqlPortRegistry{
		ports:          make(map[uint32]struct{}),
		notMysql:       make(map[uint32]struct{}),
		inferred:       make(map[string]struct{}),
		hinted:         make(map[string]struct{}),
		mockKeys:       make(map[string]struct{}),
		plainHosts:     make(map[netip.Addr]struct{}),
		containerHosts: make(map[netip.Addr]struct{}),
		fromMocks:      make(map[uint32]struct{}),
		derivedCh:      make(chan struct{}),
	}
}

// Has reports whether port has been learned or derived.
func (r *mysqlPortRegistry) Has(port uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ports[port]
	return ok
}

// Learn records a port as MySQL-speaking. Returns true if this was a
// new observation, so the caller can log the discovery exactly once.
// Clears any cached negative: a positive is strictly better evidence.
func (r *mysqlPortRegistry) Learn(port uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.notMysql, port)
	if _, ok := r.ports[port]; ok {
		return false
	}
	r.ports[port] = struct{}{}
	return true
}

// IsKnownNotMysql reports whether the probe has already concluded this
// port does not speak MySQL, so the probe can be skipped entirely.
func (r *mysqlPortRegistry) IsKnownNotMysql(port uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.notMysql[port]
	return ok
}

// LearnNotMysql caches a negative verdict. Returns true if this was a
// new observation, so the caller can log it exactly once. A port that
// is already a known positive is never demoted — DeriveFromMocks and a
// real handshake are both stronger evidence than a probe's negative.
func (r *mysqlPortRegistry) LearnNotMysql(port uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.ports[port]; ok {
		return false
	}
	if _, ok := r.notMysql[port]; ok {
		return false
	}
	r.notMysql[port] = struct{}{}
	return true
}

// LearnInferred records that this endpoint has been served MySQL as a recorded
// MySQL endpoint that moved (recordedEndpointMoved), returning true the first
// time so the caller logs once. It grants no fast path: Has() is unaffected, so
// the next connection is judged by its own destination.
func (r *mysqlPortRegistry) LearnInferred(dstAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inferred[dstAddr]; ok {
		return false
	}
	r.inferred[dstAddr] = struct{}{}
	return true
}

// LearnHinted records that this endpoint's next step has been said
// (hintUnlistedEndpoint), returning true the first time so the caller says it
// once.
func (r *mysqlPortRegistry) LearnHinted(dstAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hinted[dstAddr]; ok {
		return false
	}
	r.hinted[dstAddr] = struct{}{}
	return true
}

// recordedEndpointMoved reports whether a destination on a port the recording
// never saw is one of the recording's MySQL destinations with its port moved:
//
//   - a container (isContainer): its key must be one a recorded MySQL mock was
//     stamped with. The key decides, never the address — another container
//     published on the recorded host is another dependency — and a container
//     whose configuration could not be read (key "") is no match. A recording
//     made without container names (named false: no mock of its test set
//     carries a container stamp, containerkey.RecordingNamesContainers) knows
//     its MySQL only by its address: a container target then matches by host,
//     as without the switch.
//   - anything else, where the replay names containers (keyed, the proven
//     switch): its host must be one the recording reached MySQL on outside a
//     container. A host a container was recorded on says nothing about a
//     host-native service on it.
//   - anything else, where it does not: its host must be one the recording
//     reached MySQL on, container or not; the address is all there is.
//
// A host never matches when shared says every service on its machine is
// reached at it (Proxy.sharedHost): this machine, or the Docker host at the
// container's gateway or at an address of its own the keploy CLI gave the
// agent. A moved port there says nothing about which service it
// is, and greeting would race a client-first client there to its first bytes.
// A MySQL moved to another port of it is listed in mysqlPorts. shared is asked
// only of a host the recording holds, and outside the registry's lock: it
// reads the network namespace. why names the match, for the warning.
func (r *mysqlPortRegistry) recordedEndpointMoved(host netip.Addr, key string, isContainer, keyed, named bool, shared func(netip.Addr) bool) (why string, ok bool) {
	why, byHost, ok := r.recordedEndpoint(host, key, isContainer, keyed, named)
	if ok && byHost && shared(host) {
		return "", false
	}
	return why, ok
}

// recordedEndpoint is recordedEndpointMoved's match before a host is asked
// whether it is shared: byHost says the match is by host, not container key.
func (r *mysqlPortRegistry) recordedEndpoint(host netip.Addr, key string, isContainer, keyed, named bool) (why string, byHost, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if isContainer {
		if _, ok := r.mockKeys[key]; ok && key != "" {
			return "the container the recording reached MySQL in (" + key + ")", false, true
		}
		// A recording made without container names knows its MySQL only by
		// its address: a container on that host with the port moved is it,
		// as far as the recording can tell, as with the switch off.
		if named {
			return "", false, false
		}
		return r.hostMatchLocked(host, r.plainHosts)
	}
	if !keyed {
		if why, byHost, ok := r.hostMatchLocked(host, r.containerHosts); ok {
			return why, byHost, true
		}
	}
	return r.hostMatchLocked(host, r.plainHosts)
}

// hostMatchLocked reports whether host is one of hosts.
func (r *mysqlPortRegistry) hostMatchLocked(host netip.Addr, hosts map[netip.Addr]struct{}) (why string, byHost, ok bool) {
	if !host.IsValid() {
		return "", false, false
	}
	if _, ok := hosts[host.Unmap()]; !ok {
		return "", false, false
	}
	return "the host the recording reached MySQL on (" + host.Unmap().String() + "), on another port", true, true
}

// hasMockKeys reports whether any recorded MySQL mock carries a container
// key, so that a container destination's key can decide anything.
func (r *mysqlPortRegistry) hasMockKeys() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.mockKeys) > 0
}

// MarkSessionStale says that the recording in force is about to be replaced, so
// the next set of mocks should supersede rather than accumulate. The agent is a
// single long-lived process and this registry is allocated once, so without it
// the gate on the moved-endpoint judgement would mean "any recording this
// process has ever replayed" rather than "the recording being replayed now" — a
// test set holding MySQL mocks would arm it for a later one that holds none,
// and a connection there would be routed to a replayer with nothing to serve.
//
// It only marks. The actual swap happens in DeriveFromMocks, when the
// replacement is in hand, and that ordering is the whole point:
//
//   - Clearing here instead would blind the registry for the entire gap between
//     Mock() and this test set's first mock delivery — which spans reading the
//     whole recorded corpus off disk and shipping it to the agent. A connection
//     opened in that gap loses the known-port fast path and waits on
//     derivedSignal. That gap is real traffic: a reused compose stack or
//     --keep-app-alive holds the application and its connection pool open
//     across test-set boundaries, and a pool reconnecting there would stall.
//   - Clearing here would also have to re-arm derivedCh, and replacing that
//     channel strands anyone already parked on the old one: DeriveFromMocks
//     closes the NEW channel, so the old waiter is never released by the
//     mocks it waits for — the connection gets no MySQL and the test reports
//     status_code 0, the failure this whole mechanism exists to prevent.
//
// Swapping at derive time avoids both: the previous recording's ports stay
// usable right up to the instant the new ones replace them, and derivedCh keeps
// its original close-once semantics.
//
// Ports learned by probing a live server during a record session are never
// dropped: those are facts about a real server, not about a recording.
func (r *mysqlPortRegistry) MarkSessionStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionStale = true
}

// MockPorts returns the ports that came from a recording, as opposed to ones
// learned by probing during a record session.
func (r *mysqlPortRegistry) MockPorts() []uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uint32, 0, len(r.fromMocks))
	for p := range r.fromMocks {
		out = append(out, p)
	}
	return out
}

// Ports returns a snapshot of every known MySQL port, for logging.
func (r *mysqlPortRegistry) Ports() []uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uint32, 0, len(r.ports))
	for p := range r.ports {
		out = append(out, p)
	}
	return out
}

// DeriveFromMocks adds every port referenced by a MySQL mock's
// destAddr metadata and marks derivation complete, releasing any
// replay connections waiting on derivedSignal. It is safe to call
// repeatedly (once per test-set); later calls union additional ports
// and are no-ops for the completion signal.
func (r *mysqlPortRegistry) DeriveFromMocks(mockSets ...[]*models.Mock) []uint32 {
	var added []uint32

	r.mu.Lock()
	// Consume the stale mark: this delivery is the replacement, so drop what the
	// previous recording taught us before unioning in what this one does.
	if r.sessionStale {
		for port := range r.fromMocks {
			delete(r.ports, port)
		}
		r.fromMocks = make(map[uint32]struct{})
		r.inferred = make(map[string]struct{})
		r.hinted = make(map[string]struct{})
		r.mockKeys = make(map[string]struct{})
		r.plainHosts = make(map[netip.Addr]struct{})
		r.containerHosts = make(map[netip.Addr]struct{})
		r.sessionStale = false
	}
	for _, mocks := range mockSets {
		for _, m := range mocks {
			if m == nil || m.Kind != models.MySQL {
				continue
			}
			if key, ok := containerkey.Recorded(m); ok {
				r.mockKeys[key] = struct{}{}
			}
			destAddr := m.Spec.Metadata["destAddr"]
			port, ok := portFromAddr(destAddr)
			if !ok {
				continue
			}
			if ap, err := containerkey.ParseAddrPort(destAddr); err == nil {
				if containerkey.RecordedContainer(m) {
					r.containerHosts[ap.Addr()] = struct{}{}
				} else {
					r.plainHosts[ap.Addr()] = struct{}{}
				}
			}
			r.fromMocks[port] = struct{}{}
			if _, exists := r.ports[port]; !exists {
				r.ports[port] = struct{}{}
				added = append(added, port)
			}
		}
	}
	if !r.derived {
		r.derived = true
		close(r.derivedCh)
	}
	r.mu.Unlock()

	return added
}

// derivedSignal is closed once DeriveFromMocks has run: a replay connection
// that arrives before its mocks are stored waits on it, or on its client
// speaking first (probeMysqlReplay).
func (r *mysqlPortRegistry) derivedSignal() <-chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.derivedCh
}

// portFromAddr pulls the port out of a recorded destAddr. Handles
// "127.0.0.1:3306", "db:3306" and the IPv6 "[::1]:3306" form.
func portFromAddr(addr string) (uint32, bool) {
	if addr == "" {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil || port == 0 {
		return 0, false
	}
	return uint32(port), true
}

// deriveMysqlPorts recovers the MySQL port set from the mocks a replay
// run has just loaded, and releases any connection waiting on
// derivedSignal. Called from SetMocks / SetMocksWithWindow before the
// mocks are published to the manager.
//
// Only replay reaches these callers, so derivedCh is never closed in
// record mode. That is safe because derivedSignal is only waited on
// under MODE_TEST, and the wait also ends when the connection's client
// speaks or closes, or the run ends: a replay that never stores mocks
// leaves a waiting connection exactly where generic dispatch would
// leave it, reading its client.
func (p *Proxy) deriveMysqlPorts(mockSets ...[]*models.Mock) {
	if p.mysqlPorts == nil {
		return
	}
	added := p.mysqlPorts.DeriveFromMocks(mockSets...)
	if len(added) > 0 {
		p.logger.Info("recovered MySQL ports from recorded mocks; replay will route them to the MySQL parser",
			zap.Uint32s("ports", added))
	}
}

// mysqlProbe is what probeMysql learned, together with any connection
// state it had to create or consume while learning it.
//
// SrcConn, DstConn and Predialed must always be adopted by the caller:
// the probe may have consumed bytes off either socket, and the returned
// conns replay them. Ignoring them loses the client's first packet or
// the server's greeting.
type mysqlProbe struct {
	// IsMySQL routes the connection to the MySQL parser.
	IsMySQL bool
	// SrcConn is srcConn, wrapped to replay any client bytes the
	// probe consumed. Never nil.
	SrcConn net.Conn
	// DstConn is the upstream on a positive verdict, wrapped to replay
	// the server greeting the probe consumed. nil otherwise.
	DstConn net.Conn
	// Predialed, when not nil, is the upstream the probe dialled for
	// this connection on a host where its handshake was not held,
	// handed on with any bytes the probe read from it: the connection's
	// first dial of its destination takes it (util.Adopt), so the
	// destination sees one connection. The caller closes it if no dial
	// does (CloseIfUnused). A connection dialled while the handshake was
	// held is handed back to its own util.Predialed instead.
	Predialed *util.Predialed
	// Reason is a short tag for debug logs.
	Reason string
}

// probeMysql decides whether this connection should be handled by the
// MySQL parser. See the package doc above for the design; the short
// version is: known port → yes; record → whichever end of the
// connection speaks first; replay → the recorded mocks.
//
// It never returns a nil *mysqlProbe. A non-nil error means the
// connection is unusable (upstream dial failure) and the caller should
// abort — the probe's conns are still safe to inspect for cleanup.
func (p *Proxy) probeMysql(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
	pre *util.Predialed, // the connection dialled while the handshake was held, or nil
	port uint32,
	mode models.Mode,
	opts models.OutgoingOptions,
	logger *zap.Logger,
) (*mysqlProbe, error) {
	// ── Fast path: a port we already know ──
	// Configured mysqlPorts, the built-in defaults, or a port learned
	// earlier in this session / derived from mocks. Zero probe cost,
	// and byte-identical to the pre-auto-detection behaviour.
	if isMysqlPort(port, opts.MysqlPorts) {
		return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "configured-port"}, nil
	}
	if p.mysqlPorts.Has(port) {
		return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "known-port"}, nil
	}
	// A port already probed and found not to speak MySQL. Skipping here
	// is what keeps the probe a one-time cost per port rather than a
	// per-connection tax.
	if p.mysqlPorts.IsKnownNotMysql(port) {
		return &mysqlProbe{SrcConn: srcConn, Reason: "known-not-mysql"}, nil
	}

	// Auto-detection is opt-out: when disabled we behave exactly as
	// before — unknown ports fall through to generic dispatch (and, for
	// a real MySQL server, deadlock as they always did).
	if opts.DisableMysqlAutoDetect {
		return &mysqlProbe{SrcConn: srcConn, Reason: "auto-detect-disabled"}, nil
	}

	// notMysql caches a negative verdict and builds the probe result in
	// one place, so no negative path can forget to do it.
	notMysql := func(reason string, src net.Conn) *mysqlProbe {
		if p.mysqlPorts.LearnNotMysql(port) {
			logger.Debug("port does not speak MySQL; skipping detection for it from now on",
				zap.Uint32("port", port),
				zap.String("dstAddr", dstAddr),
				zap.String("reason", reason))
		}
		return &mysqlProbe{SrcConn: src, Reason: reason}
	}

	if mode == models.MODE_TEST {
		return p.probeMysqlReplay(ctx, srcConn, dstAddr, port, opts, logger, notMysql)
	}
	return p.probeMysqlRecord(ctx, srcConn, dstAddr, pre, port, logger, notMysql)
}

// probeMysqlRecord answers, for a record, whether the destination speaks
// MySQL by what the connection itself does first. A MySQL client sends
// nothing until its server greets it, and mysqld greets the moment it
// accepts; every other protocol keploy records is opened by its client.
// So the upstream is reached at once, both ends are read from the start,
// and whichever speaks first decides:
//
//   - the client: not MySQL. Its bytes go on to generic dispatch the moment
//     they arrive, so its first request reaches its parser, and is stamped,
//     when the application sent it.
//   - the upstream: MySQL if its greeting is a HandshakeV10, read until
//     MySQL.MatchType's answer on it can no longer change (greetingDecided).
//   - the upstream closing without a word: not MySQL (mysqld never does).
//
// No clock decides anything: until one end speaks or closes, or the run
// ends, the connection is idle, as it is without keploy — a pool's
// connection opened at startup and first used minutes later. The probe
// used to wait for 250ms of client silence and then up to 2s for a
// greeting, with the client's first bytes unread meanwhile, so a pooled
// connection first used inside that time had its call held back and
// stamped late, and recorded against the next test.
//
// The upstream is the connection dialled while the application's handshake
// was held (pre), which is the application's own; without one it is dialled
// here, when the application's connect reached the proxy, and handed on
// (mysqlProbe.Predialed) so the destination still sees one connection.
func (p *Proxy) probeMysqlRecord(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
	pre *util.Predialed,
	port uint32,
	logger *zap.Logger,
	notMysql func(reason string, src net.Conn) *mysqlProbe,
) (*mysqlProbe, error) {
	// Reuse the parser's own content matcher rather than reimplementing
	// handshake parsing here — it already validates the HandshakeV10
	// shape and is deliberately strict about false positives.
	matcher, ok := p.Integrations[integrations.MYSQL]
	if !ok {
		// No MySQL parser registered in this build: nothing could route
		// there anyway. Don't cache a negative — this says nothing about
		// the port, only about the build.
		logger.Debug("mysql parser not registered; skipping content detection")
		return &mysqlProbe{SrcConn: srcConn, Reason: "mysql-parser-unregistered"}, nil
	}
	if !wakeable(srcConn) {
		return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
	}

	client := startFirstRead(srcConn, logger)
	up := startUpstreamLook(ctx, logger, dstAddr, pre, matcher)
	cancelled := false
	select {
	case <-client.done:
	case <-up.done:
	case <-ctx.Done():
		cancelled = true
	}
	clientBytes, clientErr := client.stop()
	clientSpoke := len(clientBytes) > 0
	// A client that has gone, or a run that has ended, needs no upstream: a
	// dial still in flight is abandoned rather than waited for.
	clientGone := !clientSpoke && clientErr != nil && !isTimeout(clientErr)
	dstConn, greeting, upErr, dialErr := up.stop(cancelled || clientGone)
	src := replayConn(srcConn, clientBytes, logger)

	// handBack gives the upstream to whoever dials the destination next,
	// with what the probe read from it: back to the held connection's
	// Predialed, or as a Predialed of its own (util.Adopt).
	predialed := pre.Is(dstConn)
	handBack := func(read []byte) *util.Predialed {
		if dstConn == nil {
			return nil
		}
		var replay net.Conn
		if len(read) > 0 {
			replay = replayConn(dstConn, read, logger)
		}
		if predialed {
			if !pre.Return(dstConn, replay) {
				_ = dstConn.Close()
			}
			return nil
		}
		if replay == nil {
			replay = dstConn
		}
		return util.Adopt(dstAddr, replay)
	}
	with := func(probe *mysqlProbe, read []byte) *mysqlProbe {
		probe.Predialed = handBack(read)
		return probe
	}

	switch {
	case errors.Is(clientErr, errLookIncomplete) || errors.Is(upErr, errLookIncomplete) || errors.Is(dialErr, errLookIncomplete):
		// A read of the probe's panicked (recovered, and logged): what it
		// had read is lost, so neither end can be handed on intact.
		if dstConn != nil {
			_ = dstConn.Close()
		}
		return &mysqlProbe{SrcConn: src, Reason: "probe-failed"}, errLookIncomplete

	case cancelled:
		return with(&mysqlProbe{SrcConn: src, Reason: "cancelled"}, greeting), nil

	case clientGone:
		// EOF or a real network error before a byte: nothing to detect.
		// Hand the connection back and let the existing path produce its
		// usual diagnostics.
		return with(&mysqlProbe{SrcConn: src, Reason: "client-read-error"}, greeting), nil

	case dialErr != nil:
		// The destination cannot be reached, so the connection cannot be
		// served, whichever end spoke: generic dispatch's own dial would
		// fail the same way, so it is not tried a second time.
		return &mysqlProbe{SrcConn: src, Reason: "upstream-dial-failed"}, dialErr

	case len(greeting) > 0 && matcher.MatchType(ctx, greeting):
		// The server identified itself; a client that spoke at the same
		// moment changes nothing about that.
		if p.mysqlPorts.Learn(port) {
			logger.Info("detected MySQL wire protocol on a non-default port; routing it to the MySQL parser for the rest of this session",
				zap.Uint32("port", port),
				zap.String("dstAddr", dstAddr),
				zap.String("note", "set mysqlPorts in keploy.yml to skip detection, or disableMysqlAutoDetect: true to turn this off"))
		}
		// Keep the connection: the greeting is already on the wire and
		// the client answers it at once, as on the isMysqlPort path.
		return &mysqlProbe{
			IsMySQL: true,
			SrcConn: src,
			DstConn: replayConn(dstConn, greeting, logger),
			Reason:  "detected-handshake",
		}, nil

	case clientSpoke:
		if len(greeting) == 0 && upErr != nil && !isTimeout(upErr) && !predialed {
			// The destination closed the probe's own connection as the
			// client spoke: nothing is handed on, and the connection's
			// dial dials afresh, as it would have without the probe.
			_ = dstConn.Close()
			return notMysql("client-spoke-first", src), nil
		}
		return with(notMysql("client-spoke-first", src), greeting), nil

	case len(greeting) > 0:
		return with(notMysql("greeting-not-mysql", src), greeting), nil

	case upErr != nil && !isTimeout(upErr):
		logger.Debug("upstream closed the connection without a greeting; not classifying as mysql",
			zap.String("dstAddr", dstAddr), zap.Error(upErr))
		// The destination closed the application's own connection while
		// the application was silent: close the application's end, as the
		// destination would have. A connection the probe dialled itself is
		// not the application's: generic dispatch dials afresh.
		_ = dstConn.Close()
		if predialed && util.ReceivedNothing(srcConn) {
			_ = srcConn.Close()
		}
		return notMysql("upstream-read-error", src), nil
	}
	// Neither end said or did anything, yet a read ended: nothing to go on.
	return with(&mysqlProbe{SrcConn: src, Reason: "undecided"}, greeting), nil
}

// probeMysqlReplay answers, for a replay, whether the connection is MySQL
// from the recording: keploy is the server here, so there is no greeting to
// read. A connection that arrives before the test set's mocks are stored
// (the docker-compose path starts the app first) waits for them, or for its
// client to speak first, whichever comes first, with its client's first
// bytes read the moment they arrive.
func (p *Proxy) probeMysqlReplay(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
	port uint32,
	opts models.OutgoingOptions,
	logger *zap.Logger,
	notMysql func(reason string, src net.Conn) *mysqlProbe,
) (*mysqlProbe, error) {
	derived := p.mysqlPorts.derivedSignal()
	select {
	case <-derived:
	default:
		if !wakeable(srcConn) {
			return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
		}
		client := startFirstRead(srcConn, logger)
		cancelled := false
		select {
		case <-client.done:
		case <-derived:
		case <-ctx.Done():
			cancelled = true
		}
		read, err := client.stop()
		switch {
		case errors.Is(err, errLookIncomplete):
			// The read panicked (recovered, and logged): what it had read
			// is lost, so the connection cannot be handed on intact.
			return &mysqlProbe{SrcConn: srcConn, Reason: "probe-failed"}, err
		case len(read) > 0:
			return notMysql("client-spoke-first", replayConn(srcConn, read, logger)), nil
		case err != nil && !isTimeout(err):
			return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
		case cancelled:
			return &mysqlProbe{SrcConn: srcConn, Reason: "cancelled"}, nil
		}
	}

	if p.mysqlPorts.Has(port) {
		logger.Debug("recalled mysql port from recorded mocks", zap.Uint32("port", port))
		return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "derived-from-mocks"}, nil
	}
	// The port is not one the recording saw. The gate is MockPorts, not
	// Ports: the latter also holds ports learned by probing during a record
	// session, and the agent process does not reset this registry between
	// sessions, so it would arm the judgement below in a replay whose
	// recording contains no MySQL at all.
	recorded := p.mysqlPorts.MockPorts()
	if len(recorded) == 0 || opts.DisableMysqlEndpointDrift {
		return &mysqlProbe{SrcConn: srcConn, Reason: "no-mysql-mock-for-port"}, nil
	}
	return p.replayUnrecordedPort(ctx, srcConn, dstAddr, port, recorded, logger)
}

// replayUnrecordedPort judges a replay connection to a port the recording
// never saw, in a replay whose recording holds MySQL. The replayed application
// may have been handed a MySQL endpoint that moved since the recording — a
// random published port, a reconstructed environment — and declining it is not
// neutral: keploy is the server here, so nothing else will ever greet its
// client, and the tests that touch it fail with the recording unused.
//
// But nothing about a client that has not spoken yet says it is waiting to be
// greeted: it may simply not have spoken yet, and no clock can tell the two
// apart. So the connection is MySQL only by the identity of what it reaches:
// the container the recording reached MySQL in, or the host it reached it on
// with the port moved — never this machine or the Docker host (at the
// container's gateway, or at an address of its own the keploy CLI gave the
// agent), which every service on them shares (Proxy.sharedHost,
// recordedEndpointMoved). Then keploy, the server, greets at
// once — unless the client has already spoken, which a MySQL client never
// does. Anything else is MySQL only if mysqlPorts lists its port (the fast
// path in probeMysql), and is handed on: if its client closes having said
// nothing, it waited to be greeted, and its miss names the port to list
// (unlistedEndpointMiss); a warning says it once per endpoint, also when the
// run ends with the client silent (hintUnlistedEndpoint). No verdict here is
// cached for the port: each connection is judged by what it reaches.
func (p *Proxy) replayUnrecordedPort(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
	port uint32,
	recorded []uint32,
	logger *zap.Logger,
) (*mysqlProbe, error) {
	var host netip.Addr
	if ap, err := containerkey.ParseAddrPort(dstAddr); err == nil {
		host = ap.Addr()
	}
	// A container's key is waited for only when a recorded key could match
	// it; otherwise whether the destination is a container at all decides,
	// and placing a connection never waits on a container's configuration.
	var (
		key                string
		isContainer, keyed bool
	)
	if p.mysqlPorts.hasMockKeys() {
		key, isContainer, keyed = containerkey.KeyFromContext(ctx)
	} else {
		isContainer, keyed = containerkey.PlacedFromContext(ctx)
	}
	named := containerkey.RecordingNamesContainers(ctx)
	shared := func(a netip.Addr) bool { _, ok := p.sharedHost(a); return ok }
	if why, ok := p.mysqlPorts.recordedEndpointMoved(host, key, isContainer, keyed, named, shared); ok {
		if nothing, known := util.ArrivedNothing(srcConn); known && !nothing {
			return &mysqlProbe{SrcConn: srcConn, Reason: "client-spoke-first"}, nil
		}
		// Bookkeeping only — this must not grant the fast path: the next
		// connection on this port is judged by its own destination.
		if p.mysqlPorts.LearnInferred(dstAddr) {
			logger.Warn("serving MySQL mocks on a port the recording never saw: the destination is "+why+", so the application reaches MySQL at an endpoint that moved since the recording",
				zap.Uint32("port", port),
				zap.String("dstAddr", dstAddr),
				zap.Uint32s("recordedPorts", recorded),
				zap.String("next_step", "set mysqlPorts in keploy.yml to pin the port, or disableMysqlEndpointDrift: true if a non-MySQL dependency is being misread as MySQL here"))
		}
		return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "recorded-endpoint-moved"}, nil
	}

	if !wakeable(srcConn) {
		return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
	}
	client := startFirstRead(srcConn, logger)
	cancelled := false
	select {
	case <-client.done:
	case <-ctx.Done():
		cancelled = true
	}
	read, err := client.stop()
	switch {
	case errors.Is(err, errLookIncomplete):
		// The read panicked (recovered, and logged): what it had read is
		// lost, so the connection cannot be handed on intact.
		return &mysqlProbe{SrcConn: srcConn, Reason: "probe-failed"}, err
	case len(read) > 0:
		return &mysqlProbe{SrcConn: replayConn(srcConn, read, logger), Reason: "client-spoke-first"}, nil
	case err != nil && !isTimeout(err):
		// It waited to be greeted and gave up: a call no mock answered.
		p.sendMockNotFoundError(unlistedEndpointMiss(dstAddr, recorded, p.unlistedNextStep(dstAddr, host, port)))
		p.hintUnlistedEndpoint(logger, dstAddr, host, port, recorded, "its client closed it without sending anything")
		return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
	case cancelled:
		p.hintUnlistedEndpoint(logger, dstAddr, host, port, recorded, "the run ended with its client still silent")
		return &mysqlProbe{SrcConn: srcConn, Reason: "cancelled"}, nil
	}
	return &mysqlProbe{SrcConn: srcConn, Reason: "undecided"}, nil
}

// hintUnlistedEndpoint says, once per endpoint, which port to list in
// mysqlPorts for a connection to a port the recording never saw whose client
// ended silent, in a replay whose recording holds MySQL: an application that
// waited there for its server to speak first was reaching MySQL at an endpoint
// keploy could not tie to the recording. what says how the connection ended.
func (p *Proxy) hintUnlistedEndpoint(logger *zap.Logger, dstAddr string, host netip.Addr, port uint32, recorded []uint32, what string) {
	if !p.mysqlPorts.LearnHinted(dstAddr) {
		return
	}
	logger.Warn("a connection to a port the recording never saw ended with nothing sent ("+what+"): if the application was waiting there for its server to speak first, it reaches MySQL at an endpoint that moved since the recording, which keploy serves only by the container or the host the recording reached MySQL at (not this machine, nor the Docker host at the container's gateway or at an address of its own the keploy CLI gave the agent), so it served this one nothing",
		zap.String("dstAddr", dstAddr),
		zap.Uint32s("recordedPorts", recorded),
		zap.String("next_step", p.unlistedNextStep(dstAddr, host, port)))
}

// unlistedNextStep is the next step for a MySQL endpoint keploy cannot tie to
// the recording: list its port. On a shared host (sharedHost) it says that a
// moved port there is never taken for the recorded MySQL.
func (p *Proxy) unlistedNextStep(dstAddr string, host netip.Addr, port uint32) string {
	where := ""
	if name, ok := p.sharedHost(host); ok {
		where = " A MySQL moved to another port of " + name + " is never taken for the recorded one: only a listed port is."
	}
	return fmt.Sprintf("if %s is MySQL, add %d to mysqlPorts in keploy.yml, keeping the ports already listed there (a mysqlPorts list replaces the built-in 3306 and 4000): keploy then serves the recorded MySQL mocks on it.%s", dstAddr, port, where)
}

// unlistedEndpointMiss is the miss of a connection to a port the recording
// never saw whose client closed it without sending anything, in a replay whose
// recording holds MySQL: it waited for its server to speak first, as a MySQL
// client does, and nothing answered it. next is its next step
// (unlistedNextStep), which names the port to list.
func unlistedEndpointMiss(dstAddr string, recorded []uint32, next string) error {
	why := fmt.Sprintf("the client closed its connection to a port the recording never saw without sending anything: it waited for its server to speak first, as a MySQL client does, and keploy answered nothing (the recording holds MySQL on ports %v)", recorded)
	report := mismatch.NewReport(mismatch.ProtocolMySQL, "connection phase: server greeting").
		WithDestination(dstAddr).
		WithPhase(models.MatchPhaseNoRequest, 0).
		WithDiff(why).
		WithNextSteps(next).
		Build()
	return models.NewMockMismatchError(fmt.Errorf("mysql connection phase, server greeting: %s: %w", why, models.ErrNoMockMatched), report)
}

// wakeable reports whether conn takes a read deadline, which is what wakes a
// probe's read of it once the other end has decided. A connection that takes
// none (a closed one, say) is not read by the probe at all.
func wakeable(conn net.Conn) bool {
	return conn.SetReadDeadline(time.Time{}) == nil
}

// errLookIncomplete stands for a read or dial on a probe's goroutine that
// ended without an answer of its own (it panicked, and was recovered).
var errLookIncomplete = errors.New("the mysql probe's read did not complete")

// firstRead reads a connection's first bytes on a goroutine of its own, with
// no deadline: it ends when the peer sends, closes, or stop wakes it.
type firstRead struct {
	conn net.Conn
	done chan struct{}
	b    []byte
	err  error
}

func startFirstRead(conn net.Conn, logger *zap.Logger) *firstRead {
	r := &firstRead{conn: conn, done: make(chan struct{}), err: errLookIncomplete}
	go func() {
		defer close(r.done)
		defer util.RecoverWithoutClose(logger)
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		r.b, r.err = buf[:n], err
	}()
	return r
}

// stop ends the read if it is still waiting — a read deadline in the past
// wakes it, and no bytes are lost to that: they stay on the socket — and
// returns what the client had sent by then. The connection is left with no
// deadline.
func (r *firstRead) stop() ([]byte, error) {
	_ = r.conn.SetReadDeadline(time.Unix(1, 0))
	<-r.done
	_ = r.conn.SetReadDeadline(time.Time{})
	if len(r.b) == 0 && isTimeout(r.err) {
		// A read the deadline stopped before it began fails without
		// looking at the socket, so bytes, or the client's close, may be
		// waiting there all the same.
		switch data, closed := util.Waiting(r.conn); {
		case data:
			buf := make([]byte, 4096)
			n, err := r.conn.Read(buf) // returns at once: bytes are waiting
			r.b, r.err = buf[:n], err
		case closed:
			r.err = io.EOF
		}
	}
	return r.b, r.err
}

// upstreamLook reaches a connection's destination and reads what it sends
// first, on a goroutine of its own, until MySQL.MatchType's answer on it can
// no longer change (greetingDecided), the destination closes, or stop wakes
// it. There is no deadline: a destination that waits for its client to speak
// is waited for, as the client is.
type upstreamLook struct {
	done   chan struct{}
	cancel context.CancelFunc // abandons a dial still in flight

	mu      sync.Mutex
	conn    net.Conn // the upstream, once reached
	stopped bool

	greeting []byte
	err      error // what ended the read
	dialErr  error // the upstream could not be reached
}

// dialProbeUpstream dials a probed connection's destination when no connection
// was dialled while its handshake was held. A variable so a test can stand in a
// destination that never answers.
var dialProbeUpstream = func(ctx context.Context, logger *zap.Logger, dstAddr string) (net.Conn, error) {
	return util.DialDestination(ctx, logger, "tcp", util.DialTarget{Addr: dstAddr})
}

// startUpstreamLook reaches dstAddr through pre, the connection dialled while
// the application's handshake was held — taken even if the destination has
// closed it, which is then its answer (Look) — or by dialling it.
func startUpstreamLook(ctx context.Context, logger *zap.Logger, dstAddr string, pre *util.Predialed, matcher integrations.Integrations) *upstreamLook {
	dialCtx, cancel := context.WithCancel(ctx)
	l := &upstreamLook{done: make(chan struct{}), cancel: cancel, dialErr: errLookIncomplete}
	go func() {
		defer close(l.done)
		defer util.RecoverWithoutClose(logger)
		conn := pre.Look(dstAddr)
		if conn == nil {
			c, err := dialProbeUpstream(dialCtx, logger, dstAddr)
			if err != nil {
				l.dialErr = err
				return
			}
			conn = c
		}
		l.mu.Lock()
		// errLookIncomplete until the read returns: a read that panics
		// leaves it, and what it had read is lost with it.
		l.conn, l.dialErr, l.err = conn, nil, errLookIncomplete
		stopped := l.stopped
		l.mu.Unlock()
		if stopped {
			l.err = nil
			return
		}
		l.greeting, l.err = readGreeting(ctx, conn, matcher)
	}()
	return l
}

// stop ends the look if it is still reading, and returns the upstream, what
// was read from it, what ended the read, and why the upstream could not be
// reached. A dial in progress is let finish, since the connection is the
// application's, unless abandon says nobody will use it. The upstream is left
// with no deadline.
func (l *upstreamLook) stop(abandon bool) (conn net.Conn, greeting []byte, err, dialErr error) {
	l.mu.Lock()
	l.stopped = true
	if l.conn != nil {
		_ = l.conn.SetReadDeadline(time.Unix(1, 0))
	}
	l.mu.Unlock()
	if abandon {
		l.cancel()
	}
	<-l.done
	l.cancel()
	if l.conn != nil {
		_ = l.conn.SetReadDeadline(time.Time{})
	}
	return l.conn, l.greeting, l.err, l.dialErr
}

// readGreeting accumulates the upstream's first bytes until the MySQL matcher's
// answer on them is final (greetingDecided), or a read fails, and returns all
// it read.
//
// A single Read is not enough. MySQL.MatchType needs the complete first packet
// (it rejects a buffer shorter than the declared packet length), so a
// HandshakeV10 split across two TCP segments would be judged "not MySQL" and
// the connection would fall through to the generic path — re-introducing
// exactly the deadlock this detection exists to remove.
func readGreeting(ctx context.Context, conn net.Conn, matcher integrations.Integrations) ([]byte, error) {
	var greeting []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			greeting = append(greeting, buf[:n]...)
			if greetingDecided(ctx, greeting, matcher) {
				return greeting, nil
			}
		}
		if err != nil {
			return greeting, err
		}
	}
}

const (
	// maxHandshakePacket is the longest first packet MySQL.MatchType takes
	// for a handshake; it rejects any longer one.
	maxHandshakePacket = 512
	// maxGreetingBytes is past any plausible MySQL handshake.
	maxGreetingBytes = 1024
)

// greetingDecided reports whether buf, the start of what an upstream sent
// first, holds enough for MySQL.MatchType's answer on it to be final, so that
// reading on could not change it:
//
//   - MatchType accepts it;
//   - its first packet is complete: MatchType judges that packet alone (it
//     slices the body to the declared length), so more bytes cannot change
//     its answer;
//   - its first packet declares a length no handshake has, as any text
//     banner does ("220 ..." reads as millions of bytes);
//   - it is longer than any handshake.
//
// A destination that sends less than its first packet and then waits for its
// client would be read on until it does more; MySQL never does that, and no
// server-first protocol keploy has met does either (text banners are decided
// by their first four bytes).
func greetingDecided(ctx context.Context, buf []byte, matcher integrations.Integrations) bool {
	if matcher.MatchType(ctx, buf) || len(buf) >= maxGreetingBytes {
		return true
	}
	if len(buf) < 4 {
		return false
	}
	pktLen := int(buf[0]) | int(buf[1])<<8 | int(buf[2])<<16
	return pktLen > maxHandshakePacket || len(buf) >= 4+pktLen
}

// replayConn wraps conn so the already-consumed prefix is handed back
// on the next reads, then reads pass through to the socket. Writes,
// deadlines and Close all go straight to the real connection via the
// embedded net.Conn.
func replayConn(conn net.Conn, prefix []byte, logger *zap.Logger) net.Conn {
	if len(prefix) == 0 {
		return conn
	}
	return &util.Conn{
		Conn:   conn,
		Reader: util.NewPrefixReader(prefix, conn),
		Logger: logger,
	}
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
