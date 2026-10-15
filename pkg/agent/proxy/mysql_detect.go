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
//	derived from the loaded mocks (see DeriveMysqlPortsFromMocks).
//
// Together: auto-detect at record, auto-recall at replay. `mysqlPorts`
// remains as an escape hatch but is no longer required for correctness.
//
// # Cost
//
// The first connections to a port the proxy knows nothing about are
// probed; once one of them has a verdict the port is cached, and later
// connections skip the probe. A probed connection's client is read the
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
	"io"
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

// The windows of the replay's endpoint-drift inference (inferEndpointDrift),
// the one judgement of this file a clock still makes: a replay has no upstream
// whose greeting could decide, so a client that has not spoken yet and a MySQL
// client waiting to be greeted look the same until one of them speaks. Both are
// overridable by env var so a slow or heavily contended environment can be
// adjusted without a rebuild; the defaults are what ships. Record decides by
// what the connection does (probeMysqlRecord), and a replay on a port the
// recording names decides by the recording: neither uses them.
const (
	// defaultClientSilenceWindow is how long the client gets to send
	// its first byte before the inference treats the connection as
	// server-speaks-first. MySQL clients send *nothing* here, so any
	// traffic at all is a definitive negative.
	defaultClientSilenceWindow = 250 * time.Millisecond

	// defaultServerGreetingWindow is how much longer the first silent
	// connection to an endpoint must stay silent before the inference
	// serves it MySQL: a slow TLS client can be quiet past the silence
	// window and only then send its ClientHello.
	defaultServerGreetingWindow = 2 * time.Second
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

// mysqlPortRegistry caches what the proxy has learned about each
// destination port, so a port is probed at most once per session.
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
// existing `mysqlPorts` config key, which has always been port-only.
type mysqlPortRegistry struct {
	mu       sync.RWMutex
	ports    map[uint32]struct{}
	notMysql map[uint32]struct{}
	// inferred holds the endpoints that were served MySQL on behavioural
	// evidence alone (a silent client on a port no mock names — see
	// inferEndpointDrift). It does two things: it keeps the
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
// endpoint has to pass — see inferEndpointDrift — but never replaces the
// silence check that precedes it.
func (r *mysqlPortRegistry) IsInferred(dstAddr string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.inferred[dstAddr]
	return ok
}

// MarkSessionStale says that the recording in force is about to be replaced, so
// the next set of mocks should supersede rather than accumulate. The agent is a
// single long-lived process and this registry is allocated once, so without it
// the gate on the drift inference would mean "any recording this process has
// ever replayed" rather than "the recording being replayed now" — a test set
// holding MySQL mocks would arm the inference for a later one that holds none,
// and a silent connection there would be routed to a replayer with nothing to
// serve.
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
		r.sessionStale = false
	}
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
	// sessions, so it would arm the inference in a replay whose recording
	// contains no MySQL at all.
	if recorded := p.mysqlPorts.MockPorts(); len(recorded) > 0 && !opts.DisableMysqlEndpointDrift {
		return p.inferEndpointDrift(ctx, srcConn, dstAddr, port, recorded, logger, notMysql), nil
	}
	return &mysqlProbe{SrcConn: srcConn, Reason: "no-mysql-mock-for-port"}, nil
}

// inferEndpointDrift serves MySQL mocks on a port the recording never saw, when
// the client stays silent. A silent client is waiting to be greeted, and MySQL
// is the only server-speaks-first protocol keploy replays. Combined with "this
// recording contains MySQL mocks", a port that does not match is an
// environment difference — the replayed application was handed a different
// endpoint than it had at record time — not evidence that the connection isn't
// MySQL.
//
// Declining is not neutral. In replay keploy IS the server, so nothing else
// will ever send that greeting: the application blocks until its own timeout,
// every test touching that dependency reports status_code 0, and the whole
// recorded mock set goes unused while the recording is perfectly good.
//
// This is the one judgement in this file a clock makes. A client that has not
// spoken YET and a MySQL client waiting to be greeted look the same until one
// of them speaks, and replay has no upstream whose greeting could decide, so
// silence through the windows is taken as the signature. It never holds the
// client's bytes — every read returns the moment they arrive — and it is
// opt-out (disableMysqlEndpointDrift); pinning the port with mysqlPorts takes
// the fast path instead.
func (p *Proxy) inferEndpointDrift(
	ctx context.Context,
	srcConn net.Conn,
	dstAddr string,
	port uint32,
	recorded []uint32,
	logger *zap.Logger,
	notMysql func(reason string, src net.Conn) *mysqlProbe,
) *mysqlProbe {
	// Any client byte is a definitive negative and costs one read.
	clientBytes, err := readWithin(ctx, srcConn, clientSilenceWindow())
	if len(clientBytes) > 0 {
		return notMysql("client-spoke-first", replayConn(srcConn, clientBytes, logger))
	}
	if err != nil && !isTimeout(err) {
		// EOF or a real network error: nothing to detect. Hand the
		// connection back untouched and let the existing path produce
		// its usual diagnostics.
		return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}
	}
	logger.Debug("client silent past the probe window; treating as server-speaks-first",
		zap.String("dstAddr", dstAddr),
		zap.Uint32("port", port),
		zap.Duration("window", clientSilenceWindow()))

	// Confirm before acting, because the silence window is a weak signal on
	// its own — a slow TLS client can be quiet that long and only then send
	// its ClientHello, and answering that with a HandshakeV10 breaks a
	// connection that was working. A real MySQL client sends nothing at all
	// until it is greeted, so a second, longer wait separates the two almost
	// perfectly.
	//
	// Only the first connection to an endpoint pays it. After that the
	// silence check above is confirmation enough: it is what rejects a client
	// that speaks first, and it has already run. Re-paying the long wait on
	// every connection would add it to each one a pool opens — in the bundle
	// that motivated this change, 103 times. Keyed by endpoint, so a
	// different host on the same port number does not inherit the shortcut.
	if !p.mysqlPorts.IsInferred(dstAddr) {
		late, lateErr := readWithin(ctx, srcConn, serverGreetingWindow())
		if len(late) > 0 {
			// It spoke, just slowly. Hand the bytes back and let the normal
			// dispatch have it. The port is NOT cached as non-MySQL: this
			// connection was not MySQL, but the next one on this port still
			// deserves the same decision.
			logger.Debug("client spoke after the extended window; not inferring MySQL",
				zap.Uint32("port", port), zap.String("dstAddr", dstAddr),
				zap.Duration("window", serverGreetingWindow()))
			return &mysqlProbe{SrcConn: replayConn(srcConn, late, logger), Reason: "client-spoke-late"}
		}
		if lateErr != nil && !isTimeout(lateErr) {
			return &mysqlProbe{SrcConn: srcConn, Reason: "client-read-error"}
		}
	}
	// Bookkeeping only — this must not grant the fast path, or one inference
	// would route every later connection on this port to the MySQL replayer
	// without re-checking that the client is silent.
	if p.mysqlPorts.LearnInferred(dstAddr) {
		logger.Warn("serving MySQL mocks on a port the recording never saw: the replayed application connects to a different endpoint than it did at record time. Set mysqlPorts in keploy.yml to pin the mapping, or disableMysqlEndpointDrift: true if a non-MySQL dependency is being misread as MySQL here.",
			zap.Uint32("port", port),
			zap.String("dstAddr", dstAddr),
			zap.Uint32s("recordedPorts", recorded))
	}
	return &mysqlProbe{IsMySQL: true, SrcConn: srcConn, Reason: "inferred-endpoint-drift"}
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
