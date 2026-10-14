package agent

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

type Hooks interface {
	// AppInfo
	DestInfo
	Load(ctx context.Context, cfg HookCfg, setupOpts config.Agent) error
	WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error)
}

type HookCfg struct {
	Pid      uint32
	IsDocker bool
	Mode     models.Mode
	Rules    []models.BypassRule
	Port     uint32
}

// ProxyAddressReporter is the optional extension implemented by hooks that
// know the IPv4 address the application reaches the proxy at. The agent hands
// it to the proxy's DNS server, which answers with it for a name it has no
// recorded answer for.
//
// Optional in the same style as WindowedProxy: the agent type-asserts for it,
// and hooks that do not implement it keep the proxy's loopback default.
type ProxyAddressReporter interface {
	// ProxyIPv4 is only meaningful once Load has returned.
	ProxyIPv4() string
}

// PIDMapReporter is the optional extension implemented by hooks that count
// the inserts into their per-process maps the kernel refused, each of them a
// process they could not track (models.PIDMapRefusals). The agent
// type-asserts for it, as for ProxyAddressReporter; with hooks that do not
// implement it the agent answers "unknown"
// (models.ErrPIDMapRefusalsUnsupported), never "none refused".
type PIDMapReporter interface {
	// PIDMapRefusals is only meaningful once Load has returned.
	PIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// CaptureLossReporter is the optional extension implemented by a proxy that
// counts the calls it intercepted and could not record or mock
// (models.CaptureLosses). The agent adds these counts to the hooks' own.
type CaptureLossReporter interface {
	CaptureLosses() models.CaptureLosses
}

// ListenerCounter is what the hooks give the proxy so that the kernel counts,
// and numbers, the connections completed to the proxy's listener.
type ListenerCounter interface {
	// MarkProxyListener marks the proxy's listening socket, rc, as the one
	// whose connections the kernel counts. The mark is copied into every
	// socket the kernel creates for a connection to it, from the moment it
	// is set, so the proxy marks its socket before listen(): every
	// connection it can ever accept is then counted. The socket must be
	// plain TCP; an MPTCP one is refused, since the kernel creates its
	// connections' sockets from a TCP socket of its own that the mark never
	// reaches.
	MarkProxyListener(rc syscall.RawConn) error
	// ProxyConnPair is what the kernel left in the accepted connection rc's
	// socket as it counted it (ProxyConnPairing): its sequence number, and
	// the application connection it pairs with, with that one's original
	// destination. A connection with no number is an error.
	ProxyConnPair(rc syscall.RawConn) (ProxyConnPairing, error)
}

// ProxyConnPairing is an accepted connection of the proxy's marked listener,
// as the kernel counted it.
type ProxyConnPairing struct {
	// Seq is its number among the connections completed to the listener:
	// 1, 2, ...
	Seq uint64
	// AppSeq is the number of the application's connection it is the
	// proxy's side of, 0 when the kernel could tie it to none (not one of
	// the application's redirected connections, or one the proxy sees at
	// other addresses: a NAT between them).
	AppSeq uint64
	// Dst is that application connection's original destination, set with
	// AppSeq: where the connection is going, its own, whatever any other
	// connection on the same port was doing.
	Dst *NetworkAddress
}

// ProxyConnsReporter is the optional extension implemented by hooks that
// count, in the kernel, the connections completed to the proxy's listener
// (models.ProxyConnCounts' Established). The agent type-asserts for it; with
// hooks that do not implement it the agent answers
// models.ErrProxyConnsUnsupported, "unknown".
type ProxyConnsReporter interface {
	ListenerCounter
	// ProxyConnsEstablished is the count of the connections completed to the
	// marked listener, the highest sequence number given. It only ever
	// grows, for the life of the hooks. It fails until a listener has been
	// marked.
	ProxyConnsEstablished(ctx context.Context) (uint64, error)
	// AppConnsEstablished is the count of the application's own connections
	// to the proxy (the ones keploy redirected) whose connect() has
	// returned, the highest number the kernel gave them. A connection is in
	// it before its final ACK reaches the proxy's side. It fails as
	// ProxyConnsEstablished does, and once the kernel had no room for one of
	// its events.
	AppConnsEstablished(ctx context.Context) (uint64, error)
	// AppConnsSettled is the highest n such that every application
	// connection numbered 1..n is resolved: paired with the proxy's side of
	// it (then counted by ProxyConnsEstablished), or lost (it closed, or its
	// port was reused, first: a capture loss).
	AppConnsSettled() (uint64, error)
	// WaitAppConnsSettled returns once AppConnsSettled() >= n, or with ctx's
	// error, or when the count is unknown. No deadline of its own.
	WaitAppConnsSettled(ctx context.Context, n uint64) error
}

// ListenerCountTaker is the optional extension implemented by proxies that can
// have their listener counted by the hooks. The agent hands it the hooks
// (ListenerCounter), under the proven switch only, before StartProxy; the
// proxy then listens plain TCP, never MPTCP, marks its socket before
// listen(), and reads each accepted connection's sequence number.
type ListenerCountTaker interface {
	SetListenerCounter(c ListenerCounter)
}

// AcceptCounter is the optional extension implemented by proxies that keep
// the sequence numbers of the connections their marked listener accepts.
type AcceptCounter interface {
	// Accepted is the highest n such that every connection the kernel
	// numbered 1..n has been accepted. It fails when the listener is not
	// marked (the hooks could not mark it, it has no listener, nothing asked
	// it to), has closed, or a number could not be read: "unknown".
	Accepted() (uint64, error)
	// WaitAccepted returns once Accepted() >= n, or with ctx's error when
	// ctx ends first, or with Accepted's error, at once or when it comes.
	// It has no deadline of its own; each accept wakes it.
	WaitAccepted(ctx context.Context, n uint64) error
	// ListenDrops is the marked listener's count of the packets the kernel
	// dropped at it (models.ProxyConnCounts' ListenDrops). It fails as
	// Accepted does.
	ListenDrops() (uint64, error)
}

// ProxyConnCounter is what the agent offers a test's begin and end (S12): the
// connections to the proxy, numbered on both sides, and a wait for every call
// the application had started by a read to be in the proxy's hands. begin and
// end read ProxyConns, then WaitProxySettled(ctx, counts), then go on to the
// bytes and requests the proxy holds; a test whose ListenDrops differ between
// its begin and its end is not verified, and an application connection lost
// before the proxy got it is a capture loss (CaptureLossAppConnLost).
//
// The counts only ever grow for the life of the agent process. An agent that
// cannot count answers an error wrapping models.ErrProxyConnsUnsupported
// ("unknown", never 0). ProxyConns is cheap (a few locked map lookups, a
// locked read and a getsockopt): a watchdog may call it at every wake-up to see
// the wait progress.
type ProxyConnCounter interface {
	ProxyConns(ctx context.Context) (models.ProxyConnCounts, error)
	// WaitProxySettled returns once every application connection counts
	// numbered (AppConns) is resolved, and the proxy has accepted every
	// connection the kernel completed to it by then; or with ctx's error.
	// Connections made after counts was read cannot hold it back. No
	// deadline of its own; it looks again at each resolution and accept.
	WaitProxySettled(ctx context.Context, counts models.ProxyConnCounts) error
	// WaitProxyAccepted returns once the proxy has accepted every connection
	// the kernel numbered 1..n on its side, or with ctx's error.
	WaitProxyAccepted(ctx context.Context, n uint64) error
}

type AuxiliaryProxyHook interface {
	AfterStart(ctx context.Context, proxy Proxy) error
}

// TeardownableProxyHook is an optional extension of AuxiliaryProxyHook. When
// the registered hook implements it, the proxy calls BeforeTeardown
// synchronously during shutdown — after the listeners/handlers have stopped
// and BEFORE the OS trust store is touched (TeardownNativeCA, which may fork
// update-ca-certificates). It runs inside the proxy's awaited start()
// goroutine, so it completes before the process exits.
//
// A hook that attaches uprobes in AfterStart MUST remove them here: an unjoined
// teardown that races process exit (or the update-ca-certificates fork) can be
// caught with a fatal signal pending, which makes the kernel's breakpoint
// removal return -EINTR and leak the uprobe (and the int3) into every process
// mapping the probed file. Running the teardown here removes that race.
//
// ctx is the proxy context and is already cancelled when this is called
// (shutdown is what triggers it), so an implementation that needs to make a
// context-bound call during teardown should derive a fresh one with
// context.WithoutCancel.
type TeardownableProxyHook interface {
	BeforeTeardown(ctx context.Context) error
}

// Proxy listens on all available interfaces and forwards traffic to the destination.
//
// Proxy is the stable contract; window-aware methods live on the optional
// WindowedProxy interface below so adding them does not break third-party
// implementers. Callers should type-assert when they need windowing —
// see agent.go's UpdateMockParams for the canonical pattern.
type Proxy interface {
	StartProxy(ctx context.Context, opts ProxyOptions) error
	Record(ctx context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error)
	MakeClientDeRegisterd(ctx context.Context) error
	GetErrorChannel() <-chan error
	// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
	// When this flag is set, connection errors will be logged as debug instead of error.
	SetGracefulShutdown(ctx context.Context) error
	Mapping(ctx context.Context, mappingCh chan models.TestMockMapping)
	GetDestInfo() DestInfo
	GetIntegrations() map[integrations.IntegrationType]integrations.Integrations
	GetSession() *Session
	// GetSessionFor returns the session that owns the connection from tgid.
	// With no resolver installed it returns the single session (the
	// single-app default); a multi-app composer installs a resolver via
	// SetSessionResolver to route per app.
	GetSessionFor(tgid uint32) *Session
	// SetSessionResolver installs a per-TGID session resolver (nil reverts
	// to single-session mode). The seam by which an external composer routes
	// per-app traffic without this package knowing what an "app" is.
	SetSessionResolver(fn func(tgid uint32) *Session)
	SetAuxiliaryHook(h AuxiliaryProxyHook)
	// Per-PID (worker-keyed) mock scoping for parallel test runners (Design A).
	// SetWorkerScope registers the per-test mock-name allowlist for a worker
	// PID; ClearWorkerScope drops it (on scope end / no mapping);
	// ClearAllWorkerScopes wipes every entry when a replay session starts. A
	// connection is attributed to a worker by walking its opener's PID up the
	// /proc tree, once, when it opens; what it sees then follows the nearest
	// registered worker on that chain, read by read.
	SetWorkerScope(pid uint32, names []string)
	ClearWorkerScope(pid uint32)
	ClearAllWorkerScopes()
	// SetMappedUniverse records the union of all tests' mapped mock names so a
	// scoped worker can keep genuinely-shared (unmapped) mocks while hiding
	// other tests' mocks.
	SetMappedUniverse(names []string)
}

// PcapStreamer is the optional extension implemented by proxies
// that broadcast captured packets to dynamic subscribers. Callers
// MUST type-assert from Proxy and gracefully fall back when the
// assertion fails — this keeps third-party Proxy implementations
// compiling without forcing them to implement packet capture.
//
// SubscribePcap registers w to receive every captured frame as a
// pcap byte stream (file header followed by one record per frame).
// flush, when non-nil, is invoked after each frame so chunked
// transports (HTTP) can push bytes immediately. The unsubscribe
// func MUST be called when the consumer is done — typically on the
// HTTP request context's cancellation.
//
// Why streaming, not pulling: the cluster live-recording use case
// has no defined "stop" — the recorder is always connected. A
// fetch-on-stop model would never deliver any bytes. Subscribers
// always receive a fresh pcap header and frames from then on, so
// disconnects do not corrupt the stream of any other consumer.
type PcapStreamer interface {
	SubscribePcap(w io.Writer, flush func()) (func(), error)
}

// WindowedProxy is the optional extension implemented by proxies that
// support per-test [req,res] window enforcement (Option-1 strict
// containment). Callers MUST type-assert from Proxy and gracefully
// fall back when the assertion fails — this keeps third-party Proxy
// implementations compiling without the extra methods.
//
//	if wp, ok := p.(WindowedProxy); ok {
//	    _ = wp.SetMocksWithWindow(ctx, f, u, start, end)
//	} else {
//	    _ = p.SetMocks(ctx, f, u)
//	}
//
// StartupCutoffSeeder is the optional extension implemented by proxies whose
// mock manager can be told the earliest RECORDED test start of the set being
// staged, before any test of that set has run.
//
// Optional in the same style as WindowedProxy: the agent type-asserts for it and
// simply does not seed when the assertion fails, so no existing proxy breaks and
// third-party proxies keep compiling. Not seeding leaves the cutoff derived from
// the fired windows, which is the pre-existing behaviour.
type StartupCutoffSeeder interface {
	SeedStartupCutoff(start time.Time)
}

// RecordedWindowsSeeder is the optional extension implemented by proxies whose
// mock manager can be told the recorded window of every test of the set being
// staged (models.MockFilterParams.RecordedWindows). Optional in the same style
// as StartupCutoffSeeder: an agent whose proxy lacks it does not seed, and
// consumers then release everything at once, the pre-existing behaviour.
type RecordedWindowsSeeder interface {
	SeedRecordedWindows(ws []models.TestWindow)
}

// StatefulCursorResetter is the optional extension implemented by proxies whose
// mock manager keeps per-request cursors for stateful dependency replay (serving
// successive recorded responses in record order). The agent calls it once per
// test-set, on the staging call, so a stateful sequence replays from its first
// recorded response in each test-set rather than carrying a cursor across sets.
// Optional in the same style as StartupCutoffSeeder: a proxy that lacks it is a
// no-op (a proxy without stateful cursors has nothing to reset).
type StatefulCursorResetter interface {
	ResetStatefulCursors()
}

// VariableResetter is the optional extension implemented by proxies whose mock
// manager keeps the test variables a replay binds (package variables). Under
// the proven switch the agent calls it when a scope begins: on the staging call
// of a test-set, and at the begin of each test of keploy mock replay. A proxy
// that lacks it follows no variables, so it has none to forget.
type VariableResetter interface {
	ResetVariables()
}

// VariableBinder is the optional extension implemented by proxies that keep
// the test variables a replay binds: it begins a test's variables with the
// values its own request gave (false when it holds no table), and reads back
// what the test has bound.
type VariableBinder interface {
	StartVariables(given []models.VariableBinding) bool
	BoundVariables() ([]models.VariableBinding, bool)
}

// CarryOverPlanner is the optional extension implemented by proxies with a
// carry-over tier (models.RegisterCarryOver). For the window that starts at
// start it names the recorded-time range of registered per-test mocks the agent
// loads from its on-disk store beside the window's own mocks and passes in the
// filtered slice of the same SetMocksWithWindow call. ok is false when there is
// nothing to load. A proxy without it gets no lookahead load, the pre-existing
// behaviour.
type CarryOverPlanner interface {
	CarryOverLoadRange(start time.Time) (from, to time.Time, ok bool)
}

type WindowedProxy interface {
	// SetMocksWithWindow atomically replaces mocks AND publishes the active
	// outer-test [req,res] window.
	SetMocksWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time) error
}

// AsyncMockLoader is an optional Proxy capability. The agent hands the proxy
// the COMPLETE async-mock corpus exactly once (at mock-store time) so the
// async engine can build its per-lane ordered streams. Proxies without async
// support simply don't implement it.
type AsyncMockLoader interface {
	LoadAsyncMocks(mocks []*models.Mock)
}

// FirstWindowStartReader is an optional extension implemented by proxies
// that can report the earliest test window start observed by their
// underlying MockManager. Consumed by the agent's tier-aware
// strictMockWindow filter to distinguish startup-init mocks (req_ts <
// firstWindowStart) from stale previous-test mocks (firstWindowStart <=
// req_ts < currentStart). Returns the zero time before the first real
// test window has landed.
//
// Callers MUST type-assert from Proxy and gracefully fall back (zero
// time = "no cutoff known, keep legacy behaviour") when the assertion
// fails — keeps third-party Proxy implementations compiling without
// the extra method.
type FirstWindowStartReader interface {
	FirstTestWindowStart() time.Time
}

// ConsumedStateReader is implemented by proxies whose mock manager keeps a
// persistent (never-drained) per-name consumption map. It lets the agent apply
// filterOutDeleted from its OWN history instead of a copy the client re-sends
// every testcase — see models.MockFilterParams.AgentOwnsConsumed.
type ConsumedStateReader interface {
	GetPersistentConsumed() map[string]models.MockState
}

type IncomingProxy interface {
	Start(ctx context.Context, opts models.IncomingOptions) chan *models.TestCase
}

// AppListenPorter is the optional extension of an IncomingProxy that knows
// which port the app's own socket listens on for one of the app's ports. While
// recording, keploy's ingress forwarder holds the app's port and the app
// listens on the port its bind was moved to.
type AppListenPorter interface {
	AppListenPort(orig uint16) (port uint16, ok bool)
}

type ProxyOptions struct {
	// DNSIPv4Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv4Addr string
	// DNSIPv6Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv6Addr string
}

type DestInfo interface {
	Get(ctx context.Context, srcPort uint16) (*NetworkAddress, error)
	Delete(ctx context.Context, srcPort uint16) error
}

// ErrConnectingSocketGone is GetForHandshake's error when the application's
// connecting socket no longer exists: it gave up on the connection.
var ErrConnectingSocketGone = errors.New("the application's connecting socket is gone")

// HandshakeDestInfo is a DestInfo that can name a redirected connection's
// original destination while its handshake with the proxy is still in flight:
// client is the application's end of the connection and proxy the proxy's,
// as the SYN carries them. Get can only answer once the handshake completes,
// which is too late for the proxy to make connect itself fail the way the
// destination failed it (see synhold).
type HandshakeDestInfo interface {
	// CheckHandshakeLookup reports whether GetForHandshake can work here.
	CheckHandshakeLookup(ctx context.Context) error
	GetForHandshake(ctx context.Context, client, proxy netip.AddrPort) (*NetworkAddress, error)
}

type TestBenchInfo interface {
	// SendKeployPids(key models.ModeKey, pid uint32) error
	// SendKeployPorts(key models.ModeKey, port uint32) error
}

type NetworkAddress struct {
	Version  uint32
	IPv4Addr uint32
	IPv6Addr [4]uint32
	Port     uint32
	// KernelPid is the source process's kernel (root-namespace) PID for this
	// outgoing connection, captured in-kernel by the eBPF redirect program. It
	// lets the mock-serve path attribute a call to the test worker that made it
	// (per-PID scoping for parallel runners). 0 when unavailable (non-eBPF
	// platforms / lookup miss) — callers must treat 0 as "unknown, no scope".
	KernelPid uint32
}

// Sessions provides a thread-safe store for Session objects, keyed by ID.
// Used by the hooks packages (linux, windows, others) to track client sessions.
type Sessions struct {
	sessions sync.Map
}

func NewSessions() *Sessions {
	return &Sessions{
		sessions: sync.Map{},
	}
}

func (s *Sessions) Get(id uint64) (*Session, bool) {
	v, ok := s.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

func (s *Sessions) Set(id uint64, session *Session) {
	s.sessions.Store(id, session)
}

func (s *Sessions) Delete(id uint64) {
	s.sessions.Delete(id)
}

type Session struct {
	ID   uint64
	Mode models.Mode
	TC   chan<- *models.TestCase
	MC   chan<- *models.Mock
	models.OutgoingOptions
}
