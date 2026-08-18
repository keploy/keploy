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
//	RECORD — a real upstream exists, so ask it. If the client stays
//	silent past a short window the connection is server-speaks-first;
//	dial upstream, read whatever it greets with, and run the existing
//	MySQL.MatchType content matcher over those bytes. A positive match
//	routes to the MySQL parser and *learns* the port, so every later
//	connection to it takes the zero-cost fast path.
//
//	REPLAY — there is no upstream to greet anyone; mocks are served
//	from disk. Content detection is impossible by construction, so the
//	answer has to come from what record already observed. Every MySQL
//	mock carries Metadata["destAddr"] ("host:port"), so the port set is
//	derived from the loaded mocks (see DeriveMysqlPortsFromMocks).
//
// Together: auto-detect at record, auto-recall at replay. `mysqlPorts`
// remains as an escape hatch but is no longer required for correctness.
//
// # Cost
//
// Only connections whose client is silent past clientSilenceWindow pay
// anything at all. Client-speaks-first protocols (HTTP, gRPC, Redis,
// Postgres, Mongo, Kafka) send bytes immediately and return from
// probeMysql on the first read, so the hot path is unchanged. Ports on
// the configured/default/learned list skip the probe entirely.
package proxy

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Probe tuning. All three are overridable by env var so a slow or
// heavily contended environment can be adjusted without a rebuild;
// the defaults are what ships.
const (
	// defaultClientSilenceWindow is how long the client gets to send
	// its first byte before we conclude the connection is
	// server-speaks-first. MySQL clients send *nothing* here, so any
	// traffic at all is a definitive negative. Kept short because it
	// is pure added latency on the (rare) silent-client path, and
	// because a detected port is learned — a connection pool pays it
	// at most once per port.
	defaultClientSilenceWindow = 250 * time.Millisecond

	// defaultServerGreetingWindow bounds the wait for the upstream's
	// first bytes once we have decided the client is silent. Sending
	// the handshake is the first thing mysqld does on accept, so this
	// window only has to absorb scheduling jitter and a loaded
	// container — 2s is generous by a wide margin. It matters that it
	// IS generous: a port that stays silent through it is cached as
	// non-MySQL for the session (see mysqlPortRegistry), so an
	// under-sized window would misclassify a very slow server.
	defaultServerGreetingWindow = 2 * time.Second

	// defaultMockDeriveWait bounds how long a replay-mode connection
	// waits for the mock set to be loaded before giving up on port
	// derivation. This matters only for the docker-compose replay
	// path, where the app container is started before StoreMocks
	// lands; every other path stores mocks before the app runs. A
	// bounded wait keeps that race deterministic instead of guessing.
	defaultMockDeriveWait = 20 * time.Second
)

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func clientSilenceWindow() time.Duration {
	return envDuration("KEPLOY_MYSQL_CLIENT_SILENCE_WINDOW", defaultClientSilenceWindow)
}

func serverGreetingWindow() time.Duration {
	return envDuration("KEPLOY_MYSQL_SERVER_GREETING_WINDOW", defaultServerGreetingWindow)
}

func mockDeriveWait() time.Duration {
	return envDuration("KEPLOY_MYSQL_MOCK_DERIVE_WAIT", defaultMockDeriveWait)
}

// mysqlPortRegistry caches what the proxy has learned about each
// destination port, so a port is probed at most once per session.
//
// Positives (`ports`) come from either source:
//
//   - record: a port whose upstream answered with a HandshakeV10 that
//     MySQL.MatchType accepted.
//   - replay: a port parsed out of a loaded MySQL mock's destAddr.
//
// Negatives (`notMysql`) exist because the probe is not free — a
// silent client costs a wait, and reaching the upstream costs a dial.
// Without a negative cache every connection to a non-MySQL port pays
// that again, which for a pool of pre-opened sockets is a permanent
// per-connection tax rather than a one-time discovery cost. A port is
// cached negative once the probe reaches a conclusion about it:
//
//   - the client spoke first (a MySQL connection never does), or
//   - the upstream greeted with something that is not a HandshakeV10, or
//   - the upstream stayed silent for the whole greeting window, which
//     mysqld never does (see defaultServerGreetingWindow).
//
// Negatives are advisory only: the configured mysqlPorts list and the
// built-in defaults are consulted first, so an explicit config always
// wins over a cached negative.
//
// Matching is by port rather than host:port on purpose. The recorded
// destAddr's *host* is not stable across runs — container IPs change
// between record and replay — while the port comes from the app's DSN
// and does not. This also keeps the semantics identical to the
// existing `mysqlPorts` config key, which has always been port-only.
type mysqlPortRegistry struct {
	mu       sync.RWMutex
	ports    map[uint32]struct{}
	notMysql map[uint32]struct{}
	// inferred holds the endpoints that were served MySQL on behavioural
	// evidence alone (a silent client on a port no mock names — see the
	// endpoint-drift branch in probeMysql). It does two things: it keeps the
	// warning to one line per endpoint, and it lets the next connection to the
	// SAME endpoint skip the long confirmation wait, which a pool reopening
	// connections would otherwise re-pay every time.
	//
	// It is deliberately not `ports`, and Has() must keep returning false for
	// these: the silence check still runs on every connection, which is what
	// rejects a client that speaks first, so no membership here can turn a
	// working dependency into a permanent MySQL hijack.
	//
	// The key is the full "host:port", unlike everything else in this registry.
	// Port-only matching is right for a recording — the host changes between
	// record and replay, the port is the app's own configuration — but this map
	// weakens a check rather than recalling recorded evidence, and a different
	// service that merely shares a port number must not inherit that.
	inferred map[string]struct{}
	// fromMocks holds only the ports DeriveFromMocks read out of a recording.
	// `ports` is the union of those and anything Learn() promoted during a
	// record session, and the agent is one long-lived process that does not
	// reset this registry between sessions — so `ports` being non-empty does
	// NOT mean the recording being replayed contains MySQL. The endpoint-drift
	// inference has to be gated on evidence from the recording itself, or a
	// port learned while recording would arm it during a replay with no MySQL
	// mocks at all, and the connection would be routed to a replayer with
	// nothing to serve.
	fromMocks map[uint32]struct{}
	derived   bool
	// derivedCh is closed the first time DeriveFromMocks runs, so a
	// replay connection that arrives before mocks are stored can block
	// on it instead of guessing. Never closed twice (guarded by mu +
	// the derived flag).
	derivedCh chan struct{}
}

func newMysqlPortRegistry() *mysqlPortRegistry {
	return &mysqlPortRegistry{
		ports:     make(map[uint32]struct{}),
		notMysql:  make(map[uint32]struct{}),
		inferred:  make(map[string]struct{}),
		fromMocks: make(map[uint32]struct{}),
		derivedCh: make(chan struct{}),
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
// real handshake are both stronger evidence than a probe timeout.
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

// LearnInferred records that this endpoint has been served on inferred
// evidence, returning true the first time so the caller logs once. It grants no
// fast path: Has() is unaffected, so the next connection still has to prove it
// is server-speaks-first before anything is served.
func (r *mysqlPortRegistry) LearnInferred(dstAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inferred[dstAddr]; ok {
		return false
	}
	r.inferred[dstAddr] = struct{}{}
	return true
}

// IsInferred reports whether this endpoint has already been served on inferred
// evidence. It shortens the confirmation the NEXT connection to the same
// endpoint has to pass — see probeMysql — but never replaces the silence check
// that precedes it.
func (r *mysqlPortRegistry) IsInferred(dstAddr string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.inferred[dstAddr]
	return ok
}

// ResetSession forgets everything learned from a recording: the ports derived
// from mocks and the endpoints inferred while serving them. The agent is one
// long-lived process and this registry is allocated once, so without this the
// gate on the drift inference would mean "any recording this process has ever
// replayed" rather than "the recording being replayed now" — a test set holding
// MySQL mocks would arm the inference for a later one that holds none, and a
// silent connection there would be routed to a replayer with nothing to serve.
//
// Ports learned by probing a live server during a record session are kept:
// those are facts about a real server, not about a recording.
func (r *mysqlPortRegistry) ResetSession() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for port := range r.fromMocks {
		delete(r.ports, port)
	}
	r.fromMocks = make(map[uint32]struct{})
	r.inferred = make(map[string]struct{})
	// Re-arm the derivation gate so a connection arriving before this
	// session's mocks are stored parks instead of being decided against the
	// previous session's port set.
	r.derived = false
	r.derivedCh = make(chan struct{})
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
// replay connections parked in WaitDerived. It is safe to call
// repeatedly (once per test-set); later calls union additional ports
// and are no-ops for the completion signal.
func (r *mysqlPortRegistry) DeriveFromMocks(mockSets ...[]*models.Mock) []uint32 {
	var added []uint32

	r.mu.Lock()
	for _, mocks := range mockSets {
		for _, m := range mocks {
			if m == nil || m.Kind != models.MySQL {
				continue
			}
			port, ok := portFromAddr(m.Spec.Metadata["destAddr"])
			if !ok {
				continue
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

// WaitDerived blocks until DeriveFromMocks has run, ctx is cancelled,
// or timeout elapses. Returns true only if derivation actually
// completed. Callers reach this only on a silent-client replay
// connection, so the wait never touches normal traffic.
func (r *mysqlPortRegistry) WaitDerived(ctx context.Context, timeout time.Duration) bool {
	r.mu.RLock()
	done := r.derived
	ch := r.derivedCh
	r.mu.RUnlock()
	if done {
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
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
// run has just loaded, and unblocks any connection parked in
// WaitDerived. Called from SetMocks / SetMocksWithWindow before the
// mocks are published to the manager.
//
// Only replay reaches these callers, so derivedCh is never closed in
// record mode. That is safe because WaitDerived is only reachable under
// MODE_TEST — and it is bounded anyway (timeout plus ctx cancellation),
// so even an aborted replay that never stores mocks releases its parked
// connections rather than hanging them.
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
// SrcConn and DstConn must always be adopted by the caller: the probe
// may have consumed bytes off either socket, and the returned conns
// replay them. Ignoring them loses the client's first packet or the
// server's greeting.
type mysqlProbe struct {
	// IsMySQL routes the connection to the MySQL parser.
	IsMySQL bool
	// SrcConn is srcConn, wrapped to replay any client bytes the
	// probe consumed. Never nil.
	SrcConn net.Conn
	// DstConn is a pre-dialed upstream, wrapped to replay the
	// server greeting the probe consumed. nil when the probe never
	// dialed, in which case the caller dials as usual.
	DstConn net.Conn
	// Reason is a short tag for debug logs.
	Reason string
}

// probeMysql decides whether this connection should be handled by the
// MySQL parser. See the package doc above for the design; the short
// version is: known port → yes; client speaks first → no; client
// silent → ask the upstream (record) or the recorded mocks (replay).
//
// It never returns a nil *mysqlProbe. A non-nil error means the
// connection is unusable (upstream dial failure) and the caller should
// abort — the probe's conns are still safe to inspect for cleanup.
func (p *Proxy) probeMysql(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
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
	// per-connection tax — without it, every connection to every
	// non-MySQL port would re-pay the client wait and the upstream dial.
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

	// ── Step 1: is the client silent? ──
	// Only a server-speaks-first connection can be MySQL at this point.
	// Any client byte is a definitive negative and costs one read.
	clientBytes, err := readWithin(ctx, srcConn, clientSilenceWindow())
	if len(clientBytes) > 0 {
		return notMysql("client-spoke-first", replayConn(srcConn, clientBytes, logger)), nil
	}
	if err != nil && !isTimeout(err) {
		// EOF or a real network error: nothing to detect. Hand the
		// connection back untouched and let the existing path produce
		// its usual diagnostics.
		return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
	}

	logger.Debug("client silent past the probe window; treating as server-speaks-first",
		zap.String("dstAddr", dstAddr),
		zap.Uint32("port", port),
		zap.Duration("window", clientSilenceWindow()))

	// ── Step 2a: replay — there is no upstream, so ask the mocks ──
	if mode == models.MODE_TEST {
		if !p.mysqlPorts.WaitDerived(ctx, mockDeriveWait()) {
			logger.Debug("mysql port derivation did not complete before the deadline",
				zap.Uint32("port", port), zap.Duration("waited", mockDeriveWait()))
			return &mysqlProbe{SrcConn: srcConn, Reason: "mocks-not-derived"}, nil
		}
		if p.mysqlPorts.Has(port) {
			logger.Debug("recalled mysql port from recorded mocks", zap.Uint32("port", port))
			return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "derived-from-mocks"}, nil
		}
		// The port is not one the recording saw. Reaching here already means
		// the client opened the connection and then said nothing, so it is
		// waiting to be greeted, and MySQL is the only server-speaks-first
		// protocol keploy replays. Combined with "this recording contains
		// MySQL mocks", a port that does not match is an environment
		// difference — the replayed application was handed a different
		// endpoint than it had at record time — not evidence that the
		// connection isn't MySQL.
		//
		// Declining is not neutral. In replay keploy IS the server, so nothing
		// else will ever send that greeting: the application blocks until its
		// own timeout, every test touching that dependency reports
		// status_code 0, and the whole recorded mock set goes unused while the
		// recording is perfectly good.
		//
		// The gate is MockPorts, not Ports: the latter also holds ports learned
		// by probing during a record session, and the agent process does not
		// reset this registry between sessions, so it would arm this branch in
		// a replay whose recording contains no MySQL at all.
		if derived := p.mysqlPorts.MockPorts(); len(derived) > 0 && !opts.DisableMysqlEndpointDrift {
			// Confirm before acting, because the silence window is a weak
			// signal on its own — a slow TLS client can be quiet that long and
			// only then send its ClientHello, and answering that with a
			// HandshakeV10 breaks a connection that was working. A real MySQL
			// client sends nothing at all until it is greeted, so a second,
			// longer wait separates the two almost perfectly.
			//
			// Only the first connection to an endpoint pays it. After that the
			// silence check above is confirmation enough: it is what rejects a
			// client that speaks first, and it has already run. Re-paying the
			// long wait on every connection would add it to each one a pool
			// opens — in the bundle that motivated this change, 103 times.
			// Keyed by endpoint, so a different host on the same port number
			// does not inherit the shortcut.
			if !p.mysqlPorts.IsInferred(dstAddr) {
				late, lateErr := readWithin(ctx, srcConn, serverGreetingWindow())
				if len(late) > 0 {
					// It spoke, just slowly. Hand the bytes back and let the
					// normal dispatch have it. The port is NOT cached as
					// non-MySQL: this connection was not MySQL, but the next
					// one on this port still deserves the same decision.
					logger.Debug("client spoke after the extended window; not inferring MySQL",
						zap.Uint32("port", port), zap.String("dstAddr", dstAddr),
						zap.Duration("window", serverGreetingWindow()))
					return &mysqlProbe{SrcConn: replayConn(srcConn, late, logger), Reason: "client-spoke-late"}, nil
				}
				if lateErr != nil && !isTimeout(lateErr) {
					return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}, nil
				}
			}
			// Bookkeeping only — this must not grant the fast path, or one
			// inference would route every later connection on this port to the
			// MySQL replayer without re-checking that the client is silent.
			if p.mysqlPorts.LearnInferred(dstAddr) {
				logger.Warn("serving MySQL mocks on a port the recording never saw: the replayed application connects to a different endpoint than it did at record time. Set mysqlPorts in keploy.yml to pin the mapping, or disableMysqlEndpointDrift: true if a non-MySQL dependency is being misread as MySQL here.",
					zap.Uint32("port", port),
					zap.String("dstAddr", dstAddr),
					zap.Uint32s("recordedPorts", derived))
			}
			return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "inferred-endpoint-drift"}, nil
		}
		return &mysqlProbe{SrcConn: srcConn, Reason: "no-mysql-mock-for-port"}, nil
	}

	// ── Step 2b: record — dial upstream and let the server identify itself ──
	//
	// This probe connection is deliberately NOT handed back to the
	// caller on a negative verdict. Two reasons:
	//
	//  1. Leak safety. The generic path reassigns dstConn on several
	//     branches (TLS dial, CONNECT, Postgres SSL upstream) without
	//     closing what was there, so a pre-dialed socket handed over on
	//     a non-MySQL verdict would be dropped unreferenced. That is
	//     reachable: a slow TLS client can be silent past the client
	//     window and only then send its ClientHello.
	//
	//  2. Staleness. Generic dispatch dials just-in-time, right before
	//     forwarding the client's first bytes. Handing it a socket
	//     opened seconds earlier — while we were waiting on a client
	//     that is slow by definition — risks an upstream idle timeout
	//     closing it before the first byte is ever written.
	//
	// Redialing costs one extra connection on the first probe of a
	// port; the negative cache makes it exactly once. A server-speaks-
	// first protocol simply re-greets on the fresh connection, so
	// discarding this one loses nothing.
	dstConn, err := net.Dial("tcp", dstAddr)
	if err != nil {
		return &mysqlProbe{SrcConn: srcConn, Reason: "upstream-dial-failed"}, err
	}
	dstAdopted := false
	defer func() {
		if !dstAdopted {
			_ = dstConn.Close()
		}
	}()

	greeting, err := readGreetingWithin(ctx, dstConn, serverGreetingWindow(), p.Integrations[integrations.MYSQL])
	if len(greeting) == 0 {
		if err != nil && !isTimeout(err) {
			logger.Debug("upstream produced no greeting; not classifying as mysql",
				zap.String("dstAddr", dstAddr), zap.Error(err))
			return notMysql("upstream-read-error", srcConn), nil
		}
		// Both ends silent. mysqld greets immediately on accept, so a
		// full greeting window of silence rules MySQL out.
		return notMysql("no-greeting", srcConn), nil
	}

	// Reuse the parser's own content matcher rather than reimplementing
	// handshake parsing here — it already validates the HandshakeV10
	// shape and is deliberately strict about false positives.
	parser, ok := p.Integrations[integrations.MYSQL]
	if !ok {
		// No MySQL parser registered in this build: nothing could route
		// there anyway. Don't cache a negative — this says nothing about
		// the port, only about the build.
		logger.Debug("mysql parser not registered; skipping content detection")
		return &mysqlProbe{SrcConn: srcConn, Reason: "mysql-parser-unregistered"}, nil
	}

	if !parser.MatchType(ctx, greeting) {
		return notMysql("greeting-not-mysql", srcConn), nil
	}

	if p.mysqlPorts.Learn(port) {
		logger.Info("detected MySQL wire protocol on a non-default port; routing it to the MySQL parser for the rest of this session",
			zap.Uint32("port", port),
			zap.String("dstAddr", dstAddr),
			zap.String("note", "set mysqlPorts in keploy.yml to skip detection, or disableMysqlAutoDetect: true to turn this off"))
	}

	// Positive verdict: keep the connection. The greeting is already on
	// the wire and the client responds to it immediately, so there is no
	// staleness window here — this is exactly what the pre-existing
	// isMysqlPort branch did, minus the hardcoded port list.
	dstAdopted = true
	return &mysqlProbe{
		IsMySQL: true,
		SrcConn: srcConn,
		DstConn: replayConn(dstConn, greeting, logger),
		Reason:  "detected-handshake",
	}, nil
}

// readGreetingWithin accumulates the upstream's first bytes until the
// MySQL matcher can reach a verdict, the overall window expires, or the
// peer closes.
//
// A single Read is not enough. MySQL.MatchType needs the complete first
// packet (it rejects a buffer shorter than the declared packet length),
// so a HandshakeV10 split across two TCP segments would be judged "not
// MySQL" and the connection would fall through to the generic path —
// re-introducing exactly the deadlock this detection exists to remove,
// with a debug line that actively points the wrong way. Rare, but a
// hard hang when it happens, so we keep reading until the matcher
// commits or we run out of time.
//
// matcher may be nil (no MySQL parser registered); then this degrades
// to a single bounded read, which is all the caller needs.
func readGreetingWithin(ctx context.Context, conn net.Conn, window time.Duration, matcher integrations.Integrations) ([]byte, error) {
	deadline := time.Now().Add(window)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var greeting []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			greeting = append(greeting, buf[:n]...)
			// Stop as soon as the verdict is knowable. A positive is
			// final; for a negative we still need to be sure we are not
			// looking at a half-delivered handshake, so keep reading
			// until the window closes or the greeting grows past any
			// plausible handshake (MatchType caps packets at 512 bytes).
			if matcher != nil && matcher.MatchType(ctx, greeting) {
				return greeting, nil
			}
			if len(greeting) > 1024 {
				return greeting, nil
			}
		}
		if err != nil {
			return greeting, err
		}
		if !time.Now().Before(deadline) {
			return greeting, nil
		}
	}
}

// readWithin reads once from conn under a deadline. A timeout is
// reported as a normal (nil-byte, timeout-error) outcome rather than a
// failure — "said nothing in time" is a legitimate probe result. The
// deadline is always cleared so later reads are unaffected.
//
// Any bytes read before an error are returned, so no data is ever lost
// to a mid-read failure.
func readWithin(ctx context.Context, conn net.Conn, window time.Duration) ([]byte, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		return nil, err
	}

	// A read deadline is not cancellable, and this one can be seconds long.
	// Tripping the deadline early is what actually unblocks the Read, so a
	// Ctrl-C or an aborted test run does not have to sit through the rest of
	// the window on every silent connection still in flight.
	//
	// The early return above covers a context that is already cancelled. This
	// guard covers the other half: context.AfterFunc's stop() reports that the
	// function has already started but does not wait for it, and it runs on its
	// own goroutine — so a cancellation landing mid-read can call
	// SetReadDeadline(now) AFTER the reset below, leaving a deadline in the past
	// on a connection someone else is about to read. That surfaces as an
	// immediate "i/o timeout" with data waiting, which is very hard to place
	// from the outside. TestReadWithinLeavesNoDeadlineBehind pins the
	// deterministic half; this guard closes the concurrent one, which cannot be
	// tested without a fake conn that would deadlock on the same mutex.
	var (
		mu   sync.Mutex
		done bool
	)
	if ctx != nil && ctx.Done() != nil {
		stop := context.AfterFunc(ctx, func() {
			mu.Lock()
			defer mu.Unlock()
			if !done {
				_ = conn.SetReadDeadline(time.Now())
			}
		})
		defer func() {
			stop()
			mu.Lock()
			done = true
			mu.Unlock()
			_ = conn.SetReadDeadline(time.Time{})
		}()
	} else {
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if n > 0 {
		return buf[:n], err
	}
	if err != nil && isTimeout(err) && ctx != nil && ctx.Err() != nil {
		// The deadline we tripped ourselves — report the cancellation, not a
		// timeout the caller would read as "the client stayed silent".
		return nil, ctx.Err()
	}
	return nil, err
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
