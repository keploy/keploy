// Package proxy handles all the outgoing network calls and captures/forwards the request and response messages.
// It also handles the DNS resolution mechanism.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/agent/captureloss"
	"go.keploy.io/server/v3/pkg/agent/proxy/cbshim"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type ParserPriority struct {
	Priority   int
	ParserType integrations.IntegrationType
}

// probeFanoutEnabled toggles [PROBE/proxy] / [PROBE/dial] structured
// logs via KEPLOY_PROBE_FANOUT=1. The flag is read once at first use
// and cached in an atomic so hot-path branches are a single load.
// Used to diagnose the parallel-TLS-fanout stall (SAP + Postgres
// concurrent handshakes) without polluting normal record runs.
var (
	probeFanoutOnce    sync.Once
	probeFanoutEnabled atomic.Bool
)

func probeOn() bool {
	probeFanoutOnce.Do(func() {
		if os.Getenv("KEPLOY_PROBE_FANOUT") == "1" {
			probeFanoutEnabled.Store(true)
		}
	})
	return probeFanoutEnabled.Load()
}

// probeProxy emits a [PROBE/proxy] log tagged with a stable conn id
// and phase name. Cheap to call when the probe is off (single atomic
// load + early return); zero allocations on the hot disabled path.
func probeProxy(logger *zap.Logger, phase string, connID int64, fields ...zap.Field) {
	if !probeOn() {
		return
	}
	base := []zap.Field{
		zap.String("probe", "proxy"),
		zap.String("phase", phase),
		zap.Int64("connID", connID),
		zap.Int64("ts_ns", time.Now().UnixNano()),
	}
	logger.Info("[PROBE/proxy]", append(base, fields...)...)
}

var defaultMysqlPorts = []uint32{3306, 4000}

func isMysqlPort(port uint32, configured []uint32) bool {
	ports := configured
	if len(ports) == 0 {
		ports = defaultMysqlPorts
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// probeDial emits a [PROBE/dial] log for upstream dial timing.
func probeDial(logger *zap.Logger, phase string, connID int64, addr string, durNs int64, fields ...zap.Field) {
	if !probeOn() {
		return
	}
	base := []zap.Field{
		zap.String("probe", "dial"),
		zap.String("phase", phase),
		zap.Int64("connID", connID),
		zap.String("addr", addr),
		zap.Int64("dur_ns", durNs),
		zap.Int64("ts_ns", time.Now().UnixNano()),
	}
	logger.Info("[PROBE/dial]", append(base, fields...)...)
}

type Proxy struct {
	logger *zap.Logger

	// learnTracker keeps what record's full round served in the running
	// test, for the learn step (package matcher/learn): a test begins
	// whenever its error capture window opens (openTestErrorCapture). Under
	// the proven switch the parsers find it on their connection's context.
	learnTracker *learn.Tracker
	// procZones reads the zone each application process writes a wall
	// clock of no zone in, from /proc, by the connection's origin PID
	// (learn.WithProcZone): its calls are learned and checked in it.
	procZones *learn.ProcZones

	// recordedH2 is the stored set's recorded HTTP/2 destinations, seeded
	// from the whole recording (SeedRecordedDestinations), for the ALPN a
	// TLS client is offered while the app starts (replayTargetHasHTTP2Mock).
	recordedH2 recordedHTTP2
	// seededDests is the stored set's seeded recorded destinations, which
	// every staging derives the MySQL ports from beside what it stages.
	seededDests atomic.Pointer[[]*models.Mock]

	IP4               string
	IP6               string
	Port              uint32
	IncomingProxyPort uint16
	DNSPort           uint32

	DestInfo     agent.DestInfo
	Integrations map[integrations.IntegrationType]integrations.Integrations

	// predials are the upstream connections dialled while their application's
	// handshake was held (handshake_hold.go), until handleConnection takes them.
	predials predials
	// real is the run's mock.real, under the proven switch: nil until the
	// CLI hands a list (realdeps.go). realTracker keeps what the run's REAL
	// connections did.
	real        atomic.Pointer[realState]
	realTracker realdeps.Tracker
	// realDialResolved, when set (tests), is told each time a failed
	// connect's mock.real standing is told (realDialFailed).
	realDialResolved func()
	// agentMode is the mode the agent was started in (--mode); with
	// GlobalPassthrough it decides what happens to a connection that arrives
	// while there is no session (no_session.go). noSessionDests are the
	// destinations such a connection has been logged for, and
	// noSessionPassthroughs the ones passed through, which Record and Mock
	// close once the session is set up. A session, once set, overrides
	// agentMode for every connection.
	agentMode             models.Mode
	noSessionDests        noSessionDests
	noSessionPassthroughs noSessionPassthroughs
	// failedDests rate-limits the log line for a dependency that refused a
	// held connection.
	failedDests failedDests
	// serverFirstSaid is the destinations keploy said, in this recording
	// session, it cannot record: a server-first protocol it relays
	// (relayServerFirst).
	serverFirstSaid saidOnce
	// serverFirstDests is the destinations found to speak a server-first
	// protocol: a later record connection to one, its port learned not to
	// be MySQL, is probed again rather than skipped (probeMysql), so it is
	// relayed if it still greets so, and dispatched as on main if it is
	// another service now. Kept for the agent's life, as the port cache is,
	// the oldest forgotten past its bound.
	serverFirstDests saidOnce
	// hold is the handshake hold's lifecycle (handshake_hold.go).
	hold holdState

	integrationsPriority []ParserPriority
	errChannel           chan error

	// mysqlPorts holds destination ports known to speak the MySQL wire
	// protocol beyond the configured/default list — learned from a real
	// handshake during record, and derived from the loaded mocks'
	// destAddr metadata during replay. See mysql_detect.go.
	mysqlPorts *mysqlPortRegistry
	// readNetns, when set, stands in for sharedHost's read of the network
	// namespace the application shares with the agent (tests).
	readNetns func() (own, gateways []netip.Addr)
	// dockerHostAddr, when set, stands in for containerkey.HostAddr in
	// sharedHost (tests).
	dockerHostAddr func(netip.Addr) bool
	// readDockerHostNames, when set, stands in for sharedHost's read of the
	// Docker host's names in /etc/hosts (tests).
	readDockerHostNames func() map[netip.Addr]string

	// activeTestErrors accumulates mock-not-found errors during active test
	// execution. The continuous drain goroutine routes errors here when non-nil.
	// When nil (no window), mock-not-found errors fall to pendingMockErrors and
	// only non-mock noise (OTel, health checks) is discarded — keeping the
	// errChannel drained either way.
	activeTestErrors atomic.Pointer[testErrorAccumulator]
	// testWindows counts each test window's opening and closing (the
	// misses windows above): each value names a window or the gap after
	// it, never another (a read with no window open starts a new gap), for
	// the events of a client's connection to tell whether they happened
	// within one (testScopeNow).
	testWindows atomic.Uint64
	// captureMu serializes the drain goroutine's routing decision (read
	// activeTestErrors, then add) against BeginTestErrorCapture / GetMockErrors
	// opening or closing the window. Without it, the goroutine could add a miss
	// to an accumulator that GetMockErrors has just swapped away, losing it.
	captureMu sync.Mutex
	// errDrainActive is true while the StartErrorDrain goroutine is running. It
	// gates the GetMockErrors flush-marker rendezvous (which needs that single
	// owner to consume the marker); when false we use the legacy direct sweep.
	errDrainActive atomic.Bool
	errDrainOnce   sync.Once
	// errDrainDone is closed when the StartErrorDrain goroutine exits. A miss
	// waits for the drain to take it, and GetMockErrors waits for the drain to
	// reach its flush marker, each until this closes: the events that end those
	// waits, in place of a deadline. Set before errDrainActive goes true, so a
	// reader that sees the drain active sees it.
	errDrainDone chan struct{}
	// missLoss is why a miss was lost since the last read (the first cause):
	// it could not be queued (the error drain had stopped, or never ran) or
	// its test's list was full. The next GetMockErrors then reports its read
	// incomplete (ErrUnmatchedCallsUnread), naming the cause, instead of
	// returning a list a miss is missing from.
	missLoss atomic.Pointer[string]
	// answersDropped counts the reports of background writes answered by
	// their kind that were dropped for room (noteAnswerDropped): no read is
	// incomplete for them, and each hundredth is said.
	answersDropped atomic.Int64
	// pendingLost is set, under captureMu, when a miss made with no test's
	// window open was dropped because pendingMockErrors was full. The next
	// window decides whose loss it is: a set's first discards those misses
	// (they are no test's), so it only says so, but under the proven switch,
	// where they are the set's (beforeSetMiss) and its read is incomplete
	// (missLoss); the next test's carries them, so its read is incomplete.
	pendingLost bool
	// missCutPending is set, under the proven switch, by a test set's Mock,
	// and taken by its first staging (SetMocksWithWindow from
	// models.BaseTime), which cuts what was missed until then
	// (cutMissesAtSetStart).
	missCutPending atomic.Bool
	// setStaged is the staging (MockManager.StagingEpoch) of the running
	// test set's first, which cut what was missed until then
	// (cutMissesAtSetStart), under the proven switch; 0 before any. A miss
	// whose call was marked in an earlier staging
	// (models.MockMismatchReport.MarkEpoch) is not the set's, and the drain
	// cuts it as the set's start did, however late it arrives
	// (markedBeforeSet).
	setStaged atomic.Uint64
	// pendingMockErrors retains mock-not-found errors that arrive while no test
	// capture window is active (activeTestErrors is nil) — e.g. between tests,
	// or when the replayer/agent predates the BeginTestErrorCapture wiring.
	// ContinueTestErrorCapture carries them into the next test's window;
	// BeginTestErrorCapture (a set's first test) discards and logs them, but
	// under the proven switch, where it carries them as the set's
	// (beforeSetMiss);
	// GetMockErrors returns them where no window is opened at all. Only
	// mock-not-found errors are retained (background noise — OTel, health
	// checks — is still discarded), and the buffer is bounded so it can't grow
	// unboundedly.
	pendingMockErrors testErrorAccumulator
	// session holds the single active session for this proxy.
	// Previously this was a sync.Map keyed by appID (always 0), which is
	// no longer needed since the proxy serves a single client-app.
	sessionMu   sync.RWMutex
	session     *agent.Session
	mockManager *MockManager

	// workerScope holds per-test-worker mock allowlists for parallel replay
	// (Design A). Key is the worker's self-reported PID (ScopeReq.Pid); value is
	// the set of per-test mock names that worker's current test is allowed to
	// see. An outgoing call's origin PID (OutgoingOptions.SrcPid) is resolved up
	// the /proc tree to the nearest registered ancestor, and its per-test view
	// is intersected with that worker's set — so concurrent workers never stomp
	// each other's pool. Empty/absent ⇒ the worker serves the whole pool
	// (backward compatible with suite-level and single-worker sequential scope).
	workerScopeMu sync.RWMutex
	workerScope   map[uint32]workerScopeEntry
	// scopeSeq counts every change to the worker scopes and the mapped
	// universe, and every scoped view made. workerScopeAt is its value when
	// each worker's scope last changed (began or ended a test), universeAt
	// when the universe did: a view's version (see workerView). Guarded by
	// workerScopeMu.
	scopeSeq      uint64
	workerScopeAt map[uint32]uint64
	universeAt    uint64
	// mappedUniverse is the union of every test's mapped mock names; lets a
	// scoped worker distinguish "another test's mock" from an unmapped shared
	// recording. Guarded by workerScopeMu.
	mappedUniverse map[string]struct{}
	// retained are, by worker scope, the per-test recordings of stateful
	// dependencies the scope was served and kept for its repeats (cursor
	// mocks; scopedMockDb.MarkMockAsUsed). Consumed when the scope ends
	// (endRetained), so no other test is served them. Guarded by
	// workerScopeMu.
	retained map[string][]retainedUse

	// asyncEngine, when non-nil (config.Async.Lanes non-empty), tracks
	// async-lane replay state: registered parsers and the per-test
	// position counter consumed by Engine.Decide's anchor gating. The
	// "first window doesn't count as a completed test" rule is owned by
	// Engine.AdvanceWindow, so this struct keeps no async bookkeeping.
	asyncEngine *async.Engine

	// sessionResolver, when set, maps a connection's owning TGID to the
	// session that should handle it. It is the seam by which an external
	// composer (the enterprise multi-app agent) routes per-app traffic
	// without this package knowing what an "app" is. nil ⇒ single-session
	// mode: GetSessionFor falls back to the single session field above.
	// Guarded by sessionMu.
	sessionResolver func(tgid uint32) *agent.Session

	synchronous bool

	connMutex *sync.Mutex
	ipMutex   *sync.Mutex
	// channel to mark client connection as closed
	clientClose chan bool

	// activeConns tracks outgoing handleConnection goroutines so
	// shutdown can wait for them to finish naturally (drain grace)
	// before tearing listeners down. Count-only — connections close
	// themselves via their own defers / context cancellation; we do
	// not need references to force-close them.
	activeConns sync.WaitGroup

	// losses counts the calls the proxy intercepted and could not record or
	// mock (CaptureLosses).
	losses captureloss.Counter
	// caTrust is the client processes that sent a call through keploy's
	// certificate in this run, whose TLS hang-ups are cancels, not losses,
	// and the hang-ups held for a call of the others' (clientTLS).
	caTrust caTrust
	// accepts counts every connection the listener has accepted
	// (Accepted). They are the connections the kernel counts only when the
	// listener is the one the hooks marked: acceptsLn, set by listen while
	// it is open, or acceptsErr saying why it is not (accepts.go). Both
	// under acceptsMu.
	accepts    acceptcount.Counter
	acceptsMu  sync.Mutex
	acceptsLn  net.Listener
	acceptsErr error
	// listenerCounter is the hooks' count of the listener's connections
	// (SetListenerCounter); nil under the switch off. early holds what the
	// canary accepted before its own connection, for the accept loop, until
	// the proxy stops (closeEarly), under earlyMu: the loop's last round can
	// still be taking one as the stop closes them.
	listenerCounter agent.ListenerCounter
	earlyMu         sync.Mutex
	early           []net.Conn
	// handingOver, a test's (nil otherwise), is called by an accept round
	// with a kept connection it is about to hand over, before it does.
	handingOver func(net.Conn)
	// pairs holds each accepted connection's pairing (countAccept) until
	// handleConnection takes its destination (pairedDest).
	pairs sync.Map
	// untrackedConns counts the connections the proxy could not route
	// (untracked.go).
	untrackedConns atomic.Uint64
	// paired holds the paired connections until the proxy is done with
	// them, for the bytes a test's end waits for (pairbytes.go).
	paired pairedConns

	Listener net.Listener

	// inflight keeps, under the proven switch or for an agent that settles
	// its reads (settleReads), every accepted connection, so a test's end, or
	// a read of its misses, can tell when none holds its calls (inflight.go).
	inflight inflight
	// settleReads is the agent's --settle-reads (models.SetupOptions.
	// SettleReads), set at its start: a run that learns noise has its reads of
	// a test's misses settle the test's calls first, with the switch off too.
	settleReads bool
	// countedFromStart: once the canary had run, before the proxy said it
	// was ready, the kernel counted the connections to its listener and the
	// bytes on its paired connections could be read (decideCounted). With
	// the switch off it drops to false, for good, at the first connection
	// the kernel tied to none of the application's (dropCounting).
	countedFromStart atomic.Bool
	// countingLost is why, with the switch off, the run's calls stopped
	// being counted (dropCounting); nil while they are. countingLostCh is
	// closed then.
	countingLost   atomic.Pointer[error]
	countingLostCh chan struct{}

	// nodataRelayLogged dedupes the "relayed NODATA" Info log to once per
	// (name,qtype): glibc fires A+AAAA for essentially every hostname and every
	// IPv4-only cluster service yields an AAAA NODATA here, so an un-deduped
	// Info line would run on almost every resolution. See resolveUncachedDNSResponse.
	//
	// Bounded + TTL-expiring (not a sync.Map): the key is app-controlled, so a
	// client that resolves per-request hostnames — or a k8s search-domain setup
	// fanning one lookup into many qualified names — would otherwise grow this
	// forever in a long-lived agent. Allocated once in New and purged, never
	// replaced, by ResetRecordedDNSMocks.
	nodataRelayLogged *expirable.LRU[string, bool] // keyed by generateCacheKey

	// The DNS servers, set by startDNSServers before anything serves or
	// stops them.
	tcpDNS, udpDNS *dnsServer

	//to store the nsswitch.conf file data
	nsSwitchMutex             sync.Mutex
	nsswitchData              []byte // in test mode we change the configuration of "hosts" in nsswitch.conf file to disable resolution over unix socket
	GlobalPassthrough         bool
	OpportunisticTLSIntercept bool
	// DisableHandshakeHold mirrors config.Agent.DisableHandshakeHold: accept
	// every redirected connection at once (handshake_hold.go).
	DisableHandshakeHold bool
	IsDocker             bool

	// EnableIPv6Redirect mirrors config.Agent.EnableIPv6Redirect. When
	// true (the default), the synthetic DNS fallback answers AAAA
	// queries with ::1 so clients can reach the proxy via the IPv6
	// loopback that the BPF cgroup program redirects. When false, AAAA
	// is returned empty to preserve the legacy v4-only behaviour.
	EnableIPv6Redirect bool

	// appPID and caJavaHome are captured at proxy-construction time from
	// config.Agent (ClientNSPID + CAJavaHome) and passed to
	// pTls.SetupCAForApp so the Java keystore import targets the JDK the
	// instrumented application is actually running with — not the
	// arbitrary JDK first on PATH. On non-Java, Windows, or shared-volume
	// paths these are ignored. See pkg/agent/proxy/tls/java_detect.go for
	// the resolution order and rationale.
	appPID uint32
	// appPIDNS is the pid namespace the CLI said appPID is its pid in
	// (config.Agent.ClientPIDNS): the JAVA_HOME auto-detect reads
	// /proc/<appPID> only where that names the CLI (javaHomePID).
	appPIDNS   string
	mockMode   bool
	live       LiveHandler
	treeMu     sync.Mutex
	tree       map[net.Conn]int
	caJavaHome string
	// topology is told, under the proven switch, where the run's calls to
	// local listeners went (SetTopologySink).
	topology atomic.Pointer[topologyBox]

	// selfCalls counts the self-calls passed through, and selfCallOwners the
	// processes of the run that served them: the run's first is logged at
	// INFO, the rest at Debug, and their total when the run ends
	// (logSelfCalls, from SetGracefulShutdown). selfCallsLogged is the total
	// last logged, so a repeated shutdown call does not say it again.
	selfCalls       atomic.Int64
	selfCallOwners  sync.Map // owner pid -> struct{}
	selfCallsLogged atomic.Int64
	// unnamedConns counts the connections to a listener of the run's own
	// tree whose caller the agent could not name in its PID namespace
	// (callerOf), so that it could not tell whether a test made the calls
	// they carried. The relay does not read those calls, so a connection
	// counts once however many it carries. The first is logged at WARN as
	// it comes, and the total when the run ends (logSelfCalls).
	// unnamedConnsLogged is the total last logged.
	unnamedConns       atomic.Int64
	unnamedConnsLogged atomic.Int64

	// dnsCache is a TTL-expiring, size-bounded LRU cache for DNS responses.
	dnsCache *expirable.LRU[string, dnsCacheEntry]

	// recordedDNSMocks tracks DNS queries that have already been recorded
	// to avoid recording duplicate mocks. Key format: "name:qtype:qclass"
	// Uses bounded LRU with TTL to prevent unbounded memory growth.
	recordedDNSMocks *expirable.LRU[string, bool]

	// dnsUpstreamServers is the list of upstream DNS resolver IPs
	// captured from /etc/resolv.conf ONCE at proxy startup, BEFORE the
	// DNS server begins listening on p.DNSPort. This snapshot preserves
	// the original (pre-keploy) cluster resolver addresses (typically
	// CoreDNS in a Kubernetes pod) so that on a mock miss the
	// userspace DNS handler can forward queries upstream and return a
	// real answer for cluster-internal names (e.g.
	// `mysql.svc.cluster.local`) instead of the proxy's 127.0.0.1
	// fallback — which would cause the app to attempt DB connections
	// against the wrong IP.
	//
	// Captured via dns.ClientConfigFromFile. Empty when the file is
	// missing or unparseable; the forwarder falls back to the legacy
	// synthetic response in that case.
	dnsUpstreamServers []string
	// dnsUpstreamPort is the port read alongside dnsUpstreamServers.
	// Defaults to "53" when resolv.conf does not specify one.
	dnsUpstreamPort string
	// dnsInNetworkServers are the dnsUpstreamServers inside the replay's own
	// network (inNetworkResolvers), the only ones a replay's DNS mock miss is
	// forwarded to without an opt-in. Captured with dnsUpstreamServers.
	dnsInNetworkServers []string
	// dnsClusterDomain is an in-cluster agent's cluster DNS domain
	// (clusterDomain, e.g. cluster.local), empty elsewhere: names under it
	// are the replay network's own (networkOwnsName). Captured with
	// dnsUpstreamServers.
	dnsClusterDomain string
	// netNames holds the names a docker-mode replay's networks answer
	// themselves (networkOwnsName); nil is the agent process's own
	// (netnames.Default), which its routes fill.
	netNames *netnames.Store
	// allowEgressDNS is test.allowEgress naming models.AllowEgressDNS: a
	// replay's DNS mock miss may then be forwarded to every dnsUpstreamServers
	// entry, the host's and public resolvers included.
	allowEgressDNS bool
	// lastSessionEgress is what the last replay session's options opened
	// (sessionEgressKnobs), so a session that opens the same is not logged
	// again. Guarded by egressKnobsMu.
	lastSessionEgress []egressKnob
	egressKnobsMu     sync.Mutex
	// dnsSearch is the resolv.conf search list, kept so a query can be
	// recognised as a redundant search expansion without asking upstream.
	// A resolver probes name.S for each search domain S — before the name
	// absolutely when it carries fewer than ndots dots (5 in Kubernetes),
	// after otherwise — so a name that already ends in S is probed as
	// name.S.S. That shape is decidable from the name alone, whatever
	// upstream says, and nothing answers it, so capture never recorded it.
	dnsSearch []string
	// dnsForwardTimeout caps how long a single upstream Exchange is
	// allowed to block. Short by design — a flaky resolver must never
	// stall the app's DNS lookup. On timeout we fall through to the
	// legacy default/NXDOMAIN behaviour with a clear log line.
	dnsForwardTimeout time.Duration

	// isGracefulShutdown indicates the application is shutting down gracefully
	// When set, connection errors should be logged as debug instead of error
	isGracefulShutdown atomic.Bool

	auxiliaryHook agent.AuxiliaryProxyHook
	// teardownOnce guards runAuxTeardown so the auxiliary hook's
	// BeforeTeardown runs at most once across start()'s mutually-exclusive
	// shutdown paths (skipListener return / listener defer).
	teardownOnce sync.Once
	// auxMu orders the auxiliary hook's start (AfterStart) against its
	// teardown (runAuxTeardown): a teardown waits for a start in flight, and
	// no start begins once the teardown has run (auxTornDown). A stop that
	// came while StartProxy ran had the teardown run first, and the start
	// that followed attached what nothing removed before the exit.
	auxMu       sync.Mutex
	auxTornDown bool

	// skipListener disables the TCP accept loop. When true, the proxy
	// does not bind a port. DNS, parser init, and session state still run.
	skipListener bool

	// pcapMu guards the pcap-lifecycle fields below. The lifecycle is:
	// startPacketCapture spins up the afpacket goroutines and
	// installs a fresh broadcaster; stopPacketCapture cancels, waits
	// on done, and clears the broadcaster. Subscribers join via
	// SubscribePcap and receive every captured frame fanned out from
	// the capture loop — there is NO on-disk pcap file. In the k8s
	// live-recording use case the session never ends, so "fetch on
	// stop" is the wrong model; the recorder holds a long-lived
	// HTTP stream from the moment Record() is called.
	pcapMu          sync.Mutex
	pcapCancel      context.CancelFunc
	pcapDone        chan struct{}
	pcapBroadcaster *pcapBroadcaster

	// recordBufferCap and recordBufferQueueSize mirror
	// config.Record.RecordBuffer at proxy-construction time. Snapshotted
	// here (instead of plumbing the full *config.Config through every
	// per-connection helper) because these are read once per relay
	// construction in recordViaSupervisor and never mutated after the
	// proxy is built. Zero values fall through to the relay package's
	// own defaults via withDefaults().
	recordBufferCap       int64
	recordBufferQueueSize int

	// recordBufferStallGrace mirrors
	// config.Record.RecordBuffer.ConsumerStallGrace, snapshotted for the
	// same reason as the two above. Zero falls through to the relay
	// package's DefaultConsumerStallGrace via withDefaults().
	recordBufferStallGrace time.Duration

	// recordBufferHalfCloseGrace mirrors
	// config.RecordBuffer.HalfCloseGrace and is handed to every relay as
	// relay.Config.HalfCloseGrace. Zero means "use the relay default";
	// negative disables half-close, so it is NOT clamped to zero the way
	// recordBufferStallGrace is.
	recordBufferHalfCloseGrace time.Duration

	// capture follows the relays of the connections whose parser is
	// recording, for the recording's capture watermark
	// (capture_watermark.go). Added to the recording's manager once, as the
	// proxy starts (addCaptureWatermark).
	capture                *relay.Watermark
	captureWatermarkOnce   sync.Once
	removeCaptureWatermark func()

	// parserLag is relay.Config.ReaderLag for every recorded connection:
	// zero unless KEPLOY_DEBUG_PARSER_LAG is set (debugParserLag).
	parserLag time.Duration

	// cbshim is the eBPF-backed channel-binding shim. When non-nil,
	// upstream-TLS-handshake captures publish (mitm_hash, real_hash)
	// pairs into its BPF map so SCRAM-SHA-256-PLUS auth succeeds across
	// keploy's TLS MITM. Nil on OSS builds (no cbshim implementation
	// registered), or when the enterprise impl failed to construct
	// (missing CAP_BPF / older kernel / feature flag off); the proxy
	// keeps working for non-PLUS clients in that case.
	cbshim cbshim.CBShim

	// upstreamTLSVerifyCfg / upstreamTLSCACert are the RESOLVED
	// record.upstreamTls settings for this agent process: the precedence
	// (explicit --upstream-tls-* argv > keploy.yml record.upstreamTls >
	// default) is applied once in New, but nothing is read off disk there.
	//
	// The trust anchors themselves are loaded lazily, on the first RECORD
	// session (Proxy.Record → applyUpstreamTLSOptions), because replay never
	// verifies anything: Proxy.Mock deliberately does not stamp these options,
	// so a `keploy test` agent that happens to read a keploy.yml with
	// upstreamTls.verify set must not read the PEM, must not log "verification
	// is enabled", and must not LogError a bad caCert for a setting its mode
	// ignores.
	upstreamTLSVerifyCfg bool
	upstreamTLSCACert    string

	// upstreamTLSOnce guards the one-time load below. Record can be called
	// more than once per process (one session per /outgoing request), and the
	// PEM parse is ~150 roots — it must not be paid per session, and never on
	// the per-connection dial path.
	upstreamTLSOnce sync.Once

	// upstreamTLSVerify / upstreamTLSRootCAs are the post-load values stamped
	// onto every RECORD session's OutgoingOptions by applyUpstreamTLSOptions,
	// so the dial sites can read them straight off the options they already
	// carry. upstreamTLSVerify is upstreamTLSVerifyCfg minus any load failure.
	//
	// upstreamTLSRootCAs nil means "use Go's default" — crypto/tls falls back
	// to the platform root pool — and never "trust nothing"; see
	// pTls.LoadUpstreamRootCAs for that contract.
	upstreamTLSVerify  bool
	upstreamTLSRootCAs *x509.CertPool
	// upstreamTLSLoadFailed records that verification was ASKED FOR and the
	// trust anchors could not be built. It is distinct from
	// !upstreamTLSVerify ("never asked") for logging: in this state an
	// /outgoing request that asks to verify is refused with a message that
	// names the CA path, instead of silently verifying against roots that are
	// missing the operator's anchor.
	upstreamTLSLoadFailed bool

	// peekChunkWindow is how long one of the opportunistic-TLS relay
	// loop's reads or writes may block before the loop looks at its
	// context again (peekChunkTimeout); zero is opportunisticPeekChunkTimeout.
	// Only tests set it: a window longer than the test's own deadline
	// turns "the stop never waited a window out" into "the stop returned".
	peekChunkWindow time.Duration
}

// SetCBShim wires the eBPF channel-binding shim handle into the proxy.
// Typically called by the agent that owns the cbshim lifecycle, once
// per proxy instance, before StartProxy. Pass nil to disable.
//
// Also installs the tls.MITMPublishHook so CertForClient publishes the
// MITM half of every connection's cbshim rendezvous. Cleared (back to
// nil) when called with a nil cbshim.
func (p *Proxy) SetCBShim(c cbshim.CBShim) {
	p.cbshim = c
	if c == nil {
		pTls.SetMITMPublishHook(nil)
		return
	}
	pTls.SetMITMPublishHook(func(connID string, mitmDER []byte) {
		c.RegisterMITM(connID, mitmDER)
	})
}

// SetSkipListener disables the TCP accept loop.
func (p *Proxy) SetSkipListener(skip bool) {
	p.skipListener = skip
}

// Safe ranges for the user-tunable record buffer. Below the min the
// recorder drops on the first chunk for any non-trivial response;
// above the max the per-connection memory footprint becomes a footgun
// (with thousands of concurrent recorded connections, total RAM
// = max_conns × cap). Zero means "fall through to relay defaults"
// and is NOT clamped — that's the signal-to-defer path.
const (
	minRecordBufferCap   int64 = 1 << 20 // 1 MiB
	maxRecordBufferCap   int64 = 2 << 30 // 2 GiB
	minRecordBufferQueue       = 64
	maxRecordBufferQueue       = 1 << 16 // 65536

	// Safe range for ConsumerStallGrace. The floor keeps the grace long
	// enough that an ordinarily-scheduled parser is never mistaken for a
	// dead one on a loaded box. As above, zero is NOT clamped — it defers
	// to the relay.
	//
	// The ceiling is deliberately close to the 2s default rather than
	// generous. The wait this feeds (relay/tee.go, the deliver select) has
	// no ctx.Done() case: its only exits are the parser taking a chunk, the
	// consumer being reported gone, and this grace expiring. So the value
	// set here is an UNINTERRUPTIBLE upper bound on how long one tee can sit
	// at teardown against a parser that is never coming back — neither
	// Ctrl-C nor relay cancellation shortens it. 10s is already 5x the
	// default and far past any real parser's catch-up time, while staying
	// short enough that hitting it reads as a pause and not a hang.
	minRecordBufferStallGrace = 100 * time.Millisecond
	maxRecordBufferStallGrace = 10 * time.Second

	// dnsBindTimeout is the BACKSTOP for StartProxy's DNS-listener readiness
	// wait. A real bind error fails it at once (startDNSServers), so this only bounds the
	// pathological case of a socket that neither reports listening nor errors.
	// Generous enough never to false-fail a healthy start (a bind is a syscall,
	// near-instant) yet short enough that a truly stuck start surfaces as an
	// error instead of hanging the whole run.
	dnsBindTimeout = 30 * time.Second
)

// clampRecordBuffer validates user-supplied record-buffer values
// and clamps them to safe ranges, logging adjustments at Debug.
// Returns values suitable for relay.Config.PerConnCap and TeeChanBuf.
//
// Zero inputs pass through as zero (relay.withDefaults applies
// built-in defaults). Out-of-range values are clamped + logged at
// Debug, NOT rejected — record-path failure must never crash the
// agent. Debug-level here matches the rest of the record-buffer
// pipeline observability (see tee.drop, MarkMockIncomplete) so
// --debug surfaces the whole story end-to-end on a single switch
// without polluting normal-run logs with config-time noise.
func clampRecordBuffer(logger *zap.Logger, capBytes uint64, queue int) (int64, int) {
	var outCap int64
	switch {
	case capBytes == 0:
		outCap = 0
	case capBytes > math.MaxInt64 || int64(capBytes) > maxRecordBufferCap:
		// uint64 overflow into int64-negative is also caught here.
		logger.Debug("record-buffer maxMemoryPerConnection above safe maximum; clamping",
			zap.Uint64("requested", capBytes),
			zap.Int64("clamped", maxRecordBufferCap),
			zap.Int64("max", maxRecordBufferCap),
		)
		outCap = maxRecordBufferCap
	case int64(capBytes) < minRecordBufferCap:
		logger.Debug("record-buffer maxMemoryPerConnection below safe minimum; clamping",
			zap.Uint64("requested", capBytes),
			zap.Int64("clamped", minRecordBufferCap),
			zap.Int64("min", minRecordBufferCap),
		)
		outCap = minRecordBufferCap
	default:
		outCap = int64(capBytes)
	}

	var outQueue int
	switch {
	case queue == 0:
		outQueue = 0
	case queue > maxRecordBufferQueue:
		logger.Debug("record-buffer queueSize above safe maximum; clamping",
			zap.Int("requested", queue),
			zap.Int("clamped", maxRecordBufferQueue),
			zap.Int("max", maxRecordBufferQueue),
		)
		outQueue = maxRecordBufferQueue
	case queue < minRecordBufferQueue:
		logger.Debug("record-buffer queueSize below safe minimum; clamping",
			zap.Int("requested", queue),
			zap.Int("clamped", minRecordBufferQueue),
			zap.Int("min", minRecordBufferQueue),
		)
		outQueue = minRecordBufferQueue
	default:
		outQueue = queue
	}

	return outCap, outQueue
}

// clampConsumerStallGrace validates the operator-supplied teardown grace
// against the safe range, warning and clamping rather than failing. Kept
// separate from clampRecordBuffer instead of widening that function's
// signature: the two are read at the same call site but are otherwise
// unrelated knobs, and a (int64, int, time.Duration) return is harder to
// read at the call site than two named calls.
//
// Zero is passed through untouched — it is the "defer to the relay's
// default" signal, not a value to validate. A negative value is treated
// the same way rather than clamped up: relay.withDefaults() maps <= 0 to
// DefaultConsumerStallGrace, so deferring keeps one definition of the
// default instead of duplicating it here.
func clampConsumerStallGrace(logger *zap.Logger, grace time.Duration) time.Duration {
	switch {
	case grace <= 0:
		return 0
	case grace > maxRecordBufferStallGrace:
		logger.Debug("record-buffer consumerStallGrace above safe maximum; clamping",
			zap.Duration("requested", grace),
			zap.Duration("clamped", maxRecordBufferStallGrace),
			zap.Duration("max", maxRecordBufferStallGrace),
		)
		return maxRecordBufferStallGrace
	case grace < minRecordBufferStallGrace:
		logger.Debug("record-buffer consumerStallGrace below safe minimum; clamping",
			zap.Duration("requested", grace),
			zap.Duration("clamped", minRecordBufferStallGrace),
			zap.Duration("min", minRecordBufferStallGrace),
		)
		return minRecordBufferStallGrace
	default:
		return grace
	}
}

// isNetworkClosedErr checks if the error is due to a closed network connection.
// This includes broken pipe, connection reset by peer, and use of closed network connection errors.
// These errors are expected during graceful shutdown and should not be logged as errors.
func isNetworkClosedErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		// Windows-specific error patterns
		strings.Contains(errStr, "wsarecv") ||
		strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host")
}

// isPostgresSSLRequest reports whether the first 8 bytes of a connection
// are a Postgres SSLRequest packet:
//
//	int32 length = 8
//	int32 code   = 80877103 (0x04D2162F)
//
// This is sent by libpq / pgx / pq when `sslmode` is prefer/require/
// verify-ca/verify-full, BEFORE the TLS handshake begins. The server is
// expected to reply with a single byte: 'S' (accept TLS) or 'N' (refuse).
func isPostgresSSLRequest(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00 && b[3] == 0x08 &&
		b[4] == 0x04 && b[5] == 0xd2 && b[6] == 0x16 && b[7] == 0x2f
}

// isPostgresSSLRequestPrefix reports whether the first 5 bytes of a
// connection match the Postgres SSLRequest packet's prefix (length=8
// + first byte of the 80877103 code). This is the widest prefix we
// can detect after a 5-byte Peek — used to decide whether it's worth
// blocking on a second Peek(8) to verify the full signature. The
// prefix 00 00 00 08 04 does not collide with TLS (0x16 ...), any
// printable-ASCII protocol greeting, or any PG protocol message
// other than SSLRequest, so this check is effectively exact.
func isPostgresSSLRequestPrefix(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00 && b[3] == 0x08 && b[4] == 0x04
}

// dialPostgresSSLUpstream opens a Postgres-aware TLS connection to
// addr: plain-TCP dial, write the 8-byte SSLRequest, read the
// 1-byte 'S'/'N' response, upgrade to TLS via tls.Client only on 'S'.
// A real Postgres server on 5432 expects this sequence; tls.Dial
// would send a ClientHello directly and the server would reject it.
//
// Used by the proxy when agent.InterceptPostgresSSLRequest is set
// and the destination port is 5432. The dial deadline is borrowed
// from the passed-in context so a hung upstream does not block the
// parser goroutine indefinitely; a 10 s default kicks in when the
// context has no deadline (typical for record/replay mode).
//
// connID is the app-side source-port string; if cb is non-nil the
// upstream peer cert is published via cb.RegisterReal so channel-
// binding substitution can rendezvous it with the MITM cert from
// CertForClient. Pass nil to disable cbshim publishing (unit tests).
func dialPostgresSSLUpstream(ctx context.Context, connID, addr string, cfg *tls.Config, logger *zap.Logger, cb cbshim.CBShim, pre *util.Predialed) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	dialer := net.Dialer{Deadline: deadline}
	rawConn, err := util.DialDestinationWith(ctx, logger, util.DialTarget{Addr: addr},
		func(ctx context.Context, a string) (net.Conn, error) {
			return util.DialRaw(ctx, &dialer, "tcp", a, pre)
		})
	if err != nil {
		return nil, fmt.Errorf("postgres SSL upstream: plain dial %s failed: %w", addr, err)
	}
	if err := rawConn.SetDeadline(deadline); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("postgres SSL upstream: set deadline: %w", err)
	}
	// SSLRequest: int32 length=8, int32 code=80877103 (0x04D2162F).
	sslRequest := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}
	if _, err := rawConn.Write(sslRequest); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("postgres SSL upstream: write SSLRequest to %s failed: %w", addr, err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(rawConn, reply); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("postgres SSL upstream: read SSLRequest reply from %s failed: %w", addr, err)
	}
	switch reply[0] {
	case 'S':
		// Server accepted TLS. Upgrade the existing socket and run
		// the handshake. Clear the deadline on the raw conn first —
		// tls.Handshake manages its own.
		if err := rawConn.SetDeadline(time.Time{}); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("postgres SSL upstream: clear deadline after 'S': %w", err)
		}
		tlsConn := tls.Client(rawConn, cfg)
		handshakeCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("postgres SSL upstream: TLS handshake after 'S' failed: %w", err)
		}
		logger.Debug("Postgres SSLRequest accepted by upstream; TLS established",
			zap.String("addr", addr),
			zap.String("protocol", tlsConn.ConnectionState().NegotiatedProtocol))

		// Publish the real upstream cert to the cbshim rendezvous.
		// If CertForClient has already registered the MITM half for
		// the same connID this triggers cbmap.Publish into the BPF
		// map; otherwise the MITM half will publish when it arrives.
		if cb != nil && connID != "" {
			state := tlsConn.ConnectionState()
			if len(state.PeerCertificates) > 0 {
				leaf := state.PeerCertificates[0]
				cb.RegisterReal(connID, leaf.Raw, leaf.SignatureAlgorithm)
			}
		}

		return tlsConn, nil
	case 'N':
		_ = rawConn.Close()
		return nil, fmt.Errorf("postgres SSL upstream %s refused TLS (SSLRequest reply 'N'); the client expects TLS but the upstream was built without it — enable SSL on the Postgres server or disable InterceptPostgresSSLRequest for this deployment", addr)
	default:
		_ = rawConn.Close()
		return nil, fmt.Errorf("postgres SSL upstream %s returned unexpected byte 0x%02x as SSLRequest reply (expected 'S' or 'N'); check that the destination is actually a Postgres server and not something else listening on 5432", addr, reply[0])
	}
}

// withDstIPOf marks ctx with the IP of addr (an ip:port, as destAddr makes it)
// for the MITM leaf of a client that sends no SNI (pTls.WithDstIP); ctx as it
// is when addr's host is no IP.
func withDstIPOf(ctx context.Context, addr string) context.Context {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ctx
	}
	return pTls.WithDstIP(ctx, net.ParseIP(host))
}

// resolveUpstreamServerName decides the ServerName keploy puts on its own
// TLS dial to the real destination, in descending order of trustworthiness:
//
//  1. capturedSNI — the name the application itself asked for: the SNI it
//     sent, read off its completed handshake with keploy, else its CONNECT
//     target. Authoritative when present.
//  2. connectTargetHost — the authority from the client's CONNECT request,
//     used only when it is a real hostname. IP literals are skipped here
//     for the historical reason the original fallback skipped them: as SNI
//     they are useless (crypto/tls omits SNI for IP literals anyway).
//  3. hostFromAddr(dstAddr) — the host keploy is actually dialling, applied
//     ONLY when verification is on.
//
// Step 2 serves only a caller that passes the bare SNI as capturedSNI. No
// caller does today: handleConnection passes the SNI else the CONNECT target
// (what the port map held for the connection before), so an IP-literal
// CONNECT target is returned by step 1; opportunistic TLS makes no CONNECT.
//
// Step 3 is what makes record.upstreamTls.verify usable at all. capturedSNI
// is empty whenever the client asked for no name: it made no CONNECT and sent
// no SNI, which it omits for every IP-literal destination (RFC 6066 forbids
// it), the dominant shape for databases in dev/CI/docker-compose. With
// verification on and ServerName empty, crypto/tls rejects the config
// outright ("either ServerName or InsecureSkipVerify must be specified")
// BEFORE it examines any certificate, so verification could never even be
// attempted on those connections. An IP literal is the correct value to
// supply, not a compromise: Go matches it against the certificate's IP SANs.
//
// Step 3 is deliberately scoped to the verifying path. With verification off
// ServerName's only remaining effect is the SNI extension on the wire, and
// dstAddr IS a hostname on the CONNECT path (it is overwritten with
// connectResult.TargetAddr), so applying step 3 unconditionally could put an
// SNI on the wire that the application itself never sent. The default path
// must stay byte-identical to its pre-upstreamTls behaviour, so it keeps the
// empty ServerName it has always had.
//
// Step 3 never returns "" for a non-empty dstAddr, and that totality is the
// point: hostFromAddr yields "" for an address with no host at all (":443" —
// net.SplitHostPort accepts it), which would put us right back in the config
// crypto/tls rejects before it examines any certificate. handleConnectTunnel
// now rejects a host-less CONNECT authority at the source, so that address
// shape should no longer reach here; this branch is the belt to that braces,
// and it fails LOUDLY (an x509 hostname error naming the target) instead of
// cryptically.
func resolveUpstreamServerName(capturedSNI, connectTargetHost, dstAddr string, verify bool) string {
	if capturedSNI != "" {
		return capturedSNI
	}
	// If SNI was not captured (e.g., client omitted it after CONNECT),
	// fall back to the CONNECT target hostname for the TLS handshake.
	// Skip IP literals — Go's TLS uses IP SANs, not ServerName for those.
	if connectTargetHost != "" && net.ParseIP(connectTargetHost) == nil {
		return connectTargetHost
	}
	if verify {
		if host := hostFromAddr(dstAddr); host != "" {
			return host
		}
		return dstAddr
	}
	return ""
}

// resolveUpstreamTLSConfig settles record.upstreamTls for this agent process.
//
// Precedence is flag > yaml > default, and it must resolve IDENTICALLY on a
// native run and inside docker. That is why argv wins outright rather than
// being OR-ed with the yaml value: an OR can only ever ADD verification, so
// `keploy record --upstream-tls-verify=false` against a keploy.yml carrying
// `record.upstreamTls.verify: true` could never switch it off on a native run
// (where the agent subprocess reads that same keploy.yml through --config-path)
// while it DID switch it off under docker (where the container never sees the
// file) — one config, two behaviours, for a flag whose whole purpose is to be
// able to turn a connection-breaking check back off.
//
// The orchestrator now forwards its own resolved value unconditionally as
// `--upstream-tls-verify=%t` (pkg/platform/http/agent.go, pkg/platform/docker),
// exactly like --disable-mapping, so for an orchestrator-spawned agent the
// argv value is always present and always authoritative. The *Set markers are
// what distinguishes "the orchestrator said false" from "the orchestrator said
// nothing", which a bare bool cannot express; they are tracked per flag so a
// hand-started `keploy agent --upstream-tls-verify` still picks up a caCert
// from its own keploy.yml.
func resolveUpstreamTLSConfig(opts *config.Config) (verify bool, caCert string) {
	verify = opts.Record.UpstreamTLS.Verify
	if opts.Agent.UpstreamTLSVerifySet {
		verify = opts.Agent.UpstreamTLSVerify
	}
	caCert = opts.Record.UpstreamTLS.CACert
	if opts.Agent.UpstreamTLSCACertSet {
		caCert = opts.Agent.UpstreamTLSCACert
	}
	return verify, caCert
}

// javaHomePID is the process the JAVA_HOME auto-detect reads
// (/proc/<pid>/environ and exe): the CLI's, where its pid names it in this
// agent's pid namespace (utils.ClientPIDHere); else 0, which leaves the
// detection to keytool on PATH, never another process's JDK.
func (p *Proxy) javaHomePID() uint32 {
	return utils.ClientPIDHere(p.appPID, p.appPIDNS).PID
}

// setupCAForApp is pTls.SetupCAForApp; a test swaps it to see the pid the
// JAVA_HOME auto-detect is given.
var setupCAForApp = pTls.SetupCAForApp

// setupCA installs keploy's CA for the app, the JAVA_HOME auto-detect reading
// the CLI's process only (javaHomePID).
func (p *Proxy) setupCA(ctx context.Context) error {
	return setupCAForApp(ctx, p.logger, p.IsDocker, int(p.javaHomePID()), p.caJavaHome)
}

func New(logger *zap.Logger, info agent.DestInfo, opts *config.Config) *Proxy {
	warnIfRemovedRelayKnobSet(logger)
	proxy := &Proxy{
		logger:                    logger,
		learnTracker:              learn.NewTracker(),
		Port:                      opts.ProxyPort,
		IncomingProxyPort:         opts.IncomingProxyPort,
		DNSPort:                   opts.DNSPort, // default: 26789
		synchronous:               opts.Agent.Synchronous,
		settleReads:               opts.Agent.SettleReads,
		countingLostCh:            make(chan struct{}),
		IP4:                       "127.0.0.1", // default: "127.0.0.1" <-> (2130706433)
		IP6:                       "::1",       //default: "::1" <-> ([4]uint32{0000, 0000, 0000, 0001})
		ipMutex:                   &sync.Mutex{},
		connMutex:                 &sync.Mutex{},
		DestInfo:                  info,
		clientClose:               make(chan bool, 1),
		Integrations:              make(map[integrations.IntegrationType]integrations.Integrations),
		GlobalPassthrough:         opts.Agent.GlobalPassthrough,
		allowEgressDNS:            models.AllowsEgress(opts.Agent.AllowEgress, models.AllowEgressDNS),
		DisableHandshakeHold:      opts.Agent.DisableHandshakeHold,
		OpportunisticTLSIntercept: opts.Agent.OpportunisticTLSIntercept,
		errChannel:                make(chan error, 100), // buffered channel to prevent blocking
		IsDocker:                  opts.Agent.IsDocker,
		EnableIPv6Redirect:        opts.Agent.EnableIPv6Redirect,
		appPID:                    opts.Agent.ClientNSPID,
		appPIDNS:                  opts.Agent.ClientPIDNS,
		mockMode:                  opts.Agent.MockMode,
		agentMode:                 opts.Agent.Mode,
		caJavaHome:                opts.Agent.CAJavaHome,
		dnsCache:                  newDNSCache(),
		recordedDNSMocks:          newRecordedDNSMocksCache(),
		nodataRelayLogged:         newNodataRelayLogCache(),
		mysqlPorts:                newMysqlPortRegistry(),
		// dnsForwardTimeout is the per-forward deadline for upstream DNS
		// exchanges. 2 s is long enough to absorb a single UDP retransmit
		// against CoreDNS (~500 ms default) while keeping app-side lookup
		// latency bounded if the upstream is hard-down. Tuned to match the
		// task spec ("~2s"). Tests override by assigning to this field
		// directly on the Proxy struct; see
		// pkg/agent/proxy/dns_forward_test.go (newProxyWithUpstream). The
		// field is package-private, so sibling _test.go files have
		// legitimate direct access and we intentionally do not expose a
		// setter helper for a one-line test-only override.
		dnsForwardTimeout: 2 * time.Second,
		// Record-buffer tuning is set by clampRecordBuffer below — it
		// validates the operator-supplied uint64/int values against
		// safe ranges (1 MiB-2 GiB / 64-65536 slots), warns + clamps
		// rather than crashing, and detects uint64 → int64 wrap from
		// values > math.MaxInt64 explicitly. Setting these here would
		// be redundant and would risk a transient negative cap from a
		// bare uint64 → int64 reinterpretation cast.
	}
	proxy.recordBufferCap, proxy.recordBufferQueueSize = clampRecordBuffer(
		logger,
		opts.Record.RecordBuffer.MaxMemoryPerConnection,
		opts.Record.RecordBuffer.QueueSize,
	)
	proxy.recordBufferStallGrace = clampConsumerStallGrace(
		logger,
		opts.Record.RecordBuffer.ConsumerStallGrace,
	)
	// Passed through unclamped: zero and negative both carry meaning
	// (relay default / disabled), and relay.withDefaults resolves them.
	proxy.recordBufferHalfCloseGrace = opts.Record.RecordBuffer.HalfCloseGrace

	proxy.capture = relay.NewWatermark()
	proxy.parserLag = debugParserLag(logger)

	// keploy mock's tests are a test harness's, which gives the application
	// its inputs and keploy no request: the learn step reads them so
	// (learn.Tracker.SetClientTests).
	proxy.learnTracker.SetClientTests(opts.Agent.MockMode)
	// The application runs beside the agent, and writes a wall clock of no
	// zone in its local zone, the agent's own, or in UTC
	// (learn.Tracker.SetAppZone), read with its name where the agent's TZ or
	// /etc/localtime gives one, so a learning that depends on it names it
	// (models.ClockOffset.AppZone). Where keploy cannot read it (a TZ of a
	// POSIX rule, which Go reads as UTC; a link to no zone file; a zone file
	// whose name and rules disagree, as where the host's /etc/localtime is
	// bind-mounted over the link to the zone, or keploy cannot read its
	// mounts: learn.ReadZone), Go's own reading, and no learning names it
	// (SetAppZoneUnnamed), as before.
	tz, tzSet := os.LookupEnv("TZ")
	if loc, err := learn.ReadZone("/", tz, tzSet); err == nil {
		proxy.learnTracker.SetAppZone(loc)
	} else {
		logger.Debug("keploy cannot read the agent's zone; a learning that depends on the application's zone names no zone, and a replay reads its own", zap.Error(err))
		proxy.learnTracker.SetAppZoneUnnamed(time.Local)
	}
	proxy.procZones = learn.NewProcZones("/proc")
	proxy.sayLearnWaits()

	// Upstream TLS verification (record.upstreamTls). Only the PRECEDENCE is
	// settled here; nothing is read off disk (see upstreamTLSOnce).
	proxy.upstreamTLSVerifyCfg, proxy.upstreamTLSCACert = resolveUpstreamTLSConfig(opts)

	if len(opts.Async.Lanes) > 0 {
		// Parser instances are attached in InitIntegrations via AsyncAware;
		// start with an empty parser map (RegisterParser fills it there).
		proxy.asyncEngine = async.NewEngine(logger, opts.Async.Lanes, map[string]async.AsyncParser{})
	}

	// Plumb the proxy logger into the package-singleton SyncMockManager
	// so its drop-path Error emissions actually reach the host logger.
	// zap.L() would silently fall back to Nop here — syncMock loads at
	// package init, long before any zap.ReplaceGlobals call — which is
	// how the overflow warning became invisible on customer runs.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetLogger(logger)
	}

	// Channel-binding shim: opt-in BPF-backed feature. Gate by the
	// config flag AND on a registered factory — OSS builds have no
	// factory and skip this branch entirely; enterprise builds
	// register a factory via init() but still respect the flag, which
	// defaults to false. Failure to construct (missing CAP_BPF, older
	// kernel without uprobe support, bpf_probe_write_user disabled by
	// the kernel hardener) is logged at Info and the proxy keeps
	// working for non-SCRAM-PLUS clients. The cbshim is nil-safe at
	// every consumer site, so a nil instance silently disables the
	// channel-binding fix without affecting anything else.
	// proxy.New runs in one of two contexts:
	//   - Docker / docker-compose: a dedicated `keploy agent`
	//     subprocess receives --channel-binding-shim via argv (set
	//     by the orchestrator from docker.go's command builder) and
	//     populates cfg.Agent.ChannelBindingShim.
	//   - Native (--cmd-type native): the orchestrator's own process
	//     runs the proxy; only cfg.Record.ChannelBindingShim is set
	//     by the CLI flag / keploy.yml.
	// Honour either — same pattern other propagation-via-argv flags
	// like CapturePackets use (read from whichever the current
	// process saw populated).
	if opts != nil && (opts.Agent.ChannelBindingShim || opts.Record.ChannelBindingShim) {
		if cb, err := cbshim.NewFromFactory(logger); err == nil && cb != nil {
			proxy.SetCBShim(cb)
			logger.Info("cbshim: BPF-backed channel-binding shim enabled")
		} else if err != nil {
			logger.Info("cbshim: factory returned an error — SCRAM-SHA-256-PLUS "+
				"clients will fail across the MITM as today",
				zap.Error(err),
				zap.String("next_step", "verify the agent has CAP_BPF + CAP_PERFMON (or CAP_SYS_ADMIN on older kernels), the kernel is >= 5.5 with bpf_probe_write_user permitted (not disabled by lockdown / hardened images), and clang/llvm BPF support is available. To opt out of cbshim entirely, set record.channelBindingShim: false (or unset) in keploy.yml; non-PLUS postgres clients work without it."))
		} else {
			logger.Info("cbshim: no implementation registered (OSS build) — "+
				"SCRAM-SHA-256-PLUS clients will fail across the MITM as today",
				zap.String("next_step", "this build does not include a cbshim implementation; the OSS binary cannot intercept SCRAM-SHA-256-PLUS across the TLS MITM. Run an enterprise build (which registers a cbshim factory at init()) to get the feature, or set record.channelBindingShim: false to silence this log if non-PLUS clients are sufficient."))
		}
	}

	return proxy
}

// warnIfRemovedRelayKnobSet tells an operator that KEPLOY_NEW_RELAY no longer
// does anything.
//
// It was the global V2 rollback: set it to off/0/false/no and every V2-capable
// parser was forced onto the legacy record path. That knob is gone — every
// parser the dispatcher can route to record implements IntegrationsV2, and the
// legacy path it selected hangs EOF-delimited peers because it waits for both
// copy directions instead of tearing the second down.
//
// Silence would be the dangerous outcome: someone reaches for a documented
// rollback during an incident, sets it, restarts, sees no error, and believes
// they have rolled back. Say so once, at startup, and name the lever that does
// still work.
func warnIfRemovedRelayKnobSet(logger *zap.Logger) {
	if logger == nil {
		return
	}
	v, ok := os.LookupEnv("KEPLOY_NEW_RELAY")
	if !ok {
		return
	}
	logger.Warn("KEPLOY_NEW_RELAY is set but has been REMOVED; it is ignored",
		zap.String("value", v),
		zap.String("impact", "no rollback is in effect — every parser records through the supervisor + relay path, which is what it would have done without this variable"),
		zap.String("next_step", "unset KEPLOY_NEW_RELAY. To disable parsing during an incident use KEPLOY_DISABLE_PARSING=1 (or SIGUSR1 on Unix, or the admin endpoint), which routes new connections to raw passthrough"),
	)
}

// shouldRecordViaSupervisor is the single decision for "does this parser record
// through the supervisor + relay, or through the legacy path".
//
// Extracted so the rule is testable. It was previously written inline at three
// dispatch sites, and two of them simply omitted it — the generic catch-all and
// the MySQL probe branch both recorded legacy on the default configuration with
// no flag set. Source-grep fences over those call sites proved nothing: an
// inverted condition keeps every asserted substring and stays green.
func shouldRecordViaSupervisor(parser integrations.Integrations) bool {
	v2, ok := parser.(integrations.IntegrationsV2)
	return ok && v2.IsV2()
}

// recordMySQLOutgoing dispatches a record-mode MySQL connection that
// arrived through the PROBE branch, to either the V2 supervisor path or
// the legacy one.
//
// It is a method rather than an inlined block so the dispatch decision is
// reachable from a test. Inlined, it sat ~250 lines into handleConnection
// behind an eBPF-resolved destination and a live greeting probe, so no
// test could assert which surface the parser was handed — reverting the
// gate entirely left the whole suite green, and a source-scanning pin was
// defeated by `if false &&` or by negating the condition.
//
// tlsUpgrader is a PARAMETER rather than built here, and that is
// load-bearing. util.NewConnTLSUpgrader keeps the &srcConn/&dstConn
// pointers it is given and writes the upgraded *tls.Conn back through
// them, so that handleConnection's deferred close acts on the TLS conn
// and sends close_notify. Constructing it here would alias this
// function's PARAMETER COPIES: the legacy MySQL STARTTLS path
// (recorder/conn.go UpgradeClientTLS/UpgradeDestTLS) would still
// upgrade, but the caller's variables would keep pointing at the raw
// sockets and the deferred close would tear them down without
// close_notify. proxy.go's matched-parser site carries the same warning.
//
// The error handling deliberately mirrors the two sibling supervisor
// dispatch sites: a network-closed error is logged at Debug rather than
// as a failure, but is still returned, so all three sites answer the same
// way for the same condition.
func (p *Proxy) recordMySQLOutgoing(
	parserCtx context.Context,
	srcConn, dstConn net.Conn,
	mocks chan<- *models.Mock,
	parserErrGrp *errgroup.Group,
	mysqlLogger *zap.Logger,
	clientConnID, destConnID int64,
	outgoingOpts models.OutgoingOptions,
	tlsUpgrader models.TLSUpgrader,
) error {
	// The record-mode parsing kill switch. The matched-parser path has its own
	// gate far below in handleConnection, but the MySQL PROBE branch returns
	// before ever reaching it — so without this check KEPLOY_DISABLE_PARSING /
	// SIGUSR1 / the admin endpoint silently did NOTHING for MySQL on its normal
	// route. probeMysql routes MySQL on MysqlPorts (3306 and 4000 by default)
	// here, so the probe branch IS the common MySQL path: an operator set the
	// switch, restarted, and every MySQL connection was still parsed.
	//
	// Gated here rather than at the call site so it covers every caller of this
	// dispatch function and is reachable from a test. The probe's connections
	// are adopted unconditionally by the caller, so they already carry any
	// bytes it consumed.
	//
	// util.RelayRawPassthrough, NOT globalPassThrough, for the reason
	// relayDeclinedConn spells out below: globalPassThrough never calls
	// CloseWrite and returns on io.EOF, so a client that does shutdown(SHUT_WR)
	// — the end-of-request signal for every EOF-delimited exchange — has its
	// connection torn down instead of receiving the reply. Reproduced on this
	// exact path before the switch: the app half-closed and read back nothing.
	// The probe's fast path admits a port listed in MysqlPorts by PORT alone
	// (no content inspection), so any protocol on those ports reaches here,
	// including half-closing ones.
	if util.DefaultKillSwitch.Enabled() {
		mysqlLogger.Debug("record-mode parsing kill switch tripped; relaying raw",
			zap.String("parser", string(integrations.MYSQL)),
			zap.String("dispatch_site", "mysql-probe"))
		util.RelayRawPassthrough(srcConn, dstConn)
		return nil
	}

	mysqlParser := p.Integrations[integrations.MYSQL]
	if shouldRecordViaSupervisor(mysqlParser) {
		if err := p.recordViaSupervisor(parserCtx, srcConn, dstConn, mysqlParser,
			integrations.MYSQL, mocks, parserErrGrp, mysqlLogger,
			clientConnID, destConnID, outgoingOpts); err != nil {
			if isNetworkClosedErr(err) {
				mysqlLogger.Debug("V2 record path: connection closed", zap.Error(err))
			} else {
				utils.LogError(mysqlLogger, err, "V2 record path failed for the MySQL probe branch",
					zap.String("next_step", "set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough) while investigating"),
				)
			}
			return err
		}
		mysqlLogger.Debug("V2 record path returned", zap.String("ParserType", string(integrations.MYSQL)))
		return nil
	}

	// A probe verdict of IsMySQL does not imply the parser is registered
	// (the configured-port and known-port verdicts never check), so the
	// legacy path would nil-panic on an unregistered build.
	if mysqlParser == nil {
		// This is a decline, not a failure: relay rather than letting the
		// caller's deferred close drop the connection.
		declined := fmt.Errorf("no MySQL parser is registered: %w", integrations.ErrParserDeclined)
		return p.relayDeclinedConn(parserCtx, declined, mysqlLogger, integrations.MYSQL,
			mysqlSessionForRelay(srcConn, dstConn, mysqlLogger))
	}

	mysqlSession := &integrations.RecordSession{
		Ingress:      util.NewSafeConn(srcConn, mysqlLogger),
		Egress:       util.NewSafeConn(dstConn, mysqlLogger),
		Mocks:        mocks,
		ErrGroup:     parserErrGrp,
		TLSUpgrader:  tlsUpgrader,
		Logger:       mysqlLogger,
		ClientConnID: fmt.Sprint(clientConnID),
		DestConnID:   fmt.Sprint(destConnID),
		Opts:         outgoingOpts,
	}

	if err := mysqlParser.RecordOutgoing(parserCtx, mysqlSession); err != nil {
		p.countParserPanic(err, integrations.MYSQL, dstConn.RemoteAddr())
		// A decline is not a failure: relay rather than letting the caller's
		// deferred close drop a connection keploy merely chose not to parse.
		if errors.Is(err, integrations.ErrParserDeclined) {
			return p.relayDeclinedConn(parserCtx, err, mysqlLogger, integrations.MYSQL, mysqlSession)
		}
		utils.LogError(p.logger, err, "failed to record the outgoing message")
		return err
	}
	return nil
}

// mockMySQLOutgoing serves a replay-mode MySQL connection that arrived
// through the PROBE branch from the recorded mocks, and reports what the
// parser returns as a miss.
//
// It is a method rather than an inlined block for the reason
// recordMySQLOutgoing is: inlined, it sat behind an eBPF-resolved
// destination and a probe, so no test could check that a MySQL miss reaches
// the test's misses before the client's connection is closed.
//
// The order is load-bearing. The parser leaves the client's connection open
// when it returns, and handleConnection's deferred close shuts it after this
// returns: a returned miss is queued here (sendMockNotFoundError) before the
// application sees its connection fail, so a test that ends as soon as its
// database call fails still has its miss.
func (p *Proxy) mockMySQLOutgoing(parserCtx context.Context, srcConn net.Conn, dstAddr string, m integrations.MockMemDb, outgoingOpts models.OutgoingOptions) error {
	// Same registration hazard as the record path: a probe verdict of IsMySQL
	// does not imply the parser is registered (the configured-port and
	// known-port verdicts never check), so an unregistered build nil-panics
	// here instead of failing cleanly.
	mysqlMocker := p.Integrations[integrations.MYSQL]
	if mysqlMocker == nil {
		// Replay has no upstream to relay to, so a decline cannot be
		// rescued here the way it is on the record path; fail cleanly
		// rather than nil-panicking.
		utils.LogError(p.logger, nil, "no MySQL parser is registered",
			zap.String("next_step", "the MySQL probe matched but integrations.MYSQL is absent from p.Integrations; this is a build/registration bug"))
		return errors.New("mysql parser not registered")
	}

	// The replayer makes the client's TLS handshake itself, on its
	// SSLRequest: charged as the proxy's own client handshakes are.
	reportCtx := p.withClientTLS(parserCtx, dstAddr, srcProcess(outgoingOpts))
	mysqlDst := &models.ConditionalDstCfg{Addr: dstAddr}
	err := mysqlMocker.MockOutgoing(reportCtx, srcConn, mysqlDst, p.scopedFor(outgoingOpts.SrcPid, m), outgoingOpts)
	p.countParserPanic(err, integrations.MYSQL, dstAddr)
	if errors.Is(err, util.ErrParserPanicked) {
		// Counted as a capture loss just above; no miss (mockFailed).
		utils.LogError(p.logger, err, "failed to mock the outgoing message")
		return err
	}
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
		p.logger.Debug("mysql mock outgoing finished with error", zap.Error(err))
		p.sendMockNotFoundErrorFrom(err, outgoingOpts.SrcPid, mysqlDst)
		if provenswitch.On() && isMockMiss(err) {
			return reportedMiss{err}
		}
		return err
	}
	return nil
}

// mysqlSessionForRelay builds the minimal RecordSession the decline relay
// needs when there is no parser to build a full one for.
func mysqlSessionForRelay(srcConn, dstConn net.Conn, logger *zap.Logger) *integrations.RecordSession {
	return &integrations.RecordSession{
		Ingress: util.NewSafeConn(srcConn, logger),
		Egress:  util.NewSafeConn(dstConn, logger),
	}
}

// relayDeclinedConn relays a connection whose parser MATCHED it and then
// declined it (port allowlist, kill switch, unsupported sub-protocol),
// signalled by wrapping integrations.ErrParserDeclined.
//
// Without this the connection is left to handleConnection's deferred
// close. That close is the bug: a declining parser returned nil, the
// dispatcher logged "successfully recorded outgoing message", and the
// user's connection was torn down — while the parser's own docs promised
// the proxy would "treat the connection as a pass-through".
//
// The caller decides what a decline is; this function is only reached
// once errors.Is(err, integrations.ErrParserDeclined) has already said
// yes. It returns error to match the dispatch sites it returns through,
// and that error is always nil: a relay ending is not a record failure.
//
// It relays the SESSION's conns, not handleConnection's srcConn/dstConn
// locals, and that is load-bearing. A parser must read bytes to decide
// whether it wants the connection — gRPC reads the HTTP/2 preface — and
// it restores them by wrapping session.Ingress. handleConnection's local
// still points at the drained socket, so relaying that sends a
// preface-less stream and a real server answers GOAWAY(PROTOCOL_ERROR).
// The user's break would move rather than disappear.
//
// RECORD ONLY. dstConn is nil at every dispatch site in MODE_TEST, so
// there is no upstream to relay to; a parser that needs to decline during
// replay should fall through to the generic mock path, which is a
// separate change. (Note this is a property of the DISPATCH SITES, not of
// the whole file: proxy.go also dials in MODE_TEST when !rule.Mocking,
// and in any mode under GlobalPassthrough — both return before dispatch.)
//
// WHY util.RelayRawPassthrough. A client that does shutdown(SHUT_WR) — the
// end-of-request signal for every EOF-delimited exchange — must get its
// reply, so the relay forwards the FIN on a clean EOF and lets the surviving
// direction finish, which is what a pass-through has to mean.
// RelayRawPassthrough does. globalPassThrough did not when this was written:
// it returned on io.EOF, and the caller's close tore both sockets down, one
// round trip later than the bug this function exists to stop. It forwards the
// FIN now as well, so the choice here no longer turns on the half-close.
// (recordViaSupervisor refuses globalPassThrough for a reason of its own:
// proxy_v2.go.)
//
// WHY NO ORPHAN WINDOW. Nothing is captured on this connection from here
// on, so the test cases recorded over the same window will fail at replay
// with match_phase=no_mocks. Suppressing them was tried and removed: the
// orphan window is a TIME range read by the GLOBAL syncMock manager, so it
// suppresses every test case that overlaps it, including ones on
// unrelated, fully captured connections. Activity-scoping only narrows the
// range, never the blast radius — a busy connection never goes idle long
// enough to close the window at all. And a decline is not a transient
// incident like the V2 fallthrough's panic or memcap trip; it is a
// persistent configuration state that holds for every connection, every
// run. Suppression would turn "keploy drops my gRPC connections" into
// "keploy hands back a recording with the passing cases stripped out",
// which is not the better failure. The honest signal is the Warn below,
// one per connection, which is also what the other two pass-through paths
// (the record-mode kill switch and GlobalPassthrough) do.
func (p *Proxy) relayDeclinedConn(ctx context.Context, err error, logger *zap.Logger, parserType integrations.IntegrationType, session *integrations.RecordSession) error {
	if session == nil || !relayableConn(session.Ingress) || !relayableConn(session.Egress) {
		utils.LogError(logger, err, "parser declined but the session carried no conns to relay",
			zap.String("parser", string(parserType)))
		return nil
	}

	// Suppressed on a clean stop, for the same reason the V2 fallthrough
	// suppresses its own: on `keploy record` shutdown every in-flight
	// connection unwinds at once, and telling the user their recording is
	// incomplete at the moment they are reading the logs is noise about
	// something that did not go wrong.
	if ctx.Err() == nil {
		// Warn, not Debug: this is permanent capture loss for the rest of
		// the connection, and the user's recording is quietly smaller than
		// they think.
		logger.Warn("parser declined this connection; relaying raw and recording nothing for it",
			zap.String("parser", string(parserType)),
			zap.Error(err),
			zap.String("next_step", "user traffic is unaffected, but nothing on this connection is captured, so its test cases will report no mocks at replay. If this is traffic keploy should record, the parser declined it deliberately — check its port allowlist or sub-protocol support"),
		)
	}

	util.RelayRawPassthrough(session.Ingress, session.Egress)
	return nil
}

// relayableConn reports whether c can actually be relayed.
//
// A nil check on the session field is not enough, and the difference is
// not academic: buildRecordSession and mysqlSessionForRelay both wrap in
// util.NewSafeConn, which returns a non-nil *SafeConn even around a nil
// conn. `session.Egress == nil` is therefore false for every real caller,
// and a nil upstream would sail straight past it into a nil dereference
// inside the copy — recovered and swallowed, leaving the relay blocked
// with no error anywhere.
func relayableConn(c net.Conn) bool {
	if c == nil {
		return false
	}
	if sc, ok := c.(*util.SafeConn); ok {
		return sc.Unwrap() != nil
	}
	return true
}

// buildRecordSession constructs a RecordSession for a parser in record mode.
// It wraps src/dst in SafeConn. The caller (handleConnection) is responsible
// for creating the TLSUpgrader using pointers to its own srcConn/dstConn
// variables, so the upgrader updates the correct references on TLS upgrade.
func (p *Proxy) buildRecordSession(
	srcConn, dstConn net.Conn,
	mocks chan<- *models.Mock,
	errGrp *errgroup.Group,
	logger *zap.Logger,
	clientConnID, destConnID int64,
	opts models.OutgoingOptions,
	tlsUpgrader models.TLSUpgrader,
) *integrations.RecordSession {
	return &integrations.RecordSession{
		Ingress:      util.NewSafeConnWithReader(srcConn, srcConn, logger),
		Egress:       util.NewSafeConn(dstConn, logger),
		Mocks:        mocks,
		ErrGroup:     errGrp,
		TLSUpgrader:  tlsUpgrader,
		Logger:       logger,
		ClientConnID: fmt.Sprint(clientConnID),
		DestConnID:   fmt.Sprint(destConnID),
		Opts:         opts,
	}
}

// getSession returns the current session in a thread-safe manner.
func (p *Proxy) getSession() *agent.Session {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.session
}

// setSession replaces the current session in a thread-safe manner.
func (p *Proxy) setSession(s *agent.Session) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	p.session = s
}

// GetSession returns the current session in a thread-safe manner (exported for enterprise).
func (p *Proxy) GetSession() *agent.Session {
	return p.getSession()
}

// SetSessionResolver installs a per-TGID session resolver. Pass nil to
// revert to single-session mode. Multi-app composers (the enterprise
// agent) use this to route each app's captured traffic to its own
// session; OSS never sets it and therefore stays single-session.
func (p *Proxy) SetSessionResolver(fn func(tgid uint32) *agent.Session) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	p.sessionResolver = fn
}

// GetSessionFor returns the session that owns the connection originating
// from tgid. With a resolver installed it dispatches per-app; otherwise
// (or if the resolver returns nil for an unmapped/late connection) it
// returns the single active session, so behaviour degrades to today's
// single-session default rather than dropping the connection.
func (p *Proxy) GetSessionFor(tgid uint32) *agent.Session {
	p.sessionMu.RLock()
	resolver := p.sessionResolver
	single := p.session
	p.sessionMu.RUnlock()
	// Call the resolver outside the lock — it is external code and must
	// not run under sessionMu (reentrancy / deadlock safety).
	if resolver != nil {
		if s := resolver(tgid); s != nil {
			return s
		}
	}
	return single
}

func (p *Proxy) GetDestInfo() agent.DestInfo {
	return p.DestInfo
}

func (p *Proxy) GetIntegrations() map[integrations.IntegrationType]integrations.Integrations {
	result := make(map[integrations.IntegrationType]integrations.Integrations, len(p.Integrations))
	for k, v := range p.Integrations {
		result[k] = v
	}
	return result
}

// ResetRecordedDNSMocks clears the per-session DNS deduplication trackers.
// This should be called when starting a new recording session so DNS mocks are
// recorded fresh, and so the once-per-(name,qtype) NODATA relay notice is
// re-surfaced for the new session instead of being suppressed by state left
// over from the previous one.
//
// Purges in place rather than reallocating. expirable.NewLRU with a non-zero
// TTL starts a janitor goroutine whose done channel is never closed (see the
// note in hashicorp/golang-lru v2's expirable_lru.go), and that goroutine
// captures the cache — so every replacement cache would strand a goroutine
// AND pin the discarded entries beyond the reach of the GC. Purge keeps both
// trackers at one allocation for the process lifetime, and avoids swapping a
// pointer that the DNS handler goroutines read concurrently.
func (p *Proxy) ResetRecordedDNSMocks() {
	if p.recordedDNSMocks != nil {
		p.recordedDNSMocks.Purge()
	}
	if p.nodataRelayLogged != nil {
		p.nodataRelayLogged.Purge()
	}
	p.logger.Debug("DNS deduplication trackers reset")
}

// SetGracefulShutdown sets the graceful shutdown flag to indicate the application is shutting down
// When this flag is set, connection errors will be logged as debug instead of error
func (p *Proxy) SetGracefulShutdown(_ context.Context) error {
	p.isGracefulShutdown.Store(true)
	// A recording's stop: the requests in flight that began by now are the
	// ones it found, which are no requests of the next recording
	// (syncMock.NoteStop). keploy record's client sends this at its stop,
	// also one that never asks what the agent still holds; keploy mock's
	// once its drain is done; keploy test's and the runner's per test set,
	// where no recording is stopping and nothing a recording needs is noted.
	syncMock.Get().NoteStop(time.Now())
	// Surface the async-egress verdict at replay wind-down. This fires per
	// test-set on the native path and once per run on every path (replay.go's
	// run-level defer is ungated), so it is what covers the FINAL test-set.
	// LogReport is idempotent when nothing has moved since the last line.
	//
	// StopProxyServer also calls LogReport but is unreachable — it has no call
	// expression anywhere in the tree, only comments referring to it.
	if p.asyncEngine != nil {
		p.asyncEngine.LogReport(p.logger)
	}
	p.logSelfCalls()
	p.logger.Debug("Graceful shutdown flag set - connection errors will be logged as debug")
	// Flush any in-flight packet capture so the test-set's pcap is
	// finalised before the agent is allowed to exit.
	p.stopPacketCapture()
	return nil
}

// SubscribePcap registers w to receive every captured packet as a
// pcap byte stream (file header + one record per frame). The flush
// callback, if non-nil, is invoked after each record — agent HTTP
// handlers pass http.Flusher.Flush so packets reach the recorder
// over chunked transfer-encoding without buffering. Returns the
// unsubscribe func; safe to call once. Errors when no capture is
// active for this proxy (i.e., --capture-packets was not set on
// the recording session).
func (p *Proxy) SubscribePcap(w io.Writer, flush func()) (func(), error) {
	p.pcapMu.Lock()
	b := p.pcapBroadcaster
	p.pcapMu.Unlock()
	if b == nil {
		return nil, errors.New("packet capture is not active")
	}
	return b.subscribe(w, flush)
}

// IsGracefulShutdown returns whether the graceful shutdown flag is set
func (p *Proxy) IsGracefulShutdown() bool {
	return p.isGracefulShutdown.Load()
}

func (p *Proxy) SetAuxiliaryHook(h agent.AuxiliaryProxyHook) {
	p.auxiliaryHook = h
}

// runAuxTeardown invokes the auxiliary hook's BeforeTeardown (if it implements
// TeardownableProxyHook), at most once, synchronously. start() calls it on the
// shutdown path before TeardownNativeCA so a hook that attached uprobes removes
// them while still inside the awaited start() goroutine — before the process
// exits and before any update-ca-certificates fork — closing the race that
// otherwise leaks the uprobe.
//
// It runs under auxMu (runAuxStart), which detachThenRemoveCA holds.
func (p *Proxy) runAuxTeardown(ctx context.Context) {
	p.auxTornDown = true
	h, ok := p.auxiliaryHook.(agent.TeardownableProxyHook)
	if !ok {
		return
	}
	p.teardownOnce.Do(func() {
		if err := h.BeforeTeardown(ctx); err != nil {
			utils.LogError(p.logger, err, "auxiliary proxy hook teardown failed")
		}
	})
}

// errAuxTornDown is runAuxStart's answer once the proxy's teardown has run.
var errAuxTornDown = errors.New("the proxy was stopped before its auxiliary hook started")

// runAuxStart runs the auxiliary hook's AfterStart, unless the proxy's
// teardown has already run its BeforeTeardown (runAuxTeardown): what a start
// after that attached would be removed by nothing before the process exits.
// A teardown that comes while the start runs waits for it, and then removes
// what it attached.
//
// Where its progress can be seen (utils.TeardownProgressSeen), the start is
// teardown work of its own (utils.BeginTeardownWork), on this goroutine: a
// teardown that waits for it makes no progress of its own meanwhile, and the
// start -- loading BPF, attaching uprobes, cut short by a fatal signal like
// their removal -- is what the CLI that stops the agent, and the agent's own
// end on its kill, then go by. Before the stop, its steps are read by
// nothing. Where its progress cannot be seen, neither the start nor the
// teardown's wait for it is teardown work (detachThenRemoveCA): a start
// wedged against AfterStart's contract is then killed for want of progress.
func (p *Proxy) runAuxStart(ctx context.Context) error {
	p.auxMu.Lock()
	defer p.auxMu.Unlock()
	if p.auxTornDown {
		return errAuxTornDown
	}
	if utils.TeardownProgressSeen() {
		end := utils.BeginTeardownWork()
		defer end()
	}
	return p.auxiliaryHook.AfterStart(ctx, p)
}

// detachThenRemoveCA runs the last two stages of the proxy's shutdown: the
// auxiliary hook's teardown (runAuxTeardown), which removes TLS uprobes, and
// then the removal of this run's MITM CA from the OS trust store, so trust does
// not persist after the run (no-op in docker/k8s mode and for a persisted
// per-user CA). The CLI that stopped the agent must not kill it in the
// middle of either -- a uprobe removal cut short leaks the breakpoint, a CA
// tool cut short leaves a trust store half rewritten -- and either can take
// long without a step of its own, so both are teardown work in progress
// (utils.BeginTeardownWork), one span with a step between them.
//
// The span begins only once an auxiliary hook's start in flight is over
// (auxMu): waiting for it is not work of the teardown's own. Where the
// start's progress is seen it is teardown work of its own (runAuxStart),
// which the CLI goes by meanwhile; where it is not, a span begun before the
// wait would count as progress throughout it, and a start wedged against
// AfterStart's contract would never be killed.
func (p *Proxy) detachThenRemoveCA(ctx context.Context) {
	p.auxMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			p.auxMu.Unlock()
		}
	}
	defer unlock()
	end := utils.BeginTeardownWork()
	defer end()
	p.runAuxTeardown(ctx)
	unlock() // the CA's removal does not hold up a start, which no longer runs
	utils.TeardownStep()
	pTls.TeardownNativeCA(p.logger)
}

// getMockManager returns the current mock manager in a thread-safe manner.
func (p *Proxy) getMockManager() *MockManager {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.mockManager
}

// replayTargetHasHTTP2Mock reports whether a kind:Http2 mock was recorded for
// THIS connection's destination (sniHost + port). ALPN is negotiated
// per-connection before any request is seen, so the decision must be scoped to
// the destination being dialed: a session-global check ("any Http2 mock at
// all") would push an HTTP/1.1 target onto h2 as soon as the session contains
// any HTTP/2 target, and its recorded http/1.1 mock would never match.
//
// The check reads the live mock set on every call (via HasMocksByKind, which
// without exact serving short-circuits on the first match and allocates
// nothing). It is intentionally
// NOT cached: the mock set is replaced between test-sets on the SAME manager,
// so a cache keyed on manager identity would both miss Http2 mocks that first
// load after it was built and carry a destination's h2 preference into a later
// test-set where that destination is HTTP/1.1-only.
func (p *Proxy) replayTargetHasHTTP2Mock(sniHost string, port uint32) bool {
	m := p.getMockManager()
	// While the app starts, before the set's first test, the stored set's
	// recording, read whole when it was stored: a destination whose HTTP/2
	// mocks are all inside tests is known then, before any of them is
	// staged, as when a staging held the whole recording until the first
	// test (before keploy #4819). From the first test on, what the manager
	// serves decides, per window: its live trees, or under exact serving
	// the open view, which holds the open test's own mocks
	// (HasMocksByKind).
	if (m == nil || !m.HasFirstTestFired()) && p.recordedH2.has(sniHost, port) {
		return true
	}
	if m == nil {
		return false
	}
	return m.HasMocksByKind(models.HTTP2, func(mk *models.Mock) bool {
		if mk.Spec.HTTP2Req == nil {
			return false
		}
		h, pt := hostPortFromAuthority(mk.Spec.HTTP2Req.Authority, mk.Spec.HTTP2Req.Scheme)
		return http2DestMatches(strings.ToLower(h), pt, sniHost, port)
	})
}

// http2DestMatches reports whether a recorded Http2 destination (aHost:aPort,
// aHost lower-cased) matches the connection destination sniHost:port. A
// discriminator unknown on either side (missing port, missing SNI/host) is not
// used to reject, so the match is never stricter than the data allows; but a
// KNOWN mismatch (different port, or different host) rejects. At least one
// discriminator must actually match — otherwise we'd fall back to the old
// session-global behaviour.
func http2DestMatches(aHost string, aPort int, sniHost string, port uint32) bool {
	// aPort is decoded from a recorded mock's :authority, i.e. from on-disk
	// data we do not control, so it can be negative or far beyond the TCP port
	// space. A value outside 1..65535 cannot name a real destination, so it
	// counts as UNKNOWN rather than as a mismatch — per the contract above an
	// unknown discriminator must never reject, otherwise a mock whose host
	// genuinely matches would be discarded over a garbage port. The comparison
	// itself is done in int64, which holds both the int and the uint32 domain
	// without wrapping: narrowing aPort with uint32() would truncate, e.g.
	// authority "host:4294968296" (2^32+1000) would alias a connection to
	// port 1000 and wrongly push it onto h2.
	portKnown := port != 0 && aPort > 0 && aPort <= math.MaxUint16
	if portKnown && int64(aPort) != int64(port) {
		return false
	}
	hostKnown := sniHost != "" && aHost != ""
	if hostKnown && !strings.EqualFold(aHost, sniHost) {
		return false
	}
	return portKnown || hostKnown
}

// http2AuthorityMatchesDest parses a raw :authority and delegates to
// http2DestMatches. Retained as the unit-test entry point.
func http2AuthorityMatchesDest(authority, scheme, sniHost string, port uint32) bool {
	aHost, aPort := hostPortFromAuthority(authority, scheme)
	return http2DestMatches(strings.ToLower(aHost), aPort, sniHost, port)
}

// hostPortFromAuthority splits an HTTP/2 :authority (or a stored dst URL) into
// host and port. When no explicit port is present the port is inferred from
// the scheme (https/empty -> 443, http -> 80). Returns port 0 when unknown.
func hostPortFromAuthority(authority, scheme string) (string, int) {
	authority = strings.TrimSpace(authority)
	if i := strings.Index(authority, "://"); i >= 0 { // tolerate a scheme prefix
		authority = authority[i+3:]
	}
	if i := strings.IndexByte(authority, '/'); i >= 0 { // drop any path
		authority = authority[:i]
	}
	if authority == "" {
		return "", 0
	}
	if h, pStr, err := net.SplitHostPort(authority); err == nil {
		// SplitHostPort only splits, it does not validate the port numerically
		// ("host:-1" and "host:4294968296" both parse fine), and strconv.Atoi
		// happily returns those. Anything outside 1..65535 is not a dialable
		// TCP port, so report it as unknown (0) exactly like an unparseable
		// one instead of propagating a value callers would have to narrow.
		if pNum, err := strconv.Atoi(pStr); err == nil && pNum > 0 && pNum <= math.MaxUint16 {
			return h, pNum
		}
		return h, 0
	}
	switch strings.ToLower(scheme) {
	case "http":
		return authority, 80
	default: // https or unspecified: h2 egress is effectively always TLS
		return authority, 443
	}
}

// setMockManager replaces the current mock manager in a thread-safe manner.
//
// Swaps the new manager in while holding sessionMu, then closes the
// PREVIOUS manager after releasing the lock — Close() must not run under
// sessionMu because Close() synchronises with its own internal workers.
// Closing the outgoing manager stops its background idle-sweeper
// goroutine; without this, every Record / Mock session would leak a
// goroutine since MockManager owns a per-instance ticker that only
// stops on Close().
func (p *Proxy) setMockManager(m *MockManager) {
	if provenswitch.On() && m != nil {
		// Every mock served in a test is an earlier place of its recording
		// for the learn step (package matcher/learn), and each test's view
		// carries the tracker's open test as it is installed.
		m.SetServedObserver(p.learnTracker.ServedMock)
		m.SetServedInObserver(p.learnTracker.ServedMockIn)
		m.SetOpenTestEpoch(p.learnTracker.OpenEpoch)
		// Each staging stages the tracker's running set, though the set
		// may have begun before m was made (Mock).
		m.SetTrackerSetAt(p.learnTracker.SetAt)
	}
	p.sessionMu.Lock()
	prev := p.mockManager
	p.mockManager = m
	p.sessionMu.Unlock()
	if prev != nil {
		prev.Close()
	}
}

func (p *Proxy) InitIntegrations(_ context.Context) error {
	// initialize the integrations
	for parserType, parser := range integrations.Registered {
		logger := p.logger.With(zap.Any("Type", parserType))
		prs := parser.Initializer(logger)
		p.Integrations[parserType] = prs
		if p.asyncEngine != nil {
			// Register any AsyncParser so the engine can route to it
			// (LaneFor/Decide) — AsyncParser alone is sufficient. Separately
			// setter-inject the engine into parsers that implement AsyncAware
			// so they can reach it when serving.
			if ap, ok := prs.(async.AsyncParser); ok {
				p.asyncEngine.RegisterParser(string(parserType), ap)
			}
			if aw, ok := prs.(async.AsyncAware); ok {
				aw.SetAsyncEngine(p.asyncEngine)
			}
		}
		logger.Debug("initialized the parser integration", zap.String("ParserType", string(parserType)))
		p.integrationsPriority = append(p.integrationsPriority, ParserPriority{Priority: parser.Priority, ParserType: parserType})
	}
	sort.Slice(p.integrationsPriority, func(i, j int) bool {
		return p.integrationsPriority[i].Priority > p.integrationsPriority[j].Priority
	})
	return nil
}

// bindContainerKeys makes ctx, the agent's, the bound of its reading of
// Docker for container keys (containerkey), so it stops with the agent, and
// has p count each use of a container whose read the daemon did not answer
// (countUnreadContainers). Only under the proven switch: without it the
// agent never reads Docker.
func (p *Proxy) bindContainerKeys(ctx context.Context) {
	if provenswitch.On() {
		containerkey.Default.Bind(ctx)
		p.countUnreadContainers()
	}
}

// startContainerKeys starts a session's container keys under the proven
// switch: fresh scopes, and the local daemon's containers indexed before the
// app runs (containerkey). Without the switch it does nothing.
func (p *Proxy) startContainerKeys(ctx context.Context) {
	if provenswitch.On() {
		containerkey.Default.Reset()
		containerkey.Default.Watch(ctx, p.logger)
	}
}

// In proxy.go
func (p *Proxy) StartProxy(ctx context.Context, opts agent.ProxyOptions) error {
	p.bindContainerKeys(ctx)

	// Skip the TCP listener if configured. DNS + parsers + session still run.
	if agent.SkipProxyListener {
		p.skipListener = true
	}

	//first initialize the integrations
	err := p.InitIntegrations(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to initialize the integrations")
		return err
	}

	// The recording's capture watermark: from here on its stream holds test
	// cases until their verdict is final, and a stop drains what the
	// parsers still hold (capture_watermark.go).
	p.addCaptureWatermark()

	// Start the continuous error drain so the error channel never fills up.
	// This must happen before any connections are handled.
	p.StartErrorDrain(ctx)

	// set up the CA for tls connections.
	//
	// On failure we record the terminal error via MarkCAFailed so the
	// /agent/ready handler can return a clear "CA setup failed"
	// diagnostic instead of an indefinite "not yet ready". We still
	// continue starting the proxy — the proxy can serve non-TLS
	// traffic and surfacing the error to readiness probes is a better
	// signal to operators than hard-aborting the agent here.
	// Use the PID-aware SetupCAForApp so the Java truststore import
	// (installJavaCAForHome) targets the JDK the instrumented app is
	// actually running with. On non-Java workloads / shared-volume
	// mode / appPID==0 these extra args are harmless — SetupCAForApp
	// falls back to the legacy PATH-keytool behaviour. See
	// pkg/agent/proxy/tls/java_detect.go for the resolution order.
	err = p.setupCA(ctx)
	utils.BootStep("the CA's install is over")
	if err != nil {
		// Terminal: the CA cannot be installed in this process and
		// Keploy-proxied TLS will fail cert-verify for every workload
		// that gets routed through the proxy. Log at Error (not Warn)
		// to match the severity, and include a next_step so operators
		// see the likely fix without grepping source.
		p.logger.Error(
			"SetupCA failed — Keploy-proxied TLS will fail cert-verify. "+
				"The /agent/ready endpoint will return 503 with this error "+
				"so dependents don't wait forever for a readiness that "+
				"will never come.",
			zap.Error(err),
			zap.String("next_step",
				"Verify the agent container has write access to the "+
					"shared /tmp/keploy-tls volume (docker/k8s mode) or "+
					"to the host's CA trust store under /usr/local/share/"+
					"ca-certificates or /etc/pki/ca-trust/source/anchors "+
					"(native mode). Restart the agent after fixing."),
		)
		pTls.MarkCAFailed(err)
	}

	// Channel-binding shim: now that appPID is known, walk the app's
	// process tree, register every visible descendant in the BPF
	// allowlist, attach uprobes to whatever libcryptos they map, and
	// kick off a background rescan that picks up workers forked
	// later (gunicorn/uwsgi) and lazy dlopen'd wheels (psycopg2-
	// binary loads its bundled libssl only on the first connect).
	//
	// Nil-safe: if cbshim failed to load (no CAP_BPF, older kernel,
	// etc.) p.cbshim is nil and these calls are no-ops. p.appPID==0
	// means we don't yet know the app PID — also a no-op; future
	// session-bind paths can call this explicitly.
	if p.cbshim != nil && p.appPID != 0 {
		// PID membership is BPF-side via task_in_agent_ns (lazy
		// namespace classification — see cbshim.bpf.c). No userspace
		// PID-walking polling loop. The library-refresh loop below
		// only handles late-loaded libcrypto files (bundled wheel
		// libs that appear after worker fork-exec) — its job is
		// per-file uprobe attach, NOT PID tracking.
		p.logger.Info("cbshim: scanning process tree for libcrypto/libpq mappings",
			zap.Uint32("appPID", p.appPID))
		if err := p.cbshim.AttachToProcessTree(int(p.appPID)); err != nil {
			p.logger.Info("cbshim: AttachToProcessTree returned error (continuing — library refresh will retry)",
				zap.Uint32("appPID", p.appPID), zap.Error(err),
				zap.String("next_step", "the first-pass scan of /proc/<appPID>/maps + descendants failed to find or attach a uprobe for libcrypto/libpq; the BPF discovery hook (cbshim's security_mmap_file fentry) will catch them on the next library mmap, so this is non-fatal. If SCRAM-PLUS still fails after a few requests, rerun with --debug to see per-process scanProcessMaps results and confirm the agent has CAP_BPF + the kernel allows bpf_probe_write_user."))
		} else {
			p.logger.Info("cbshim: AttachToProcessTree completed", zap.Uint32("appPID", p.appPID))
		}
		// Kick the sched_process_exec ringbuf consumer. The kernel
		// emits an event every time a process in the agent's PID
		// namespace completes execve(); consumer walks /proc/<tgid>/
		// maps to discover late-loaded libcrypto/libpq files (bundled
		// wheel libs, etc.) and attaches uprobes. Replaces the old
		// 2s polling loop with an event-driven design.
		p.cbshim.StartProcEventConsumer(ctx)
	} else if p.cbshim == nil {
		// Neutral debug-level log — nil cbshim is the EXPECTED state
		// when the feature flag is off or no implementation is
		// registered (OSS builds), not just when BPF load failed.
		// At Info this would spam every record-mode start.
		p.logger.Debug("cbshim: not attached (feature disabled or no implementation registered)")
	} else {
		p.logger.Debug("cbshim: appPID==0 — skipping AttachToProcessTree (no app PID yet)")
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		// start() is what removes the CA installed above: it never runs.
		p.detachThenRemoveCA(ctx)
		return errors.New("failed to get the error group from the context")
	}
	// Create a channel to signal readiness of each server
	readyChan := make(chan error, 1)

	// start the proxy server. p.start is the sole writer of readyChan:
	// it sends the startup error (or nil on success) exactly once on
	// every code path — skipListener, listen failure, and successful
	// listen. Do not send again here; readyChan is buffered size 1
	// and a second send would block forever because the caller at
	// line below only reads it once. Before this was fixed, the
	// skipListener path leaked this goroutine at shutdown.
	g.Go(func() error {
		defer utils.Recover(p.logger)
		err := p.start(ctx, readyChan)
		if err != nil {
			utils.LogError(p.logger, err, "error while running the proxy server")
			return err
		}
		return nil
	})

	//change the ip4 and ip6 if provided in the opts in case of docker environment
	if len(opts.DNSIPv4Addr) != 0 {
		p.IP4 = opts.DNSIPv4Addr
	}

	if len(opts.DNSIPv6Addr) != 0 {
		p.IP6 = opts.DNSIPv6Addr
	}

	// Capture the ORIGINAL cluster resolver list from /etc/resolv.conf
	// BEFORE binding our own listener on p.DNSPort. This MUST happen
	// here (not lazily on first query) because in Kubernetes sidecar
	// deployments the injector rewrites the app container's resolv.conf
	// nameserver entry to 127.0.0.1 and redirects DNS traffic to the
	// agent's port 26789 via iptables/eBPF (resolv.conf itself has no
	// port syntax — standard `nameserver` lines are IP-only). If we
	// ever pick that loopback entry up as our "upstream" we forward
	// queries back to ourselves. The sidecar's own /etc/resolv.conf
	// still points at CoreDNS at this point, so the snapshot we take
	// here is the correct set of real upstream resolvers. Errors are
	// non-fatal: on failure the forwarder simply falls back to the
	// legacy default response.
	p.captureDNSUpstream()
	p.logAgentEgressKnobs()

	// Wait for the proxy server to be ready or fail
	if err := <-readyChan; err != nil {
		return err
	}
	utils.BootStep("the proxy listens")

	// Serve DNS over TCP and UDP, and wait for both to be bound.
	if err := p.startDNSServers(ctx, g); err != nil {
		return err
	}
	utils.BootStep("the DNS servers listen")

	if p.auxiliaryHook != nil {
		if err := p.runAuxStart(ctx); err != nil {
			if errors.Is(err, errAuxTornDown) {
				return err // the stop came first: nothing failed
			}
			// Do NOT swallow the error. An auxiliary hook only gets
			// registered when a caller has explicitly opted into the
			// feature it implements (currently: --low-latency, which
			// registers the proxyless BPF startup). Silently
			// continuing into userspace-proxy mode after the hook has
			// declared a failure would give the user a mode they
			// didn't ask for and no deterministic signal that the
			// requested mode wasn't delivered.
			//
			// The hook itself is expected to embed actionable
			// remediation in the returned error (e.g. the enterprise
			// proxyless hook wraps the BPF verifier error
			// with a "Next steps: upgrade kernel / drop --low-latency
			// / rebuild without the BPF variant" message). Log once
			// here with the hook's message visible in zap.Error and
			// a generic next_step pointing at that embedded guidance,
			// then propagate so the caller can abort. Partial-proxy
			// goroutines started before this point are torn down by
			// the caller cancelling proxyCtx (see service/agent.go).
			utils.LogError(p.logger, err, "auxiliary proxy hook failed; requested feature cannot run on this host",
				zap.String("next_step", "the hook's error wraps its own 'Next steps:' remediation — read the full error chain for the feature-specific fix (kernel upgrade, disabling the feature flag, or rebuilding without the BPF variant)"),
			)
			return fmt.Errorf("auxiliary proxy hook failed: %w", err)
		}
	}

	p.logger.Info("Keploy has taken control of the DNS resolution mechanism, your application may misbehave if you have provided wrong domain name in your application code.")

	if p.skipListener {
		p.logger.Info("Proxy TCP listener intentionally not bound (SkipProxyListener is set); DNS and session services are live but no port is accepting connections")
	} else {
		p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	}
	return nil
}

// runSkipListenerFlush drives the proxyless (skipListener) recording path's
// owned-window flush loop. Proxyless capture has no TCP accept loop, so it has
// no analog to the listener path's periodic FlushOwnedWindows — the drain that
// retro-bins a mock which landed in the syncMock buffer AFTER its ingress
// window resolved (the async egress-parse-vs-ingress-resolve race, or a
// keep-alive outbound call made while a request is being served). This method
// restores that drain: it flushes on the given interval while recording is
// live, and on ctx cancellation performs one final best-effort drain before
// returning. That final drain is the shutdown twin of the periodic tick — the
// enterprise persistMocks consumer keeps reading MC for a grace period past
// ctx.Done, so a mock flushed here is still persisted before the consumer
// exits. Deleting either the periodic tick or the final drain silently
// reintroduces the lost-mock bug. It blocks until ctx is cancelled; start()
// passes the production 1 s cadence, tests pass a shorter one for fast,
// deterministic ticks.
func (p *Proxy) runSkipListenerFlush(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if mgr := syncMock.Get(); mgr != nil {
				mgr.FlushOwnedWindows()
			}
			return
		case <-ticker.C:
			if mgr := syncMock.Get(); mgr != nil {
				mgr.FlushOwnedWindows()
			}
		}
	}
}

// beginBootWork is utils.BeginBootWork: the work of the agent's start that the
// proxy does on goroutines of its own (the listener's start, the DNS servers'
// binds) while StartProxy's stage waits for it. A variable for tests.
var beginBootWork = utils.BeginBootWork

// start function starts the proxy server on the idle local port.
// When skipListener is true, no TCP listener is created.
// The function blocks on ctx.Done and handles cleanup on shutdown.
func (p *Proxy) start(ctx context.Context, readyChan chan<- error) error {

	if p.skipListener {
		// Info-level notice is emitted once in StartProxy (near the
		// DNS-control log, which is the conventional "startup banner"
		// spot). Here we only want a debug trace for the start()
		// internal entry point so logs aren't duplicated.
		p.logger.Debug("Proxy TCP listener skipped; DNS and session services active")
		readyChan <- nil

		// Proxyless capture has no TCP accept loop. Ingress windows still
		// resolve — ResolveRange fires from the ingress CaptureHook
		// (pkg/agent/hooks/conn), not from a per-conn listener handler — so
		// recentWindows IS populated on this path. What has no analog here is
		// the listener path's *periodic* FlushOwnedWindows, which drains mocks
		// that landed in the syncMock buffer AFTER their window resolved. An
		// outbound mock that lands post-resolution — the async
		// egress-parse-vs-ingress-resolve race, or a keep-alive outbound call
		// made while the app is serving a request (firstReqSeen already true) —
		// would then sit in the buffer until shutdown, where the cancelled
		// recorder ctx drops it. That silently loses every post-first-request
		// outbound mock (e.g. HTTP enrich calls) while the pre-firstReqSeen
		// bootstrap calls, which bypass the buffer, survive. Mirror the listener
		// path's periodic FlushOwnedWindows so recentWindows retro-bins each late
		// mock into its already-resolved window and the healthy write path
		// persists it. NOT CloseOutChan: skipListener mode has no
		// clientConnErrGrp guaranteeing producers exited, so closing MC could
		// race an in-flight send (see the note below).
		//
		// Known residual: a mock AddMock'd AFTER the final FlushOwnedWindows
		// (below) still loses the shutdown-tail race — proxyless mode has no
		// producer-quiescence barrier to flush behind. It is mitigated, not
		// eliminated, by the enterprise persistMocks consumer continuing to read
		// MC for a grace period past ctx.Done; a full fix needs a capture-layer
		// drain signal and is tracked separately.

		// Drive the periodic + final owned-window flush for the proxyless
		// path. Blocks until ctx is cancelled: it drains attributable
		// buffered mocks on a 1 s cadence while recording is live, then runs
		// one final best-effort drain on shutdown. See runSkipListenerFlush.
		p.runSkipListenerFlush(ctx, 1*time.Second)
		// Intentionally do NOT close p.session.MC in the skipListener
		// path. The non-skip path above closes MC only after
		// clientConnErrGrp.Wait() guarantees every per-conn producer
		// has exited, so no send-to-closed-channel panic can fire.
		// In skipListener mode there is no clientConnErrGrp — DNS
		// mock producers and any external capture-layer senders are
		// still running up to ctx cancellation and may race a close.
		// Shutdown of downstream consumers is driven by ctx.Done()
		// instead; the Go runtime garbage-collects MC once all
		// references drop.
		utils.TeardownStep()
		p.nsSwitchMutex.Lock()
		if string(p.nsswitchData) != "" {
			if err := p.resetNsSwitchConfig(); err != nil {
				utils.LogError(p.logger, err, "failed to reset the nsswitch config, please retry shutdown or restore the resolver configuration manually from /etc/nsswitch.conf")
			}
		}
		p.nsSwitchMutex.Unlock()
		utils.TeardownStep()
		// Remove TLS uprobes (and any other hook-owned kernel state) BEFORE
		// forking update-ca-certificates: a uprobe teardown that races that
		// fork can inherit the last link reference into the child and then be
		// killed mid-removal, leaking the breakpoint.
		p.detachThenRemoveCA(ctx)
		p.logger.Debug("proxy (skipListener) stopped")
		return nil
	}

	// The listener's start below -- its bind, the kernel count's proof, the
	// handshake hold -- is the agent's start's own work, done here while
	// StartProxy's stage waits for it on readyChan: a stage of the start of
	// its own, on this goroutine, so the CLI that waits for the agent sees
	// it make progress (utils.BeginBootWork). The wait alone shows none. It
	// ends before readyChan says how the start went.
	endListen := beginBootWork("the proxy listener's start")
	defer endListen() // a panic's path: its thread let go, the stage over

	// It will listen on all the interfaces
	listener, err := p.listen(ctx, fmt.Sprintf(":%v", p.Port))
	if err != nil {
		utils.LogError(p.logger, err, fmt.Sprintf("failed to start proxy on port:%v", p.Port))
		// What StartProxy installed before this -- the run's CA in the trust
		// store, and whatever the auxiliary hook holds -- is removed now:
		// the shutdown below that removes it is never set up (a port in use,
		// a previous agent still around, left the anchor trusted).
		p.detachThenRemoveCA(ctx)
		endListen()
		// Notify failure
		readyChan <- err
		return err
	}
	p.Listener = listener
	// The kernel's count is proven before the proxy says it is ready
	// (accepts.go); the stop ends the proof.
	p.canary(ctx, listener)
	p.decideCounted()
	p.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	// Handshakes are held from the first session whose connections dial
	// their destination (ensureHandshakeHold); one already set is honoured
	// now, before the proxy is reported ready.
	p.armHandshakeHold(ctx)
	defer p.stopHandshakeHold()
	endListen()
	// Signal that the server is ready
	readyChan <- nil
	defer func(listener net.Listener) {
		err := listener.Close()

		if err != nil && !isNetworkClosedErr(err) {
			p.logger.Error("failed to close the listener", zap.Error(err))
		}
		p.logger.Debug("proxy stopped...")
	}(listener)

	clientConnCtx, clientConnCancel := context.WithCancel(ctx)
	clientConnErrGrp, _ := errgroup.WithContext(clientConnCtx)

	// Periodically drain attributable buffered mocks WHILE recording is
	// live. The request-driven drains (ResolveRange / DeleteMocksStrictlyBefore)
	// can't reach a per-test mock that lands after the final request's HTTP
	// window closed — e.g. a multi-MB Mongo document still decoding when its
	// response was captured. Without this, such a mock waits in the buffer
	// until shutdown, where the cancelled recorder ctx makes the relay, the
	// consumer, and InsertMock all drop it. Ticking here persists it through
	// the healthy write path. Stops when clientConnCtx is cancelled (the
	// shutdown defer's clientConnCancel), before CloseOutChan.
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-clientConnCtx.Done():
				return
			case <-ticker.C:
				if mgr := syncMock.Get(); mgr != nil {
					mgr.FlushOwnedWindows()
				}
			}
		}
	}()

	defer func() {
		clientConnCancel()

		// Close the listener synchronously BEFORE waiting for connection
		// handlers. Defers are LIFO, so without this the listener-close
		// defer (func1) runs AFTER this defer (func2). The accept
		// goroutine blocks on listener.Accept() — if we wait for
		// clientConnErrGrp before closing the listener, the accept
		// goroutine never exits, and on 2-CPU CI runners the scheduler
		// may not run other unblocking goroutines promptly, causing a hang.
		if listener != nil {
			listener.Close()
		}
		// What the canary kept for the accept loop and the loop has not
		// taken, it never will.
		p.closeEarly()
		// What the listener never accepted, it never will: every wait on
		// its count ends now, not after the slower steps below.
		p.closeAccepts()
		// Nothing accepts any more: stop holding handshakes now, not after
		// the connections drain, so a SYN that arrives meanwhile is refused
		// at once rather than dialled for and then refused.
		p.stopHandshakeHold()

		err := clientConnErrGrp.Wait()
		if err != nil {
			p.logger.Debug("failed to handle the client connection", zap.Error(err))
		}
		// Every connection's handler has returned: the count of the
		// connections the proxy could not route is final (untracked.go).
		p.untrackedTotal()
		// Each stage of the proxy's shutdown is a step of the agent's
		// teardown, which the CLI that stopped it reads (utils.TeardownStep).
		utils.TeardownStep()
		// Close the mock channel (if any in record mode) via
		// SyncMockManager so the close is serialized with in-flight
		// AddMock sends. A bare close(p.session.MC) here races the
		// record session's parser goroutines — the race detector
		// flagged this on keploy#4045 during the petclinic
		// record_build_replay_latest job, where the race also
		// correlated with a 28-min process hang at runtime.
		// CloseOutChan takes an RWMutex that AddMock holds for read,
		// guaranteeing every send completes before close runs.
		if mgr := syncMock.Get(); mgr != nil {
			mgr.CloseOutChan()
		} else {
			p.sessionMu.RLock()
			if p.session != nil && p.session.MC != nil {
				close(p.session.MC)
			}
			p.sessionMu.RUnlock()
		}

		p.nsSwitchMutex.Lock()
		if string(p.nsswitchData) != "" {
			// reset the hosts config in nsswitch.conf of the system (in test mode)
			err = p.resetNsSwitchConfig()
			if err != nil {
				utils.LogError(p.logger, err, "failed to reset the nsswitch config")
			}
		}
		p.nsSwitchMutex.Unlock()
		utils.TeardownStep()
		// Remove TLS uprobes (and any other hook-owned kernel state) before
		// forking update-ca-certificates — see the skipListener path.
		p.detachThenRemoveCA(ctx)
	}()

	for {
		clientConnCh := make(chan net.Conn, 1)
		errCh := make(chan error, 1)
		// handedOver is closed once this round's accept has handed over
		// what it got, or ended without anything.
		handedOver := make(chan struct{})
		go func() {
			defer close(handedOver)
			defer utils.Recover(p.logger)
			if early := p.takeEarly(); early != nil {
				if p.handingOver != nil {
					p.handingOver(early)
				}
				clientConnCh <- early
				return
			}
			conn, err := listener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					errCh <- nil
					return
				}
				utils.LogError(p.logger, err, "failed to accept connection to the proxy")
				errCh <- err
				return
			}
			clientConnCh <- conn
		}()
		select {
		case <-ctx.Done():
			// This round's accept ends once the shutdown has closed the
			// listener: what it hands over after the stop -- a connection
			// the canary kept, or one it accepted -- is closed, not left
			// open until the process exits.
			go func() {
				<-handedOver
				select {
				case c := <-clientConnCh:
					_ = c.Close()
				default:
				}
			}()
			return nil
		case err := <-errCh:
			return err
		// handle the client connection
		case clientConn := <-clientConnCh:
			if ctx.Err() != nil {
				// The proxy stopped as this round handed it over: nothing
				// serves it.
				_ = clientConn.Close()
				return nil
			}
			// Under the proven switch, or for an agent that settles its
			// reads, the connection is kept from its accept on, so a test's
			// end sees what it holds (inflight.go): kept before it is
			// counted, or a wait that saw the count reach it could find it
			// nowhere and call the test settled while its request sat unread.
			raw := clientConn
			var tracked *trackedConn
			if p.keepsConnections() {
				tracked = p.inflight.trackTold(clientConn, p.flights())
				clientConn = tracked
			}
			// Its sequence number is taken before anything else is done
			// with it: the agent waits on the numbers the kernel gave the
			// connections completed to this listener (accepts.go).
			p.countAccept(raw)
			// A paired connection is kept too, whatever the switch says
			// (only a counted listener pairs one): its close is read
			// before it happens (pairbytes.go).
			if p.paired.holds(raw) {
				if tracked == nil {
					tracked = p.inflight.trackTold(clientConn, p.flights())
					clientConn = tracked
				}
				tracked.beforeClose = func() { p.pairClosing(clientConnCtx, raw, tracked) }
			}
			// Track the connection BEFORE spawning the goroutine to
			// close the Add-vs-Wait race that sync.WaitGroup's
			// contract forbids. If Add ran inside the goroutine
			// (i.e. after clientConnErrGrp.Go returns), a concurrent
			// StopProxyServer calling waitForConnDrain -> Wait could
			// observe count=0 and return before this connection's
			// goroutine registered itself — the shutdown would
			// then proceed as if there were no in-flight work, and
			// the late Add could trigger "sync: WaitGroup misuse"
			// panics in some interleavings. Done() still runs in
			// the goroutine's deferred cleanup.
			p.activeConns.Add(1)
			clientConnErrGrp.Go(func() error {
				// Last, once handleConnection has closed it: a test's end
				// stops waiting for bytes on it (pairbytes.go).
				defer p.paired.remove(raw)
				defer p.activeConns.Done()
				defer p.inflight.forget(tracked)
				// A connection's end is a step of the agent's teardown once it
				// is stopping: the CLI that stopped it reads them as it drains
				// (utils.TeardownStep). Before, it counts nothing anyone reads.
				defer utils.TeardownStep()
				// What follows the handler (its error's logging) is
				// recovered here, as the handler is in handleRecovered.
				defer util.Recover(p.logger, clientConn, nil)
				err := p.handleRecovered(clientConnCtx, clientConn)
				if err != nil && err != io.EOF {
					// Network closed errors are expected when client closes connection (e.g., app shutdown)
					// Only log as error if it's not a shutdown/network closed error
					if isShutdownError(err) || isNetworkClosedErr(err) {
						p.logger.Debug("failed to handle the client connection (connection closed)", zap.Error(err))
					} else {
						if errors.As(err, new(reportedMiss)) {
							p.logger.Debug("failed to handle the client connection: no mock matched its call", zap.Error(err))
						} else if refusedLocally(err) {
							p.logger.Debug("failed to handle the client connection: nothing is listening at the local destination yet", zap.Error(err))
						} else {
							utils.LogError(p.logger, err, "failed to handle the client connection")
						}
					}
				}
				return nil
			})
		}
	}
}

func (p *Proxy) MakeClientDeRegisterd(_ context.Context) error {
	p.logger.Info("Inside MakeClientDeregisterd of proxyServer")
	p.clientClose <- true
	return nil
}

// handleRecovered is handleConnection with a panic in it recovered, as the
// accept loop serves every connection: the connection is closed and the panic
// logged, and it is counted as a capture loss of kind parser_panicked, as a
// parser goroutine's panic is (countParserPanic). A panic there (a parser that
// serves in the handler's own goroutine, as MockOutgoing's setup does, or the
// proxy around it) left the connection unserved, and nothing counted it: the
// test that made the call could pass.
func (p *Proxy) handleRecovered(ctx context.Context, conn net.Conn) error {
	defer util.RecoverWith(p.logger, conn, nil, func() {
		p.losses.Add(models.CaptureLossParserPanic, "the proxy's connection handler, on a connection from "+fmt.Sprint(conn.RemoteAddr()))
	})
	return p.handleConnection(ctx, conn)
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (p *Proxy) handleConnection(ctx context.Context, srcConn net.Conn) error {
	// tracked is the connection as the accept kept it (inflight.go:
	// keepsConnections): srcConn is wrapped again below (a peek's reader, a
	// prefix, TLS), and what a test's end reads of it goes to this one.
	tracked, _ := srcConn.(*trackedConn)
	// activeConns.Add/Done are paired at the goroutine-spawn site
	// in the accept loop (see caller) to avoid the "Add concurrent
	// with Wait" race sync.WaitGroup documents as a misuse.
	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	// making a new client connection id for each client connection
	clientConnID := util.GetNextID()

	defer func(start time.Time) {
		duration := time.Since(start)
		p.logger.Debug("time taken by proxy to execute the flow", zap.Any("Client ConnectionID", clientConnID), zap.Int64("Duration(ms)", duration.Milliseconds()))
	}(start)

	// dstConn stores conn with actual destination for the outgoing network call
	var dstConn net.Conn

	//Dialing for tls conn
	destConnID := util.GetNextID()

	remoteAddr := srcConn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	// The upstream connection dialled while this connection's handshake was
	// held, if it was: the first dial of its destination uses it. It was
	// dialled at destAddr of the connect hook's record for this socket, the
	// same record redirect_proxy_map copies and dstAddr below is built from,
	// so the two addresses are the same string.
	var predialed *util.Predialed
	if end, ok := clientEnd(remoteAddr); ok {
		predialed = p.predials.take(end)
	}
	defer predialed.CloseIfUnused()
	appConn := srcConn

	// Claim this source port for the current connection (O7), and drop what
	// the MITM files under it (pTls.SrcPortToDstURL) when the connection
	// finishes, but ONLY if this connection still owns the port: the OS
	// releases the port when srcConn is closed (the defers below, which run
	// BEFORE this one), and a newer connection on it may have filed its own.
	// Nothing in this module reads the map; a connection's names are its own
	// (connectTargetHost, clientSNI below). This bounds the map for the
	// readers outside the module that still use it.
	pTls.ClaimSrcPort(sourcePort, destConnID)
	defer pTls.ReleaseSrcPortIfOwner(sourcePort, destConnID)

	// The accepted connection is this function's to close, on every return.
	// Nothing returns before this line, and every later path is covered no
	// matter what srcConn has since been replaced by: a return that skipped
	// the close left the application blocked on its call until its own
	// timeout. The cleanup further down still closes srcConn first, so a TLS
	// connection says goodbye before its socket goes; this close then finds
	// the socket already closed and does nothing.
	defer func() { _ = appConn.Close() }()

	probeProxy(p.logger, "accept", clientConnID, zap.Int("srcPort", sourcePort))

	p.logger.Debug("Inside handleConnection of proxyServer", zap.Int("source port", sourcePort), zap.Int64("Time", time.Now().Unix()))
	// When the proxy uses the kernel's pairing (the proven switch, or calls
	// counted from its start: usesPairing), a connection the kernel paired
	// with the application's carries its own destination; one it paired with
	// none is refused under the switch, and with it off drops the count and
	// is routed by its source port, as it always was. A paired one never
	// takes a destination by source port, which another connection may share
	// (accepts.go).
	var err error
	destInfo, counted := p.pairedDest(srcConn)
	if counted && destInfo == nil && !provenswitch.On() && p.dropCounting(srcConn) {
		// With the switch off a connection the kernel tied to none of the
		// application's (a NAT between them) is routed by its source port,
		// as it always was: the run is no longer counted, and every later
		// read of its misses is unread (CountingLost).
		counted = false
	}
	if counted {
		if destInfo == nil {
			p.refuseUnpaired(srcConn)
			return nil
		}
		// The redirect entry is this connection's too: taken, so the
		// shared map never fills. It may be gone already.
		_ = p.dropDest(ctx, srcConn, sourcePort)
	} else {
		destInfo, err = p.destFor(ctx, srcConn, sourcePort)
		if err != nil {
			// An untracked connection: the hooks hold no destination for it.
			// It is closed (the client sees EOF), and said, loud, unless the
			// proxy is stopping (untracked.go).
			p.untracked(ctx, srcConn, err)
			return nil
		}

		p.logger.Debug("Handling outgoing connection to destination port", zap.Uint32("Destination port", destInfo.Port))

		// releases the occupied source port when done fetching the destination info
		err = p.dropDest(ctx, srcConn, sourcePort)
		if err != nil {
			utils.LogError(p.logger, err, "failed to delete the destination info", zap.Int("Source port", sourcePort))
			return err
		}
	}
	seeProcess(Process{Pid: destInfo.AgentNsPid, KernelPid: destInfo.KernelPid})

	var dstAddr string

	switch destInfo.Version {
	case 4:
		p.logger.Debug("the destination is ipv4")
		dstAddr = destAddr(destInfo) // the address a held handshake's upstream was dialled at
		p.logger.Debug("", zap.Uint32("DestIp4", destInfo.IPv4Addr), zap.Uint32("DestPort", destInfo.Port))
	case 6:
		p.logger.Debug("the destination is ipv6")
		dstAddr = destAddr(destInfo)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Uint32("DestPort", destInfo.Port))
	}

	//get the session rule
	rule := p.getSession()
	if rule == nil {
		// The agent has no session yet (it has just started, or restarted,
		// and its client has not armed it): the agent's own mode decides.
		// Nothing on the connection is captured, so its close is no loss
		// (pairClosing).
		if tracked != nil {
			p.paired.servedWithoutSession(tracked.Conn)
		}
		return p.serveWithoutSession(ctx, srcConn, dstAddr, predialed)
	}

	// The proven model names a container destination by its container key,
	// not by the address it was reached at (containerkey): in the scope open
	// now, from the index of the local daemon's containers, which never waits
	// on Docker. The entry goes when this function returns, after the cleanup
	// below has waited for the parsers, so every mock they record is stamped
	// first.
	proven := provenswitch.On()
	// ctrKey is the connection's container key, or its label while its
	// configuration is unread (topology's MOCKED rows); containerRepo the
	// image repository of the container it reaches, which a mock.real
	// container: entry names (realdeps.go).
	var ctrKey, containerRepo string
	if proven {
		connID := fmt.Sprint(clientConnID)
		// The process the connection came from, which each mock its parsers
		// record names (starts.Registry.StampOrigin): kept
		// until they are done, as the container key is.
		starts.Default.NoteOrigin(connID, destInfo.KernelPid)
		defer starts.Default.ForgetOrigin(connID)
		if c, _, isContainer := containerkey.Default.Name(p.logger, connID, dstAddr); isContainer {
			// Its key, or, while its configuration is unread, its
			// repository and port: a container either way, never a host
			// listener (containerkey.Label).
			ctrKey = containerkey.Label(c)
			containerRepo = containerkey.Repository(c.Image)
		}
		defer containerkey.Default.Forget(connID)
	}

	// Create a local copy of OutgoingOptions to avoid data race when multiple
	// goroutines handle connections concurrently and modify DstCfg/Synchronous
	outgoingOpts := rule.OutgoingOptions
	// Stamp this connection's origin PID (eBPF redirect map) on the per-conn
	// copy so the mock-serve and record-capture paths can attribute the call to
	// the test worker that made it (per-PID scoping). 0 when unavailable.
	outgoingOpts.SrcPid = destInfo.KernelPid
	outgoingOpts.SrcNsPid = destInfo.AgentNsPid
	if p.mockMode {
		starts.Default.Note(fmt.Sprint(clientConnID), outgoingOpts.SrcPid, time.Now())
	}

	mgr := syncMock.Get()
	mgr.SetOutputChannel(rule.MC)
	// The address this connection reached, which every mock its parsers
	// record names (starts.Registry.StampDest), under keploy record as under
	// keploy mock: a replay tells by it what a port held. Kept until they are
	// done, as its process is.
	starts.Default.Dest(fmt.Sprint(clientConnID), dstAddr)
	defer starts.Default.ForgetDest(fmt.Sprint(clientConnID))

	if predialed != nil {
		if predialed.Addr() != dstAddr {
			// Not this connection's: a pre-dial left by an earlier
			// connection from the same (reused) port.
			predialed.CloseIfUnused()
		} else {
			// Until it is used, a destination that closes it closes the
			// application's end too, if the application has sent nothing:
			// that is what the destination would have done to the
			// application's own connection.
			predialed.WatchUntilTaken(func() {
				if util.ReceivedNothing(appConn) {
					_ = appConn.Close()
				}
			})
		}
	}

	// This is used to handle the parser errors
	parserErrGrp, parserCtx := errgroup.WithContext(ctx)
	parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, parserErrGrp)
	parserCtx = context.WithValue(parserCtx, models.ClientConnectionIDKey, fmt.Sprint(clientConnID))
	parserCtx = context.WithValue(parserCtx, models.DestConnectionIDKey, fmt.Sprint(destConnID))
	// Under the proven switch, or settleReads, the accept kept the connection
	// (inflight.go): a parser that reads ahead says through it which requests
	// it holds (integrations.HoldRequest).
	if tracked != nil {
		parserCtx = integrations.WithRequestHolder(parserCtx, tracked)
	}
	if proven {
		parserCtx = containerkey.WithConn(parserCtx, containerkey.Default, fmt.Sprint(clientConnID))
		parserCtx = learn.WithTracker(parserCtx, p.learnTracker)
		// The zone of the process that made the connection, by its origin
		// PID: its calls' clock reads are learned and checked in it, never
		// in the agent's (a zero zone where keploy cannot read it). A
		// docker-mode agent, in a PID namespace of its own, reads none by
		// a PID: it takes the zone keploy's CLI read of the app container
		// that made the connection.
		if p.procZones != nil {
			z, zerr := p.procZones.Of(destInfo.KernelPid)
			if errors.Is(zerr, learn.ErrPIDNamespace) && containerkey.TakesPushed() {
				z, zerr = connZoneOf(ctx, appConn)
			}
			if zerr != nil {
				p.logger.Debug(learn.ZoneUnreadLog+"; its calls are learned in the agent's zone, naming no zone, and a replay here checks a learned clock read in the zone its learn step named (one learned in the application's zone alone and checked at an hour or coarser must hold read in UTC too, and an hour is refused where that zone stands more than an hour from UTC), else in the agent's, refusing one cut to an hour or coarser that it learned in the application's zone alone (one learned in either zone is checked in UTC and the agent's)", zap.Uint32("pid", destInfo.KernelPid), zap.Error(zerr))
			}
			parserCtx = learn.WithProcZone(parserCtx, z)
		}
	}
	// A handshake made under parserCtx (a parser's STARTTLS upgrade, MySQL
	// or Postgres; opportunistic TLS) names its leaf for the IP the client
	// dialled when the client sends no SNI, as the proxy's own handshake
	// does below: a client that dialled an IP literal and checks the
	// server's identity verifies the leaf against an IP SAN.
	parserCtx = withDstIPOf(parserCtx, dstAddr)
	parserCtx, parserCtxCancel := context.WithCancel(parserCtx)

	// Capture the initial srcConn before it gets reassigned (TLS upgrade, wrapping).
	initialSrcConn := srcConn

	// connCloser is started later (after dstConn is established) to close
	// both connections on context cancellation, unblocking any goroutine
	// stuck in a blocking read (ReadBytes → reader.Read).
	var connCloserStarted bool
	startConnCloser := func() {
		if connCloserStarted {
			return
		}
		connCloserStarted = true
		// Capture dstConn NOW (after dial) so there's no data race, and
		// parserCtx's Done too, so a later reassignment of parserCtx cannot
		// race the goroutine.
		closeDst := dstConn
		done := parserCtx.Done()
		go func() {
			<-done
			initialSrcConn.Close()
			if closeDst != nil {
				closeDst.Close()
			}
		}()
	}

	defer func() {
		parserCtxCancel()

		if srcConn != nil {
			// The handler is done: a request still aloft is given no
			// answer. Ended first, as a layer over the kept connection
			// writes as it closes (TLS's close_notify), which would land
			// it as an answer's first bytes do (trackedConn.Write).
			if tracked != nil {
				tracked.endUntold(true)
			}
			util.LogCloseErr(p.logger, srcConn.Close(), "failed to close the source connection", zap.Any("clientConnID", clientConnID))
		}

		if dstConn != nil {
			util.LogCloseErr(p.logger, dstConn.Close(), "failed to close the destination connection")
		}

		err = parserErrGrp.Wait()
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle the parser cleanUp")
		}
	}()

	// With the proxy's life (ctx), not the connection's: what the live
	// handler starts may outlive the connection (LiveHandler). While
	// serveTreeListener runs, parserCtx ends only as ctx does: no parser has
	// joined parserErrGrp yet, and its cancel is the deferred one above.
	served, outside, err := p.serveTreeListener(withLifeOf(ctx, parserCtx), srcConn, destInfo, dstAddr, fmt.Sprint(clientConnID))
	if served {
		return err
	}

	// A mock.real destination (proven switch only) is decided here, before
	// the MySQL probe or anything else reads the connection, and relayed to
	// the real thing (design v14 §4). Anything else continues below, reading
	// any bytes keploy read to name the connection from the returned conn.
	if handled, conn, err := p.serveReal(parserCtx, srcConn, dstAddr, containerRepo, clientConnID, predialed); handled {
		return err
	} else {
		srcConn = conn
	}

	// Opportunistic TLS intercept: a separate passthrough variant
	// where we relay bytes verbatim AND peek for a TLS handshake;
	// when one shows up, we hijack and MITM both halves so the keys
	// land in the keylog fanout sink. Strictly more capable than
	// GlobalPassthrough for sessions where MITM is acceptable, but
	// lives on its own flag so deployments that need true
	// no-MITM passthrough (cert pinning, channel-binding clients,
	// other parser-incompatible workloads) keep their existing
	// semantics. Takes precedence over GlobalPassthrough.
	// From the session's snapshot, as the handshake hold decides it: the
	// proxy field is rewritten by Record while connections are handled.
	if outgoingOpts.OpportunisticTLSIntercept {
		if err := p.opportunisticTLSIntercept(parserCtx, srcConn, dstAddr, predialed, rule.Backdate, outgoingOpts); err != nil {
			utils.LogError(p.logger, err, "opportunistic TLS intercept failed",
				zap.String("server address", dstAddr))
			return err
		}
		return nil
	}

	//check for global passthrough in test mode
	if p.GlobalPassthrough || (!rule.Mocking && (rule.Mode == models.MODE_TEST)) {
		dstConn, err = util.DialDestination(parserCtx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
		if err != nil {
			utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
			return err
		}

		err = p.globalPassThrough(parserCtx, srcConn, dstConn)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle the global pass through")
			return err
		}
		return nil
	}
	// keploy test of a connection to a port a bypassRule with a host or a
	// path names: one whose server speaks first, which shows keploy no host
	// and which keploy record passed through, is passed through before its
	// client's bytes are read (lookUndecidedReplay). Not listed mocked.
	looked := false
	if rule.Mode == models.MODE_TEST {
		look, err := p.lookUndecidedReplay(parserCtx, p.logger, srcConn, dstAddr, predialed, uint(destInfo.Port), outgoingOpts.Rules)
		srcConn = look.Src
		if look.Predialed != nil {
			predialed, looked = look.Predialed, true
			defer predialed.CloseIfUnused()
		}
		if look.Dst != nil {
			dstConn = look.Dst
			return p.passBypassRulesUndecided(parserCtx, p.logger, srcConn, &dstConn, nil, look.Undecided, dstAddr, "")
		}
		if err != nil {
			switch {
			case refusedLocally(err):
				p.logger.Debug("nothing is listening at the destination yet; the caller sees the connection refused", zap.String("server address", dstAddr))
			case errors.Is(err, errLookIncomplete):
				utils.LogError(p.logger, err, "keploy could not tell whether this connection's server speaks first, and closed it", zap.String("server address", dstAddr), zap.String("next_step", "a recovered panic is logged above this line: report both to keploy"))
			default:
				utils.LogError(p.logger, err, "failed to dial the server of a port a bypassRule names, to see whether it speaks first", zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
			}
			return err
		}
	}

	if p.synchronous {
		outgoingOpts.Synchronous = true
	}

	// Decide whether this connection speaks MySQL. Historically this was
	// a pure port check against outgoingOpts.MysqlPorts (defaults
	// [3306, 4000]); probeMysql keeps a listed port as its zero-cost fast
	// path, takes the defaults and learned ports as a hint the connection
	// confirms, and detects every other port — by reading the upstream's
	// handshake during record, and by recalling ports from the recorded
	// mocks during replay. See mysql_detect.go for why the two modes need
	// different mechanisms.
	//
	// The probe may consume bytes off either socket and may dial
	// upstream, so its connections are adopted unconditionally — both
	// on the MySQL path and on the fall-through to generic dispatch.
	// An upstream it dialled and did not keep becomes this connection's
	// pre-dialled one, for the dial below to take.
	probe, err := p.probeMysql(parserCtx, srcConn, dstAddr, predialed, uint32(destInfo.Port), rule.Mode, outgoingOpts, p.logger)
	if probe != nil {
		if probe.SrcConn != nil {
			srcConn = probe.SrcConn
		}
		if probe.DstConn != nil {
			dstConn = probe.DstConn
		}
		if probe.Predialed != nil {
			predialed = probe.Predialed
			defer predialed.CloseIfUnused()
		}
	}
	if err != nil {
		switch {
		case refusedLocally(err):
			p.logger.Debug("nothing is listening at the destination yet; the caller sees the connection refused", zap.String("server address", dstAddr))
		case errors.Is(err, errLookIncomplete):
			utils.LogError(p.logger, err, "keploy could not tell whether this connection speaks MySQL, and closed it", zap.String("server address", dstAddr), zap.String("next_step", "a recovered panic is logged above this line: report both to keploy"))
		default:
			utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
		}
		return err
	}
	p.logger.Debug("mysql probe result",
		zap.Bool("isMySQL", probe.IsMySQL),
		zap.String("reason", probe.Reason),
		zap.Uint32("destPort", destInfo.Port))

	// A server-first connection, and a MySQL one, shows no host or path: a
	// bypassRule with one for this port passes it through
	// (bypassRulesUndecided), as the kernel passed the rule's whole port
	// before, and not as a protocol keploy cannot record (no loss is
	// charged). keploy refuses one at start for a port listed in mysqlPorts.
	// In a record, the probe's connection (dstConn, or the one it handed
	// back) replays what it read of the server.
	serverFirst := probe.Reason == reasonServerFirst || probe.Reason == reasonRefusedAtAccept
	if serverFirst || probe.IsMySQL {
		if undecided := bypassRulesUndecided(p.logger, outgoingOpts.Rules, uint(destInfo.Port), "", false); len(undecided) > 0 {
			return p.passBypassRulesUndecided(parserCtx, p.logger, srcConn, &dstConn, func() (net.Conn, error) {
				return util.DialDestination(parserCtx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
			}, undecided, dstAddr, "")
		}
	}
	if rule.Mode != models.MODE_TEST && serverFirst {
		// The destination spoke first with a server-first protocol's
		// greeting (on a port already learned, the probe looked again at a
		// destination marked as one), whose client waits to be greeted; or,
		// under the switch, refused the connection at accept. Relayed from
		// the greeting, not dispatched on the client's first bytes (P250).
		return p.relayServerFirst(parserCtx, srcConn, dstAddr, predialed, probe.Reason == reasonRefusedAtAccept, p.logger.With(zap.String("Destination Address", dstAddr)))
	}

	if probe.IsMySQL {
		// Recorded, or answered from mocks: under the proven switch a
		// local destination is listed mocked (design v14 §3.1), once
		// nothing passes it through (above): a destination keploy relays
		// is reached for real, so listing it would claim an isolation that
		// did not happen. The other connections are listed below, past
		// the last pass-through.
		if proven {
			p.noteMockedLocal(dstAddr, destInfo.Port, outside, ctrKey)
		}
		if rule.Mode != models.MODE_TEST {
			// probeMysql already dialed when it had to read the
			// upstream greeting; dstConn then replays those bytes.
			if dstConn == nil {
				dstConn, err = util.DialDestination(parserCtx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
				if err != nil {
					utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
					return err
				}
			}
			dstCfg := &models.ConditionalDstCfg{
				Addr: dstAddr,
				Port: uint(destInfo.Port),
			}
			outgoingOpts.DstCfg = dstCfg

			mysqlLogger := p.logger.With(
				zap.String("Client ConnectionID", fmt.Sprint(clientConnID)),
				zap.String("Destination ConnectionID", fmt.Sprint(destConnID)),
				zap.String("Destination Address", dstAddr),
			)
			// This branch used to build a legacy RecordSession and call
			// RecordOutgoing directly, with no IsV2 gate — so a MySQL
			// connection reaching keploy through the PROBE recorded
			// legacy on the default configuration, while the very same
			// parser recorded via the supervisor when it arrived through
			// the matched-parser branch a few hundred lines below. Two
			// dispatch sites, two different answers, for one parser.
			//
			// keploy#4526 fixed the identical omission on the generic
			// catch-all and deliberately left this one alone, because
			// MySQL's V2 path leaked the client's ClientHello upstream
			// in cleartext and needed its own soak. That leak is fixed
			// (the client write hold), so the gate goes on here too and
			// shouldRecordViaSupervisor becomes the single answer for
			// every dispatch site.
			// Built HERE, in handleConnection scope, so it holds pointers to
			// the real srcConn/dstConn variables the deferred close uses —
			// not to copies. See recordMySQLOutgoing's doc comment.
			mysqlUpgrader := util.NewConnTLSUpgrader(&srcConn, &dstConn, p.logger, p.clientTLSHandler(dstAddr, srcProcess(outgoingOpts)))
			return p.recordMySQLOutgoing(parserCtx, srcConn, dstConn, rule.MC,
				parserErrGrp, mysqlLogger, clientConnID, destConnID, outgoingOpts, mysqlUpgrader)
		}

		m := p.getMockManager()
		if m == nil {
			utils.LogError(p.logger, nil, "failed to fetch the mock manager")
			return err
		}
		return p.mockMySQLOutgoing(parserCtx, srcConn, dstAddr, m, outgoingOpts)
	}

	reader := bufio.NewReader(srcConn)
	// Peek 5 bytes first — exactly what the TLS record header needs,
	// and the widest Peek we can do without risking a deadlock on
	// short plaintext greetings (e.g. Redis 'PING\r\n' is 6 bytes and
	// some protocols send <8 bytes then wait for a server reply).
	testBuffer, err := reader.Peek(5)
	if err != nil {
		if err == io.EOF && len(testBuffer) == 0 {
			p.logger.Debug("received EOF, closing conn", zap.Any("connectionID", clientConnID), zap.Error(err))
			return nil
		}
		// Network closed errors are expected when client closes connection (e.g., app shutdown)
		if isShutdownError(err) || isNetworkClosedErr(err) {
			p.logger.Debug("failed to peek the request message in proxy (connection closed)", zap.Uint32("proxy port", p.Port), zap.Error(err))
		} else {
			utils.LogError(p.logger, err, "failed to peek the request message in proxy", zap.Uint32("proxy port", p.Port))
		}
		return err
	}

	// ── Postgres SSLRequest handshake (opt-in) ──
	// When agent.InterceptPostgresSSLRequest is enabled, the proxy itself
	// replies 'S' and upgrades to TLS. Disabled by default because the
	// default build registers a Postgres parser (via extraparsers.go)
	// that already handles SSLRequest through the TLSUpgrader interface;
	// double-handling breaks the parser-driven flow. Downstream builds
	// that ship without a Postgres parser (pure proxy-mode) flip the
	// flag on via agent.SetInterceptPostgresSSLRequest.
	//
	// This block handles the client-facing side of SSLRequest: read
	// the 8-byte preamble, reply 'S', and terminate TLS with the
	// client. It is NOT client-side-only for the whole handshake —
	// when the destination is port 5432, the upstream dial path
	// (dialPostgresSSLUpstream) independently originates its own
	// Postgres SSLRequest before wrapping the upstream socket in
	// tls.Client. See the runtime_hooks.go docstring on
	// InterceptPostgresSSLRequest for the full deployment-shape
	// matrix. End-to-end MITM against a vanilla Postgres now works
	// under this flag; a registered parser-driven TLSUpgrader, when
	// one is wired via pkg/agent/proxy/extraparsers.go, remains the
	// richer option when you want protocol-aware mocking.
	//
	// We only extend the Peek to 8 bytes when the first 5 bytes match
	// the SSLRequest prefix (`00 00 00 08 04`). That prefix is unique
	// enough to rule out TLS (0x16 ...), CONNECT ("CONN"), and other
	// common greetings, so the extra 3-byte wait is deterministic for
	// exactly the one client that's actually speaking SSLRequest.
	// This keeps the short-greeting deadlock (Redis PING, memcached
	// 'quit', etc.) off the default code path.
	if agent.InterceptPostgresSSLRequest && isPostgresSSLRequestPrefix(testBuffer) {
		sslBuf, perr := reader.Peek(8)
		if perr != nil {
			utils.LogError(p.logger, perr, "client started with a Postgres SSLRequest prefix (00 00 00 08 04) but did not deliver the full 8-byte packet; the connection likely dropped mid-handshake — verify the client is a standard libpq/pgx/pq and check for a mismatched sslmode or an intermediary terminating the TCP stream",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return perr
		}
		testBuffer = sslBuf
	}
	// keploy answered the client's Postgres SSLRequest itself: its server
	// has not seen it, so the client's TLS cannot be handed to the server
	// as it is (the pass-through before keploy's TLS, below).
	answeredSSLRequest := false
	if agent.InterceptPostgresSSLRequest && isPostgresSSLRequest(testBuffer) {
		answeredSSLRequest = true
		p.logger.Debug("Postgres SSLRequest detected, accepting and upgrading to TLS",
			zap.Int("sourcePort", sourcePort),
			zap.String("dstAddr", dstAddr),
		)
		// Consume the 8-byte SSLRequest from the buffered reader so the
		// downstream parser never sees it.
		if _, derr := reader.Discard(8); derr != nil {
			utils.LogError(p.logger, derr, "failed to discard Postgres SSLRequest bytes; the 8-byte SSLRequest was peeked but the bufio reader could not be advanced. This usually means the underlying TCP connection was reset between peek and discard — retry the client connection, and if it persists capture a packet trace between the client and the proxy listener",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return derr
		}
		// Reply 'S' (TLS accepted). Write straight to the underlying TCP
		// connection; writes bypass the buffered reader.
		if _, werr := srcConn.Write([]byte{'S'}); werr != nil {
			utils.LogError(p.logger, werr, "failed to write 'S' SSLResponse to Postgres client; the proxy accepted the SSLRequest but could not send the one-byte acknowledgment. Verify the client is still connected (not a half-closed stream), check for client-side read timeouts shorter than the proxy's accept latency, and confirm no firewall is dropping 1-byte segments",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return werr
		}
		// Re-peek for the TLS ClientHello. The client app sends it as soon
		// as it gets 'S'. 5 bytes is enough for IsTLSHandshake().
		testBuffer, err = reader.Peek(5)
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				p.logger.Debug("Postgres client closed conn after SSLRequest reply")
				return nil
			}
			// Any non-nil error here means we could not read a complete
			// 5-byte TLS record header — either the client sent fewer
			// than 5 bytes before closing (EOF with partial buffer) or
			// the network errored out. Falling through would leave the
			// downstream TLS detection running against a truncated
			// prefix and misclassify the stream as plaintext, so bail.
			utils.LogError(p.logger, err, "failed to read a complete 5-byte TLS record header after replying 'S' to the Postgres SSLRequest; the client did not deliver enough bytes to identify a TLS ClientHello. Check the client's sslmode (require/verify-* should always follow with ClientHello after 'S'), confirm InterceptPostgresSSLRequest is only enabled in pure-proxy builds without a Postgres parser, and capture the bytes sent immediately after 'S' to verify a full TLS record header arrives (starting with 0x16)",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr),
				zap.Int("bytesRead", len(testBuffer)))
			return err
		}
	}

	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: reader,
		Logger: p.logger,
	}

	// ── CONNECT tunnel handling ──
	// If the first bytes are an HTTP CONNECT request (app is using a corporate
	// HTTP proxy), we handle the CONNECT handshake here, then re-enter the
	// normal TLS MITM + parser flow on the unwrapped tunnel.
	//
	// Record mode: forward CONNECT to the real proxy, relay the 200, then MITM.
	// Test mode:   respond with 200 directly (no external proxy needed).
	var connectResult *connectTunnelResult
	// The host the CONNECT request named, "" without one: what the client
	// asked for until its ClientHello names something (SNI), so what the
	// MITM leaf is named for and keploy dials when it sends no SNI.
	var connectTargetHost string
	if isConnectRequest(testBuffer) {
		p.logger.Debug("Detected HTTP CONNECT request, handling tunnel",
			zap.Int("sourcePort", sourcePort),
			zap.String("dstAddr", dstAddr),
		)

		isTestMode := rule.Mode == models.MODE_TEST

		// In record mode, we need a connection to the corporate proxy.
		var proxyConn net.Conn
		if !isTestMode {
			proxyConn, err = util.DialDestination(ctx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
			if err != nil {
				utils.LogError(p.logger, err, "failed to dial corporate proxy for CONNECT; verify the proxy address is correct, DNS/network is reachable, and HTTP_PROXY/HTTPS_PROXY settings are configured correctly",
					zap.String("proxy_addr", dstAddr))
				return err
			}
			// dstConn from the moment it is dialled, so the deferred close
			// owns it on every return below — including the ones after the
			// tunnel is up and before the inner bytes arrive, which used to
			// return with it unreferenced. Once the tunnel is established it
			// is the tunneled connection the TLS dial below wraps.
			dstConn = proxyConn
		}

		connectResult, err = handleConnectTunnel(p.logger, srcConn, proxyConn, isTestMode)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle CONNECT tunnel; check HTTP_PROXY/HTTPS_PROXY settings, proxy authentication (407), DNS/network reachability to the proxy, and egress firewall rules")
			return err
		}
		connectTargetHost = connectResult.TargetHost
		if connectResult.DstReader != nil && connectResult.DstReader.Buffered() > 0 {
			// keploy record: what the corporate proxy's reader holds past its
			// 200 is the server's (a greeting), so every reader of the tunnel
			// below (keploy's TLS dial, a parser, a pass-through) reads it
			// first.
			dstConn = &util.Conn{Conn: dstConn, Reader: connectResult.DstReader, Logger: p.logger}
		}
		// A parser that terminates TLS on this connection later (a STARTTLS
		// upgrade) names its leaf for the target too.
		parserCtx = pTls.WithConnectTarget(parserCtx, connectTargetHost)

		// The CONNECT handshake is now complete. The app will send the next
		// bytes (typically a TLS ClientHello) over the same srcConn.
		// We need to re-peek to detect TLS on the inner connection.
		//
		// Also update dstAddr and destInfo to reflect the real target
		// (e.g., api.example.com:443) instead of the corporate proxy.
		dstAddr = connectResult.TargetAddr
		destInfo.Port = 443 // CONNECT targets are almost always TLS on 443
		if connectResult.TargetPort != "" {
			if portNum, err := strconv.ParseUint(connectResult.TargetPort, 10, 16); err == nil && portNum >= 1 && portNum <= 65535 {
				destInfo.Port = uint32(portNum)
			}
		}

		// Re-peek the next bytes to detect TLS on the inner tunnel.
		// Use the BufferedReader from handleConnectTunnel to preserve any
		// bytes it read ahead (e.g., TLS ClientHello pipelined by the client).
		innerReader := connectResult.BufferedReader
		testBuffer, err = innerReader.Peek(5)
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				p.logger.Debug("CONNECT tunnel closed immediately after handshake")
				return nil
			}
			if err != io.EOF {
				utils.LogError(p.logger, err, "failed to peek inner tunnel data after CONNECT")
				return err
			}
			// Partial read with EOF — proceed with what we have.
		}

		// Wrap the raw TCP connection with innerReader for subsequent reads.
		// innerReader is the bufio.Reader from handleConnectTunnel that may
		// hold pipelined bytes (TLS ClientHello) in its internal buffer.
		srcConn = &util.Conn{
			Conn:   stripUtilConn(srcConn),
			Reader: innerReader,
			Logger: p.logger,
		}

		p.logger.Debug("CONNECT tunnel established, proceeding with inner connection",
			zap.String("target", connectResult.TargetAddr),
			zap.Bool("innerTLS", pTls.IsTLSHandshake(testBuffer)),
		)
	}

	var clientPeerCert *x509.Certificate
	var isMTLS bool
	// The SNI of the client's ClientHello, read off its handshake with the
	// MITM below; "" when it sent none.
	var clientSNI string
	isTLS := pTls.IsTLSHandshake(testBuffer)
	// The TLS server the client asked for: its SNI, else its CONNECT target
	// ("" when not TLS, or none).
	var serverName string

	if isTLS && !answeredSSLRequest && hostRulesAt(outgoingOpts.Rules, uint(destInfo.Port)) {
		// A bypassRule with a host and no path that names the server the
		// client's ClientHello asks for (its SNI, else its CONNECT target)
		// passes the connection through before keploy's TLS, in every mode:
		// the client's handshake reaches its server untouched, so a client
		// certificate (mTLS) and a pinned server certificate work as they
		// do without keploy, and nothing of the connection is recorded or
		// mocked. A ClientHello that names no server, on a port such a rule
		// names, goes on to keploy's TLS, and then to the rules it cannot
		// decide (bypassRulesUndecided). What was read of the client is
		// handed on ahead of the rest, for whichever reads it next.
		name, hello, helloErr := serverNameOf(srcConn)
		read := srcConn
		srcConn = &util.Conn{Conn: stripUtilConn(read), Reader: io.MultiReader(bytes.NewReader(hello), read), Logger: p.logger}
		if name == "" {
			name = connectTargetHost
		}
		if bypassRule, ok := bypassByServerName(p.logger, outgoingOpts.Rules, name, uint(destInfo.Port)); ok && helloErr == nil {
			p.logger.Debug("a bypassRule names the TLS server the client asks for; passing the connection through before keploy's TLS",
				zap.String("serverName", name), zap.String("ruleHost", bypassRule.Host), zap.Uint("rulePort", bypassRule.Port))
			return p.passBypassRulesUndecided(parserCtx, p.logger, srcConn, &dstConn, func() (net.Conn, error) {
				return util.DialDestination(parserCtx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
			}, nil, dstAddr, name)
		}
	}

	if isTLS {
		probeProxy(p.logger, "tls-handshake-start", clientConnID,
			zap.Int("srcPort", sourcePort),
			zap.Uint32("dstPort", destInfo.Port),
			zap.String("dstAddr", dstAddr),
		)
		tlsStart := time.Now()
		// On replay, decide the MITM's ALPN offer PER-CONNECTION. If THIS
		// destination has a recorded kind:Http2 mock, advertise h2 so a client
		// offering it stays on HTTP/2 and its request matches the mock (instead
		// of being downgraded to http/1.1 and finding no matching mock). The
		// check is scoped to the destination on purpose: a mixed session (some
		// HTTP/2 targets, some HTTP/1.1) must not push the HTTP/1.1 targets onto
		// h2, or their recorded http/1.1 mocks would never match. Record and
		// http/1.1-only recordings are unaffected (PreferH2 stays false).
		//
		// The destination is (sniHost, port). port (destInfo.Port) is always
		// known. sniHost is only known pre-handshake when the connection made
		// a CONNECT request — the CONNECT-tunnel / sidecar path (connect.go); on
		// the transparent proxyless path the SNI only becomes available inside
		// the handshake, so sniHost is empty here and scoping falls back to
		// port-only (best-effort — see http2DestMatches). Port already
		// disambiguates the common mixed-session case (services on different
		// ports); same-port-different-host disambiguation only engages where
		// sniHost is present.
		//
		// A client that sends no SNI gets a leaf named for what this
		// connection itself says of its destination: the host its CONNECT
		// request named, else the IP it dialled — a client that dialled an IP
		// literal sends no SNI and checks the leaf's IP SAN. Both go to the
		// MITM on hsCtx, so nothing another connection left can name it.
		hsCtx := withDstIPOf(pTls.WithConnectTarget(ctx, connectTargetHost), dstAddr)
		if rule.Mode == models.MODE_TEST {
			sniHost, _ := hostPortFromAuthority(connectTargetHost, "")
			preferH2 := rule.OutgoingOptions.PreferH2
			if !preferH2 {
				preferH2 = p.replayTargetHasHTTP2Mock(sniHost, destInfo.Port)
			}
			if preferH2 {
				// On hsCtx, not ctx: the destination IP set above must
				// survive the hint.
				hsCtx = pTls.WithPreferH2(hsCtx)
				p.logger.Debug("replay: advertising h2 ALPN (destination has Http2 mock)",
					zap.String("sniHost", sniHost), zap.Uint32("dstPort", destInfo.Port))
			}
		}
		// Into its own variable, not srcConn: a failed handshake returns a
		// nil conn, and assigning that over srcConn would hide the
		// connection from the deferred close.
		var tlsSrcConn net.Conn
		// What the handshake, and the client just after it, lose is charged
		// to the destination (clientTLS).
		hsCtx = p.withClientTLS(hsCtx, dstAddr, srcProcess(outgoingOpts))
		tlsSrcConn, isMTLS, err = pTls.HandleTLSConnection(hsCtx, p.logger, srcConn, rule.Backdate)
		probeProxy(p.logger, "tls-handshake-done", clientConnID,
			zap.Int("srcPort", sourcePort),
			zap.Int64("dur_ns", time.Since(tlsStart).Nanoseconds()),
			zap.Bool("isMTLS", isMTLS),
			zap.Error(err),
		)
		if err != nil {
			err = pTls.HandshakeCause(err)
			utils.LogError(p.logger, err, "failed to handle TLS conn")
			return err
		}
		srcConn = tlsSrcConn
		clientSNI = util.ClientServerName(tlsSrcConn)

		if isMTLS {
			if tlsConn, ok := srcConn.(*tls.Conn); ok {
				state := tlsConn.ConnectionState()
				if len(state.PeerCertificates) > 0 {
					clientPeerCert = state.PeerCertificates[0]
				}
			}
		}
	}

	clientID, ok := parserCtx.Value(models.ClientConnectionIDKey).(string)
	if !ok {
		utils.LogError(p.logger, err, "failed to fetch the client connection id")
		return err
	}

	destID, ok := parserCtx.Value(models.DestConnectionIDKey).(string)
	if !ok {
		utils.LogError(p.logger, err, "failed to fetch the destination connection id")
		return err
	}

	logger := p.logger.With(zap.String("Client ConnectionID", clientID), zap.String("Destination ConnectionID", destID), zap.String("Destination Address", dstAddr), zap.String("Client IP Address", srcConn.RemoteAddr().String()))

	var initialBuf []byte
	// attempt to read conn until buffer is either filled or conn is closed
	initialBuf, err = util.ReadInitialBuf(parserCtx, p.logger, srcConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial buffer")
		return err
	}
	if isTLS && len(initialBuf) > 0 {
		// The client sent a call through keploy's certificate: its
		// process trusts keploy's CA (caTrust), and its hang-ups held in
		// this test are cancels.
		p.noteTLSCall(outgoingOpts.SrcPid)
	}

	if util.IsHTTPReq(initialBuf) && !util.HasCompleteHTTPHeaders(initialBuf) {
		// HTTP headers are never chunked according to the HTTP protocol,
		// but at the TCP layer, we cannot be sure if we have received the entire
		// header in the first buffer chunk. This is why we check if the headers are complete
		// and read more data if needed.

		// Some HTTP requests, like AWS SQS, may require special handling in Keploy Enterprise.
		// These cases may send partial headers in multiple chunks, so we need to read until
		// we get the complete headers.

		logger.Debug("Partial HTTP headers detected, reading more data to get complete headers")

		// Read more data from the TCP connection to get the complete HTTP headers.
		headerBuf, err := util.ReadHTTPHeadersUntilEnd(parserCtx, p.logger, srcConn)
		if err != nil {
			// Log the error if we fail to read the complete HTTP headers.
			utils.LogError(logger, err, "failed to read the complete HTTP headers from client")
			return err
		}
		// Append the additional data to the initial buffer.
		initialBuf = append(initialBuf, headerBuf...)
	}

	// For HTTP/2 connections, the initial buffer may only contain the client
	// preface and SETTINGS frames. Protocol matchers (gRPC vs h2c) need to
	// inspect the HEADERS frame's content-type to decide.
	//
	// Without the proven switch a short timeout read is used instead of a
	// blocking read because:
	//  - h2c clients typically send preface+SETTINGS+HEADERS in quick succession
	//  - gRPC clients send preface+SETTINGS, then WAIT for server SETTINGS_ACK
	//    before sending HEADERS — a blocking read would deadlock.
	//
	// If HEADERS arrives within the timeout → matchers can inspect content-type.
	// If it doesn't (gRPC pattern) → matchers treat missing HEADERS as gRPC.
	//
	// Under the proven switch nothing is decided from a silence (row W3).
	// On replay keploy is the server: it answers the client preface with
	// its own, and reads until the HEADERS arrive, however long the client
	// takes (readHTTP2UntilHeaders). On record the HTTP/2 parser claims
	// every HTTP/2 connection whatever its HEADERS say, and the real server
	// answers the preface, so nothing is waited for.
	if provenswitch.On() && isHTTP2PrefacePrefix(initialBuf) {
		// Part of the client preface came in the first read: the rest is on
		// its way, and is read before anything is decided on it.
		initialBuf, err = readHTTP2Preface(parserCtx, srcConn, initialBuf)
		if err != nil {
			utils.LogError(logger, err, "failed to read the HTTP/2 client preface")
			return err
		}
	}
	if provenswitch.On() && util.IsHTTP2Preface(initialBuf) && !util.HasHTTP2HeadersFrame(initialBuf) {
		if rule.Mode == models.MODE_TEST {
			logger.Debug("HTTP/2 preface detected but no HEADERS frame yet; answering the preface and reading until HEADERS")
			initialBuf, srcConn, err = readHTTP2UntilHeaders(parserCtx, srcConn, initialBuf)
			if err != nil {
				utils.LogError(logger, err, "failed to read HTTP/2 HEADERS frame from client")
				return err
			}
		}
	} else if util.IsHTTP2Preface(initialBuf) && !util.HasHTTP2HeadersFrame(initialBuf) {
		logger.Debug("HTTP/2 preface detected but no HEADERS frame yet, attempting short timeout read")

		const http2HeadersReadTimeout = 150 * time.Millisecond
		moreBuf, err := util.ReadWithTimeout(srcConn, http2HeadersReadTimeout)
		if err != nil {
			utils.LogError(logger, err, "failed to read HTTP/2 HEADERS frame from client")
			return err
		}
		if len(moreBuf) > 0 {
			initialBuf = append(initialBuf, moreBuf...)
		}
	}

	//update the src connection to have the initial buffer
	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: util.NewPrefixReader(initialBuf, srcConn),
		Logger: p.logger,
	}

	dstCfg := &models.ConditionalDstCfg{
		Port: uint(destInfo.Port),
	}

	//make new connection to the destination server
	if isTLS {

		// The name the client asked for: its SNI, else its CONNECT target.
		dstURL := clientSNI
		if dstURL == "" {
			dstURL = connectTargetHost
		}
		// A bypassRule with a host and no path that names this server
		// passed the connection through before keploy's TLS (above).
		serverName = dstURL

		logger.Debug("the external call is tls-encrypted", zap.Bool("isTLS", isTLS))

		nextProtos := []string{"http/1.1"} // default safe

		isHTTP := util.IsHTTPReq(initialBuf)
		isCONNECT := bytes.HasPrefix(initialBuf, []byte("CONNECT "))
		logger.Debug("ALPN decision debug",
			zap.Bool("isHTTPReq", isHTTP),
			zap.Bool("isCONNECT", isCONNECT),
			zap.Int("initialBufLen", len(initialBuf)),
		)

		// Allow H2 if:
		// 1. It's not an HTTP/1.x request (could be gRPC/HTTP2 frames), OR
		// 2. It's a CONNECT request (used by gRPC parser for tunneling, ALB requires H2)
		if !isHTTP || isCONNECT {
			// not an HTTP/1.x request line; could be HTTP/2 (gRPC) frames
			nextProtos = []string{"h2", "http/1.1"}
			logger.Debug("Offering H2 for ALPN", zap.Strings("nextProtos", nextProtos))
		} else {
			logger.Debug("NOT offering H2 (HTTP/1.x detected)", zap.Strings("nextProtos", nextProtos))
		}

		serverName := resolveUpstreamServerName(dstURL, connectTargetHost, dstAddr, outgoingOpts.UpstreamTLSVerify)

		cfg := &tls.Config{
			// Off by default: keploy must never be stricter than the app it
			// records. An app on sslmode=require / tls=skip-verify chose not
			// to authenticate its upstream, and a verification failure here
			// is silent — the supervisor falls through to raw passthrough,
			// the app keeps working and the mock is DROPPED. Opt in with
			// record.upstreamTls.verify. Not a CA-bundle limitation:
			// crypto/tls uses the platform root pool when RootCAs is nil.
			InsecureSkipVerify: !outgoingOpts.UpstreamTLSVerify, // nolint:gosec
			RootCAs:            outgoingOpts.UpstreamTLSRootCAs,
			ServerName:         serverName,
			NextProtos:         nextProtos,
			KeyLogWriter:       pTls.KeyLogWriter(),
		}

		// In keploy test the server is dialled only for a call a bypassRule
		// passes through, with this cfg: an HTTP call the HTTP parser
		// matches by its Host and path, or a connection keploy cannot
		// decide (bypassRulesUndecided).
		scopedRules := len(outgoingOpts.Rules) > 0
		if isMTLS && (rule.Mode != models.MODE_TEST || scopedRules) && clientPeerCert != nil {
			err = p.applyMTLSClientCert(cfg, clientPeerCert, outgoingOpts.TLSPrivateKey)
			if err != nil && rule.Mode != models.MODE_TEST {
				return err
			}
			if err != nil {
				logger.Debug("no client certificate for a call a bypassRule may pass through", zap.Error(err))
			}
		}

		addr := dstAddr
		if dstURL != "" {
			addr = net.JoinHostPort(dstURL, fmt.Sprint(destInfo.Port))
			predialed.Alias(addr) // the connection dialled while the handshake was held, if it was
		}

		if rule.Mode != models.MODE_TEST {
			if connectResult != nil && dstConn != nil {
				// CONNECT tunnel: dstConn is already connected through the
				// corporate proxy tunnel, and reads first what the proxy's
				// reader holds past its 200 (above).
				tlsConn := tls.Client(dstConn, cfg)
				if err := tlsConn.Handshake(); err != nil {
					utils.LogError(logger, err, "failed TLS handshake over CONNECT tunnel; verify the corporate proxy allows CONNECT to this target, check proxy auth/egress rules, and confirm the target hostname is reachable",
						zap.String("target", addr))
					return err
				}
				dstConn = tlsConn
				logger.Debug("TLS over CONNECT tunnel established",
					zap.String("protocol", tlsConn.ConnectionState().NegotiatedProtocol),
					zap.String("target", addr))
			} else {
				// When InterceptPostgresSSLRequest is on AND the
				// destination is a Postgres service (port 5432 is
				// the reliable-enough heuristic — documented in the
				// hook docstring), precede the TLS dial with the
				// Postgres SSLRequest preamble the server expects.
				// Otherwise tls.Dial to 5432 sends a ClientHello
				// directly, which a vanilla Postgres rejects — the
				// flag's client-side 'S' reply would then succeed
				// while the upstream connection fails, leaving the
				// replayed client staring at a broken stream.
				if agent.InterceptPostgresSSLRequest && destInfo.Port == 5432 {
					probeProxy(p.logger, "upstream-dial-start", clientConnID, zap.String("branch", "pg-ssl"), zap.String("addr", addr))
					dialStart := time.Now()
					dstConn, err = dialPostgresSSLUpstream(ctx, strconv.Itoa(sourcePort), addr, cfg, p.logger, p.cbshim, predialed)
					probeDial(p.logger, "pg-ssl-upstream", clientConnID, addr, time.Since(dialStart).Nanoseconds(), zap.Error(err))
					// Release cbshim's per-connection rendezvous state
					// at connection exit. dialPostgresSSLUpstream may
					// have already called RegisterReal; if the matching
					// MITM half from CertForClient never arrives
					// (handshake fails, postgres rejects auth pre-SCRAM,
					// client disconnects mid-handshake, etc.), Publish
					// never fires and the pending entry would leak
					// until process exit. Snapshot the handle so a
					// concurrent SetCBShim(nil) during shutdown can't
					// race the deferred call. No-op when Publish has
					// already drained the entry.
					if cb := p.cbshim; cb != nil {
						connID := strconv.Itoa(sourcePort)
						defer cb.CleanupConnection(connID)
					}
				} else {
					probeProxy(p.logger, "upstream-dial-start", clientConnID, zap.String("branch", "synchronous"), zap.String("addr", addr))
					dialStart := time.Now()
					dstConn, err = util.DialDestinationTLS(ctx, p.logger, "tcp", util.DialTarget{Addr: addr, Predialed: predialed}, cfg)
					probeDial(p.logger, "synchronous-tls", clientConnID, addr, time.Since(dialStart).Nanoseconds(), zap.Error(err))
				}
				if err != nil {
					utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", addr), zap.String("next_step", util.NextStepDialDestination))
					return err
				}

				conn := dstConn.(*tls.Conn)
				state := conn.ConnectionState()

				p.logger.Debug("Negotiated protocol:", zap.String("protocol", state.NegotiatedProtocol))
			}
		}

		dstCfg.TLSCfg = cfg
		dstCfg.Addr = addr
	} else {
		// Only dial if dstConn not already set (e.g., from PostgreSQL SSL handling)
		if rule.Mode != models.MODE_TEST && dstConn == nil {
			probeProxy(p.logger, "upstream-dial-start", clientConnID, zap.String("branch", "plain-tcp"), zap.String("addr", dstAddr))
			dialStart := time.Now()
			dstConn, err = util.DialDestination(parserCtx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
			probeDial(p.logger, "plain-tcp", clientConnID, dstAddr, time.Since(dialStart).Nanoseconds(), zap.Error(err))
			if err != nil {
				if refusedLocally(err) {
					logger.Debug("nothing is listening at the destination yet; the caller sees the connection refused", zap.String("server address", dstAddr))
					return err
				}
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
				return err
			}
		}
		dstCfg.Addr = dstAddr
	}

	outgoingOpts.DstCfg = dstCfg

	// Start the shutdown goroutine now that dstConn is established.
	startConnCloser()

	// get the mock manager for the current app
	m := p.getMockManager()
	if m == nil {
		utils.LogError(logger, err, "failed to fetch the mock manager")
		return err
	}

	generic := true

	var matchedParser integrations.Integrations
	var parserType integrations.IntegrationType

	//Checking for all the parsers according to their priority.
	for _, parserPair := range p.integrationsPriority { // Iterate over ordered priority list
		parser, exists := p.Integrations[parserPair.ParserType]
		if !exists {
			continue // Skip if parser not found
		}

		p.logger.Debug("Checking for the parser", zap.String("ParserType", string(parserPair.ParserType)))
		if parser.MatchType(parserCtx, initialBuf) {
			matchedParser = parser
			parserType = parserPair.ParserType
			generic = false
			break
		}
	}

	probeProxy(p.logger, "integration-select", clientConnID,
		zap.String("parser", string(parserType)),
		zap.Bool("generic", generic),
	)

	// A bypassRule with a host or a path is applied by the HTTP parser (each
	// call's Host and path) and above (a TLS connection's server name). A
	// connection that shows keploy no host or path for one is passed
	// through, as the kernel passed the rule's whole port before, and keploy
	// says so once for each rule (bypassRulesUndecided). In keploy test the
	// server is dialled here, as the HTTP parser dials it for a call.
	// A plain CONNECT tunnel shows its target's host.
	if serverName == "" {
		serverName = connectTargetHost
	}
	httpParsed := !generic && parserType == integrations.HTTP
	dialServer := func() (net.Conn, error) {
		target := util.DialTarget{Addr: dstCfg.Addr, Predialed: predialed}
		if dstCfg.TLSCfg != nil {
			return util.DialDestinationTLS(parserCtx, logger, "tcp", target, dstCfg.TLSCfg)
		}
		return util.DialDestination(parserCtx, logger, "tcp", target)
	}
	// A connection to a server a rule with a host and no path names
	// (bypassByServerName) that is still here is passed through whole: a
	// plain CONNECT tunnel that is not HTTP inside (no parser sees a host
	// in it), or a TLS one that could not be passed through before keploy's
	// TLS (keploy answered its client's Postgres SSLRequest, or its
	// ClientHello could not be read whole), passed through after it, as
	// before. An HTTP call in a plain tunnel is the HTTP parser's to decide.
	if serverName != "" && (isTLS || !httpParsed) {
		if _, ok := bypassByServerName(logger, outgoingOpts.Rules, serverName, uint(destInfo.Port)); ok {
			return p.passBypassRulesUndecided(parserCtx, logger, srcConn, &dstConn, dialServer, nil, dstAddr, serverName)
		}
	}
	if undecided := bypassRulesUndecided(logger, outgoingOpts.Rules, uint(destInfo.Port), serverName, httpParsed); len(undecided) > 0 {
		return p.passBypassRulesUndecided(parserCtx, logger, srcConn, &dstConn, dialServer, undecided, dstAddr, serverName)
	}
	if looked {
		// Nothing below dials with it in keploy test: a call a bypassRule
		// passes through dials its own (the HTTP parser's
		// passOneCallThrough). So the server lookUndecidedReplay reached for
		// this connection is let go now, not held open, unused, while the
		// connection lasts, where it would keep a server that serves one
		// connection at a time from serving it.
		predialed.CloseIfUnused()
	}
	// From here the connection is recorded, or answered from mocks: under
	// the proven switch a local destination is listed mocked (design v14
	// §3.1). Not before: a connection a bypassRule passes through (above),
	// or a server-first one a record relays (relayServerFirst), is reached
	// for real, and listing it would claim an isolation that did not
	// happen. An HTTP connection a bypassRule with a host or a path may
	// pass calls of through is listed at its first call the parser records
	// or mocks (integrations.CallServed), so one whose every call the rule
	// passes through is not listed.
	if proven {
		if httpParsed && callRulesAt(outgoingOpts.Rules, uint(destInfo.Port)) {
			var listed sync.Once
			parserCtx = integrations.WithCallServed(parserCtx, func() {
				listed.Do(func() { p.noteMockedLocal(dstAddr, destInfo.Port, outside, ctrKey) })
			})
		} else {
			p.noteMockedLocal(dstAddr, destInfo.Port, outside, ctrKey)
		}
	}

	// Record-mode parsing kill switch: if record-time parsing has been
	// disabled via env var (KEPLOY_DISABLE_PARSING), SIGUSR1, or admin
	// endpoint, skip parser dispatch and hand the connection to
	// globalPassThrough. Existing connections whose parsers are already
	// running are unaffected; this only gates new dispatch.
	//
	// MODE_TEST is intentionally NOT gated here — the kill switch is a
	// record-time stability escape hatch only; replay must continue to
	// match mocks regardless of record-time kill-switch state.
	if rule.Mode == models.MODE_RECORD && util.DefaultKillSwitch.Enabled() {
		logger.Debug("record-mode parsing kill switch tripped; routing to passthrough",
			zap.String("parser", string(parserType)), zap.Bool("generic", generic))
		return p.globalPassThrough(parserCtx, srcConn, dstConn)
	}

	if !generic {
		p.logger.Debug("The external dependency is supported. Hence using the parser", zap.String("ParserType", string(parserType)))
		switch rule.Mode {
		case models.MODE_RECORD:
			// V2 path: parsers that have been migrated to the supervisor +
			// relay + FakeConn architecture declare it via IntegrationsV2.
			// The dispatcher routes them through recordViaSupervisor which
			// isolates parser panics/hangs and falls through to passthrough
			// on failure. The legacy path below is preserved for parsers
			// that have not yet migrated.
			if shouldRecordViaSupervisor(matchedParser) {
				if err := p.recordViaSupervisor(parserCtx, srcConn, dstConn, matchedParser, parserType, rule.MC, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts); err != nil {
					if isNetworkClosedErr(err) {
						logger.Debug("V2 record path: connection closed", zap.Error(err))
					} else {
						utils.LogError(logger, err, "V2 record path failed",
							zap.String("parser", string(parserType)),
							zap.String("next_step", "set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough) while investigating; the supervisor has already fallen through to passthrough for this connection so user traffic continues"),
						)
					}
					return err
				}
				// Outcome-specific logging lives inside recordViaSupervisor
				// (success vs. fallthrough-to-passthrough look the same
				// from this layer — both return nil error — but mean
				// different things for the mocks captured). Here we only
				// know the record path completed without a caller-visible
				// error; the parser outcome was already logged.
				logger.Debug("V2 record path returned", zap.String("ParserType", string(parserType)))
				break
			}

			// Create TLSUpgrader in handleConnection scope so it holds pointers to
			// the real srcConn/dstConn variables (not copies in buildRecordSession).
			var upgrader models.TLSUpgrader
			if parserType == integrations.MYSQL ||
				parserType == integrations.POSTGRES_V2 ||
				parserType == integrations.POSTGRES_V3 {
				upgrader = util.NewConnTLSUpgrader(&srcConn, &dstConn, p.logger, p.clientTLSHandler(dstAddr, srcProcess(outgoingOpts)))
			}
			session := p.buildRecordSession(srcConn, dstConn, rule.MC, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts, upgrader)
			err := matchedParser.RecordOutgoing(parserCtx, session)
			p.countParserPanic(err, parserType, dstAddr)
			if errors.Is(err, integrations.ErrParserDeclined) {
				return p.relayDeclinedConn(parserCtx, err, logger, parserType, session)
			}
			if err != nil {
				if isNetworkClosedErr(err) {
					logger.Debug("failed to record the outgoing message (connection closed)", zap.Error(err))
				} else {
					utils.LogError(logger, err, "failed to record the outgoing message")
				}
				return err
			}
			logger.Debug("successfully recorded outgoing message", zap.String("ParserType", string(parserType)))
		case models.MODE_TEST:
			// Hand the parser a reporter so it can record a mock-mismatch
			// out-of-band — surfacing the miss to the test report without
			// returning the error that closes this connection. Returning the
			// error is fine for most protocols, but a Pulsar SEND mismatch must
			// keep the connection alive (a close makes pulsar-client-go
			// reconnect the producer and crash on a nil schema); such a parser
			// reports via this hook, replies normally, and keeps serving.
			// The reporter names the process the connection came from on
			// each miss and answer it takes (UnmatchedCall.Source).
			srcPid := outgoingOpts.SrcPid
			testCtx := models.WithMockMismatchReporter(parserCtx, func(err error) { p.sendMockNotFoundErrorFrom(err, srcPid, dstCfg) })
			// A replayer that makes the client's TLS handshake itself, in
			// band (Postgres's, on an SSLRequest), has it charged as the
			// proxy's own client handshakes are, and a build's client TLS
			// gate readied for it (withClientTLS), as mockMySQLOutgoing's.
			testCtx = p.withClientTLS(testCtx, dstAddr, srcProcess(outgoingOpts))
			// A test's end tells this connection's requests apart only if its
			// parser answers before it reads again, or marks what it holds
			// (inflight.go).
			if tracked != nil {
				tracked.servedBy(parserType, matchedParser)
			}
			err := matchedParser.MockOutgoing(testCtx, srcConn, dstCfg, p.scopedFor(outgoingOpts.SrcPid, m), outgoingOpts)
			p.countParserPanic(err, parserType, dstAddr)
			if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
				// Send specific error type to error channel for external monitoring
				return p.mockFailedFrom(logger, err, srcPid, dstCfg)
			}
		}
	}

	if generic {
		logger.Debug("The external dependency is not supported. Hence using generic parser")
		if rule.Mode == models.MODE_RECORD {
			return p.recordGenericOutgoing(parserCtx, srcConn, dstConn, rule.MC,
				parserErrGrp, logger, clientConnID, destConnID, outgoingOpts)
		}
		return p.mockGenericOutgoing(parserCtx, srcConn, dstCfg, m, outgoingOpts, logger)
	}
	return nil
}

// recordGenericOutgoing dispatches a record-mode connection that matched no
// specific parser to the GENERIC catch-all.
//
// It is a function rather than an inlined block for the same reason
// recordMySQLOutgoing is: inlined, the dispatch decision sat deep inside
// handleConnection behind an eBPF-resolved destination, so no test could reach
// it — deleting the unregistered-parser guard below left the whole proxy suite
// green.
func (p *Proxy) recordGenericOutgoing(
	parserCtx context.Context,
	srcConn, dstConn net.Conn,
	mocks chan<- *models.Mock,
	parserErrGrp *errgroup.Group,
	logger *zap.Logger,
	clientConnID, destConnID int64,
	outgoingOpts models.OutgoingOptions,
) error {
	genericParser := p.Integrations[integrations.GENERIC]
	// Route through the supervisor exactly like the matched-parser path. This
	// branch previously called RecordOutgoing directly with NO IsV2 gate, so
	// every connection that matched no parser recorded through generic's LEGACY
	// path — on the default configuration, with no flag set. Generic has
	// declared IsV2() == true all along; nothing consulted it here, so the
	// migration silently skipped the widest code path in the proxy.
	if shouldRecordViaSupervisor(genericParser) {
		if err := p.recordViaSupervisor(parserCtx, srcConn, dstConn, genericParser, integrations.GENERIC, mocks, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts); err != nil {
			if isNetworkClosedErr(err) {
				logger.Debug("V2 generic record path: connection closed", zap.Error(err))
			} else {
				utils.LogError(logger, err, "V2 generic record path failed",
					zap.String("next_step", "set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough) while investigating; the supervisor has already fallen through to passthrough for this connection so user traffic continues"),
				)
			}
			return err
		}
		logger.Debug("V2 generic record path returned")
		return nil
	}

	// Generic.IsV2() is unconditionally true, so reaching here means the GENERIC
	// parser is not registered at all — and the legacy call below would nil-panic
	// on the interface. The MySQL dispatch site guards the same case; this one
	// did not, and with the KEPLOY_NEW_RELAY rollback removed an unregistered
	// build is the ONLY way to get here, so the branch's sole remaining
	// reachable state was a crash.
	//
	// Treat it as a decline, not a failure: relay the connection rather than
	// letting the caller's deferred close drop it.
	if genericParser == nil {
		declined := fmt.Errorf("no generic parser is registered: %w", integrations.ErrParserDeclined)
		return p.relayDeclinedConn(parserCtx, declined, logger, integrations.GENERIC,
			p.buildRecordSession(srcConn, dstConn, mocks, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts, nil))
	}

	genericSession := p.buildRecordSession(srcConn, dstConn, mocks, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts, nil)
	err := genericParser.RecordOutgoing(parserCtx, genericSession)
	p.countParserPanic(err, integrations.GENERIC, dstConn.RemoteAddr())
	if errors.Is(err, integrations.ErrParserDeclined) {
		return p.relayDeclinedConn(parserCtx, err, logger, integrations.GENERIC, genericSession)
	}
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "tls: user canceled") {
		utils.LogError(logger, err, "failed to record the outgoing message")
		return err
	}
	return nil
}

// mockGenericOutgoing serves a replay-mode connection that matched no specific
// parser from the GENERIC catch-all.
//
// Extracted for the same reason as recordGenericOutgoing: inlined in
// handleConnection, the unregistered-parser guard below was unreachable from a
// test and deleting it left the whole suite green.
func (p *Proxy) mockGenericOutgoing(
	parserCtx context.Context,
	srcConn net.Conn,
	dstCfg *models.ConditionalDstCfg,
	m integrations.MockMemDb,
	outgoingOpts models.OutgoingOptions,
	logger *zap.Logger,
) error {
	genericParser := p.Integrations[integrations.GENERIC]
	if genericParser == nil {
		// Same unregistered-build case the record half guards; without it the
		// map lookup nil-derefs. There is no upstream connection to relay to on
		// the replay path (every destination dial in handleConnection is
		// mode-gated), so report rather than relay — which is what the MySQL
		// replay site does too.
		err := errors.New("no generic parser is registered to mock this connection")
		return p.mockFailed(logger, err)
	}
	err := genericParser.MockOutgoing(parserCtx, srcConn, dstCfg, p.scopedFor(outgoingOpts.SrcPid, m), outgoingOpts)
	p.countParserPanic(err, integrations.GENERIC, dstCfg.Addr)
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
		// Send specific error type to error channel for external monitoring
		return p.mockFailedFrom(logger, err, outgoingOpts.SrcPid, dstCfg)
	}
	return nil
}

func (p *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	p.logger.Info("stopping proxy server...")

	// Surface the async-egress verdict (served / shape-flags / not-exercised)
	// at end of replay so it is visible in keploy's output.
	if p.asyncEngine != nil {
		p.asyncEngine.LogReport(p.logger)
	}

	// Coordinated shutdown sequence (PLAN.md §3.7, partial I8).
	//
	// 1. Trip the parsing kill switch first. Any connection still in
	//    flight between Accept and parser dispatch is routed to raw
	//    globalPassThrough instead of a parser — so we don't start a
	//    parser goroutine for a connection that's about to be torn
	//    down.
	// 2. Close the listener, stopping further Accepts.
	// 3. Give existing parser goroutines a bounded grace window to
	//    finish naturally via activeConns WaitGroup (they see
	//    ctx.Done via the parent context and return; their deferred
	//    srcConn/dstConn closes fire on return).
	// 4. Past the grace window, remaining goroutines continue to exit
	//    asynchronously via ctx cancellation — we do NOT hold
	//    references to their net.Conn values, which avoids
	//    double-close races with the deferred closes above. Shutdown
	//    is bounded by the grace timeout; stragglers are abandoned
	//    to the parent context, not force-closed from under their
	//    own defers.
	//
	// The kernel-side proxy_ready BPF gate that would prevent new
	// connects from being redirected here once the userspace process
	// is unhealthy requires BPF source changes (the source is not in
	// this repo) and is tracked as deferred work in PLAN.md.
	util.DefaultKillSwitch.Trip()
	defer util.DefaultKillSwitch.Reset()

	var cleanupErrors []error
	if p.Listener != nil {
		if err := p.Listener.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to close listener: %w", err))
		}
		p.Listener = nil
	}

	// Drain grace: let in-flight parsers finish naturally before we
	// pull the rug on their client connections. 5 seconds is long
	// enough for typical request/response cycles but short enough
	// that shutdown doesn't hang indefinitely on a stuck parser.
	const drainGrace = 5 * time.Second
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainGrace)
	defer drainCancel()
	p.waitForConnDrain(drainCtx)

	// Any connections still in flight after the grace window have
	// already received parent-context cancellation (propagated into
	// the per-connection parserCtx in handleConnection). Their own
	// deferred srcConn/dstConn closes run on return. We intentionally
	// do NOT keep a shared slice of net.Conn values: that requires
	// tracking lifecycle hooks we don't own reliably, and the
	// WaitGroup + ctx cancellation combination already covers the
	// "stuck parser eventually exits" case without double-close
	// races on the real sockets.
	if err := p.stopDNSServers(); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to stop DNS servers: %w", err))
	}

	// Channel-binding shim teardown — detach uprobes, drop allowlist
	// entries, release BPF maps. Nil-safe so the no-BPF path doesn't
	// trip a panic here. WatchProcessTree's goroutine has already
	// exited via the proxy context being cancelled by the caller of
	// StopProxyServer; Close() is idempotent against an in-flight
	// goroutine racing it.
	if p.cbshim != nil {
		// Detach the global tls.MITMPublishHook so NEW CertForClient
		// callers can't observe the handle. SetMITMPublishHook(nil)
		// blocks until any in-flight publishMITM returns — see
		// pkg/agent/proxy/tls/ca.go for the RWMutex drain — so no
		// CertForClient → RegisterMITM call survives this point.
		//
		// We deliberately do NOT mutate p.cbshim here. In-flight
		// connection-scoped goroutines (notably the opportunistic-TLS
		// path) read p.cbshim multiple times within one connection;
		// a racing nil-write would tear those reads. Since shutdown
		// is in progress (listener closed, parent ctx cancelled, no
		// new accepts), leaving p.cbshim pointing at the (now-quiesced)
		// handle is safe — its publisher-facing surface is already
		// inert via the cleared global hook.
		//
		// The actual cb.Close() is deferred onto a goroutine that
		// waits for activeConns to drain. dialPostgresSSLUpstream
		// captures p.cbshim into a function-local at call time, so
		// goroutines mid-pg-SSL-handshake when shutdown starts are
		// covered by the WaitGroup. Drain already had drainGrace
		// seconds above; the deferred Close keeps the BPF maps +
		// uprobes alive until the last in-flight goroutine returns.
		// Kernel reclaims any leaked BPF resources at process exit
		// if Wait never returns.
		cb := p.cbshim
		pTls.SetMITMPublishHook(nil)
		go func() {
			p.activeConns.Wait()
			if err := cb.Close(); err != nil {
				p.logger.Debug("cbshim deferred close error",
					zap.Error(err))
			}
		}()
	}

	// The capture's watermark leaves the recording's manager with the proxy.
	if p.removeCaptureWatermark != nil {
		p.removeCaptureWatermark()
	}

	p.CloseErrorChannel()
	if len(cleanupErrors) > 0 {
		for _, err := range cleanupErrors {
			utils.LogError(p.logger, err, "cleanup error in StopProxyServer")
		}
		p.logger.Error("proxy stopped with cleanup errors", zap.Int("error_count", len(cleanupErrors)))
	} else {
		p.logger.Debug("proxy stopped cleanly...")
	}
}

func (p *Proxy) applyMTLSClientCert(cfg *tls.Config, clientPeerCert *x509.Certificate, tlsPrivateKey string) error {
	if cfg == nil || clientPeerCert == nil {
		return nil
	}

	if tlsPrivateKey == "" {
		err := errors.New("failed to read private key from outgoing options")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	keyBytes := []byte(tlsPrivateKey)
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		err := errors.New("failed to decode PEM block containing private key")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	var privKey any
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		privKey = key
	} else if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		privKey = key
	} else if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		privKey = key
	}

	if privKey == nil {
		err := errors.New("failed to parse private key from PEM")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	cfg.Certificates = []tls.Certificate{{
		Certificate: [][]byte{clientPeerCert.Raw},
		PrivateKey:  privKey,
	}}
	p.logger.Debug("Successfully constructed mTLS certificate using client peer cert and local key")
	return nil
}

// applyUpstreamTLSOptions stamps the agent-resolved upstream-TLS trust material
// onto a RECORD session's OutgoingOptions, so every dial site can read it off
// the options it already carries instead of reaching back into the Proxy.
// Record-only by design — see the note in Mock.
//
// The pool is deliberately NOT part of the request the CLI sends: x509.CertPool
// has only unexported fields, so JSON round-tripping it would deliver a non-nil
// EMPTY pool to the agent — a trust store that trusts nothing and fails every
// handshake (see models.OutgoingOptions.UpstreamTLSRootCAs). Only the boolean
// crosses the wire; the pool is built here, agent-side, from the CA path that
// travelled over argv.
//
// The agent's own resolution is AUTHORITATIVE; the request's boolean is not
// OR-ed in. Two reasons, and both are about not producing silent mock loss:
//
//   - An OR can only ever add verification, never remove it, which is what
//     made `--upstream-tls-verify=false` unable to switch off a keploy.yml
//     `verify: true` on a native run while it worked under docker. Precedence
//     has to be resolved in exactly one place — resolveUpstreamTLSConfig — and
//     this is the consumer of that decision, not a second opinion on it.
//   - A request that asks to verify against an agent that resolved NO trust
//     material (never configured, or the CA failed to load) would verify with
//     Go's default roots and the operator's private CA silently absent — the
//     dest handshake then fails, the supervisor falls through to raw
//     passthrough, the app keeps working and the mock is DROPPED. That is the
//     exact outcome the load-failure path exists to prevent, so it is refused
//     here too, loudly, naming the configured CA path.
func (p *Proxy) applyUpstreamTLSOptions(opts *models.OutgoingOptions) {
	p.loadUpstreamTLSTrustAnchors()

	if opts.UpstreamTLSVerify && !p.upstreamTLSVerify {
		p.logger.Warn("ignoring the recording session's request to verify upstream TLS certificates: this agent has no upstream trust anchors resolved",
			zap.Bool("agent_ca_load_failed", p.upstreamTLSLoadFailed),
			zap.String("agent_ca_cert", p.upstreamTLSCACert),
			zap.String("next_step", "start the agent with --upstream-tls-verify (and --upstream-tls-ca-cert for a private CA, resolved on the AGENT's filesystem), or set record.upstreamTls in the keploy.yml the agent reads. Verification is left OFF for this session so mocks are still recorded rather than silently dropped by a failed dest-side handshake."))
	}

	opts.UpstreamTLSVerify = p.upstreamTLSVerify
	opts.UpstreamTLSRootCAs = p.upstreamTLSRootCAs
}

// loadUpstreamTLSTrustAnchors builds the root pool for record.upstreamTls.verify
// at most once per agent process, on the first RECORD session.
//
// Deliberately not done in New: New runs for every agent regardless of mode, and
// replay never verifies (Proxy.Mock does not stamp these options at all), so
// resolving there meant a `keploy test` agent reading a keploy.yml with
// upstreamTls.verify set would read the PEM, log "verification is enabled" for a
// mode that verifies nothing, and LogError a bad caCert with a remediation for a
// setting replay ignores.
//
// Still once, not per dial: the load reads a PEM off disk and parses ~150 roots.
func (p *Proxy) loadUpstreamTLSTrustAnchors() {
	p.upstreamTLSOnce.Do(func() {
		if !p.upstreamTLSVerifyCfg {
			return
		}
		pool, err := pTls.LoadUpstreamRootCAs(p.logger, p.upstreamTLSCACert)
		if err != nil {
			// Verifying against a trust store that is MISSING the operator's
			// CA is worse than not verifying at all: the dest-side handshake
			// fails, the supervisor falls through to raw passthrough, the app
			// keeps working, and the mock is dropped with no user-visible
			// cause. So fall back to today's behaviour (skip verification) and
			// make the misconfiguration loud instead of turning it into
			// missing mocks.
			utils.LogError(p.logger, err, "failed to load upstream TLS trust anchors; upstream certificate verification is DISABLED for this session",
				zap.String("next_step", "fix record.upstreamTls.caCert (the path is resolved on the AGENT's filesystem — bind-mount the PEM for docker/k8s runs) and restart, or unset record.upstreamTls.verify to silence this"))
			p.upstreamTLSLoadFailed = true
			return
		}
		p.upstreamTLSRootCAs = pool
		p.upstreamTLSVerify = true
		p.logger.Info("upstream TLS certificate verification is enabled",
			zap.Bool("extra_ca_cert", p.upstreamTLSCACert != ""))
	})
}

func (p *Proxy) Record(ctx context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	// A recording replays nothing: no port is recorded, whatever a replay's
	// store seeded before it.
	recorded.forget()
	p.serverFirstSaid.reset()
	p.startContainerKeys(ctx)
	// Reconcile the two mock-noise spellings on receipt. This is the process
	// boundary: opts arrives over the agent API from a client that may predate
	// the schema-noise -> mock-noise rename and therefore sets only the
	// deprecated fields. Unknown JSON keys decode silently, so without this the
	// toggle would simply not take effect, with nothing logged anywhere.
	opts.NormalizeMockNoise()
	// Reset graceful shutdown flag for a new recording session.
	p.isGracefulShutdown.Store(false)
	// Reset DNS mock deduplication tracker for fresh recording
	p.ResetRecordedDNSMocks()
	// The process's mock manager starts the recording before its first
	// request, whatever an earlier recording of this process left. An
	// already-running application's request that reaches the ingress after
	// the recording's incoming stream attached but before this Record is
	// forgotten here: its calls from then on are recorded as startup calls
	// (sent on at once, not windowed), and the next request signals again.
	syncMock.Get().StartRecordSession()
	// Lift the per-session opportunistic-TLS toggle onto the proxy
	// for handleConnection to read. Independent of GlobalPassthrough.
	p.OpportunisticTLSIntercept = opts.OpportunisticTLSIntercept
	p.applyUpstreamTLSOptions(&opts)
	rule := &agent.Session{
		Mode:            models.MODE_RECORD,
		MC:              mocks,
		OutgoingOptions: opts,
	}
	// Start holding handshakes before the session takes effect. (A
	// connection that arrives while the rules install is accepted at once,
	// as before.)
	p.ensureHandshakeHold(rule)
	// What a connection under the session needs is set up before the
	// session takes effect: the packet capture, so a connection's start is
	// in it, and a mock manager, so the recording has one to hand its
	// mocks to (the first session's had none: a connection under it was
	// dropped). The manager is put in place just before the session, so
	// a replay session before it serves from its own until then.
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger)
	if opts.CapturePackets {
		// Agent picks its own scratch dir on its OWN filesystem; the
		// recorder cannot pass a path here because the two processes
		// usually run in separate filesystems (Docker, k8s, separate
		// hosts). The recorder pulls the finalised bytes back via the
		// agent's /agent/pcap/{traffic,keylog} HTTP endpoints.
		// Use a context detached from the per-call ctx so the capture
		// outlives this Record() call and runs for the full session;
		// stopPacketCapture is invoked on the next Record()/Mock() or
		// when the proxy shuts down.
		if err := p.startPacketCapture(context.Background(), ""); err != nil {
			utils.LogError(p.logger, err, "failed to start packet capture; continuing without pcap")
		}
	} else {
		// New session without capture — make sure any previous capture
		// from a prior test-set is torn down.
		p.stopPacketCapture()
	}
	// What was passed through while there was no session is reset, and
	// no more is passed through, before the first session takes effect: a
	// passthrough still open under it would carry the application's calls
	// unrecorded while it records. The application reconnects, and its
	// reconnection is recorded (one that comes before the session is
	// refused, and reconnects again). Under a session already in effect a
	// reconnection would be the old session's, so a later session resets
	// what it does not pass through once it is in effect, as before.
	first := p.getSession() == nil
	if first {
		p.endNoSessionPassthroughs(rule)
	}
	p.setMockManager(mm)
	p.setSession(rule)
	if !first {
		p.endNoSessionPassthroughs(rule)
	}
	p.ensureHandshakeHold(rule) // the listener may have opened in between
	return nil
}

func (p *Proxy) Mock(ctx context.Context, opts models.OutgoingOptions) error {
	// The ports the stored set's recording reached, seeded with its on-disk
	// store (SeedRecordedDestinations), stay: keploy mock replay publishes
	// the store after its Mock, keploy test may before.
	recorded.reset()
	p.startContainerKeys(ctx)
	// Reconcile the two mock-noise spellings on receipt. This is the process
	// boundary: opts arrives over the agent API from a client that may predate
	// the schema-noise -> mock-noise rename and therefore sets only the
	// deprecated fields. Unknown JSON keys decode silently, so without this the
	// toggle would simply not take effect, with nothing logged anywhere.
	opts.NormalizeMockNoise()
	// Reset graceful shutdown flag for a new mocking session.
	p.isGracefulShutdown.Store(false)
	// Forget the previous recording's MySQL ports before this test set derives
	// its own. The agent is a single long-lived process and this registry is
	// allocated once, so without the reset the derived port set — and the
	// endpoint-drift inference it arms — would mean "any recording this process
	// has ever replayed" rather than "the recording being replayed now": a test
	// set holding MySQL mocks would arm the inference for a later one holding
	// none, and a silent connection there would be routed to a replayer with
	// nothing to serve.
	//
	// Mock() is the hook because it is what the replayer reaches once per test
	// set (Agent.MockOutgoing → here), and it runs before that set's first
	// UpdateMockParams → SetMocksWithWindow delivers any mocks. SetMocks is NOT
	// a candidate despite appearances: it is only the fallback arm for a
	// third-party proxy that does not implement WindowedProxy, and this proxy
	// does, so a stock agent never calls it. SetMocksWithWindow is not one
	// either — it arrives repeatedly with a per-test SUBSET.
	//
	// This only marks; the swap happens when the replacement mocks arrive, so
	// the boundary leaves no window in which known ports are forgotten.
	p.mysqlPorts.MarkSessionStale()
	// Mock is replay; no pcap capture during replay. Tear down any
	// capture left over from a previous record session.
	p.stopPacketCapture()
	// Deliberately NOT calling applyUpstreamTLSOptions here. upstreamTls is a
	// `record:` setting: it exists so the traffic being CAPTURED comes from an
	// authenticated upstream. Replay serves its traffic from mocks; the dials
	// it still makes are passthrough of calls that were never recorded, and
	// failing those on a certificate check would break a test run over trust
	// material that has no bearing on what is being asserted. Replay therefore
	// keeps today's behaviour exactly, and because this is the only caller-side
	// stamp, the trust anchors are never even loaded in MODE_TEST (see
	// loadUpstreamTLSTrustAnchors).
	rule := &agent.Session{
		Mode:            models.MODE_TEST,
		OutgoingOptions: opts,
	}
	p.ensureHandshakeHold(rule)
	// What a connection under the session needs is set up before the
	// session takes effect, as Record does: a mock manager above all (the
	// first session's had none, and a connection under it was dropped).
	//
	// Reuse the existing MockManager when this proxy has already been
	// put into mock mode at least once. Replay calls Mock() per test-set
	// (Agent.MockOutgoing → Proxy.Mock); allocating a fresh MockManager
	// on every call strands long-lived parser goroutines on the previous
	// instance. The postgres-v3 replayer's runLoop is the canonical
	// case — its PerTestProvider/SessionProvider/StartupProvider capture
	// the *MockManager handed in at MockOutgoing time and keep polling
	// it for the connection's lifetime. With a per-test-set replacement
	// the new manager's StoreMocks / UpdateMockParams (and the revision
	// bumps keploy/integrations#203 relies on for revision-gated
	// rebuilds) land on the new instance the parsers cannot see, while
	// the parsers keep reading test-set-N-1's cohort off the orphaned
	// manager. Production globality autoreplay exhibits exactly this
	// pattern: one user app survives the whole replay, the asyncpg pool
	// keeps its TCP connection to the proxy warm across test-set
	// boundaries, the v3 runLoop survives, and PerTestProvider.Current
	// never observes the swap. Reusing the manager keeps revision-
	// tracking continuous; the filtered / unfiltered / startup trees
	// still get replaced atomically by SetMocksWithWindow per test-set,
	// and ResetForReplaySession below clears connection-scoped state
	// (connectionTrees, noConnMocks, droppedOutOfWindow) so the
	// previous test-set's per-connID mock pool doesn't leak into the
	// next one — SetMocksWithWindow seeds new connection mocks via
	// AddConnectionMock but never evicts prior entries, so without the
	// explicit reset a long-lived connection could match
	// LifetimeConnection mocks from the prior test-set or merge them
	// with the current one's.
	if provenswitch.On() {
		// A new test set: what is missed until its mocks are staged is not
		// its (cutMissesAtSetStart, at the set's staging).
		p.missCutPending.Store(true)
		// A new test set: what the previous set served is no earlier place
		// of this one's, whose mocks reuse its names (package matcher/learn).
		// A fresh agent holds no manager yet, so its first set begins on the
		// tracker alone; the set's staging reads the tracker's set itself
		// (MockManager.SetTrackerSetAt).
		p.learnBoundary(func(t *learn.Tracker) { t.NewSet() })
	}
	if mm := p.getMockManager(); mm == nil {
		p.setMockManager(NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger))
	} else {
		mm.ResetForReplaySession()
		if opts.StartsTestSet {
			// Another test set: what the previous set's pool was consumed
			// of and nobody drained is that set's, never this one's.
			mm.ArmSetFence()
		}
	}
	if provenswitch.On() {
		// A new test set, as for the learn step above: its variables begin
		// with none bound, the previous set's run's neither (its mocks
		// reuse the names), and what its start-up binds before its first
		// test is the run's, which each of its tests reads.
		//
		// The replay's re-registration of a set's mocks with an agent it
		// took for replaced mid bring-up (ensureAgentHoldsStoredMocks)
		// reaches Mock too, without StartsTestSet. Where the agent was not
		// in fact replaced, what the start-up bound so far is forgotten
		// here, as the learn step's tracker forgets what it served (above),
		// so the two stay one set: a start-up call made again binds anew,
		// and a test's use of a value bound before the repair is a miss,
		// never an answer from a binding the learn step no longer knows.
		//
		// Neither is skipped for a call without StartsTestSet. The flag says
		// a set begins; its absence does not say the set goes on: a CLI from
		// before the flag (2026-10-17) sends none at any set. Kept across a
		// set that does begin, the previous set's exchanges and run names,
		// under the mock names the next set numbers afresh, would explain
		// and answer the next set's calls as another mock's: a false pass,
		// where forgetting at a repair is a false fail.
		p.getMockManager().NewVariableSet()
		// Record's full round pairs a call nothing answered with a recording
		// another test owns: only its session sets Learn.
		p.getMockManager().SetPairing(opts.Learn)
		// A session's learn step begins anew: what ends it is a filter of
		// a set's later cycle (SetLearnEnded).
		p.getMockManager().SetLearnEnded(false)
		p.learnTracker.SetLearnEnded(false)
		// Nor is it held: a test that held it belongs to the previous
		// session (SetLearnHeld).
		p.getMockManager().SetLearnHeld(false)
		p.learnTracker.SetLearnHeld(false)
	}

	// Drop any per-worker scopes from a prior session so a crashed worker that
	// never sent /agent/scope/end cannot leak an allowlist that mis-scopes a
	// recycled PID in this session.
	p.ClearAllWorkerScopes()

	// What was passed through while there was no session is reset before the
	// first session takes effect, so the application's reconnection is
	// served under it (one that comes before it is refused, and reconnects
	// again); a later session resets what it does not pass through once it
	// is in effect, so the reconnections are its own (Record).
	first := p.getSession() == nil
	if first {
		p.endNoSessionPassthroughs(rule)
	}
	p.setSession(rule)
	if !first {
		p.endNoSessionPassthroughs(rule)
	}
	p.ensureHandshakeHold(rule) // the listener may have opened in between

	p.logSessionEgressKnobs(opts)

	p.nsSwitchMutex.Lock()
	defer p.nsSwitchMutex.Unlock()
	if string(p.nsswitchData) == "" {
		// setup the nsswitch config to redirect the DNS queries to the proxy
		err := p.setupNsswitchConfig()
		if err != nil {
			utils.LogError(p.logger, err, "failed to setup nsswitch config")
			return errors.New("failed to mock the outgoing message")
		}
	}
	return nil
}

// Compile-time proof that *Proxy satisfies the agent's optional
// AsyncMockLoader capability. pkg/agent/proxy already imports pkg/agent
// (as agent), and pkg/agent does not import back, so this asserts without
// introducing an import cycle.
var _ agent.AsyncMockLoader = (*Proxy)(nil)

// LoadAsyncMocks forwards the complete async-mock corpus to the async engine
// (which REPLACES the previous test-set's corpus). No-op when async is not
// configured.
func (p *Proxy) LoadAsyncMocks(mocks []*models.Mock) {
	if p.asyncEngine == nil {
		return
	}
	// Flush the previous test-set's verdict at the boundary.
	//
	// Order relative to Load is NOT load-bearing — Load touches streams,
	// completed and windowSeen, never the tally — so this reads before it only
	// because that is the order the boundary happens in, not because anything
	// depends on it.
	//
	// This is the only seam that runs once per test-set on EVERY replay path.
	// The SetGracefulShutdown seam does not: replay.go gates its per-test-set
	// notify on `r.instrument && !serveTest`, and serveTest is true for
	// docker-compose replay with mocking on — which is the DEFAULT — so on that
	// path only the run-level notify fires and sets 2..N would stay invisible.
	//
	// Emitting here is ordering-independent for the same reason Load itself
	// replaces here rather than on a reset seam: the two replay branches call
	// StoreMocks and MockOutgoing in opposite orders, and Load is the one call
	// that carries the corpus.
	//
	// The last set is still covered: replay.go's run-level defer calls
	// NotifyGracefulShutdown ungated, which reaches SetGracefulShutdown and
	// flushes whatever the final set accumulated.
	p.asyncEngine.LogReport(p.logger)
	p.asyncEngine.Load(mocks)
}

func (p *Proxy) SetMocks(_ context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	// Replay cannot detect MySQL from content — no upstream exists to
	// send a handshake — so the port set is recovered here, from the
	// destAddr every MySQL mock records. Must happen before the mocks
	// are published, so a connection that wakes on SetFilteredMocks
	// already sees the derived ports.
	p.deriveMysqlPorts(filtered, unFiltered, p.seededDestinations())
	recorded.add(filtered, unFiltered)
	if m := p.getMockManager(); m != nil {
		m.SetFilteredMocks(filtered)
		m.SetUnFilteredMocks(unFiltered)
		p.dnsCache.Purge()
	}
	if p.asyncEngine != nil {
		// This non-windowed path never advances the async window (only
		// SetMocksWithWindow does), so test-anchored deliveries can't arm
		// here. Warn once rather than silently reporting them not-exercised.
		p.asyncEngine.WarnNonWindowed()
	}

	return nil
}

// SetMocksWithWindow atomically updates mocks AND the test window in a
// single call so concurrent readers cannot observe a torn (newMocks,
// oldWindow) view. Used to satisfy the WindowedProxy extension interface.
func (p *Proxy) SetMocksWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time) error {
	p.stageWithWindow(ctx, filtered, unFiltered, start, end, func(m *MockManager) {
		m.SetMocksWithWindow(filtered, unFiltered, start, end)
	})
	return nil
}

// OpenTestWithWindow stages a test's window as SetMocksWithWindow does, and
// begins the test's variables with given bound in the same step
// (MockManager.OpenTestWithWindow). With no mock manager there is nothing to
// stage, nor to bind given to (StartVariables). Satisfies the agent's
// optional WindowedTestOpener extension interface.
func (p *Proxy) OpenTestWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time, given []models.VariableBinding) error {
	if p.getMockManager() == nil && len(given) > 0 {
		return errNoMockManager
	}
	p.stageWithWindow(ctx, filtered, unFiltered, start, end, func(m *MockManager) {
		m.OpenTestWithWindow(filtered, unFiltered, start, end, given)
	})
	return nil
}

// stageWithWindow is SetMocksWithWindow, the mock manager's part staged by
// stage.
func (p *Proxy) stageWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time, stage func(*MockManager)) {
	p.deriveMysqlPorts(filtered, unFiltered, p.seededDestinations())
	recorded.add(filtered, unFiltered)
	if m := p.getMockManager(); m != nil {
		stage(m)
		p.dnsCache.Purge()
	}
	// A test set's staging (from models.BaseTime, the first since its Mock,
	// under the proven switch): what was missed until its mocks were in is
	// not the set's (cutMissesAtSetStart).
	if start.Equal(models.BaseTime) && p.missCutPending.CompareAndSwap(true, false) {
		p.cutMissesAtSetStart(ctx)
	}
	if p.asyncEngine != nil && opensTestWindow(start) {
		// Engine owns the "first window doesn't count" rule.
		p.asyncEngine.AdvanceWindow()
	}
}

// opensTestWindow reports whether a staging from start opens a test's window,
// which is what the async lanes count (async.Engine.AdvanceWindow): a test
// set's staging, from the models.BaseTime sentinel, stages its mocks before
// any test runs and opens none, as the mock manager reads it too
// (setMocksWithWindowLocked). Counting it served every async epoch one test
// early: an epoch received after test k started, effective from test k+1, was
// served to test k. A test recorded without times (a zero start) still opens
// one.
func opensTestWindow(start time.Time) bool {
	return !start.Equal(models.BaseTime)
}

// SetScopeMocks stages the pool of a keploy mock replay test begun (open) or
// ended (!open) without a worker pid, with the proven switch off: see
// MockManager.SetScopeMocks. Satisfies the agent's optional ScopeStager
// extension interface. Everything else a staging does, it does as
// SetMocksWithWindow does.
func (p *Proxy) SetScopeMocks(_ context.Context, filtered, unFiltered []*models.Mock, open bool) error {
	p.deriveMysqlPorts(filtered, unFiltered, p.seededDestinations())
	recorded.add(filtered, unFiltered)
	if m := p.getMockManager(); m != nil {
		m.SetScopeMocks(filtered, unFiltered, open)
		p.dnsCache.Purge()
	}
	if p.asyncEngine != nil && open {
		// A test begun opens a window; one ended opens none (see
		// opensTestWindow).
		p.asyncEngine.AdvanceWindow()
	}
	return nil
}

var _ agent.ScopeStager = (*Proxy)(nil)

// FirstTestWindowStart returns the earliest test window start observed
// by the underlying MockManager, or zero before any non-BaseTime
// SetMocksWithWindow has landed. Satisfies the agent's optional
// FirstWindowStartReader extension interface.
//
// Used by the agent's tier-aware strictMockWindow filter to preserve
// startup-init mocks (req < firstWindowStart) while still dropping
// stale cross-test bleed (firstWindowStart <= req < currentStart).
// Zero-time return means "no cutoff known yet" and callers should fall
// back to legacy strict-gate semantics.
func (p *Proxy) FirstTestWindowStart() time.Time {
	if m := p.getMockManager(); m != nil {
		return m.FirstTestWindowStart()
	}
	return time.Time{}
}

// SeedStartupCutoff seeds the underlying MockManager's startup-init cutoff from
// the earliest RECORDED test of the set being staged. Satisfies the agent's
// optional StartupCutoffSeeder extension interface.
//
// Without it the cutoff follows whichever test fires FIRST, so a --test-sets
// selection, an ignored test or the streaming deferral leaves it late and mocks
// from a test that never runs are served as bootstrap traffic.
func (p *Proxy) SeedStartupCutoff(start time.Time) {
	if m := p.getMockManager(); m != nil {
		m.SeedStartupCutoff(start)
	}
}

// SeedStatementCatalog hands the underlying MockManager the stored set's
// statement catalog (MockManager.SeedStatementCatalog). Satisfies the agent's
// optional StatementCatalogSeeder extension interface.
func (p *Proxy) SeedStatementCatalog(mocks []*models.Mock) {
	if m := p.getMockManager(); m != nil {
		m.SeedStatementCatalog(mocks)
	}
}

// SeedRecordedWindows hands the underlying MockManager the recorded window of
// every test of the set being staged. Satisfies the agent's optional
// RecordedWindowsSeeder extension interface.
func (p *Proxy) SeedRecordedWindows(ws []models.TestWindow) {
	if m := p.getMockManager(); m != nil {
		m.SeedRecordedWindows(ws)
	}
}

// ResetStatefulCursors drops the mock manager's per-request stateful cursors so
// each test-set replays its stateful sequences from the first recorded response.
// Satisfies the agent's optional StatefulCursorResetter extension interface.
func (p *Proxy) ResetStatefulCursors() {
	if m := p.getMockManager(); m != nil {
		m.ResetStatefulCursors()
	}
}

// ResetVariables forgets every test variable the replay has bound: a new scope
// (a test case of keploy test, or a test of keploy mock replay) begins.
// Satisfies the agent's optional VariableResetter extension interface.
func (p *Proxy) ResetVariables() {
	if m := p.getMockManager(); m != nil {
		m.ResetVariables()
	}
}

// StartVariables begins a test's variables with the values its test case's
// request gave bound (variables.Table.Start). false when the proxy keeps no
// mock manager to hold them. Satisfies the agent's optional VariableBinder
// extension interface.
func (p *Proxy) StartVariables(given []models.VariableBinding) bool {
	m := p.getMockManager()
	if m == nil {
		return false
	}
	m.StartVariables(given)
	return true
}

// BoundVariables returns the test variables bound so far in the test, and
// false when the proxy keeps no mock manager to hold them.
func (p *Proxy) BoundVariables() ([]models.VariableBinding, bool) {
	if m := p.getMockManager(); m != nil {
		return m.BoundVariables(), true
	}
	return nil, false
}

// CarryOverLoadRange reports which recorded-time range of carry-over mocks the
// agent should load beside the window that starts at start. Satisfies the
// agent's optional CarryOverPlanner extension interface.
func (p *Proxy) CarryOverLoadRange(start time.Time) (time.Time, time.Time, bool) {
	if m := p.getMockManager(); m != nil {
		return m.CarryOverLoadRange(start)
	}
	return time.Time{}, time.Time{}, false
}

// InFlightFrom implements agent.InFlightPlanner (MockManager.InFlightFrom).
func (p *Proxy) InFlightFrom(start time.Time) (time.Time, bool) {
	if m := p.getMockManager(); m != nil {
		return m.InFlightFrom(start)
	}
	return time.Time{}, false
}

// errNoMockManager: a call that needs the mock manager came before the proxy
// served any replay.
var errNoMockManager = errors.New("the proxy serves no replay: no mock manager")

// ServeExactTest implements agent.ExactServer (exact.go).
func (p *Proxy) ServeExactTest(test string, owned []*models.Mock) error {
	return p.openExact(func(m *MockManager) { m.ServeExactTest(test, owned) })
}

// OpenExactTest implements agent.ExactTestOpener: ServeExactTest, with the
// test's variables begun with given bound in the same step
// (MockManager.OpenExactTest).
func (p *Proxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	return p.openExact(func(m *MockManager) { m.OpenExactTest(test, owned, given) })
}

// GiveExactTest implements agent.ExactTestGiver (MockManager.GiveExactTest).
// Under the proven switch the learn step also holds the value as an earlier
// place of the test's calls (learn.Tracker.GiveValue), so a value made from
// it (a part, a pick by the clock) is refused naming the given, never learned
// as noise.
func (p *Proxy) GiveExactTest(test string, b models.VariableBinding) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	if err := m.GiveExactTest(test, b); err != nil {
		return err
	}
	if provenswitch.On() {
		p.learnTracker.GiveValue(learn.TestGiven(b.Mock, b.Name, b.Recorded, b.Value))
	}
	return nil
}

// openExact opens a test under exact serving on the mock manager (open).
func (p *Proxy) openExact(open func(*MockManager)) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	open(m)
	if p.asyncEngine != nil {
		// A test opening is a window as the async lanes count them: where a
		// per-test SetMocksWithWindow advanced them before exact serving.
		p.asyncEngine.AdvanceWindow()
	}
	return nil
}

// SetPoolNames implements agent.PoolNamesSetter: the mock manager's pool
// membership (MockManager.SetPoolNames) and the key of the test process
// whose tests list each mock (MockManager.SetPoolOwners), the owners first.
func (p *Proxy) SetPoolNames(names []string, owners map[string]string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.SetPoolOwners(owners)
	m.SetPoolNames(names)
	return nil
}

// learnBoundary runs step (the learn tracker's Begin, End or NewSet) on the
// proxy's learn tracker through the mock manager it holds
// (MockManager.LearnBoundary): holding the manager's round lock, so a learn
// step's one read of the round (integrations.RoundReader) sees the tracker
// and the store's view of one instant. With no manager, on the tracker
// alone.
func (p *Proxy) learnBoundary(step func(*learn.Tracker)) {
	if mm := p.getMockManager(); mm != nil {
		mm.LearnBoundary(p.learnTracker, step)
		return
	}
	step(p.learnTracker)
}

// TakeTestRequest gives the learn step the running keploy test case's own
// request (agent.TestRequestTaker; learn.Tracker.Give), under the proven
// switch: the test's earliest place, which it reads as any earlier place.
func (p *Proxy) TakeTestRequest(req *models.TestRequest) {
	if provenswitch.On() {
		p.learnTracker.Give(learn.GivenRequest(req))
	}
}

// BeginTestCaseCapture is BeginTestErrorCapture of a test keploy test opens
// (agent.TestRequestTaker): under the proven switch the learn step's test
// begins awaiting its test case's request, which its opening filter brings
// (TakeTestRequest), in the same step (learn.Tracker.BeginAwaiting).
func (p *Proxy) BeginTestCaseCapture() { p.openTestErrorCapture(false, true) }

// ContinueTestCaseCapture is ContinueTestErrorCapture of a test keploy test
// opens, as BeginTestCaseCapture is BeginTestErrorCapture's.
func (p *Proxy) ContinueTestCaseCapture() { p.openTestErrorCapture(true, true) }

var _ agent.TestRequestTaker = (*Proxy)(nil)

// ClientTests reports whether the learn step reads the agent's tests as
// keploy mock's client tests (agent.ClientTestsReader).
func (p *Proxy) ClientTests() bool { return p.learnTracker.ClientTests() }

var _ agent.ClientTestsReader = (*Proxy)(nil)

// TakeRecordedBegin gives the learn step the instant the next test began at
// in its recording (agent.RecordedBeginTaker; learn.Tracker.RecordedBegin),
// under the proven switch.
func (p *Proxy) TakeRecordedBegin(at time.Time) {
	if provenswitch.On() {
		p.learnTracker.RecordedBegin(at)
	}
}

var _ agent.RecordedBeginTaker = (*Proxy)(nil)

// SetAppClock makes the learn step read the app's clock through f
// (agent.AppClockSetter).
func (p *Proxy) SetAppClock(f func(real time.Time) (time.Time, bool)) {
	p.learnTracker.SetAppClock(f)
}

var _ agent.AppClockSetter = (*Proxy)(nil)

// SetAppClockRun makes the learn step read, through f, where the app's
// clock's run to a real instant began (agent.AppClockRunSetter).
func (p *Proxy) SetAppClockRun(f func(real time.Time) (time.Time, bool)) {
	p.learnTracker.SetAppClockRun(f)
}

var _ agent.AppClockRunSetter = (*Proxy)(nil)

// ServeExactTestConcurrent implements agent.ExactConcurrentServer (exact.go).
func (p *Proxy) ServeExactTestConcurrent(test string, owned, cands []*models.Mock) error {
	return p.openExact(func(m *MockManager) { m.ServeExactTestConcurrent(test, owned, cands) })
}

// OpenExactTestConcurrent implements agent.ExactConcurrentServer:
// ServeExactTestConcurrent, with the test's variables begun with given bound
// in the same step (MockManager.OpenExactTestConcurrent).
func (p *Proxy) OpenExactTestConcurrent(test string, owned, cands []*models.Mock, given []models.VariableBinding) error {
	return p.openExact(func(m *MockManager) { m.OpenExactTestConcurrent(test, owned, cands, given) })
}

var _ agent.ExactConcurrentServer = (*Proxy)(nil)

// ServeExactPool implements agent.ExactServer (exact.go).
func (p *Proxy) ServeExactPool(key string, names []string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.ServeExactPool(key, names)
	return nil
}

// ServeExactPoolAhead implements agent.ExactAheadServer (exact.go).
func (p *Proxy) ServeExactPoolAhead(key string, names []string, ahead []*models.Mock) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.ServeExactPool(key, names, ahead...)
	return nil
}

// ServeExactWorker implements agent.ExactWorkerServer (exact.go).
func (p *Proxy) ServeExactWorker(names []string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.ServeExactWorker(names)
	return nil
}

// StageExactBackground implements agent.ExactBackgroundServer
// (exact_background.go).
func (p *Proxy) StageExactBackground(rules string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.StageExactBackground(rules)
	return nil
}

// ServeExactBackground implements agent.ExactBackgroundServer
// (exact_background.go).
func (p *Proxy) ServeExactBackground(rules string, pool, worker []string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.ServeExactBackground(rules, pool, worker)
	return nil
}

var _ agent.ExactBackgroundServer = (*Proxy)(nil)

// SetRecordedOfKind implements agent.RecordedOfKindSetter
// (exact_background.go).
func (p *Proxy) SetRecordedOfKind(src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.SetRecordedOfKind(src)
	return nil
}

var _ agent.RecordedOfKindSetter = (*Proxy)(nil)

// SetRecordingTimes implements agent.RecordingTimesSetter (exact.go).
func (p *Proxy) SetRecordingTimes(src func(visit func(name string, req, res time.Time, protocolPhase bool, clock string) bool) error) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.SetRecordingTimes(src)
	return nil
}

var _ agent.RecordingTimesSetter = (*Proxy)(nil)

// SetRecordedBefore implements agent.RecordedBeforeSetter (exact.go).
func (p *Proxy) SetRecordedBefore(src func(t time.Time, visit func(*models.Mock) bool) error) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.SetRecordedBefore(src)
	return nil
}

var _ agent.RecordedBeforeSetter = (*Proxy)(nil)

// lastRecorded is the latest instant a recording's request can be read at
// (time.Time.UnixNano's last): every recording is recorded before it.
var lastRecorded = time.Unix(0, math.MaxInt64)

// WarmLearnSet implements agent.LearnSetWarmer: in record's full round,
// while the set's learn step is on (MockManager.learns), the learn tracker
// reads, in the background, what the learn step reads of each recording of
// the staged set (learn.Tracker.WarmSet): every one the manager stages and
// the agent keeps on disk, in recorded order across the two
// (rangeRecordedInOrder), so the recordings of the set's earlier tests are
// read first, before the later tests that read them open. The set's
// calls then find its payloads read, off their answer path, and each waits
// only for a recording of its own the staging is reading. Any other session
// reads nothing: no learn step reads the recordings. With no mock manager
// there is no set (errNoMockManager). Once the staging ends, the agent logs
// what it read and what the set's tiers keep (logSetUse).
func (p *Proxy) WarmLearnSet() error {
	done, err := p.warmLearnSet()
	if err != nil || done == nil {
		return err
	}
	go p.logSetUse(done)
	return nil
}

// logSetUse logs, once the set's staging ends, what it read and what the
// set's tiers keep (learn.SetUse): the line a round reads to see whether the
// set fit its budget. A staging that stopped short because the set's
// recordings could not be read (learn.SetUse.Err) is logged at Warn with
// that error: its calls read the rest on their answer path. A channel
// closed with no use (no tracker) read nothing and logs nothing.
func (p *Proxy) logSetUse(done <-chan learn.SetUse) {
	u, ok := <-done
	if !ok {
		return
	}
	fields := []zap.Field{
		zap.Int("read_at_staging", u.Staged), zap.Int("read_by_calls", u.Called), zap.Int("finished_at_staging", u.Finished), zap.Int("staging_readers", u.Readers),
		zap.Int("payloads_decoded", u.Decoded), zap.Int64("kept_held_bytes", u.Held), zap.Int64("kept_words_bytes", u.Words),
		zap.Int64("kept_forms_bytes", u.Forms), zap.Int64("kept_payload_bytes", u.Bodies), zap.Int64("set_budget_bytes", u.Budget), zap.Bool("held_full", u.HeldFull), zap.Bool("forms_full", u.FormsFull),
	}
	if u.Err != nil {
		p.logger.Warn("the learn step stopped reading the test set's recordings as it was staged: they could not be read, so its calls read the rest themselves, on their answer path",
			append(fields, zap.Error(u.Err))...)
		return
	}
	p.logger.Info("the learn step read the test set's recordings as it was staged, before its calls read them", fields...)
}

// warmLearnSet is WarmLearnSet's gate and its read: the staging's channel
// (learn.Tracker.WarmSet), or nil where the session reads nothing (the
// proven switch off, not record's full round, or the set's learn step
// ended), decided before anything is read.
func (p *Proxy) warmLearnSet() (<-chan learn.SetUse, error) {
	m := p.getMockManager()
	if m == nil {
		return nil, errNoMockManager
	}
	if !provenswitch.On() || !m.learns() {
		return nil, nil
	}
	return p.learnTracker.WarmSet(func(visit func(*models.Mock) bool) error {
		return m.rangeRecordedInOrder(lastRecorded, visit)
	}), nil
}

var _ agent.LearnSetWarmer = (*Proxy)(nil)

// SetLearnEnded implements agent.LearnEndSetter: the mock store says it
// (MockManager.SetLearnEnded, integrations.LearnEnded), and so does the learn
// tracker the parsers find on their connection's context
// (learn.Tracker.SetLearnEnded), which then gives none (learn.TrackerFrom).
// With no mock manager there is no learn step to take back up; one to end is
// refused (errNoMockManager).
func (p *Proxy) SetLearnEnded(ended bool) error {
	m := p.getMockManager()
	if m == nil {
		if ended {
			return errNoMockManager
		}
		p.learnTracker.SetLearnEnded(false)
		return nil
	}
	m.SetLearnEnded(ended)
	p.learnTracker.SetLearnEnded(ended)
	return nil
}

var _ agent.LearnEndSetter = (*Proxy)(nil)

// SetLearnHeld implements agent.LearnHolder: the mock store holds the learn
// step (MockManager.SetLearnHeld), and so does the learn tracker the parsers
// find on their connection's context (learn.Tracker.SetLearnHeld). With no
// mock manager there is no learn step to release; one to hold is refused
// (errNoMockManager).
func (p *Proxy) SetLearnHeld(held bool) error {
	m := p.getMockManager()
	if m == nil {
		if held {
			return errNoMockManager
		}
		p.learnTracker.SetLearnHeld(false)
		return nil
	}
	m.SetLearnHeld(held)
	p.learnTracker.SetLearnHeld(held)
	return nil
}

var _ agent.LearnHolder = (*Proxy)(nil)

// SetKeptLearning implements agent.KeptLearningSetter: the mock store puts
// what keploy record keeps of the set's first-cycle learning on the staged
// recordings it hands out (MockManager.SetKeptLearning). With no mock
// manager there is nothing to put it on, and learning to put is refused
// (errNoMockManager): the retry cycles would be served the staged
// recordings without it.
func (p *Proxy) SetKeptLearning(l *learn.KeptLearning) error {
	m := p.getMockManager()
	if m == nil {
		if l != nil {
			return errNoMockManager
		}
		return nil
	}
	m.SetKeptLearning(l)
	return nil
}

var _ agent.KeptLearningSetter = (*Proxy)(nil)

// SetRecordingSource implements agent.RecordingSourceSetter (exact.go).
func (p *Proxy) SetRecordingSource(src func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.SetRecordingSource(src)
	return nil
}

// RestartExactKey implements agent.ExactKeyRestarter (exact.go).
func (p *Proxy) RestartExactKey(key string, names []string) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	m.RestartExactKey(key, names)
	return nil
}

// ExactTestOutcome implements agent.ExactServer (exact.go).
func (p *Proxy) ExactTestOutcome() (string, []string, []string, bool) {
	m := p.getMockManager()
	if m == nil {
		return "", nil, nil, false
	}
	return m.ExactTestOutcome()
}

var (
	_ agent.ExactServer        = (*Proxy)(nil)
	_ agent.ExactTestOpener    = (*Proxy)(nil)
	_ agent.WindowedTestOpener = (*Proxy)(nil)
)

// GetConsumedMocks returns the consumed filtered mocks.
func (p *Proxy) GetConsumedMocks(_ context.Context) ([]models.MockState, error) {
	m := p.getMockManager()
	if m == nil {
		return nil, fmt.Errorf("mock manager not found to get consumed filtered mocks")
	}
	return m.GetConsumedMocks(), nil
}

// ServedCounts forwards how many times each mock answered a request this
// session (the mock manager's ServedCounts); nil when no manager.
func (p *Proxy) ServedCounts() map[string]uint64 {
	m := p.getMockManager()
	if m == nil {
		return nil
	}
	return m.ServedCounts()
}

// GetPersistentConsumed forwards the mock manager's never-drained per-name
// consumption map (implements coreAgent.ConsumedStateReader; see
// models.MockFilterParams.AgentOwnsConsumed). Returns nil when no manager.
func (p *Proxy) GetPersistentConsumed() map[string]models.MockState {
	m := p.getMockManager()
	if m == nil {
		return nil
	}
	return m.GetPersistentConsumed()
}

// testErrorAccumulator collects errors during an active test case.
// It is goroutine-safe via an internal mutex.
type testErrorAccumulator struct {
	mu   sync.Mutex
	errs []error
	// answers is how many of errs are answered calls' reports
	// (models.ReportAnswer): they have room of their own
	// (addBounded), so no number of answers crowds out a miss.
	answers int
	// read is set, under Proxy.captureMu, when GetMockErrors has read a window
	// it could not close (the degenerate path): what is filed into it after
	// that was made after its test's misses were taken, between two tests.
	read bool
}

func (a *testErrorAccumulator) add(err error) {
	a.mu.Lock()
	a.errs = append(a.errs, err)
	if isAnswerReport(err) {
		a.answers++
	}
	a.mu.Unlock()
}

func (a *testErrorAccumulator) drain() []error {
	a.mu.Lock()
	e := a.errs
	a.errs, a.answers = nil, 0
	a.mu.Unlock()
	return e
}

// maxPendingMockErrors bounds the retained mock-not-found errors (and the
// per-test capture accumulator) so a replay session can't grow memory
// unboundedly. The replayer drains after every test, so it stays small in
// practice.
const maxPendingMockErrors = 512

// addBounded appends err unless the accumulator already holds max entries
// of its class: misses (and every other error) and answered calls' reports
// (models.ReportAnswer) each have room for max, so a background writer
// answered by its kind, as often as it writes, never takes a miss's room.
// Returns false when the entry was dropped.
func (a *testErrorAccumulator) addBounded(err error, max int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	answer := isAnswerReport(err)
	held := len(a.errs) - a.answers
	if answer {
		held = a.answers
	}
	if held >= max {
		return false
	}
	a.errs = append(a.errs, err)
	if answer {
		a.answers++
	}
	return true
}

// The causes a read of the misses is incomplete for (missLoss).
var (
	missLostListFull        = fmt.Sprintf("a miss was dropped: the test's list of misses was full (%d)", maxPendingMockErrors)
	missLostBetweenFull     = fmt.Sprintf("a miss made between two tests was dropped: the list of misses waiting for the next test was full (%d)", maxPendingMockErrors)
	missLostBeforeSetFull   = fmt.Sprintf("a miss made before the test set's first test was dropped: the list of misses waiting for it was full (%d)", maxPendingMockErrors)
	missLostDrainStopped    = "a miss could not be queued for its test: keploy's error drain had stopped"
	missLostNoDrainChanFull = "a miss could not be queued for its test: keploy's error channel was full and no error drain was running"
)

// cutMissesAtSetStart drops, once a test set's mocks are staged (its first
// staging since its Mock, under the proven switch), what was missed before:
// after the previous set's last read (a call its application made as it
// stopped, or later still, where keploy did not stop it), and while this set's
// mocks were not in yet (a warm application's call, which missed for keploy's
// staging, not for the call). It is no test's, and not this set's. Left, this
// set's first test would read it as made before this set (beforeSetMiss), and
// fail the set with it. It is said, with the calls, as a miss no test is
// charged with is. An application keploy starts for the set starts after the
// staging, so none of its startup is cut. The drain is flushed first, so a miss
// it holds is cut with the rest; a flush that cannot complete (the drain
// stopped, the staging given up) cuts what is filed. A call whose parser
// marked its round before the staging (models.MockMismatchReport.MarkEpoch)
// is cut whenever its miss reaches the drain (markedBeforeSet): before the
// flush, here, after it, in the drain, so whether it is counted never rests
// on the scheduler. An unmarked call matched before the staging whose miss
// reaches the drain only after the flush is not cut: nothing tells it from
// the set's, so it is read as the set's, red, never green.
func (p *Proxy) cutMissesAtSetStart(ctx context.Context) {
	if m := p.getMockManager(); m != nil {
		// Before the flush: a marked miss the drain files after this is cut
		// there (markedBeforeSet), one it filed before, here.
		p.setStaged.Store(m.StagingEpoch())
	}
	if p.errDrainActive.Load() && p.errChannel != nil {
		_, _ = p.flushOnEvents(ctx, &flushMarker{done: make(chan struct{})})
	}
	p.captureMu.Lock()
	var tail []error
	if acc := p.activeTestErrors.Swap(nil); acc != nil {
		tail = acc.drain()
	}
	tail = append(tail, p.pendingMockErrors.drain()...)
	lost := p.pendingLost
	p.pendingLost = false
	// A loss since the previous set's last read was of its tail: no read of
	// this set is short of it.
	p.missLoss.Store(nil)
	p.captureMu.Unlock()
	p.logDroppedMisses(missesCutAtSetStart, tail)
	if lost {
		p.logger.Warn("more mock mismatches were made after the previous test set's last read, or before this set's mocks were staged, than keploy keeps; the rest were dropped, and they are not attributed to a test or a test set either",
			zap.Int("kept", maxPendingMockErrors))
	}
}

// missesCutAtSetStart is what the set start's cut says of the misses it drops
// (cutMissesAtSetStart, markedBeforeSet).
const missesCutAtSetStart = "mock mismatches made after the previous test set's last read, or before this set's mocks were staged, are not attributed to a test or a test set"

// markedBeforeSet reports whether err is a miss whose call's parser marked
// its round (models.MockMismatchReport.Mark) in a staging before the running
// test set's first (Proxy.setStaged): a call made before the set's mocks
// were staged, which is not the set's however late its report reaches the
// drain, as the set start's cut says (cutMissesAtSetStart). False before a
// set's first cut, and for a parser that marks no round.
func (p *Proxy) markedBeforeSet(err error) bool {
	at := p.setStaged.Load()
	if at == 0 {
		return false
	}
	var pe models.ParserError
	if !errors.As(err, &pe) || pe.MismatchReport == nil {
		return false
	}
	r := pe.MismatchReport
	return r.Mark != models.RoundUnmarked && r.MarkEpoch < at
}

// stagingMissNote is what a miss made while the replay was staging a test
// set's mocks says first in its next steps (models.ParserError.Staging):
// no recording answers a call made then, whatever it recorded.
const stagingMissNote = "This call was refused while the test set's mocks were being staged: no recording answers a call made then, whatever the set recorded for it, so a call the application makes between test sets (a background job, or a connection kept alive from the previous set) is refused."

// withStagingNote is next, a miss's next steps, led by stagingMissNote when
// the miss was made while a set's mocks were being staged.
func withStagingNote(e models.ParserError, next string) string {
	if !e.Staging || (e.MismatchReport != nil && e.MismatchReport.Answer != "") {
		return next
	}
	return strings.TrimSpace(stagingMissNote + " " + next)
}

// beforeSetMiss is a miss made before a test set's first test began, with no
// test's window open: the application's startup, as it is readied for the
// set. Under the proven switch it is the set's, no test's: the first test's
// window carries it, so it is read in order with every other miss, and the
// read returns it marked (models.UnmatchedCall.BeforeSet) for the replay to
// fail the set with, charging no test. It was dropped with a warning, and a
// set whose application made, as it started, a call no mock answers passed.
type beforeSetMiss struct{ error }

// Unwrap is the miss itself, for errors.As.
func (b beforeSetMiss) Unwrap() error { return b.error }

// noteMissLost records that a miss was lost, and why: the next read is then
// incomplete, naming the first cause, rather than silently short. With the
// proven switch off too: a read short of a miss is one whose test a miss may
// be missing from, and whose differences auto-noise must not learn. Only a
// miss that counts against a test is a loss (missCounts): a DNS lookup's,
// which the proxy answers itself, is not.
//
// An answered call's report (models.ReportAnswer) lost for room is no miss.
// One record's judging reads (a pairing, AnswerProvisional, which it bakes)
// makes the read incomplete as a miss's loss does, the cause saying it was
// no miss. One answered by kind (AnswerByKind) changes no verdict: only the
// count of background writes a report says were answered unchecked is short
// of it, which is said (noteAnswerDropped), and the read stays complete.
func (p *Proxy) noteMissLost(err error, cause string) {
	if !isMockNotFoundErr(err) {
		return
	}
	if isByKindAnswer(err) {
		p.noteAnswerDropped(err)
		return
	}
	if missCounts(err) {
		if isAnswerReport(err) {
			// No miss: an answered call's report, which record's judging
			// reads, lost where a miss would be.
			cause = "an answered call's report (no miss) was lost: " + cause
		}
		p.missLoss.CompareAndSwap(nil, &cause)
	}
}

// isAnswerReport reports whether err is an answered call's report
// (models.ReportAnswer), not a miss.
func isAnswerReport(err error) bool {
	var pe models.ParserError
	return errors.As(err, &pe) && pe.MismatchReport != nil && pe.MismatchReport.Answer != ""
}

// isByKindAnswer reports whether err is the report of a background write
// answered by its kind (models.AnswerByKind): unchecked, read by
// no judging, only counted in the reports.
func isByKindAnswer(err error) bool {
	var pe models.ParserError
	return errors.As(err, &pe) && pe.MismatchReport != nil && pe.MismatchReport.Answer == models.AnswerByKind
}

// noteAnswerDropped says that the report of a background write answered by
// its kind was dropped for room (its window's answers, or those waiting for
// the next test, were full): the first, then every 100th. No read is
// incomplete for it, as no verdict reads it; what is short is the count of
// unchecked writes a report gives. Under Proxy.captureMu, or with no window.
func (p *Proxy) noteAnswerDropped(err error) {
	n := p.answersDropped.Add(1)
	if p.logger == nil || (n != 1 && n%100 != 0) {
		return
	}
	var pe models.ParserError
	kind := ""
	if errors.As(err, &pe) && pe.MismatchReport != nil {
		kind = pe.MismatchReport.WriteKind
	}
	p.logger.Warn("a background write answered by its kind was not kept for its test's report: no verdict reads it, but the count of writes the report says were answered unchecked is short of it",
		zap.String("write_kind", kind), zap.Int64("dropped", n),
		zap.Int("kept", maxPendingMockErrors))
}

// missDroppedForRoom notes that a miss was dropped because its test's list is
// full (maxPendingMockErrors).
func (p *Proxy) missDroppedForRoom(err error) { p.noteMissLost(err, missLostListFull) }

// pendingDroppedForRoom notes, under captureMu, that a miss made with no window
// open was dropped because pendingMockErrors is full; the next window decides
// whose loss it is (pendingLost).
func (p *Proxy) pendingDroppedForRoom(err error) {
	if !isMockNotFoundErr(err) {
		return
	}
	if isByKindAnswer(err) {
		p.noteAnswerDropped(err)
		return
	}
	if missCounts(err) {
		p.pendingLost = true
	}
}

// missCounts reports whether a miss counts against a test
// (models.MockMismatchReport.CountsAgainstTest): one without a report is
// taken to, as nothing says it does not. So does, for what is lost, an
// answered call's report (models.ReportAnswer) other than one
// answered by kind: it counts against nothing, but record's judging reads it
// (a pairing it bakes), and a read short of one is incomplete. One answered
// by kind is read by no judging (noteMissLost).
func missCounts(err error) bool {
	var pe models.ParserError
	if errors.As(err, &pe) && pe.MismatchReport != nil {
		r := pe.MismatchReport
		return r.CountsAgainstTest() || (r.Answer != "" && r.Answer != models.AnswerByKind)
	}
	return true
}

// isMockNotFoundErr reports whether err is a parser mock-not-found error —
// the only error class GetMockErrors surfaces to the replayer, and thus the
// only class worth retaining in pendingMockErrors.
func isMockNotFoundErr(err error) bool {
	var parserErr models.ParserError
	if errors.As(err, &parserErr) && parserErr.ParserErrorType == models.ErrMockNotFound {
		return true
	}
	return errors.Is(err, models.ErrNoMockMatched)
}

// StartErrorDrain launches a background goroutine that continuously reads from
// errChannel so it never fills up. Errors are routed to the activeTestErrors
// accumulator when a test window is open; otherwise mock-not-found errors are
// retained in pendingMockErrors and only non-mock noise is discarded.
// This prevents background services (OTel, health checks) from saturating the
// 100-slot error channel and blocking test coordination.
// flushMarker is a sentinel GetMockErrors pushes onto errChannel. Because the
// single drain goroutine consumes errChannel in FIFO order, once it pulls the
// marker every error queued before it has already been routed — so closing the
// capture window at that point cannot strand a miss the goroutine had received
// but not yet filed. The goroutine closes done to signal it reached the marker.
type flushMarker struct{ done chan struct{} }

func (*flushMarker) Error() string { return "keploy: capture flush marker" }

func (p *Proxy) StartErrorDrain(ctx context.Context) {
	p.errDrainOnce.Do(func() {
		done := make(chan struct{})
		p.errDrainDone = done
		p.errDrainActive.Store(true)
		var discarded atomic.Int64
		go func() {
			defer utils.Recover(p.logger)
			defer close(done)
			defer p.errDrainActive.Store(false)
			for {
				select {
				case <-ctx.Done():
					return
				case err, ok := <-p.errChannel:
					if !ok {
						return
					}
					// Flush-marker rendezvous: signal that we've reached it
					// (all prior errors are already routed) and move on.
					if m, isMarker := err.(*flushMarker); isMarker {
						close(m.done)
						continue
					}
					if p.markedBeforeSet(err) {
						// Made before the running set's mocks were
						// staged: cut as the set's start cut what it
						// held, whichever way the two fell.
						p.logDroppedMisses(missesCutAtSetStart, []error{err})
						continue
					}
					// Route under captureMu so the window can't be swapped
					// between the Load and the add. The accumulator is bounded
					// too, so even a window left open (e.g. an error path that
					// skipped GetMockErrors) can't grow without limit.
					p.captureMu.Lock()
					acc := p.activeTestErrors.Load()
					if acc != nil {
						if !acc.addBounded(err, maxPendingMockErrors) {
							p.missDroppedForRoom(err)
						}
						p.captureMu.Unlock()
						continue
					}
					retained := isMockNotFoundErr(err) && p.pendingMockErrors.addBounded(err, maxPendingMockErrors)
					if !retained {
						p.pendingDroppedForRoom(err)
					}
					p.captureMu.Unlock()
					if !retained {
						// Log only the first discard and then every 100th to reduce noise.
						n := discarded.Add(1)
						if n == 1 || n%100 == 0 {
							p.logger.Debug("discarding mock error outside active test",
								zap.Error(err), zap.Int64("totalDiscarded", n))
						}
					}
				}
			}
		}()
	})
}

// BeginTestErrorCapture starts collecting mock-not-found errors for the current
// test case into a fresh per-test accumulator, so the replayer's GetMockErrors
// returns only THIS test's misses instead of draining a shared global queue.
// The replayer opens it for the first test of a test set. Misses retained
// before it, with no window open (the app's startup, or a previous set's last
// test's traffic), are not this test's: they are discarded so they don't
// attach to it, and logged, so none is lost without a word. A miss made after
// a set's last test is not left for the next set's first window: under the
// proven switch the replayer reads it at the set's end (GetMockErrors) and it
// fails the set. One made after that read reaches no verdict.
// GetMockErrors retrieves and the next Begin/ContinueTestErrorCapture resets
// the window.
func (p *Proxy) BeginTestErrorCapture() { p.openTestErrorCapture(false, false) }

// ContinueTestErrorCapture opens the next test's window, as
// BeginTestErrorCapture does, but carries into it the misses made since the
// previous test's window closed. The replayer opens it for every test after a
// set's first. A miss made with no test running, between two tests, is the
// next test's: the test the replay is running when the miss can first be
// reported, as a miss that lands in a later test's window is that test's.
// Discarding it would drop a real miss silently: a SEND the app publishes in
// reaction to something the replay did between two tests is reported against
// no test at all.
func (p *Proxy) ContinueTestErrorCapture() { p.openTestErrorCapture(true, false) }

// openTestErrorCapture opens the next test's capture window (carry: as
// ContinueTestErrorCapture). Under the proven switch the learn step's test
// begins with it, awaiting its test case's request where keploy test opened
// it (awaits: learn.Tracker.BeginAwaiting), in one step: no call reads the
// test's places between its begin and the wait.
func (p *Proxy) openTestErrorCapture(carry, awaits bool) {
	if provenswitch.On() {
		// The learn step's earlier places are the running test's.
		p.learnBoundary(func(t *learn.Tracker) {
			if awaits {
				t.BeginAwaiting()
			} else {
				t.Begin()
			}
		})
	}
	acc := &testErrorAccumulator{}
	p.captureMu.Lock()
	// Misses made with no test's window open — or into a window its test's
	// GetMockErrors already read (the degenerate path) — were made between
	// tests. A window never read at all belongs to a test that can no longer
	// report for it (its GetMockErrors failed, or it never ran): carrying its
	// misses into THIS window would claim them as this test's, so they are
	// dropped.
	var unread []error
	var between []error
	old := p.activeTestErrors.Swap(acc)
	p.testWindows.Add(1)
	if old != nil {
		if old.read {
			between = old.drain()
		} else {
			unread = old.drain()
		}
	}
	between = append(between, p.pendingMockErrors.drain()...)
	pendingLost := p.pendingLost
	p.pendingLost = false
	// Under the proven switch a set's first test carries what was missed
	// before it as the set's (beforeSetMiss): nothing is dropped.
	beforeSet := !carry && provenswitch.On()
	switch {
	case carry:
		for _, err := range between {
			if !acc.addBounded(err, maxPendingMockErrors) {
				p.missDroppedForRoom(err)
			}
		}
		if pendingLost {
			// They were this test's: its read is short of them.
			p.missLoss.CompareAndSwap(nil, &missLostBetweenFull)
		}
	case beforeSet:
		for _, err := range between {
			if !acc.addBounded(beforeSetMiss{err}, maxPendingMockErrors) {
				p.noteMissLost(err, missLostBeforeSetFull)
			}
		}
		if pendingLost {
			// They were the set's: the read that carries them is short of them.
			p.missLoss.CompareAndSwap(nil, &missLostBeforeSetFull)
		}
	}
	p.captureMu.Unlock()
	if !carry && !beforeSet {
		p.logDroppedMisses("mock mismatches made while no test's capture window was open are not attributed to a test", between)
		if pendingLost {
			// They were no test's either: said, and no read is short of them.
			p.logger.Warn("more mock mismatches were made while no test's capture window was open than keploy keeps; the rest were dropped, and they are not attributed to a test either",
				zap.Int("kept", maxPendingMockErrors))
		}
	}
	p.logDroppedMisses("mock mismatches of a test whose capture window was never read are dropped", unread)
}

// logDroppedMisses says which misses are dropped: at Warn, except misses that
// count against no test alone (models.MockMismatchReport.CountsAgainstTest:
// an offline runner's upstream DNS at the app's startup, or a connection that
// sent nothing, whose port a warning already names once per endpoint), which
// the end-of-run summary leaves out of a green run too, at Debug.
func (p *Proxy) logDroppedMisses(msg string, errs []error) {
	if len(errs) == 0 {
		return
	}
	level := zap.DebugLevel
	for _, err := range errs {
		var pe models.ParserError
		if !errors.As(err, &pe) || pe.MismatchReport == nil || pe.MismatchReport.CountsAgainstTest() {
			level = zap.WarnLevel
			break
		}
	}
	p.logger.Log(level, msg, zap.Int("count", len(errs)), zap.Strings("calls", missSummaries(errs)))
}

// sayLearnWaits has the learn step's tracker say in the agent's log as a call
// waits for its test case's request, and, at the default level, as a test's
// window closes without it.
func (p *Proxy) sayLearnWaits() {
	// A call made after keploy test opened a test's window and before the
	// filter that opens it brought its test case's request waits for the
	// request (learn.Tracker.AwaitRequest): said, so a call that waits is
	// never silent.
	p.learnTracker.OnAwait(func() {
		p.logger.Debug("a call waits for its test case's request, which the filter that opens the test brings, before the learn step reads it")
	})
	// A test's window that closes before that filter brought the request
	// leaves the calls that waited, and every later call of the test,
	// learning from a request the learn step could not read: each value
	// that changed in them is refused. Said at the default level.
	p.learnTracker.OnUnreadClose(func(waiting int) {
		p.logger.Warn("a test case's capture window closed before the filter that opens the test brought its request to the agent: the learn step refuses every value that changed in the test's calls, as it could not read the request the application was given; the test's mocks keep comparing those fields",
			zap.Int("callsWaiting", waiting),
			zap.String("next_step", "check that keploy test reached the agent with the test case's filter (its log says why it did not), and that keploy and its agent run the same version"))
	})
}

// missSummaries names, for a log line, the calls of up to the first 10 misses.
func missSummaries(errs []error) []string {
	out := make([]string, 0, min(len(errs), 10))
	for _, err := range errs {
		if len(out) == 10 {
			break
		}
		var pe models.ParserError
		if errors.As(err, &pe) && pe.MismatchReport != nil && pe.MismatchReport.ActualSummary != "" {
			out = append(out, pe.MismatchReport.ActualSummary)
			continue
		}
		out = append(out, err.Error())
	}
	return out
}

// WithdrawTestErrorCapture takes back the window the latest Begin or
// ContinueTestErrorCapture opened, for a test that did not begin after all:
// the agent's scope begin opens the window first, so the learn tracker's test
// begins before the test's view is installed, and a begin that then fails
// served the test nothing. The tracker's test ends, and the window is left as
// one its test already read: what it holds, the misses made since the
// previous test's window closed included, and what is missed until the next
// window opens are carried into the next test's window (openTestErrorCapture),
// as they would have been had this window never opened. Nothing is dropped
// or charged to the test that did not begin. The tracker's test ends
// through the mock manager, as every boundary of its tracker does
// (learnBoundary), so the manager's round reads the tracker as it is.
func (p *Proxy) WithdrawTestErrorCapture() {
	if provenswitch.On() {
		p.learnBoundary(func(t *learn.Tracker) { t.End() })
	}
	p.captureMu.Lock()
	if acc := p.activeTestErrors.Load(); acc != nil {
		acc.read = true
	}
	p.captureMu.Unlock()
}

// EndTestErrorCapture stops collecting errors and returns all accumulated errors.
func (p *Proxy) EndTestErrorCapture() []error {
	if provenswitch.On() {
		p.learnBoundary(func(t *learn.Tracker) { t.End() })
	}
	acc := p.activeTestErrors.Swap(nil)
	p.testWindows.Add(1)
	if acc != nil {
		return acc.drain()
	}
	return nil
}

// GetErrorChannel returns the error channel for external monitoring.
// Once StartProxy has run, StartErrorDrain continuously drains this channel in a
// background goroutine, so an external direct consumer would compete with the
// drain for reads and is not supported. Per-test mock-not-found errors are
// exposed via BeginTestErrorCapture/GetMockErrors instead.
func (p *Proxy) GetErrorChannel() <-chan error {
	return p.errChannel
}

// GetMockErrors drains all mock-not-found errors and returns them.
// When StartErrorDrain is active, it reads from the test accumulator instead
// of the channel (which is drained by the background goroutine).
func (p *Proxy) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	// Rendezvous with the drain goroutine BEFORE taking captureMu: push a flush
	// marker and wait until the goroutine pulls it. FIFO ordering then
	// guarantees every error the goroutine had already received from errChannel
	// has been routed into the window — so closing the window below cannot
	// strand an in-flight miss (the remaining race the previous fix left open).
	// The app has already responded by now, and every parser queues a miss
	// before the app can see it (an HTTP 502 included), so the pushes of every
	// miss decided before the app answered precede the marker. A miss decided
	// after it (the app answered without waiting for the call, on its own
	// client timeout) is not ordered by this: with the proven switch on,
	// Agent.settle waits for in-flight calls before the read; with it off,
	// such a miss is charged to the next test, and this read cannot know it,
	// until the settle-before-read follow-up (auto-replay runs) lands.
	//
	// Nothing is bounded by a clock, with the proven switch on or off: the
	// waits end on the marker being reached, the drain stopping, or the caller
	// giving up (unreadCause), and a read that could not vouch for every miss
	// that reached the proxy's queue says so (ErrUnmatchedCallsUnread) rather
	// than returning a list such a miss may be missing from. On a starved
	// machine the drain is slow, not wrong.
	// With the switch off this was a two-second deadline, and a full channel
	// skipped the marker: either left a miss the drain had not routed yet to
	// be read with the next test's, on a loaded runner, charging it to that
	// test, which neither failed for it nor kept auto-noise from masking it.
	unreadCause := ""
	rendezvousOK := false
	drainActive := p.errDrainActive.Load()
	if drainActive && p.errChannel != nil {
		marker := &flushMarker{done: make(chan struct{})}
		rendezvousOK, unreadCause = p.flushOnEvents(ctx, marker)
	}
	if lost := p.missLoss.Swap(nil); lost != nil && unreadCause == "" {
		unreadCause = *lost
	}

	p.captureMu.Lock()

	// Pre-window stragglers (retained before the first capture window — e.g.
	// startup traffic). Oldest first. A read that takes them is short of any
	// that were dropped for room.
	rawErrs := p.pendingMockErrors.drain()
	if p.pendingLost {
		p.pendingLost = false
		if unreadCause == "" {
			unreadCause = missLostBetweenFull
		}
	}

	// Legacy path only (no drain goroutine): errors sit unrouted in errChannel,
	// so sweep them here. With the drain goroutine active, the rendezvous above
	// has already routed everything into the window, so sweeping is unnecessary
	// and would only risk pulling a NEXT test's stray error into this one.
	if !drainActive {
	drainLoop:
		for {
			select {
			case err, ok := <-p.errChannel:
				if !ok {
					break drainLoop
				}
				if m, isMarker := err.(*flushMarker); isMarker {
					close(m.done)
					continue
				}
				rawErrs = append(rawErrs, err)
			default:
				break drainLoop
			}
		}
	}

	if rendezvousOK || !drainActive {
		// Safe to END the window: the rendezvous confirmed every received error
		// is routed (or there's no drain goroutine). Swap to nil so the drain
		// goroutine stops appending — otherwise the last test's window would
		// stay installed and grow for the rest of the process.
		if acc := p.activeTestErrors.Swap(nil); acc != nil {
			rawErrs = append(rawErrs, acc.drain()...)
		}
		p.testWindows.Add(1)
		if provenswitch.On() {
			// The test is over: no test is open until the next one begins.
			p.learnBoundary(func(t *learn.Tracker) { t.End() })
		}
	} else {
		// Rendezvous could not complete (the drain stopped, or the caller gave
		// up): the read reports itself incomplete (unreadCause). Take what the
		// window holds now but DON'T close it,
		// so a miss the goroutine is still routing isn't dropped from THIS fetch.
		// Marked read: what the drain files into it afterwards was made after
		// this test's misses were taken, and the next window carries it as a
		// miss made between tests (ContinueTestErrorCapture), or a set's
		// first discards it. Degenerate path.
		if acc := p.activeTestErrors.Load(); acc != nil {
			rawErrs = append(rawErrs, acc.drain()...)
			acc.read = true
		}
		p.testWindows.Add(1)
	}
	p.captureMu.Unlock()

	var errs []models.UnmatchedCall
	for _, err := range rawErrs {
		// A miss made before the set's first test (beforeSetMiss) is read
		// as the set's, an outside_test miss.
		before, isBefore := err.(beforeSetMiss)
		if isBefore {
			err = before.error
		}
		first := len(errs)
		if parserErr, ok := err.(models.ParserError); ok && parserErr.ParserErrorType == models.ErrMockNotFound {
			if parserErr.MismatchReport != nil {
				errs = append(errs, models.UnmatchedCall{
					At:            parserErr.MismatchReport.At,
					Protocol:      parserErr.MismatchReport.Protocol,
					ActualSummary: parserErr.MismatchReport.ActualSummary,
					Destination:   parserErr.MismatchReport.Destination,
					ClosestMock:   parserErr.MismatchReport.ClosestMock,
					Diff:          parserErr.MismatchReport.Diff,
					NextSteps:     withStagingNote(parserErr, parserErr.MismatchReport.NextSteps),
					MatchPhase:    parserErr.MismatchReport.MatchPhase,
					// Separate axis from MatchPhase: the phase still reports
					// where the cascade stopped, the scope says whether any
					// mock this miss was compared against targeted that
					// upstream. Deliberately a statement about the COMPARED
					// SET, not about the whole recording — mocks served
					// earlier in the run are no longer in that set, so a
					// wider claim would be false.
					DestinationScope: parserErr.MismatchReport.DestinationScope,
					CandidateCount:   parserErr.MismatchReport.CandidateCount,
					FieldDiffs:       parserErr.MismatchReport.FieldDiffs,
					ClosestMockReq:   parserErr.MismatchReport.ClosestMockReq,
					ReceivedReq:      parserErr.MismatchReport.ReceivedReq,
					Kind:             parserErr.MissKind,
					Recorded:         parserErr.Recorded,
					Differs:          parserErr.MismatchReport.Differs,
					FieldReasons:     parserErr.MismatchReport.FieldReasons,
					VariablePlaces:   parserErr.MismatchReport.VariablePlaces,
					Given:            parserErr.MismatchReport.Given,
					NotOwned:         parserErr.MismatchReport.NotOwned,
					// Why the call missed, when record's full round's learn
					// step decided it: judging's evidence.
					Reason:     parserErr.MismatchReport.Reason,
					Concurrent: parserErr.MismatchReport.Concurrent,
					// An answered call's report (models.ReportAnswer, proven
					// switch): how it was answered, never a miss.
					Answer:     parserErr.MismatchReport.Answer,
					Drift:      parserErr.MismatchReport.Drift,
					Reused:     parserErr.MismatchReport.Reused,
					WriteKind:  parserErr.MismatchReport.WriteKind,
					Source:     parserErr.MismatchReport.Source,
					RecordedIn: parserErr.MismatchReport.RecordedIn,
					Repeat:     parserErr.MismatchReport.Repeat.Clone(),
				})
			} else if errors.Is(parserErr.Err, models.ErrNoMockMatched) {
				// A genuine miss without a structured report must still reach
				// the user's report — silently dropping it here is how whole
				// protocols' misses used to vanish from
				// FailureInfo.UnmatchedCalls. Errors that are NOT wrapped
				// around models.ErrNoMockMatched are infrastructure/decode
				// failures mislabeled by sendMockNotFoundError; reporting
				// them as unmatched calls would misdirect the user, so they
				// stay out of the report (they are already logged).
				errs = append(errs, models.UnmatchedCall{
					At:            parserErr.At,
					Protocol:      "unknown",
					ActualSummary: parserErr.Err.Error(),
					NextSteps:     withStagingNote(parserErr, "This protocol's matcher does not emit structured mismatch reports yet; check the agent logs around this test for the mock-miss details."),
					Kind:          parserErr.MissKind,
				})
			}
		}
		if isBefore {
			for i := first; i < len(errs); i++ {
				errs[i].BeforeSet = true
				errs[i].Kind = models.MissOutsideTest
			}
		}
	}
	if unreadCause != "" {
		return errs, fmt.Errorf("%w: %s", models.ErrUnmatchedCallsUnread, unreadCause)
	}
	return errs, nil
}

// flushOnEvents is GetMockErrors' rendezvous with the error drain, with the
// proven switch on or off: queue the flush marker and wait for the drain to
// reach it, ending only on events.
// It returns whether the drain reached the marker, and when it did not, why:
// the drain stopped (nothing will route a miss into the window again) or the
// caller gave up on the read (its context ended).
func (p *Proxy) flushOnEvents(ctx context.Context, marker *flushMarker) (bool, string) {
	select {
	case p.errChannel <- marker:
	case <-p.errDrainDone:
		return false, "keploy's error drain stopped before this read"
	case <-ctx.Done():
		return false, "the read was given up before the error drain took it: " + context.Cause(ctx).Error()
	}
	select {
	case <-marker.done:
		return true, ""
	case <-p.errDrainDone:
		return false, "keploy's error drain stopped before it routed every miss"
	case <-ctx.Done():
		return false, "the read was given up before the error drain routed every miss: " + context.Cause(ctx).Error()
	}
}

// SendError sends an error to the error channel for external monitoring.
//
// A miss is never dropped for want of room, with the proven switch on or off:
// while the error drain runs it waits for the drain to take it, which the
// drain always does (it holds nothing a sender holds). A miss that still
// cannot be queued, because the drain has stopped or never started, is lost,
// and the next GetMockErrors reports its read incomplete instead of silently
// short.
func (p *Proxy) SendError(err error) {
	miss := isMockNotFoundErr(err)
	if miss && p.errDrainActive.Load() {
		select {
		case p.errChannel <- err:
			return
		case <-p.errDrainDone:
		}
		// The drain stopped while this waited: a select picks either ready
		// case, so a miss with room is still queued, not lost.
		select {
		case p.errChannel <- err:
			return
		default:
		}
		p.noteMissLost(err, missLostDrainStopped)
		p.logger.Error("keploy's error drain stopped; a call no mock answered could not be reported to its test", zap.Error(err))
		return
	}
	select {
	case p.errChannel <- err:
	default:
		p.logger.Error("Error channel is full, dropping error", zap.Error(err))
		p.noteMissLost(err, missLostNoDrainChanFull)
	}
}

// reportedMiss marks a call no mock matched under the proven switch, where
// keploy's verdict names each miss once, with its test, at the end of the
// run: the connection handler logs it at Debug, not as an error.
type reportedMiss struct{ err error }

func (r reportedMiss) Error() string { return r.err.Error() }

func (r reportedMiss) Unwrap() error { return r.err }

// mismatchReporter is an error that carries the report of a call no mock
// matched.
type mismatchReporter interface {
	MismatchReport() *models.MockMismatchReport
}

// isMockMiss reports whether err is a call no mock matched, the kind a
// replay counts as missed, rather than a parser or I/O failure.
func isMockMiss(err error) bool {
	var reporter mismatchReporter
	return errors.Is(err, models.ErrNoMockMatched) || (errors.As(err, &reporter) && reporter != nil)
}

// mockFailed reports a MockOutgoing failure and returns the error the
// connection handler gets. Under the proven switch, whose verdict names each
// miss once with its test at the end of the run, a miss is logged at Debug
// and returned as reportedMiss; anything else, and every run with the switch
// off, keeps the error log it always had.
func (p *Proxy) mockFailed(logger *zap.Logger, err error) error {
	return p.mockFailedFrom(logger, err, 0, nil)
}

// mockFailedFrom is mockFailed for a connection from process pid (0 when the
// redirect gave none), whom its miss names, dialled at dst (nil when none is
// known), which its miss names when its parser named no destination
// (sendMockNotFoundErrorFrom).
func (p *Proxy) mockFailedFrom(logger *zap.Logger, err error, pid uint32, dst *models.ConditionalDstCfg) error {
	if errors.Is(err, util.ErrParserPanicked) {
		// keploy's failure, counted as a capture loss where the parser
		// returned it (countParserPanic): no miss, so neither the mismatch
		// line nor a slot among the misses kept for the test's read.
		utils.LogError(logger, err, "failed to mock the outgoing message")
		return err
	}
	quiet := provenswitch.On() && isMockMiss(err)
	if quiet {
		logger.Debug("failed to mock the outgoing message", zap.Error(err))
	} else {
		utils.LogError(logger, err, "failed to mock the outgoing message")
	}
	// A miss its parser reported before answering the app (an HTTP 502) is
	// queued for its test already; queued again it would count twice, the
	// second time possibly against the next test.
	if !models.IsMismatchReported(err) {
		p.sendMockNotFoundErrorFrom(err, pid, dst)
	}
	if quiet {
		return reportedMiss{err}
	}
	return err
}

// mismatchLog is how the mock-mismatch line is logged: Warn, so a replay
// shows each miss as it happens, except under the proven switch, whose
// verdict names each miss once with its test at the end (Debug there, or the
// miss would be printed twice).
func (p *Proxy) mismatchLog() func(string, ...zap.Field) {
	if provenswitch.On() {
		return p.logger.Debug
	}
	return p.logger.Warn
}

// sendMockNotFoundError builds a ParserError from a mock-miss error,
// extracting the MismatchReport if the error carries one.
func (p *Proxy) sendMockNotFoundError(err error) {
	p.sendMockNotFoundErrorFrom(err, 0, nil)
}

// sendMockNotFoundErrorFrom is sendMockNotFoundError for a call from process
// pid (0 when the redirect gave none). Under the proven switch the report,
// a miss's or an answer's (models.ReportAnswer), names that process
// (MockMismatchReport.Source) when its parser named none. An
// answered call (models.ErrAnswered) is stamped as a miss is, and kept for
// the test's read with the misses, but it is no miss: it is not logged as
// one.
//
// dst is where the connection was dialled (nil when that is not known). A
// report whose parser named no destination names dst's address: an HTTP call
// with no Host header (an HTTP/1.0 client, an instance-metadata probe) has no
// authority of its own, and its miss said nothing of where the call went. A
// fabricated address (models.ConditionalDstCfg.AddrFabricated) names no
// server, so it is not named either.
func (p *Proxy) sendMockNotFoundErrorFrom(err error, pid uint32, dst *models.ConditionalDstCfg) {
	proxyErr := models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		Err:             err,
		At:              time.Now(),
	}
	// Extract diff report from the error chain if available.
	// Use errors.As to traverse wrapped errors.
	var reporter mismatchReporter
	if errors.As(err, &reporter) && reporter != nil {
		proxyErr.MismatchReport = reporter.MismatchReport()
	}
	// Every miss says when it happened: `keploy mock` files it under the test
	// it was made in by that time, and drops one without it. A parser that
	// built its report without one gets the time the miss reached here (a
	// copy: the report is the parser's).
	if r := proxyErr.MismatchReport; r != nil && r.At.IsZero() {
		stamped := *r
		stamped.At = proxyErr.At
		proxyErr.MismatchReport = &stamped
	}
	// The address the call was dialled at, when its parser named none. A
	// copy, as above; the report's destination scope was settled by its
	// parser, on what the parser saw.
	if r := proxyErr.MismatchReport; r != nil && r.Destination == "" && dst != nil && dst.Addr != "" && !dst.AddrFabricated {
		stamped := *r
		stamped.Destination = dst.Addr
		proxyErr.MismatchReport = &stamped
	}
	// The process that made the call (proven switch): its comm and argv[0],
	// and, only to travel to keploy's receipt, its pid and whole command
	// line, redacted; where keploy runs in a PID namespace of its own, no
	// name, why, and its kernel pid (starts.Registry.SourceOf). A copy, as
	// above.
	if r := proxyErr.MismatchReport; r != nil && r.Source == nil && pid != 0 && provenswitch.On() {
		if src := starts.Default.SourceOf(pid); src != nil {
			stamped := *r
			stamped.Source = src
			proxyErr.MismatchReport = &stamped
		}
	}
	// Under exact serving (the proven switch) a miss says where it was made:
	// in the open test, outside every test, or, as its matcher found, as one
	// call more than the test recorded.
	if r := proxyErr.MismatchReport; r != nil {
		proxyErr.Recorded = r.Recorded
	}
	if m := p.getMockManager(); m != nil {
		if proxyErr.MissKind = m.exactMissKind(proxyErr.MismatchReport); proxyErr.MissKind != "" {
			// Only a miss made in the open test is an extra call, and only
			// an extra call says how often the call was recorded. The
			// matcher found it one against the test's own answered mocks,
			// which only an open test has: an extra call filed after its
			// test ended (its last call in flight) stays one.
			if r := proxyErr.MismatchReport; r != nil && r.MissKind == models.MissExtraCall {
				proxyErr.MissKind = models.MissExtraCall
			} else {
				proxyErr.Recorded = 0
			}
			// A miss made while the replay was staging the set's mocks says
			// so (stagingMissNote), told where its call was decided.
			proxyErr.Staging = m.missMadeInStaging(proxyErr.MismatchReport)
		}
	}
	// Single, protocol-agnostic mock-mismatch log. EVERY parser's MockOutgoing
	// miss funnels through here, so this one line covers HTTP, generic, MySQL,
	// gRPC, etc. uniformly — it names WHICH upstream missed and how far matching
	// got, instead of each parser logging it differently (or not at all). The
	// per-parser decode loggers stay at Debug to avoid double-logging.
	if r := proxyErr.MismatchReport; r != nil && r.Answer != "" {
		// An answered call (models.ReportAnswer): no mismatch.
		p.logger.Debug("an answered call, reported for judging",
			zap.String("answer", r.Answer),
			zap.String("protocol", r.Protocol),
			zap.String("destination", r.Destination),
			zap.String("write_kind", r.WriteKind),
			zap.String("recorded", r.ClosestMock),
			zap.String("recorded_in", r.RecordedIn),
			zap.Bool("drift", r.Drift),
			zap.Bool("reused", r.Reused))
	} else if r != nil {
		fields := []zap.Field{
			zap.String("protocol", r.Protocol),
			zap.String("destination", r.Destination),
			zap.String("actual", r.ActualSummary),
			zap.String("match_phase", r.MatchPhase),
			zap.Int("candidates", r.CandidateCount),
			zap.String("closest", r.ClosestMock),
			zap.String("next_step", r.NextSteps),
		}
		// destination_scope is greppable in a node-wide agent log:
		// "not_in_compared_set" is the one value that says "nothing this miss
		// was compared against even targets that upstream" — in Kubernetes
		// most often a sibling container's egress (replay intercepts the whole
		// pod while record armed only the container the user named), and in
		// any mode endpoint drift or a per-test mock window that excluded the
		// mock. models.OutOfScopeDestinationCauses carries the user-facing
		// wording; the log field is the machine-greppable half.
		//
		// A string, and emitted ONLY when the question was actually answered.
		// The mechanism is protocol-agnostic, but HTTP is the only protocol
		// that supplies destination evidence today (nothing else records an
		// upstream authority in its mocks), so an unconditional boolean
		// printed a negative on every mongo/mysql/generic miss — an unchecked
		// negative that reads as "we checked, and it was fine". Absent means
		// "not established", which is the truth.
		if r.DestinationScope != models.DestinationScopeUnknown {
			fields = append(fields, zap.String("destination_scope", r.DestinationScope))
		}
		p.mismatchLog()("mock mismatch: no matching mock for outgoing call", fields...)
	} else {
		p.mismatchLog()("mock mismatch: no matching mock for outgoing call (no structured report)", zap.Error(err))
	}
	p.SendError(proxyErr)
}

// CloseErrorChannel closes the error channel
func (p *Proxy) CloseErrorChannel() {
	close(p.errChannel)
}

func (p *Proxy) Mapping(ctx context.Context, mappingCh chan models.TestMockMapping) {
	mgr := syncMock.Get()
	if mgr != nil {
		mgr.SetMappingChannel(ctx, mappingCh)
	}
}

func isShutdownError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	errStr := err.Error()
	if strings.Contains(errStr, "use of closed network connection") {
		return true
	}
	if neterr.IsConnReset(err) || neterr.IsConnAborted(err) {
		return true
	}
	if strings.Contains(errStr, "connection reset by peer") {
		return true
	}
	// Kept for errors that arrive as plain strings with no errno left in the
	// chain. Note the two halves are not equally reliable: "wsarecv" and
	// "wsasend" are Go's own syscall op names (net/fd_windows.go), so they
	// are locale-independent and already match every socket read/write error
	// on Windows — broadly enough that the errno check above is redundant at
	// this particular site. "forcibly closed by the remote host" is the
	// opposite: Windows renders it in the system display language, so it is
	// an English-only match and must never be the only thing carrying a
	// classification.
	if strings.Contains(errStr, "wsarecv") || strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host") {
		return true
	}
	return false
}

func refusedLocally(err error) bool {
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Addr == nil {
		return false
	}
	host, _, splitErr := net.SplitHostPort(op.Addr.String())
	if splitErr != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// LiveHandler serves a test's call to the app it started (the ingress's
// ServeLive), recording it as a test case, and returns once conn ends. ctx
// carries the connection's values and lives as the proxy does, not as the
// connection does: a test case is built after its exchange, off the
// forwarding path, and so may still be in the making when conn has ended.
type LiveHandler func(ctx context.Context, conn net.Conn, upstream string, port uint16)

// connValues is a context with the values of a connection's context and the
// life of another, longer one (Done, Err, Deadline): what the proxy hands the
// live handler (LiveHandler), with the proxy's life. The connection's own
// context is cancelled as its handler returns, and the test case of a
// connection's last exchange, built after it, was dropped by that cancel
// whenever it came first: half the live calls of a client that closes each
// connection after one exchange (Connection: close) were recorded as
// nothing. conn descends from life, so a key conn lacks life lacks too, but
// for the one context.WithoutCancel hides: the context package's own, by
// which a context derived from this one, and context.Cause, find the cancel
// they belong to. That one is life's, read from life: a derived context is
// life's child and ends as life does, with life's cause, and the
// connection's cancel is never taken for this context's.
type connValues struct {
	context.Context
	values context.Context
}

func (c connValues) Value(key any) any {
	if v := c.values.Value(key); v != nil {
		return v
	}
	return c.Context.Value(key)
}

// withLifeOf is conn's values with life's cancellation (connValues). conn
// must descend from life.
func withLifeOf(life, conn context.Context) context.Context {
	return connValues{Context: life, values: context.WithoutCancel(conn)}
}

func (p *Proxy) SetLiveHandler(h LiveHandler) {
	p.live = h
	starts.Default.OnMark(p.closeDependencyConns)
}

func ownIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// pidToUint32 narrows a PID to the uint32 the dependency tracker keys on,
// guarding the architecture-dependent int->uint32 conversion (CWE-681): the
// PID flows from a strconv.Atoi of /proc and is always a small non-negative
// value, so an out-of-range int (never expected for a real PID) collapses to
// 0 — which Dependency reads as "no such process", the safe default.
func pidToUint32(pid int) uint32 {
	if pid < 0 || pid > math.MaxUint32 {
		return 0
	}
	return uint32(pid)
}

func (p *Proxy) trackTree(conn net.Conn, owner int) func() {
	p.treeMu.Lock()
	if p.tree == nil {
		p.tree = map[net.Conn]int{}
	}
	p.tree[conn] = owner
	p.treeMu.Unlock()
	return func() {
		p.treeMu.Lock()
		delete(p.tree, conn)
		p.treeMu.Unlock()
	}
}

// logSelfCalls logs how many self-calls the run passed through, and how many
// of its processes served them, when there was more than the one already
// logged at INFO, and only when the count has moved since it last did:
// SetGracefulShutdown, which calls it, may be called more than once.
func (p *Proxy) logSelfCalls() {
	p.logUnnamedConns()
	n := p.selfCalls.Load()
	if n < 2 {
		return
	}
	for {
		last := p.selfCallsLogged.Load()
		if n <= last {
			return
		}
		if p.selfCallsLogged.CompareAndSwap(last, n) {
			break
		}
	}
	processes := 0
	p.selfCallOwners.Range(func(_, _ any) bool {
		processes++
		return true
	})
	p.logger.Info("self-calls passed through to the run's own listeners; the real handler answered them, not a mock",
		zap.Int64("calls", n), zap.Int("processes", processes),
		zap.String("reason", models.ReasonLoopbackOutsideRun))
}

// unnamedCallerWarning is what the agent says of a connection to a listener
// of the run's own tree whose caller it cannot name in its PID namespace.
const unnamedCallerWarning = "a connection to the run's own listener came from a process keploy cannot name in its PID namespace, so keploy cannot tell whether a test made its calls: it was passed to the listener and NONE of its calls was recorded as a test case"

// logUnnamedConns logs, when the run ends, how many connections to the run's
// own listeners came from a process the agent could not name (callerOf),
// when there was more than the one already logged as it came, and only when
// the count has moved since it last did.
func (p *Proxy) logUnnamedConns() {
	n := p.unnamedConns.Load()
	if n < 2 {
		return
	}
	for {
		last := p.unnamedConnsLogged.Load()
		if n <= last {
			return
		}
		if p.unnamedConnsLogged.CompareAndSwap(last, n) {
			break
		}
	}
	p.logger.Warn("connections to the run's own listeners came from processes keploy cannot name in its PID namespace; none of the calls they carried was recorded as a test case",
		zap.Int64("connections", n),
		zap.String("next_step", "run keploy in the PID namespace of the app's tests, or in the host's (--pid=host, or hostPID in Kubernetes)"))
}

func (p *Proxy) closeDependencyConns() {
	p.treeMu.Lock()
	defer p.treeMu.Unlock()
	for conn, owner := range p.tree {
		if starts.Default.Dependency(pidToUint32(owner)) {
			p.logger.Debug("closing a connection opened before the app was marked to a process that turned out to be a dependency", zap.Int("owner", owner))
			_ = conn.Close()
		}
	}
}

// serveTreeListener relays a call to a listener of the run's own process
// tree to that listener (served). Under the proven switch, a call it leaves
// to the mock path that is addressed to this machine comes back as outside,
// its mocked-local row (without its test), for the caller to list once it
// knows the call is recorded or mocked. ctx is the one the live handler is
// handed (LiveHandler): the connection's values with the proxy's life.
func (p *Proxy) serveTreeListener(ctx context.Context, srcConn net.Conn, dest *agent.NetworkAddress, dstAddr string, conn string) (served bool, outside *models.MockedRow, err error) {
	if !p.mockMode || p.IsDocker || p.appPID == 0 {
		return false, nil, nil
	}
	host, _, err := net.SplitHostPort(dstAddr)
	if err != nil {
		return false, nil, nil
	}
	ip := net.ParseIP(host)
	if !ownIP(ip) {
		return false, nil, nil
	}
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			ip = net.IPv4(127, 0, 0, 1)
		} else {
			ip = net.IPv6loopback
		}
	}
	owner, listening := listenerOwner(ip, dest.Port)
	if !listening && ip != nil && ip.IsLoopback() && (!recorded.has(dest.Port) || recorded.child(dest.Port)) {
		if _, isReal := p.realLabelAt(dstAddr); isReal {
			// A mock.real destination nothing listens at is mock.real's
			// (serveReal): its connection fails as the app's would, and the
			// open test's REAL row counts the failure, as it does when the
			// handshake hold refuses the connect (realDialFailed).
			return false, nil, nil
		}
		p.logger.Debug("nothing listens at the local destination yet; closing the call as the connect would have been refused", zap.String("destination", dstAddr))
		return true, nil, nil
	}
	// Under the proven switch the run's process tree is the membership
	// (release1-cut §5.1 items 3 and 8): app marking is retired, so a
	// listener the tree owns is always the run's own and always live, and
	// any other local listener is a dependency, recorded and mocked, and
	// listed (design v14 §3.1).
	proven := provenswitch.On()
	if owner == 0 || !descends(owner, int(p.appPID)) {
		p.logger.Debug("the destination is not a process the test command started", zap.String("destination", dstAddr), zap.Int("owner", owner), zap.Bool("listening", listening))
		if proven && (listening || recorded.has(dest.Port)) {
			// Listed whether or not anything listens there now: a
			// recorded local dependency that is not running (CI) is
			// mocked all the same, and may be the app under test. A call
			// nothing answers and no recording has (a record's dial that
			// will fail, a replay's miss) is neither recorded nor mocked.
			outside = localListener(owner, dstAddr, dest.Port)
		}
		return false, outside, nil
	}
	seeProcess(Process{Pid: pidToUint32(owner)})
	if !proven && starts.Default.Dependency(pidToUint32(owner)) {
		p.logger.Debug("the destination is a dependency the tests started; recording and serving it as mocks", zap.String("destination", dstAddr), zap.Int("owner", owner))
		starts.Default.Child(conn)
		return false, nil, nil
	}
	// A port the recording called as a (non-child) outside dependency stays on
	// the mock path even when something in the run's tree now listens there:
	// those recorded responses are the dependency's, and serving the live
	// listener would skip the mock the test set is built on. A genuine
	// self-call is unaffected — an app's own ingress port is never a recorded
	// dependency destination. Not under the proven switch: there a listener
	// of the run's tree is live whatever a recording says (design v14 §3.1),
	// and a recording that mocked it leaves its mocks unconsumed (U11).
	if !proven && ip.IsLoopback() && RecordedPort(dest.Port) {
		p.logger.Debug("the destination is a port the recording called as an outside dependency; serving it as mocks", zap.String("destination", dstAddr), zap.Int("owner", owner))
		return false, nil, nil
	}
	caller, unnamed := callerOf(dest)
	// A mock.real destination (proven switch only) that the tree's listener
	// serves is reached through it: counted as a relay to the destination
	// would be (treeRealConn), or the run, whose recordings list it, fails
	// as never reaching it.
	realLabel, isReal := p.realLabelAt(dstAddr)
	var realID int64
	if isReal {
		realID, _ = strconv.ParseInt(conn, 10, 64)
	}
	if proven {
		// A call to a listener another process of the tree owns is LIVE,
		// listed per test; one a process makes to its own listener (an
		// in-process test server) is not.
		if caller != owner {
			srcConn = p.liveConn(srcConn, owner, dest.Port)
		}
	} else {
		// Only app marking closes these (closeDependencyConns).
		defer p.trackTree(srcConn, owner)()
	}
	// A self-call: the destination is a listener owned by the run's own process
	// tree, so it is passed through to the real handler instead of served from a
	// mock, and a broken self-handler fails the run rather than passing on a
	// stale mock. The run's first self-call is logged at INFO and the rest at
	// Debug; logSelfCalls logs the total when the run ends. A runner that calls
	// its own in-process server (supertest's request(app) starts one per
	// request) makes a self-call per connection, so a line each at INFO would
	// flood the log.
	p.selfCallOwners.LoadOrStore(owner, struct{}{})
	if p.selfCalls.Add(1) == 1 {
		p.logger.Info("self-call passed through to the run's own listener; the real handler answers it, not a mock",
			zap.String("destination", dstAddr), zap.Int("owner", owner), zap.Int("caller", caller),
			zap.Uint32("caller_kernel_pid", dest.KernelPid),
			zap.String("reason", models.ReasonLoopbackOutsideRun),
			zap.String("note", "further self-calls are logged at debug, and counted at the end of a run that finishes"))
	} else {
		p.logger.Debug("self-call passed through to the run's own listener; the real handler answers it, not a mock",
			zap.String("destination", dstAddr), zap.Int("owner", owner), zap.Int("caller", caller),
			zap.Uint32("caller_kernel_pid", dest.KernelPid),
			zap.String("reason", models.ReasonLoopbackOutsideRun))
	}
	if p.live != nil && unnamed {
		// A test's call to the app it started is recorded as a test case
		// (the live handler), and only a test's: a connection whose caller
		// the agent cannot name may carry such calls, so it is counted and
		// said, not passed through in silence: over the agent's life here
		// (the first warned of, the total logged as the run ends), and on
		// the recording's manager, whose session summary and stream end
		// marker report the session's count. Connections, not calls: this
		// relays them unread.
		syncMock.FromContextOrGlobal(ctx).NoteUnnamedCallerConn()
		if p.unnamedConns.Add(1) == 1 {
			p.logger.Warn(unnamedCallerWarning,
				zap.String("destination", dstAddr), zap.Int("owner", owner),
				zap.Uint32("caller_kernel_pid", dest.KernelPid),
				zap.String("next_step", "run keploy in the PID namespace of the app's tests, or in the host's (--pid=host, or hostPID in Kubernetes)"),
				zap.String("note", "further such connections are counted at the end of a run that finishes"))
		}
	}
	if p.live != nil && caller != owner && descends(owner, caller) {
		if isReal {
			// The live handler dials for itself: the listener's answer
			// reaches the destination, and the caller's bytes are the open
			// test's use, with no use point (Tracker.SentLive).
			srcConn = &treeRealConn{Conn: srcConn, p: p, label: realLabel, id: realID, live: true}
		}
		p.live(ctx, srcConn, dstAddr, uint16(dest.Port))
		return true, nil, nil
	}
	dstConn, err := util.DialDestination(ctx, p.logger, "tcp", util.DialTarget{Addr: dstAddr})
	if isReal && err == nil {
		// DialDestination falls back to the other loopback family when the
		// app's address refuses: that reaches an address an IP-literal
		// entry does not list, which relayReal, dialing exactly the app's
		// address, never counts. A localhost entry lists both families.
		if label, ok := p.realLabelAt(dstConn.RemoteAddr().String()); !ok || label != realLabel {
			isReal = false
		}
	}
	if isReal {
		// A dial that fails counts in the open test's row; one that
		// connects reaches the destination (relayReal).
		p.realTracker.Dialed(realID, realLabel, err == nil)
	}
	if err != nil {
		return true, nil, err
	}
	defer dstConn.Close()
	if isReal {
		srcConn = &treeRealConn{Conn: srcConn, p: p, label: realLabel, id: realID}
	}
	util.RelayRawPassthrough(srcConn, dstConn)
	return true, nil, nil
}
