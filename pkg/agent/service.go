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
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

type Hooks interface {
	// AppInfo
	DestInfo
	// Load loads the hooks; what it leaves running goes on the errgroup ctx
	// carries (models.ErrGroupKey) and ends once ctx is cancelled. Load itself
	// must return without waiting for ctx to be cancelled: the agent's
	// teardown cancels it only once Load has returned (Agent.Hook). It runs as
	// teardown work while a stop waits for it, where its progress can be seen
	// (Linux, utils.TeardownProgressSeen; utils.BeginTeardownWork): what it
	// waits for on another goroutine must be teardown work of its own there.
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

// CaptureLossDrainer is the optional extension implemented by hooks whose
// counts of capture losses are fed by reports the kernel makes: before a read
// of them, DrainCaptureLosses waits until every report made before it has
// been counted, so a call the application made before the read is in it.
type CaptureLossDrainer interface {
	DrainCaptureLosses(ctx context.Context) error
}

// CaptureLossErrReporter is the optional extension implemented by hooks whose
// count of a capture loss could not start (under the proven switch): while
// CaptureLossesErr is not nil, what it would count is unknown, never 0, and
// the agent reads no capture losses.
type CaptureLossErrReporter interface {
	CaptureLossesErr() error
}

// ReadinessReporter is the optional extension implemented by hooks that can
// tell, under the proven switch, whether the application listens and whether
// its start is going on (models.AppReadiness). An error is "unknown", never
// "not ready".
type ReadinessReporter interface {
	AppReadiness(since uint64) (models.AppReadiness, error)
	MarkReadiness() (models.AppReadiness, error)
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

// ProxyConnCounter is what the agent offers a test's begin and end: the
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
	// numbered (AppConns) is resolved, the proxy has accepted every
	// connection the kernel completed to it by then, and the proxy's side of
	// every live connection paired with one of those has received the bytes
	// the application had written to it (a pooled connection's call); or
	// with ctx's error. Connections made, and bytes written, after counts
	// was read cannot hold it back. No deadline of its own; it looks again at
	// each resolution, accept and arrival.
	WaitProxySettled(ctx context.Context, counts models.ProxyConnCounts) error
	// WaitProxyAccepted returns once the proxy has accepted every connection
	// the kernel numbered 1..n on its side, or with ctx's error.
	WaitProxyAccepted(ctx context.Context, n uint64) error
}

// InFlightReader is the optional extension of a proxy that tells, under the
// proven switch, whether a test's calls are still in its hands once every
// connection the kernel completed to it was accepted (the end count, design
// v14 §5.2): bytes an accepted connection holds unread, and, in a replay, a
// request read and not yet answered. It waits by events, never by a clock.
type InFlightReader interface {
	// InFlightChanged is closed at the next change of what InFlightState
	// reads: take it before reading.
	InFlightChanged() <-chan struct{}
	// InFlightPendingProgress is, for each connection that holds such work
	// now (an opaque id), a count of what moved its own work, for a watchdog:
	// a wait whose pending connections' counts do not move is stuck,
	// whatever moves elsewhere.
	InFlightPendingProgress(answers bool) map[uint64]uint64
	// InFlightState reads each accepted connection once, as one view:
	// pending is how many hold such work (answers counts unanswered
	// requests too: a replay, whose answers are the proxy's own work; not a
	// record, where a request waits for its real server); unknown names the
	// protocols of those, in a replay, whose parser reads ahead of its
	// answers and does not say which requests it holds
	// (integrations.RequestHolding), or says that for now it cannot tell
	// (integrations.MarkUnknown): whether they hold one cannot be told, so
	// a test's end is not verified. There is no separate count of either:
	// read apart, a connection whose parser ends a state it could not tell
	// between the two reads is seen neither busy nor unknown.
	InFlightState(answers bool) (pending int, unknown []string)
}

// InFlightScoper is the optional extension of an InFlightReader whose wait
// can be kept to the connections a settle began with (InFlightScope), so
// that an application that keeps calling cannot hold a test's end.
type InFlightScoper interface {
	// InFlightScope keeps the accepted connections now: once every
	// connection and byte a settle waits for is in the proxy's hands, the
	// connections that can hold its calls.
	InFlightScope() InFlightScope
}

// InFlightScope is InFlightState and InFlightPendingProgress over the
// connections kept when it began, each until it is seen holding no work
// once: a connection accepted later never counts, nor work one takes on
// after it was seen settled. unknown names every protocol whose parser,
// as its connection was seen settled, could not say what it held.
type InFlightScope interface {
	State(answers bool) (pending int, unknown []string)
	PendingProgress(answers bool) map[uint64]uint64
}

// PairedBytesWaiter is the optional extension of a proxy that holds the
// connections the kernel paired with the application's (under the proven
// switch) and can wait for the bytes on them: WaitPairedBytes returns once
// the proxy's side of every such connection paired with an application
// connection numbered 1..appConns has received every byte the application had
// written to it when the wait began. Bytes that can no longer arrive (the
// connection was reset, or the proxy closed it, first) are a capture loss
// (models.CaptureLossAppBytesLost). No deadline of its own: each read the
// proxy makes on such a connection, and its close, wakes it; so do looks of
// its own at the orphaned socket of one the proxy closed while the
// application's FIN is still on its way to it, which nothing else wakes it
// for, and it answers unknown only once the agent could run for a hang
// budget with nothing it waits on moving. A proxy that cannot tell answers
// an error wrapping models.ErrProxyConnsUnsupported.
type PairedBytesWaiter interface {
	WaitPairedBytes(ctx context.Context, appConns uint64) error
	// PairedBytesProgress counts the waits' progress (a waited-for
	// connection received more, saw its orphan change state, or was
	// resolved), for a watchdog: it only ever grows.
	PairedBytesProgress() uint64
}

// PairedBytesChecker is the proxy's account of whether it can count the bytes
// on its paired connections: nil while it can (PairedBytesWaiter's waits can
// be answered).
type PairedBytesChecker interface {
	PairedBytesErr() error
}

// CountingReporter says whether the kernel counted the calls waiting for the
// proxy when it came up (its listener marked, its own check of the count
// passed, and the bytes on its paired connections readable), and why, with
// the proven switch off, they stopped being counted since (a NAT'd
// connection), nil while they are.
type CountingReporter interface {
	CountedFromStart() bool
	// CountingLost is a channel closed once there is that reason, and the
	// reason.
	CountingLost() (<-chan struct{}, error)
}

type AuxiliaryProxyHook interface {
	// AfterStart runs once the proxy has started, ordered against
	// BeforeTeardown: a teardown that comes while it runs waits for it. Where
	// its progress can be seen (Linux, utils.TeardownProgressSeen) it runs as
	// teardown work (utils.BeginTeardownWork), and keeps the stopping agent
	// from being killed in its middle only while it makes progress on its own
	// goroutine -- what it waits for on another goroutine must be teardown
	// work of its own there. Elsewhere it is not teardown work, and a stop
	// that waits for it is killed for want of progress. It must return once
	// ctx is cancelled.
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
// It is also called when the proxy fails to start after the CA install --
// its listener cannot bind -- and AfterStart has then not run, nor will: an
// implementation removes whatever it holds, and nothing more. It runs once,
// never before an AfterStart in flight has returned, and no AfterStart
// begins after it (the proxy orders the two).
//
// It runs as teardown work (utils.BeginTeardownWork). Where its progress can
// be seen (Linux, utils.TeardownProgressSeen), the stopping agent is kept
// from being killed in the middle of it only while it makes progress on its
// own goroutine: what it waits for on another goroutine -- a uprobe
// registration, say -- shows no progress of its, and must be teardown work of
// its own, begun on that goroutine. Elsewhere it counts as making progress
// until it returns.
//
// ctx is the proxy context, cancelled when shutdown is what triggers the call
// and possibly not when a failed start is, so an implementation that needs to
// make a context-bound call during teardown should derive a fresh one with
// context.WithoutCancel, and none should infer from ctx's state why it was
// called.
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

// StatementCatalogSeeder is the optional extension implemented by proxies whose
// mock manager can hold the stored set's statement catalog (the first recorded
// mock of each statement the agent keeps on disk), describe-only, for the
// parsers' integrations.StatementCatalog capability. Optional in the same style
// as StartupCutoffSeeder: an agent whose proxy lacks it does not seed, and a
// statement prepared at app start that its recordings hold only inside tests
// is then described from what the parser may serve, as before.
type StatementCatalogSeeder interface {
	SeedStatementCatalog(mocks []*models.Mock)
}

// RecordedDestinationsSeeder is the optional extension implemented by proxies
// whose destination rules (the MySQL ports, the loopback ports a recording
// reached, the destinations recorded speaking HTTP/2) can be told the stored
// set's recorded destinations: the first mock of each that the agent keeps
// on disk. Optional in the same style as StatementCatalogSeeder: an agent
// whose proxy lacks it does not seed, and those rules then read only what is
// staged, as before.
type RecordedDestinationsSeeder interface {
	SeedRecordedDestinations(mocks []*models.Mock)
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

// TestRequestTaker is the optional extension implemented by proxies whose
// learn step reads a keploy test case's own request (models.TestRequest): the
// agent hands it the request the filter that opens the test case carries,
// before the test case's mocks go live (nil: the filter carried none, as a
// keploy older than its agent sends it). Under the proven switch the learn
// step reads it as the test's earliest place. A proxy that lacks it
// reads none.
//
// BeginTestCaseCapture and ContinueTestCaseCapture open the capture window of
// a test keploy test opens, as BeginTestErrorCapture and
// ContinueTestErrorCapture do, and in the same step begin the learn step's
// test awaiting its request, which its opening filter brings: until
// TakeTestRequest gives it, a call that reads the test's places waits for it,
// so the learn step's verdict on a call does not hang on whether the call
// came before the filter or after it. The agent calls one as keploy test
// opens the test's capture window.
type TestRequestTaker interface {
	BeginTestCaseCapture()
	ContinueTestCaseCapture()
	TakeTestRequest(req *models.TestRequest)
}

// ClientTestsReader is the optional extension implemented by proxies whose
// learn step reads keploy mock's tests as a test harness's client tests
// (learn.Tracker.SetClientTests): ClientTests reports whether it does, which
// the agent of a keploy mock session answers the session's MockOutgoing
// with (models.ClientTestsReadHeader). A proxy that lacks it reads none so.
type ClientTestsReader interface {
	ClientTests() bool
}

// RecordedBeginTaker is the optional extension implemented by proxies whose
// learn step bounds a client test's calls by the test's begin: the agent
// gives it, as a keploy mock test begins and before the proxy opens the
// test, the instant the test began at in its recording (the one the test's
// begin moves the app's clock to), so the test's first call has a window
// on the app's clock in the recording too (learn.Tracker.RecordedBegin). A
// proxy that lacks it bounds no call so.
type RecordedBeginTaker interface {
	TakeRecordedBegin(at time.Time)
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

// InFlightPlanner is the optional extension of a proxy that knows the set's
// recorded test windows: for the test window that starts at start, the
// exclusive start of the gap before it. The agent loads, beside the window's
// own mocks, the per-test mocks sent in that gap and answered at or after
// start (models.WindowSchedule.InFlightInto): calls in flight when the test
// began, which are its. ok is false when there is no such gap. A proxy
// without it loads none, the pre-existing behaviour.
type InFlightPlanner interface {
	InFlightFrom(start time.Time) (from time.Time, ok bool)
}

// ExactServer is the optional extension of a proxy that serves each test its
// own mocks exactly, under the proven switch (design v14 §5.2): while a test
// is open, each of its test mocks answers at most one request, in recorded
// order, and the pool answers only protocol-phase requests; while none is,
// the current key's pool slice answers. It is the one visibility path there:
// the agent moves it at every begin and end, and nothing else narrows or
// widens what a parser is served.
// ExactWorkerServer is implemented by a proxy that serves a test set's worker
// scope under exact serving (models.Mapping.Worker): ServeExactWorker names the
// worker-scope mocks the set's run has reached the place of, cumulative. Each
// view installed after it (ServeExactTest, ServeExactPool) shows them, whether
// a test is open or not, after the open test's own; each answers at most one
// request in the staging.
type ExactWorkerServer interface {
	ServeExactWorker(names []string) error
}

// ExactBackgroundServer is implemented by a proxy that serves a test set's
// background (its worker scope and its pool) by the background rules under
// exact serving (models.RecordedWithS12Routing, replay.RegisterS12Serving):
// StageExactBackground opens each set's staging under the rules the set is
// served by, before the staging installs a view, so every view of the
// staging reads the set's rules; ServeExactBackground then names the set's
// pool and its whole worker scope, once its mocks are staged; any rules but
// the background rules are today's. A proxy that does not implement it serves every set by
// today's rules, and the agent says so to the replay
// (models.BackgroundRulesHeader), which then keeps today's checks.
type ExactBackgroundServer interface {
	StageExactBackground(rules string) error
	ServeExactBackground(rules string, pool, worker []string) error
}

// RecordedOfKindSetter is implemented by a proxy whose pairing (record's full
// round) reads, beside what is staged, the set's recordings of
// a kind the agent keeps on disk: a test's own are staged only as it opens.
// SetRecordedOfKind sets where (src visits the on-disk mocks of a kind, in
// recorded order, decoding one only as visit loads it; nil for none).
type RecordedOfKindSetter interface {
	SetRecordedOfKind(src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error) error
}

// RecordingSourceSetter is implemented by a proxy whose miss diagnosis under
// exact serving (a call that is a recording made elsewhere) can read, beside
// what is staged, the set's recordings the agent keeps on disk: a test's own
// are staged only as it opens, so a call made before its test would find
// none. SetRecordingSource sets where (src visits the on-disk mocks of a kind
// indexed under any of keys, models.RequestKeys, in recorded order, decoding
// one only as visit loads it; nil for none). For diagnosis only: nothing it
// reads is served.
type RecordingSourceSetter interface {
	SetRecordingSource(src func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error) error
}

// RecordedBeforeSetter is implemented by a proxy whose learn step reads, as
// the earlier places of a pool recording, every recording of the staged set
// made before it (integrations.RecordedBeforeReader), the ones the agent keeps
// on disk too: a test's own are staged only as it opens. SetRecordedBefore
// sets where (src visits the on-disk mocks recorded before t, by request
// time, decoding one at a time; nil for none). Nothing it reads is served.
type RecordedBeforeSetter interface {
	SetRecordedBefore(src func(t time.Time, visit func(*models.Mock) bool) error) error
}

// LearnEndSetter is implemented by a proxy whose learn step a keploy test
// filter can end for the rest of a test set (models.MockFilterParams.
// LearnEnded): SetLearnEnded says whether it has ended. The agent sets it from
// every keploy test filter, before the filter's view goes live.
type LearnEndSetter interface {
	SetLearnEnded(ended bool) error
}

// KeptLearningSetter is implemented by a proxy that serves a test set's
// retry cycles the staged recordings (the pool's, the worker scope's) with
// what keploy record keeps of the set's first-cycle learning on them
// (models.MockFilterParams.Learned): SetKeptLearning puts it there, nil
// takes it off. The agent's own store carries it on the recordings it hands
// the proxy (a test's own); the staged ones are the proxy's.
type KeptLearningSetter interface {
	SetKeptLearning(l *learn.KeptLearning) error
}

type ExactServer interface {
	// ServeExactTest opens test, served exactly owned: its test mocks, in
	// recorded order. A test opened again starts with none answered.
	ServeExactTest(test string, owned []*models.Mock) error
	// ServeExactPool closes the open test, if any, and serves the pool
	// slice names of key between tests.
	ServeExactPool(key string, names []string) error
	// ExactTestOutcome is the latest test opened, open or ended, and which
	// of its test mocks have answered (in the order they did) and which have
	// not (in recorded order); final once the test stops being served. ok is
	// false when exact serving is off.
	ExactTestOutcome() (test string, consumed, unconsumed []string, ok bool)
}

// ExactTestOpener is an optional ExactServer capability of a proxy that keeps
// the test variables a replay binds (VariableBinder): OpenExactTest opens test
// as ServeExactTest does, and begins its variables with given bound in the
// same step, so no call its mocks answer binds before them. Begun after the
// test opened, a call its own mock answers between the two binds in no scope
// of the test's: before a set's first test, in the run's, which every later
// test of the set reads.
type ExactTestOpener interface {
	OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error
}

// AppClockSetter is an optional proxy capability: SetAppClock hands the
// proxy how to read the app's clock at a real instant, as the agent's hooks
// move it (a time-freeze; false where they cannot tell), so that the
// parsers' learn steps read a call's clock reads in its own window
// (learn.Tracker.CallWindow).
type AppClockSetter interface {
	SetAppClock(f func(real time.Time) (time.Time, bool))
}

// PoolNamesSetter is an optional ExactServer capability: SetPoolNames hands the
// proxy the names the replay's scope table puts in a pool slice, of every key,
// so that the parsers' learn steps can tell the pool's mocks from a test's
// where the replay sends no recorded test windows (keploy mock replay):
// integrations.PoolMembership.
type PoolNamesSetter interface {
	SetPoolNames(names []string) error
}

// ExactAheadServer is an optional ExactServer capability: ServeExactPoolAhead
// is ServeExactPool that also answers, while no test is open, the next test's
// mocks that were in flight when it began (ahead, models.ExactServing.Ahead),
// each at most once, as that test's: one answered so counts as answered when
// that test opens (ServeExactTest).
type ExactAheadServer interface {
	ServeExactPoolAhead(key string, names []string, ahead []*models.Mock) error
}

// ExactConcurrentServer is an optional ExactServer capability: it opens a test
// that shares recordings with the tests whose requests were open beside its
// own at record time (models.ExactServing.Concurrent, keploy record's full
// round): after owned, cands answer, each at most once in the staging, for
// whichever of those tests makes its call first, never required of the test,
// and never learned against (a parser's learn step refuses a call closest to
// one: another of those tests may have made it). OpenExactTestConcurrent opens
// it so and begins its variables with given bound in the same step, as
// ExactTestOpener does. An agent whose proxy has it says so on /storemocks
// (models.ExactConcurrentHeader).
type ExactConcurrentServer interface {
	ServeExactTestConcurrent(test string, owned, cands []*models.Mock) error
	OpenExactTestConcurrent(test string, owned, cands []*models.Mock, given []models.VariableBinding) error
}

// ExactKeyRestarter is an optional ExactServer capability: it starts a claim
// key's calls outside every test afresh, for a test process that claims the
// key again (a re-run is a new scope): the recordings of names, the key's
// pool slice, consumed outside every test answer again, and the key's
// stateful cursors start over.
type ExactKeyRestarter interface {
	RestartExactKey(key string, names []string) error
}

// ScopeStager is an optional Proxy capability: it stages the pool of a test
// of keploy mock replay that its harness began (open) or ended (!open) without
// a worker pid, with the proven switch off. Such a test is served the mocks
// its mapping names, so its window has no recorded-time bounds; it is open
// while the test runs. Between tests the pool is served as before the first
// test, less every per-test mock served while a test was open, which the
// proxy leaves out. A proxy without it is staged through WindowedProxy with
// the models.BaseTime sentinel, which opens no window, so its tier-aware
// parsers answer a test's calls from the startup tier, which then holds the
// test's mocks and the reusable tier.
type ScopeStager interface {
	SetScopeMocks(ctx context.Context, filtered, unFiltered []*models.Mock, open bool) error
}

type WindowedProxy interface {
	// SetMocksWithWindow atomically replaces mocks AND publishes the active
	// outer-test [req,res] window.
	SetMocksWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time) error
}

// WindowedTestOpener is an optional WindowedProxy capability of a proxy that
// keeps the test variables a replay binds (VariableBinder):
// OpenTestWithWindow stages a test's window as SetMocksWithWindow does, and
// begins the test's variables with given bound in the same step, as
// ExactTestOpener opens a test under exact serving.
type WindowedTestOpener interface {
	OpenTestWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time, given []models.VariableBinding) error
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

// ConnDestInfo is the optional extension of a DestInfo that names a
// connection's original destination by the application's end of it, its
// address and port, as the proxy sees the connection's remote end. The Linux
// hooks key their redirects so: an IPv4 and an IPv6 connection on the same
// local port are two connections. The proxy prefers it; Get and Delete by
// source port alone remain for hooks whose shims key by port (darwin,
// windows).
type ConnDestInfo interface {
	GetForConn(ctx context.Context, app netip.AddrPort) (*NetworkAddress, error)
	DeleteForConn(ctx context.Context, app netip.AddrPort) error
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
