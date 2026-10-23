// Package agent contains methods for setting up hooks and proxy along with registering keploy clients.
package agent

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type ClientMockStorage struct {
	// filtered = resident per-test mocks (only the disk-ineligible ones when
	// diskMocks is set; the full slice on the legacy blob path). unfiltered =
	// resident config/session pool.
	filtered   []*models.Mock
	unfiltered []*models.Mock
	// diskMocks (non-nil on the streaming path) holds window-eligible per-test
	// mocks on disk; UpdateMockParams loads only the current window from it.
	diskMocks *proxyPkg.DiskMocks
	mu        sync.RWMutex
	// refused names the mocks this store refused under the proven switch
	// (variables this keploy cannot follow, followable): each was logged as
	// it was refused. A mapping that lists one owns nothing for it, and the
	// call it recorded misses (mocksByName). The value says whether the mock
	// is a test mock (data-plane): one is a recorded call its test is judged
	// for (refusedOf).
	refused map[string]bool
	// statements is the set's statement catalog, read from diskMocks once
	// when it is built (DiskMocks.LoadStatements) and seeded on the proxy when
	// the store is published (seedStatementCatalog); nil with no on-disk store.
	statements []*models.Mock
}

type Agent struct {
	logger          *zap.Logger
	coreAgent.Proxy                // embedding the Proxy interface to transfer the proxy methods to the core object
	coreAgent.Hooks                // embedding the Hooks interface to transfer the hooks methods to the core object
	dockerClient    kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	coreAgent.IncomingProxy
	proxyStarted bool
	config       *config.Config
	// countsCalls: once its proxy was up, the kernel counted the calls
	// waiting for it (the proxy's CountedFromStart: its canary counted, the
	// bytes on its paired connections readable). Latched at the start
	// (latchCounting): a count lost later is a read that cannot vouch for
	// every miss, never a return to reads that do not settle.
	countsCalls atomic.Bool
	// readLosses is the agent's count of its capture losses at the last read
	// of a test's misses settled with the switch off (lostSinceLastRead).
	readLosses atomic.Uint64
	// activeClients sync.Map
	// New field for storing client-specific mocks
	clientMocks sync.Map // map[uint64]*ClientMockStorage
	Ip          string

	// strictLogOnce de-dupes the "strict mock window enabled" Info log
	// so it fires at most once per agent process instead of once per
	// test. Operators searching agent logs for strict-mode activity
	// still find a hit; they just don't get swamped by one line per
	// test on large suites.
	strictLogOnce sync.Once

	// setupCtx is the Setup-scoped context captured during Setup().
	// It outlives any single /agent/incoming HTTP stream — which is the
	// whole point: StartIncomingProxy uses this instead of the request ctx
	// so the ingress proxy's :8080 listener survives the recorder closing
	// the stream on /record/stop. Without this, k8s-proxy's stop sequence
	// (HTTP graceful-shutdown notification + stream close) collapses the
	// listener while the agent + app are still running, kubelet probes
	// get connection refused, and the pod enters CrashLoopBackOff.
	//
	// Cancellation semantics, exactly: setupCtx is the ctx returned by
	// `errgroup.WithContext(parentCtx)` inside Setup, so it cancels when
	// EITHER (a) the parent ctx cancels — the process-shutdown signal
	// path (SIGTERM via utils/ctx.go), OR (b) any goroutine added to that
	// errgroup returns a non-nil error. (b) is the intended hook-load
	// failure path — if hooks fail mid-flight the proxy should also stop;
	// don't try to decouple this without first auditing what consumes
	// `models.ErrGroupKey` downstream.
	setupCtx context.Context

	// tcChan is the test case channel returned by IncomingProxy.Start.
	// We hold a reference so SetGracefulShutdown can spawn a drainer:
	// the recorder closes its consuming HTTP stream on /record/stop and
	// nothing else reads tcChan after that. Without draining, the 100-
	// slot buffer fills within seconds and subsequent ingress traffic
	// (including kubelet probes) blocks at the channel send.
	//
	// Reads + writes must hold incomingMu — StartIncomingProxy (HTTP
	// /agent/incoming handler) writes, SetGracefulShutdown (a different
	// HTTP handler goroutine) reads when spawning the drainer.
	tcChan chan *models.TestCase

	// drainCancel cancels the currently-active tcChan drainer (if any).
	// SetGracefulShutdown stashes the cancel func of the drainer it
	// spawned; StartIncomingProxy calls it when a new /agent/incoming
	// stream attaches, so a new recording session takes over the channel
	// without competing against the previous session's drainer. A new
	// SetGracefulShutdown after a fresh StartIncomingProxy spawns a
	// fresh drainer — this gives proper session-aware lifecycle (vs the
	// original sync.Once design which left a permanent drainer running
	// after the first /agent/graceful-shutdown and starved any
	// subsequent reconnect's HandleIncoming of test cases).
	//
	// Guarded by drainMu so SetGracefulShutdown and StartIncomingProxy
	// can't race on the cancel.
	drainCancel context.CancelFunc
	drainMu     sync.Mutex

	// streamCtx is the request ctx of the latest /agent/incoming stream
	// passed into StartIncomingProxy. The drainer in SetGracefulShutdown
	// waits on its cancellation BEFORE consuming from tcChan — otherwise
	// the drainer races with the still-active HandleIncoming for-loop
	// over the same channel and steals tail-end test cases that the
	// recorder hasn't picked up yet. The /agent/graceful-shutdown call
	// from the recorder precedes its stream close by however long it
	// takes the recorder to drain its own client-side queue and tear
	// the connection down; we must not start consuming during that
	// window.
	//
	// Guarded by incomingMu — see tcChan.
	streamCtx context.Context

	// incomingMu protects streamCtx and tcChan against concurrent access
	// from StartIncomingProxy and SetGracefulShutdown (different HTTP
	// handler goroutines). The single-watcher guarantee — i.e. that
	// reconnecting recorders don't leak ListenForIngressEvents goroutines
	// — is enforced inside IncomingProxy.Start itself (via a sync.Once
	// on the IngressProxyManager), not here, so subsequent Start calls
	// with different IncomingOptions still update the proxy's filtering
	// behavior instead of being silently ignored.
	incomingMu sync.Mutex

	// clockMu serialises the moves of the run's frozen clock (beginTestClock,
	// endTestClock, releaseTestClock) with their state, so a begin and an end
	// that cross never land out of order. clockHeld is the open test (its
	// worker and name) that holds the clock; clockMoved is the test whose
	// move is in force, the zero key when the clock is at the start time.
	// They part when a test's end cannot move the clock back: the test no
	// longer holds it, so the next test can begin, but the clock is still off
	// the start time until a move back succeeds. clockMu is never held while
	// scopeMu is taken.
	clockMu    sync.Mutex
	clockHeld  scopeKey
	clockMoved scopeKey
	// clockHeldStart is the holder's worker's start time (procStartTime),
	// "" when the agent cannot see that process: a begin from another worker
	// while the holder's has exited (or its pid is another process's now)
	// knows the holder's end will never come.
	clockHeldStart string

	// scope state backs the /agent/scope/* and /agent/mock/stats API that a
	// user's test runner (pytest / go test / jest / glue-code) calls to mark
	// per-test boundaries. In record mode it collects the agent-clock window for
	// each named test; in test (replay) mode it uses the CLI-supplied table to
	// restrict the served pool to one test's mocks. All fields guarded by scopeMu.
	scopeMu      sync.Mutex
	workerOpen   map[scopeKey]time.Time // record: (worker PID, test name) -> begin time (agent clock)
	scopeWindows []models.ScopeWindow   // record: closed per-test windows
	scopeMeta    map[scopeKey]scopeMeta
	scopeTable   map[string][]string  // replay: test name -> mock names (from mappings.yaml)
	testBegins   map[string]time.Time // replay: test name -> when it began in its recording (SetTestBegins)
	testReal     map[string][]string  // replay: test name -> the mock.real destinations its recording lists (SetTestBegins)
	testPool     []string             // keploy test, proven switch: the set's pool, served between test cases (exact_proven.go)
	testWorker   []models.MockEntry   // keploy test, proven switch: the set's worker scope, each mock with its place (exact_proven.go)
	testBegun    map[string]bool      // keploy test, proven switch: the set's test cases begun since its staging (exact_proven.go)
	testBegunAt  map[string]int       // keploy test, proven switch: each begun test case's place in the order they began (exact_proven.go)
	testBg       *testBackground      // keploy test, proven switch: the set's background under S12's rules, nil for today's (exact_proven.go)
	loadedMocks  int                  // replay: count of mocks stored, for /agent/mock/stats
	// replay: the tests allowed to run (SetScopeGate); nil = every test runs.
	// Cleared by resetScopeState.
	gateRun    map[string]struct{}
	gateReason string
	gateWarned bool // NoteUngatable said so this session
	// replay: the scopes the gate told a harness to skip, and whether
	// noteEndOfGated said so this session.
	gatedScopes    map[scopeKey]struct{}
	gatedEndWarned bool

	// provenMu guards proven, the single-worker integration wire's state
	// (scope_proven.go), served under the proven switch. Never held while
	// taking scopeMu.
	provenMu sync.Mutex
	proven   provenWire
	// wire is held across every operation of the wire (a claim, a begin,
	// an end, the final reap) so they never interleave; taken before
	// provenMu.
	wire wireLock

	// replay, under the proven switch: the start time of the process that
	// opened each window in workerOpen (procStartTime), where it is known.
	openProc map[scopeKey]string
}

func New(logger *zap.Logger, hook coreAgent.Hooks, proxy coreAgent.Proxy, client kdocker.Client, ip coreAgent.IncomingProxy, config *config.Config) *Agent {
	instrumentation := &Agent{
		logger:        logger,
		Hooks:         hook,
		Proxy:         proxy,
		IncomingProxy: ip,
		dockerClient:  client,
		config:        config,
	}
	if coreAgent.ProxyHook != nil && proxy != nil {
		proxy.SetAuxiliaryHook(coreAgent.ProxyHook)
	}
	// Where the run's calls to local listeners went, which the proxy tells
	// under the proven switch only (topology_proven.go).
	if s, ok := proxy.(topologySinkSetter); ok {
		s.SetTopologySink(instrumentation)
	}
	coreAgent.RegisterIncomingProxy(ip)
	// Propagate the connection-pool idle retention from config into
	// the MockManager package default. Reset unconditionally — whether
	// config is present or nil — so a prior setter invocation
	// (embedder, multi-agent test) does not bleed retention state into
	// this agent instance. The setter itself treats <=0 as "revert to
	// the 5-minute default", so both paths below are safe.
	if config != nil {
		proxyPkg.SetConnectionIdleRetention(config.Test.ConnectionPoolIdleRetention)
	} else {
		proxyPkg.SetConnectionIdleRetention(0)
	}
	return instrumentation
}

// agentReadyFile is kdocker.AgentReadyFile, which Setup removes: a variable
// only so that a test running Setup leaves the host's alone.
var agentReadyFile = kdocker.AgentReadyFile

// Setup will create a new app and store it in the map, all the setup will be done here
func (a *Agent) Setup(ctx context.Context, startCh chan int) error {

	// Remove stale readiness file from a previous run so the Docker
	// healthcheck (`cat <AgentReadyFile>`) does not pass before the
	// CLI has stored mocks on the agent. The file is re-created later
	// by the MakeAgentReady HTTP handler in pkg/agent/routes/record.go
	// once setup is complete.
	if err := os.Remove(agentReadyFile); err != nil && !os.IsNotExist(err) {
		a.logger.Debug("failed to remove stale agent readiness file", zap.Error(err))
	}

	// A replay's outcome, left as the agent is stopped for a CLI that cannot
	// ask for it (see stopOutcomePath).
	if path := stopOutcomePath(a.config.Agent.SetupOptions); path != "" {
		a.armStopOutcome(path, utils.RegisterPreCancelHook)
	}

	// The mismatch hints name the command that records this kind of run again.
	mismatch.SetMockMode(a.config.Agent.MockMode)

	a.logger.Debug("Starting the agent in ", zap.String("mode", string(a.config.Agent.Mode)))
	// Setup returns only once what it started is torn down, however it
	// returns: Hook's teardown (the proxy's, which restores nsswitch.conf and
	// removes the CA from the trust store, then the hooks' unload) runs on
	// the group, and the process exits as soon as Setup returns (main). A
	// stop that came before the port handoff, or while Hook was loading,
	// returned at once and so cut that teardown short. stop ends the run
	// whatever ctx does, for a Setup that returns before ctx is done.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	tearDown := func() error {
		stop()
		a.logger.Debug("waiting for the agent's teardown before Setup returns")
		return errGrp.Wait()
	}

	// Capture the process-lifetime ctx (with errgroup attached) so
	// StartIncomingProxy can keep the ingress listener alive across
	// /agent/incoming HTTP stream closes.
	a.setupCtx = ctx

	passPortsUint := a.config.Agent.PassThroughPorts

	rules := make([]models.BypassRule, len(a.config.Agent.PassThroughPorts))
	for i, port := range passPortsUint {
		rules[i] = models.BypassRule{
			Port: port,
		}
	}

	err := a.Hook(ctx, models.HookOptions{
		Mode:          a.config.Agent.Mode,
		IsDocker:      a.config.Agent.IsDocker,
		EnableTesting: a.config.Agent.EnableTesting,
		Rules:         rules,
	})
	if err != nil {
		if ctx.Err() != nil {
			// The stop came while the agent was starting: nothing failed.
			a.logger.Info("The agent was stopped while it was starting; stopping what it had started", zap.Error(err))
		} else {
			a.logger.Error("failed to hook into the app", zap.Error(err))
			// The start has failed, here, before the teardown below, which
			// can take long: the CLI that waits for the agent fails it at
			// once, with the code it exits with (cli/agent.go).
			utils.FailBoot(err)
		}
		// What Hook loaded or started before it failed, or before the stop
		// that ended it, is torn down before the process exits.
		if terr := tearDown(); terr != nil && !errors.Is(terr, context.Canceled) {
			utils.LogError(a.logger, terr, "error while tearing down a failed agent setup")
		}
		return err
	}

	// Hooked into the app, the proxy up. The memory guard's start reads the
	// cgroup's files: a stage of the start (utils.BeginBootWork).
	endGuard := utils.BeginBootWork("the memory guard's start")
	err = memoryguard.Start(ctx, a.logger, a.config.Agent.IsDocker, a.config.Agent.MemoryLimit)
	endGuard()
	if err != nil {
		a.logger.Info("Memory guard unavailable, continuing without memory-aware recording. "+
			"Ensure cgroup filesystem is mounted in the container or set --memory-limit=0 to disable.",
			zap.Error(err))
	}

	select {
	case startCh <- int(a.config.Agent.AgentPort):
	case <-ctx.Done():
		a.logger.Info("Context cancelled before agent becomes healthy. Stopping the agent.")
		if terr := tearDown(); terr != nil && !errors.Is(terr, context.Canceled) {
			utils.LogError(a.logger, terr, "error while stopping the agent before it became healthy")
		}
		return nil
	}

	<-ctx.Done()
	err = tearDown()
	if err != nil && !errors.Is(err, context.Canceled) {
		utils.LogError(a.logger, err, "error during agent setup")
		return err
	}
	a.logger.Debug("Context cancelled, stopping the agent")
	return context.Canceled

}

func (a *Agent) StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) {
	// IncomingProxy.Start is given the agent's Setup-scoped ctx so the
	// ingress proxy's :8080 listener and the WatchBindEvents goroutine
	// outlive any single /agent/incoming HTTP stream — the listener must
	// keep serving kubelet probes and forwarding application traffic
	// right up until the agent process itself receives SIGTERM. See
	// incident notes 2026-05 (CrashLoopBackOff on /record/stop).
	//
	// The CALLER's ctx (the /agent/incoming request) is captured into
	// a.streamCtx so SetGracefulShutdown's drainer can wait for it to
	// cancel — that's the signal the recorder has finished consuming
	// tcChan and the drainer can safely take over without racing.
	//
	// Defensive guard: in the normal CLI boot path, Setup() runs in a
	// goroutine and assigns a.setupCtx before sending the agent port on
	// startCh, so the HTTP server (and any /agent/incoming handler) can
	// only come up after setupCtx is set. But misuse from tests or future
	// embedders that wire this struct manually could skip Setup; passing
	// a nil ctx into IncomingProxy.Start would later panic inside
	// WatchBindEvents on `<-ctx.Done()`. Returning a clear error here
	// makes the misuse diagnosable instead of crashing the agent process.
	if a.setupCtx == nil {
		return nil, errors.New("agent setupCtx is not initialized; ensure Setup() was called before StartIncomingProxy")
	}

	// A recording's incoming stream attaching begins a recording whose app
	// is not stopping (incoming stopping.go). The ingress is told its app is
	// stopping only by a client that started the agent (SetGracefulShutdown),
	// and such a client's streams never reconnect (they resume only to an
	// agent docker compose restarts), so an attach here is never one in the
	// middle of a stop.
	if s, ok := a.IncomingProxy.(interface{ SetStopping(bool) }); ok {
		s.SetStopping(false)
	}

	// IncomingProxy.Start is now idempotent (a sync.Once inside
	// IngressProxyManager.Start gates the ListenForIngressEvents
	// goroutine) AND it updates the proxy's IncomingOptions every call,
	// so we can safely invoke it on each /agent/incoming reconnect to
	// re-apply opts without leaking watchers or stomping in-flight
	// captures. Returns the same tcChan on repeat calls.
	tc := a.IncomingProxy.Start(a.setupCtx, opts)

	// Refresh streamCtx + cache tcChan under the same mutex
	// SetGracefulShutdown reads them under. A reconnecting recorder
	// gets a new request ctx each time, and the drainer must wait on
	// the LATEST one so it doesn't race with whichever HandleIncoming
	// for-loop is currently draining the channel.
	a.incomingMu.Lock()
	a.streamCtx = ctx
	a.tcChan = tc
	a.incomingMu.Unlock()

	// Cancel any drainer left over from a previous session — without
	// this, a recorder reconnecting after /agent/graceful-shutdown
	// would find the previous session's drainer still consuming
	// tcChan and the new HandleIncoming for-loop would race for /
	// lose test cases. Cancelling here gives the new session
	// uncontested ownership of the channel until its own
	// /agent/graceful-shutdown (which will spawn a fresh drainer).
	a.drainMu.Lock()
	if a.drainCancel != nil {
		a.drainCancel()
		a.drainCancel = nil
	}
	a.drainMu.Unlock()

	a.logger.Debug("Ingress proxy manager started and is listening for bind events.")
	return tc, nil
}

// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
// When this flag is set, connection errors will be logged as debug instead of error.
func (a *Agent) SetGracefulShutdown(ctx context.Context) error {
	a.logger.Debug("Setting graceful shutdown flag on proxy")

	// Defensive: same misuse case as StartIncomingProxy — a manually-wired
	// Agent that skipped Setup would crash inside the drainer goroutine on
	// `<-a.setupCtx.Done()`. Surface as an error so HandleGracefulShutdown
	// can return 500, matching the no-Warn convention in AGENTS.md.
	if a.setupCtx == nil {
		return errors.New("agent setupCtx is not initialized; ensure Setup() was called before SetGracefulShutdown")
	}

	// The recorder (k8s-proxy / keploy CLI) closes its /agent/incoming
	// streaming connection right after this call, which means no one
	// drains tcChan anymore. With the listener now decoupled from the
	// request ctx (StartIncomingProxy fix above), traffic keeps flowing
	// through the proxy — and every captured test case still gets sent
	// onto tcChan. Without an explicit drainer the 100-slot buffer
	// fills within a few probe cycles, and subsequent ingress traffic
	// blocks at the channel send.
	//
	// Spin up a SESSION-AWARE drainer: it lives only until either the
	// agent process exits OR the next /agent/incoming stream attaches
	// (StartIncomingProxy cancels us via drainCancel so the new session
	// gets uncontested ownership of tcChan).
	a.incomingMu.Lock()
	tc := a.tcChan
	streamCtx := a.streamCtx
	a.incomingMu.Unlock()
	if tc != nil {
		a.drainMu.Lock()
		// Cancel any drainer left over from a prior graceful-shutdown
		// before installing the new one. Without this, repeated
		// /agent/graceful-shutdown calls would leak goroutines (each
		// waiting on streamCtx then competing on tcChan).
		if a.drainCancel != nil {
			a.drainCancel()
		}
		drainCtx, cancel := context.WithCancel(a.setupCtx)
		a.drainCancel = cancel
		a.drainMu.Unlock()

		go func() {
			// Wait for the recorder's /agent/incoming stream to close
			// before consuming. Otherwise this drainer races with the
			// HandleIncoming for-loop over the same tc channel and
			// steals tail-end test cases the recorder hasn't picked
			// up yet — manifests as test cases silently disappearing
			// from the last few seconds of a recording.
			//
			// Note: `Proxy.SetGracefulShutdown` (called below) does
			// NOT stop the ingress CaptureHook from producing new
			// test cases — it only flips an atomic flag that
			// downgrades connection-error severity in the proxy's
			// own logs and stops the optional packet-capture
			// pcap stream. The proxy keeps capturing test traffic
			// until its ctx is cancelled (process SIGTERM). So
			// "wait for streamCtx" is the only correct gate: it
			// guarantees HandleIncoming has stopped reading from
			// tcChan, and the drainer can take over without
			// racing.
			if streamCtx != nil {
				select {
				case <-streamCtx.Done():
				case <-drainCtx.Done():
					return
				}
			}
			for {
				select {
				case <-drainCtx.Done():
					return
				case <-tc:
					// discard — recording session is over
				}
			}
		}()
	}

	// A recording client that drains after its stop (the route marks ctx so)
	// stops the app it started before the agent: a connection the app
	// refuses from here is it gone on purpose (incoming stopping.go).
	if s, ok := a.IncomingProxy.(interface{ SetStopping(bool) }); ok && models.DrainsAfterStop(ctx) {
		s.SetStopping(true)
	}
	return a.Proxy.SetGracefulShutdown(ctx)
}

// SubscribePcap synchronously subscribes w to the proxy's packet
// broadcaster. Returns an unsub func and nil on success; returns an
// error (and nil unsub) when capture is not active. Does not block.
func (a *Agent) SubscribePcap(w io.Writer, flush func()) (func(), error) {
	streamer, ok := a.Proxy.(coreAgent.PcapStreamer)
	if !ok {
		return nil, errors.New("packet capture not supported by this proxy")
	}
	return streamer.SubscribePcap(w, flush)
}

// StreamPcap subscribes w to the proxy's packet broadcaster. Blocks
// until ctx is cancelled. Returns an error when the underlying Proxy
// does not implement PcapStreamer (third-party impls) or when
// capture is not active (--capture-packets was off for this
// session). Error reporting is up to the HTTP layer; the recorder
// treats it as "no stream available".
func (a *Agent) StreamPcap(ctx context.Context, w io.Writer, flush func()) error {
	unsub, err := a.SubscribePcap(w, flush)
	if err != nil {
		return err
	}
	defer unsub()
	<-ctx.Done()
	return nil
}

// StreamKeylog subscribes w to the package-level NSS keylog fanout.
// Blocks until ctx is cancelled. Always succeeds — when capture is
// off the sink is just empty and w receives no bytes until the
// recorder also turns capture on (or the call ends).
func (a *Agent) StreamKeylog(ctx context.Context, w io.Writer) error {
	unsub := pTls.AddKeyLogSubscriber(w)
	defer unsub()
	<-ctx.Done()
	return nil
}

// outgoingMockBufferBytes is the byte budget for the agent → CLI mock
// channel. This is the *design* unit — operators (and the comment
// block in pkg/agent/proxy/syncMock/syncMock.go's drop log) reason
// about memory, not slot count, and PR #4172's relay-layer cap
// (DefaultPerConnCap, also bytes) uses the same convention. At
// 64 MiB this matches PR #4172's per-connection budget, which keeps
// the two layers symmetric and easy to audit against the agent's
// cgroup limit.
//
// Implementation detail: a Go channel is slot-counted, so the
// runtime cap is derived as outgoingMockBufferBytes / nominalMockSizeBytes.
// nominalMockSizeBytes is an *estimate* of "what one mock typically
// costs in memory" — it is NOT a measured average, and the runtime
// neither tracks per-mock byte size nor enforces this value as a
// per-mock ceiling. It exists solely to translate the design-unit
// byte budget into the slot count Go channels actually need.
//
// 4 KiB was picked from the per-mock size distribution observed in
// production bundles (provider-engagement test EKS, 2026-05-04:
// median ~1 KiB, mean ~2.3 KiB, p95 ~10 KiB, max ~27 KiB). 4 KiB
// sits between mean and p95 — high enough that workloads with
// mostly-typical mocks fit comfortably under the byte budget, low
// enough that workloads skewed toward p95 still get a useful number
// of slots. A workload with consistently-larger mocks (e.g. mostly
// p95+ Postgres rows) will pin proportionally more memory at slot
// cap than the nominal 64 MiB; this is the same expected-vs-worst
// tradeoff every slot-counted Go channel makes.
//
// Bumped (effectively) from 100-slot to 64 MiB-equivalent after
// the same production bundles showed the old 100-slot cap silently
// dropping ≥2048 mocks per pod within minutes of recording start.
// At the bundles' MEAN per-mock size of ~2.3 KiB (computed as
// 1.0 MiB total / 450 mocks for the 8eafdd8e bundle, ~2.2 KiB
// for 452c57e7, ~2.5 KiB for c48b2e08), 100 slots held only
// ~230 KiB worst-case — less than 1/4 of the 1.0 MiB the
// *successful* part of those sessions ended up writing to disk.
// The mean is what matters here, not the median (~1 KiB), because
// the producer fills slots with whatever it produces — bigger
// individual mocks weigh more on the channel even at slot cap.
//
// Why "byte budget then derive" instead of "two independent caps
// (slots AND bytes) like PR #4172"? PR #4172 needs the two-cap shape
// because individual chunks at the relay layer can be up to 32 KiB
// each (forward buffer size) and a single connection might emit
// thousands of them — the slot cap protects against pathological
// chunk-count explosions even when bytes are fine. At the
// outChan layer downstream of the relay, mocks are already
// per-protocol-message granularity (one HTTP req/resp pair, one
// PG query, etc.) — count and bytes track each other much more
// tightly, so a single byte budget is sufficient and avoids the
// "two knobs, both must be set, can drift" maintenance burden.
//
// The drop path itself is unchanged and still intentional: it is
// far better to lose a mock and keep recording than to OOM-kill the
// agent and lose every mock captured in the run. We just stop hitting
// the cliff at trivial loads.
const (
	outgoingMockBufferBytes int64 = 64 * 1024 * 1024 // 64 MiB
	nominalMockSizeBytes    int64 = 4 * 1024         // 4 KiB
	outgoingMockChanCap           = int(outgoingMockBufferBytes / nominalMockSizeBytes)
)

// resetScopeState clears the per-session scope bookkeeping — open scopes, the
// closed per-test windows, and their metadata — at the start of a record or
// replay session. A reused, long-lived agent process (compose, standalone)
// serves many sessions back to back; without this, one session's windows would
// bleed into the next and corrupt the mappings.yaml a later record builds from
// GetScopeWindows. It does NOT touch scopeTable/loadedMocks: the CLI installs
// those for a replay session via SetScopeTable before serving begins. The maps
// are lazily re-created by openWindow/NoteScope, so nil is the correct zero.
func (a *Agent) resetScopeState() {
	a.scopeMu.Lock()
	a.workerOpen, a.openProc = nil, nil
	a.scopeWindows = nil
	a.scopeMeta = nil
	// A gate belongs to the replay that installed it; the CLI installs this
	// session's after the reset.
	a.gateRun, a.gateReason, a.gateWarned = nil, "", false
	a.gatedScopes, a.gatedEndWarned = nil, false
	// The tests' recorded begins belong to the replay that installed them;
	// the clock's holder is given back by releaseTestClock, before this.
	a.testBegins = nil
	a.testReal = nil
	// keploy test's pool belongs to the set that staged it.
	a.testPool = nil
	a.testWorker, a.testBegun = nil, nil
	a.testBegunAt, a.testBg = nil, nil
	a.scopeMu.Unlock()
}

func (a *Agent) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	// First, so a clock that cannot move back fails the session before
	// anything of the previous one is reset (MockOutgoing).
	if err := a.releaseTestClock(ctx); err != nil {
		return nil, fmt.Errorf("keploy could not move the run's clock back from the previous session's last test: %w", err)
	}
	m := make(chan *models.Mock, outgoingMockChanCap)

	starts.Default.Reset()
	ids.Default.Reset()
	if err := a.resetSession(ctx); err != nil {
		return nil, err
	}
	err := a.Proxy.Record(ctx, m, opts)
	if err != nil {
		return nil, err
	}

	// Gate the deferred-orphan revoke protocol on what THIS CLI negotiated.
	// The proxy wires its outChan into the package-global syncMock manager
	// (syncMock.Get()) inside Record above; flip revoke emission on/off to
	// match the decoded capability before returning the stream so an older CLI
	// (SupportsDroppedRevoke=false, the default) never receives a RevokedTests
	// control frame it can't divert.
	syncMock.Get().SetRevokeCapable(opts.SupportsDroppedRevoke)

	return m, nil
}

func (a *Agent) GetMapping(ctx context.Context) (<-chan models.TestMockMapping, error) {
	mappingCh := make(chan models.TestMockMapping, 100)
	a.Proxy.Mapping(ctx, mappingCh)

	return mappingCh, nil
}

func (a *Agent) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	// Redact secrets before dumping the options at Debug (mongoPassword / TLS key).
	redactedOpts := opts
	if redactedOpts.MongoPassword != "" {
		redactedOpts.MongoPassword = "****"
	}
	if redactedOpts.TLSPrivateKey != "" {
		redactedOpts.TLSPrivateKey = "****"
	}
	a.logger.Debug("MockOutgoing function called", zap.Any("options", redactedOpts))

	// A test the previous session left holding the run's clock (its end never
	// came, or could not move it back) gives it back before the new session
	// starts; a clock that cannot move back fails the session, which would
	// otherwise start on it, before anything of the previous one is reset.
	if err := a.releaseTestClock(ctx); err != nil {
		return fmt.Errorf("keploy could not move the run's clock back from the previous session's last test: %w", err)
	}
	// A new mock-serving session begins here (called once per replay). Clear the
	// on-miss capture buffer so a reused / long-lived agent process (compose,
	// standalone) never drains one replay's captured-on-miss mocks into a later,
	// unrelated set. Harmless when --on-miss is not "record" (buffer stays empty).
	httpparser.ResetCaptured()
	starts.Default.Reset()
	ids.Default.Reset()
	if err := a.resetSession(ctx); err != nil {
		return err
	}

	err := a.Proxy.Mock(ctx, opts)
	if err != nil {
		return err
	}

	return nil
}

func (a *Agent) Hook(ctx context.Context, opts models.HookOptions) error {
	// Every failure below is WRAPPED, never replaced: what failed decides the
	// agent's exit status (utils.ExitCodeFor), and that status is the only way
	// the CLI that launched this agent learns whether it lacks privileges or
	// the environment lacks something. A fresh "failed to hook into the app"
	// error here is what made every such agent exit the same.

	parentErrGrp := ctx.Value(models.ErrGroupKey).(*errgroup.Group)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// create a new error group for the hooks
	hookErrGrp, _ := errgroup.WithContext(ctx)
	hookCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the hookCtx to control the lifecycle of the hooks
	hookCtx, hookCtxCancel := context.WithCancel(hookCtx)
	hookCtx = context.WithValue(hookCtx, models.ErrGroupKey, hookErrGrp)

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	// The body below adds the proxy's goroutines (start(), whose shutdown
	// removes the CA) and the hooks' (their unload) to their groups. The
	// teardown waits on those groups only once the body is over: a stop that
	// came while it ran found them still empty, returned at once, and left
	// the work the body added after it to no one, cut short by the exit.
	bodyDone := make(chan struct{})
	defer close(bodyDone)
	// startErr is the proxy's failure to start, which the body reports: the
	// teardown, which reads it once the body is over, does not report it
	// again as a failure to stop.
	var startErr error
	parentErrGrp.Go(func() error {
		<-ctx.Done()

		// Each stage of the teardown is a step of its progress, which the
		// CLI that stopped this agent reads (utils.TeardownStep).
		utils.TeardownStep()
		// Cancelled first: what the body is waiting on in the proxy ends.
		proxyCtxCancel()
		<-bodyDone
		err := proxyErrGrp.Wait()
		switch {
		case err != nil && startErr != nil:
			a.logger.Debug("the proxy's group ended with its failed start", zap.Error(err))
		case err != nil:
			utils.LogError(a.logger, err, "failed to stop the proxy")
		}
		utils.TeardownStep()

		hookCtxCancel()
		err = hookErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to unload the hooks")
		}
		utils.TeardownStep()
		return nil
	})

	// The start below -- the hooks' load, the proxy's start, its CA install
	// -- is teardown work while a stop waits for it (bodyDone): it runs on
	// this goroutine, and a stop that came while it ran is kept from killing
	// the agent part way only while it makes progress here
	// (utils.BeginTeardownWork). It ends before bodyDone closes.
	// Where that progress cannot be seen (utils.TeardownProgressSeen), the
	// start is not marked so: it would count as progress throughout, and a
	// start wedged against the hooks' contract would never be killed.
	if utils.TeardownProgressSeen() {
		starting := utils.BeginTeardownWork()
		defer starting()
	}

	// load hooks if the mode changes ..
	hookCfg := coreAgent.HookCfg{
		Pid:      0,
		IsDocker: opts.IsDocker,
		Mode:     opts.Mode,
		Rules:    opts.Rules,
	}

	if coreAgent.EbpfProxyPortOverride != 0 {
		hookCfg.Port = coreAgent.EbpfProxyPortOverride
	}
	// The load can take long without a step (the kernel's verifier, on a
	// loaded host): a stage of the start, making progress while its thread
	// runs (utils.BeginBootWork).
	endLoad := utils.BeginBootWork("the hooks' load")
	err := a.Hooks.Load(hookCtx, hookCfg, a.config.Agent)
	endLoad()

	if err != nil {
		if ctx.Err() != nil {
			a.logger.Debug("the hooks' load ended with the stop", zap.Error(err))
		} else {
			utils.LogError(a.logger, err, "failed to load hooks")
		}
		return fmt.Errorf("failed to hook into the app: %w", err)
	}

	if a.proxyStarted {
		a.logger.Debug("Proxy already started")
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// DNSIPv4 is the address the proxy's DNS server answers with when it has
	// no recorded answer to serve, so it has to be one the application reaches
	// the proxy at — which the hooks that just loaded know, and hooks that do
	// not say leave the proxy's loopback default. Asking the machine for a
	// non-loopback address here instead, for every agent, kept a native one
	// from starting on a machine with none: a laptop with its network off,
	// exactly where replaying recorded mocks has to work.
	var DNSIPv4 string
	if h, ok := a.Hooks.(coreAgent.ProxyAddressReporter); ok {
		DNSIPv4 = h.ProxyIPv4()
	}
	if coreAgent.ProxyHook != nil {
		a.Proxy.SetAuxiliaryHook(coreAgent.ProxyHook)
	}
	// Under the proven switch, or for an agent that settles its reads, the
	// proxy's listener is marked for the hooks' count of the connections
	// waiting for it (proxyconns.go).
	a.markProxyListener()

	// The proxy's start can take long without a step: its CA's install runs
	// keytool, update-ca-certificates (whose Java hook starts a JVM) and
	// certutil, and the auxiliary hook loads BPF. A stage of the start, on
	// this goroutine (utils.BeginBootWork), with steps of its own inside.
	endProxy := utils.BeginBootWork("the proxy's start")
	err = a.Proxy.StartProxy(proxyCtx, coreAgent.ProxyOptions{
		DNSIPv4Addr: DNSIPv4,
		//DnsIPv6Addr: ""
	})
	endProxy()

	if err != nil {
		startErr = err
		if ctx.Err() != nil {
			a.logger.Debug("the proxy's start ended with the stop", zap.Error(err))
		} else {
			utils.LogError(a.logger, err, "failed to start proxy")
		}
		// StartProxy may have spawned proxy goroutines before the
		// failure point (TCP accept loop, TCP DNS, UDP DNS) — they
		// stay live until proxyCtx is cancelled. Before this fix
		// they leaked until the outer agent context was cancelled,
		// holding ports and consuming resources. Cancel here so the
		// partial proxy is torn down immediately. Matters now that
		// StartProxy propagates auxiliary-hook failures (keploy#4078)
		// rather than swallowing them.
		proxyCtxCancel()
		return fmt.Errorf("failed to hook into the app: %w", err)
	}

	a.proxyStarted = true
	a.latchCounting()
	return nil
}

func (a *Agent) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return a.Proxy.GetConsumedMocks(ctx)
}

func (a *Agent) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	// Under the proven switch a replay's read of its misses waits, as a
	// test's end does, until every call the application made is in the
	// proxy's hands and answered (settle_proven.go): the run's last read
	// included, so a call made after the last test's end is read, never
	// filed after it. One keploy cannot settle is a read that cannot vouch
	// for every miss. With the switch off an agent started for a run that
	// learns noise waits the same way where it can count the calls
	// (settlesReads): that noise is learned from these reads, and a miss
	// decided after the app answered its test (its own client gave up first)
	// would otherwise be read with the next test's.
	var settleErr error
	if (provenswitch.On() || a.settlesReads()) && a.replaying() {
		if lost := a.countingLost(); lost != nil {
			// With the switch off a NAT'd connection has dropped the run to
			// not counted: no read of it can vouch for every miss, and none
			// waits for calls that can never settle.
			settleErr = lost
		} else {
			// keploy test reads a test case's misses after its end: what the
			// listener dropped since is no test's, and is not compared here.
			_, settleErr = a.settle(ctx, true)
			if lost := a.countingLost(); settleErr == nil && lost != nil {
				// Dropped while it settled: a call accepted since is not
				// kept, so the settle could not have seen it.
				settleErr = lost
			}
		}
		if !provenswitch.On() {
			// Counted whatever the settle said, so a loss is one read's.
			if err := a.lostSinceLastRead(ctx); settleErr == nil {
				settleErr = err
			}
		}
	}
	errs, err := a.Proxy.GetMockErrors(ctx)
	if settleErr != nil && err == nil {
		err = fmt.Errorf("%w: keploy could not tell that every call reached it: %v", models.ErrUnmatchedCallsUnread, settleErr)
	}
	if err != nil && !errors.Is(err, models.ErrUnmatchedCallsUnread) {
		// A read that failed is answered without its list over HTTP: the
		// misses the wire charged stay for the read that succeeds. A read
		// that could not vouch for every miss is answered with what it read,
		// the charged misses included.
		return errs, err
	}
	// The misses the proven wire charged to a test at its end were drained
	// from the proxy then; they are still the run's misses.
	if charged := a.takeChargedMisses(); len(charged) > 0 {
		errs = append(charged, errs...)
	}
	return errs, err
}

// PIDMapRefusals counts the inserts into the hooks' PID maps the kernel has
// refused since they loaded, each of them a process whose calls this agent
// could not intercept (models.PIDMapRefusals). Hooks that do not count them give
// models.ErrPIDMapRefusalsUnsupported, "unknown".
func (a *Agent) PIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	r, ok := a.Hooks.(coreAgent.PIDMapReporter)
	if !ok {
		return models.PIDMapRefusals{}, models.ErrPIDMapRefusalsUnsupported
	}
	return r.PIDMapRefusals(ctx)
}

// BeginTestErrorCapture opens a per-test mock-error capture window so the
// replayer's subsequent GetMockErrors returns only this test's misses instead
// of draining a process-global queue. The replayer calls it before each test
// (in-process here, over HTTP via the agent client). Safe no-op return.
func (a *Agent) BeginTestErrorCapture(_ context.Context) error {
	// a.Proxy is the agent.Proxy interface; reach the concrete capability via a
	// type-assertion so the interface stays unchanged. Older proxies without it
	// simply fall back to the legacy global queue.
	if b, ok := a.Proxy.(interface{ BeginTestErrorCapture() }); ok {
		b.BeginTestErrorCapture()
	}
	if t := a.realTracker(); t != nil && provenswitch.On() {
		// A set's first test: what the REAL connections did before it (the
		// app's start-up) touched no test and is the run's already; its own
		// uses start here.
		t.BeginTest()
	}
	a.awaitTestRequest()
	return nil
}

// awaitTestRequest tells the learn step that the test keploy test opens has
// a request of its own, which the filter that opens it brings
// (coreAgent.TestRequestTaker), under the proven switch.
func (a *Agent) awaitTestRequest() {
	if taker, ok := a.Proxy.(coreAgent.TestRequestTaker); ok && provenswitch.On() {
		taker.AwaitTestRequest()
	}
}

// ContinueTestErrorCapture opens the next test's capture window, carrying in
// the misses made since the previous test's window closed (the proxy's
// ContinueTestErrorCapture). A proxy without it opens a window as Begin does.
func (a *Agent) ContinueTestErrorCapture(ctx context.Context) error {
	if c, ok := a.Proxy.(interface{ ContinueTestErrorCapture() }); ok {
		c.ContinueTestErrorCapture()
		if t := a.realTracker(); t != nil && provenswitch.On() {
			// What was not captured while no test case ran is the run's
			// (design v14 §3.2 as amended for class e); this one's starts
			// here.
			t.BeginTest()
		}
		a.awaitTestRequest()
		return nil
	}
	return a.BeginTestErrorCapture(ctx)
}

// collectAsyncMocks returns the async subset (Mock.IsAsync, i.e. Spec.Async != nil).
func collectAsyncMocks(mocks []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		if m != nil && m.IsAsync() {
			out = append(out, m)
		}
	}
	return out
}

// wantsAsyncMocks reports whether the proxy consumes async mocks, so store
// paths can skip the (otherwise unconditional) async-tag scan when it doesn't.
func (a *Agent) wantsAsyncMocks() bool {
	_, ok := a.Proxy.(coreAgent.AsyncMockLoader)
	return ok
}

// loadAsyncIntoProxy hands the async corpus to the proxy if it supports it.
// Engine.Load REPLACES the corpus, so callers pass the COMPLETE async corpus
// for the test-set in a single call per store path — never per window.
func (a *Agent) loadAsyncIntoProxy(asyncMocks []*models.Mock) {
	// No early return on an empty corpus: Load REPLACES, so a test-set holding
	// no async mocks has to clear the previous set's rather than inherit them.
	if loader, ok := a.Proxy.(coreAgent.AsyncMockLoader); ok {
		loader.LoadAsyncMocks(asyncMocks)
	}
}

// StoreMocks stores the filtered and unfiltered mocks for a client ID.
//
// Unification (Phase 1): every mock is run through DeriveLifetime on
// entry so TestModeInfo.Lifetime is populated before any matcher reads
// it. This is a second safety-net — the mockdb disk loader already
// derives at load time, but StoreMocks is also reachable from other
// paths (grpc instrumentation transport, in-memory tests, etc.) where
// no prior DeriveLifetime has run. Calling it again is idempotent
// thanks to the LifetimeDerived-bool short-circuit in DeriveLifetime
// (LifetimePerTest is the zero value of Lifetime, so the bool is the
// only reliable "already classified" signal), so re-deriving here
// never double-bumps the legacyKindFallbackFires counter or races
// with concurrent matchers.
//
// Caveat: because the stored slices carry pointer copies (not deep
// copies) of the caller's mocks, DeriveLifetime's write to
// TestModeInfo.Lifetime is visible through the caller's slice as
// well. This is the intended semantic — the agent's storage shares the
// caller's objects, so the caller benefits from the cached Lifetime too
// (the proxy's pools hold the copies UpdateMockParams' filters make of
// them). Do NOT introduce a deep copy here unless a concrete
// mutation-safety regression lands first.
func (a *Agent) StoreMocks(ctx context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error {
	storage := &ClientMockStorage{
		filtered:   make([]*models.Mock, len(filtered)),
		unfiltered: make([]*models.Mock, len(unfiltered)),
	}

	// Shallow copy the slices — only the outer backing array is
	// duplicated, the *models.Mock pointers are shared with the
	// caller's slices, so the DeriveLifetime below classifies the
	// caller's mocks too (see the caveat block before this function).
	// The proxy's pools hold the copies UpdateMockParams' filters make
	// of these objects (pkg/util.go), except when a call carries no
	// test window and the filters pass them through.
	copy(storage.filtered, filtered)
	copy(storage.unfiltered, unfiltered)
	if provenswitch.On() {
		// Which recorded container keys are ambiguous is read from the whole
		// set, before any mock is refused or consumed.
		var tally containerkey.RecordedTally
		for _, m := range storage.filtered {
			tally.Add(m)
		}
		for _, m := range storage.unfiltered {
			tally.Add(m)
		}
		containerkey.Default.SetRecorded(&tally)
		storage.filtered = a.refuseUnfollowable(storage.filtered, storage)
		storage.unfiltered = a.refuseUnfollowable(storage.unfiltered, storage)
	}

	// Derive Lifetime once per mock before they enter the runtime pool.
	// Idempotent: DeriveLifetime short-circuits when the Lifetime
	// field is already set (i.e. the disk loader has already run),
	// so re-deriving here is safe and never double-counts the
	// legacyKindFallbackFires telemetry.
	for _, m := range storage.filtered {
		if m != nil {
			m.DeriveLifetime()
			mocknoise.MaterializeCorrelations(m)
		}
	}
	for _, m := range storage.unfiltered {
		if m != nil {
			m.DeriveLifetime()
			mocknoise.MaterializeCorrelations(m)
		}
	}

	a.finalizeClientMocks(ctx, storage)
	// Non-stream path keeps everything resident, so the complete async corpus
	// is exactly the async subset of filtered+unfiltered.
	if a.wantsAsyncMocks() {
		a.loadAsyncIntoProxy(append(collectAsyncMocks(filtered), collectAsyncMocks(unfiltered)...))
	}
	a.logger.Debug("Successfully stored mocks for client")
	return nil
}

// finalizeClientMocks publishes a fully-built storage as the active client mock
// pool (one sync.Map store, so a concurrent UpdateMockParams never sees a
// half-built pool) and seeds the freeze anchor. Shared by StoreMocks and
// StoreMocksStream.
func (a *Agent) finalizeClientMocks(ctx context.Context, storage *ClientMockStorage) {
	// Close the superseded generation's on-disk store file (temp-file leak otherwise).
	if prev, ok := a.clientMocks.Load(uint64(0)); ok {
		if ps, _ := prev.(*ClientMockStorage); ps != nil && ps.diskMocks != nil && ps.diskMocks != storage.diskMocks {
			_ = ps.diskMocks.Close()
		}
	}
	a.clientMocks.Store(uint64(0), storage)
	a.seedStatementCatalog(storage)
	a.seedRecordedDestinations(storage)

	// Track the loaded count for /agent/mock/stats.
	a.scopeMu.Lock()
	a.loadedMocks = len(storage.filtered) + len(storage.unfiltered)
	if storage.diskMocks != nil {
		a.loadedMocks += storage.diskMocks.Len()
	}
	a.scopeMu.Unlock()

	// Seed the freeze anchor from the earliest request timestamp across the WHOLE
	// pool, including on-disk mocks (they often carry the earliest, boot, times).
	anchor := earliestReqTimestamp(storage.filtered, storage.unfiltered)
	if storage.diskMocks != nil {
		if se := storage.diskMocks.EarliestReqTs(); !se.IsZero() && (anchor.IsZero() || se.Before(anchor)) {
			anchor = se
		}
	}
	if !anchor.IsZero() {
		if err := ActiveHooks.SetFreezeAnchor(ctx, anchor); err != nil {
			a.logger.Warn("SetFreezeAnchor hook returned error",
				zap.Error(err), zap.Time("anchor", anchor))
		}
	}
	a.wireAppClock()
}

// wireAppClock hands the proxy the hooks' reading of the app's clock, when
// both have one (AppClockReader, coreAgent.AppClockSetter).
func (a *Agent) wireAppClock() {
	r, ok := ActiveHooks.(AppClockReader)
	if !ok {
		return
	}
	if s, ok := a.Proxy.(coreAgent.AppClockSetter); ok {
		s.SetAppClock(r.AppClock)
	}
}

// StoreMocksStream decodes the mock corpus one frame at a time from dec (header
// counts split it into filtered then unfiltered), runs DeriveLifetime per mock,
// and publishes the built storage once. A decode error fails the whole ingest
// and leaves the previously-published pool intact — no partial publish.
func (a *Agent) StoreMocksStream(ctx context.Context, header models.MockStreamHeader, dec *gob.Decoder) error {
	total := header.FilteredCount + header.UnfilteredCount
	if header.FilteredCount < 0 || header.UnfilteredCount < 0 || total < 0 {
		return fmt.Errorf("storemocks stream: invalid counts (filtered=%d unfiltered=%d)", header.FilteredCount, header.UnfilteredCount)
	}
	// Pre-size from the header counts, but cap the capacity: the counts are
	// client-provided, so an inflated header must not force a huge upfront
	// allocation. A larger real count just grows via append; a lying count
	// still fails fast at the first Decode past the actual data.
	storage := &ClientMockStorage{
		filtered:   make([]*models.Mock, 0, presizeCap(header.FilteredCount)),
		unfiltered: make([]*models.Mock, 0, presizeCap(header.UnfilteredCount)),
	}

	// Engage the on-disk store when strict mock-window is in effect — the OSS
	// config flag config.Test.StrictMockWindow, on by default (seeded by
	// config.New(); KEPLOY_STRICT_MOCK_WINDOW=0 opts out). Under lax the filter
	// promotes out-of-window mocks, needing the whole per-test set every test, so
	// windowing to disk would only add reload cost. Best-effort — fall back to
	// full-resident if the store can't be created.
	var disk *proxyPkg.DiskMocks
	if pkg.IsStrictMockWindow(a.config != nil && a.config.Test.StrictMockWindow) {
		d, diskErr := proxyPkg.NewDiskMocks(a.logger)
		if diskErr != nil {
			a.logger.Warn("on-disk mock store unavailable; keeping per-test mocks resident", zap.Error(diskErr))
		} else {
			disk = d
		}
	}
	storage.diskMocks = disk
	failStream := func(err error) error {
		if disk != nil {
			_ = disk.Close()
		}
		return err
	}

	// Collect async mocks DURING decode, from every decoded mock, BEFORE the
	// disk/resident routing below. Under strict mock-window (the default) the
	// per-test async egress is parked on disk, not left in storage.filtered, so
	// collecting from the resident slices afterward would silently drop it.
	var asyncMocks []*models.Mock
	wantAsync := a.wantsAsyncMocks() // skip the per-mock async check when unused
	// Under the proven switch, which recorded container keys are ambiguous
	// is read from the whole set (containerkey.RecordedTally), every mock
	// counted as it is decoded, before any is refused or parked on disk.
	proven := provenswitch.On()
	var tally containerkey.RecordedTally

	for i := 0; i < total; i++ {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return failStream(err)
			}
		}
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return failStream(fmt.Errorf("storemocks stream: decode mock %d/%d: %w", i+1, total, err))
		}
		mock := &m
		mock.DeriveLifetime()
		mocknoise.MaterializeCorrelations(mock)
		if proven {
			tally.Add(mock)
		}
		if proven && !a.followable(mock) {
			storage.noteRefused(mock)
			continue
		}
		if wantAsync && mock.IsAsync() {
			asyncMocks = append(asyncMocks, mock)
		}
		if i < header.FilteredCount {
			// Per-test region: window-eligible mocks go to disk, ineligible ones
			// (missing/invalid timestamps) stay resident so filter routing is exact.
			if disk != nil && proxyPkg.EligibleForDisk(mock) {
				if err := disk.Add(mock); err != nil {
					// Disk write failed — keep it resident so it's never lost.
					a.logger.Warn("on-disk mock write failed; keeping mock resident",
						zap.String("mock", mock.Name), zap.Error(err))
					storage.filtered = append(storage.filtered, mock)
				}
			} else {
				storage.filtered = append(storage.filtered, mock)
			}
		} else {
			storage.unfiltered = append(storage.unfiltered, mock)
		}
	}
	if proven {
		containerkey.Default.SetRecorded(&tally)
	}
	if disk != nil {
		disk.Finalize()
		statements, err := disk.LoadStatements()
		if err != nil {
			return failStream(fmt.Errorf("storemocks stream: read the statement catalog from the on-disk store: %w", err))
		}
		storage.statements = statements
		a.logger.Info("agent mock residency: per-test mocks parked on disk (windowed)",
			zap.Int("onDisk", disk.Len()),
			zap.Int("spilledResponses", disk.SpilledResponses()),
			zap.Int64("diskBytes", disk.DiskBytes()),
			zap.Int("residentPerTest", len(storage.filtered)),
			zap.Int("residentConfig", len(storage.unfiltered)))
	}

	a.finalizeClientMocks(ctx, storage)
	// Load the complete async corpus once (captures disk-parked async mocks too,
	// since they were collected in the decode loop above).
	a.loadAsyncIntoProxy(asyncMocks)
	a.logger.Debug("Successfully stored streamed mocks for client",
		zap.Int("filtered", len(storage.filtered)),
		zap.Int("unfiltered", len(storage.unfiltered)))
	return nil
}

// followable reports whether this keploy can follow the variables a mock
// declares (models.MockVariables): those of a plain HTTP call, and those the
// parser of the mock's kind says it follows (integrations.VariableFollower,
// which a parser registers). A mock whose variables it cannot follow is never
// served, under the proven switch: served as recorded, it would answer a call
// whose variable does not hold. It is named in an error, and the call it was
// recorded for misses.
func (a *Agent) followable(m *models.Mock) bool {
	if m == nil || len(m.Spec.Variables) == 0 {
		return true
	}
	why := ""
	follow, registered := integrations.VariableFollowerOf(m.Kind)
	switch {
	case m.Kind == models.HTTP && m.IsAsync():
		why = "keploy does not follow variables on an async HTTP call"
	case m.Kind == models.HTTP:
		return true
	case !registered:
		why = "keploy follows variables on HTTP calls, and on the calls of a parser that says it does; the " + string(m.Kind) + " parser does not"
	default:
		ok, w := follow(m)
		if ok {
			return true
		}
		why = w
		if why == "" {
			why = "the " + string(m.Kind) + " parser does not follow this mock's variables"
		}
	}
	a.logger.Error("keploy cannot follow the variables of a recorded call, so it will not answer that call",
		zap.String("mock", m.Name), zap.String("kind", string(m.Kind)), zap.String("why", why),
		zap.String("next_step", "re-record the test with this keploy, or remove the variables block of "+m.Name+" to replay the call as recorded"))
	return false
}

// refuseUnfollowable drops the mocks whose variables this keploy cannot follow
// (followable), noting each in store.
func (a *Agent) refuseUnfollowable(mocks []*models.Mock, store *ClientMockStorage) []*models.Mock {
	out := mocks[:0]
	for _, m := range mocks {
		if a.followable(m) {
			out = append(out, m)
		} else if m != nil {
			store.noteRefused(m)
		}
	}
	return out
}

// noteRefused notes that the store refused mock; before the store is
// published.
func (s *ClientMockStorage) noteRefused(mock *models.Mock) {
	if mock == nil || mock.Name == "" {
		return
	}
	if s.refused == nil {
		s.refused = map[string]bool{}
	}
	s.refused[mock.Name] = !mock.IsProtocolPhase()
}

// maxStreamPresize bounds the slice capacity pre-allocated from a /storemocks
// header's client-provided counts, so a malformed header can't trigger a huge
// upfront allocation (CodeQL: untrusted allocation size). Real pools are far
// smaller; a larger real count grows via append.
const maxStreamPresize = 1 << 20

func presizeCap(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxStreamPresize {
		return maxStreamPresize
	}
	return n
}

// earliestReqTimestamp returns the earliest non-zero ReqTimestampMock seen
// across the supplied mock slices, or a zero time.Time if none are set.
// Used to seed the time-freeze offset before the user app's first
// datetime.now() so cached recorded credentials don't appear expired.
func earliestReqTimestamp(filtered, unfiltered []*models.Mock) time.Time {
	var anchor time.Time
	consider := func(m *models.Mock) {
		if m == nil {
			return
		}
		ts := m.Spec.ReqTimestampMock
		if ts.IsZero() {
			return
		}
		if anchor.IsZero() || ts.Before(anchor) {
			anchor = ts
		}
	}
	for _, m := range filtered {
		consider(m)
	}
	for _, m := range unfiltered {
		consider(m)
	}
	return anchor
}

// loadPerTestMocks builds the per-test slice the filter runs over, loading from
// disk only what the filter's effective mode would keep (so the result matches
// the old full-resident path): mapping -> named mocks; strict -> window +
// startup band; lax -> all. Resident ineligible mocks are merged in; with no
// disk store (legacy path) the resident slice is returned unchanged.

// effectiveFirstWindowStart resolves the startup-band cutoff for this call.
//
// readerVal is what the proxy reports. On the FIRST window it is the zero time,
// because SetMocksWithWindow records it later in this same call — so every
// startup-band guard downstream is switched off for test #1, which is precisely
// the test a slow application bootstrap lands in. Deriving the cutoff from this
// call's own window start closes that, and uses the same predicate the manager
// applies (a zero or BaseTime start means "no window").
//
// isWindowedProxy gates the derivation because it is only sound where the
// manager adopts this same start as firstWindowStart in this call. A proxy with
// neither the reader nor that adoption reports zero on EVERY window, so an
// unguarded fallback would substitute the CURRENT window's start each time,
// collapsing "req < firstWindowStart" into "req < afterTime" and preserving
// every stale previous-test mock — the cross-test bleed strict mode exists to
// prevent, on the one path where strict is actually live.
func effectiveFirstWindowStart(isWindowedProxy bool, readerVal, afterTime time.Time) time.Time {
	if !readerVal.IsZero() || !isWindowedProxy {
		return readerVal
	}
	if afterTime.IsZero() || afterTime.Equal(models.BaseTime) {
		return readerVal
	}
	return afterTime
}

// inFlightFrom is, for a test window's call, the exclusive start of the gap
// before the window, from the proxy's recorded windows
// (coreAgent.InFlightPlanner): the calls sent after it and answered at or
// after the window's start are the window's test's. ok is false for a
// staging call, with no planner, or with no such gap.
func (a *Agent) inFlightFrom(params models.MockFilterParams) (time.Time, bool) {
	if params.AfterTime.IsZero() || params.AfterTime.Equal(models.BaseTime) || params.BeforeTime.IsZero() {
		return time.Time{}, false
	}
	planner, ok := a.Proxy.(coreAgent.InFlightPlanner)
	if !ok {
		return time.Time{}, false
	}
	return planner.InFlightFrom(params.AfterTime)
}

// loadPerTestMocks reads, from the on-disk store disk, the per-test mocks
// params' load mode (perTestLoadMode) asks for, beside resident. pool names
// per-test mocks to read as well (the exact staging's pool, stageMocks):
// those the mode's own load has not read already, nor holds resident, so
// none is decoded twice. The staging call says how many it read (poolMocks).
func (a *Agent) loadPerTestMocks(resident []*models.Mock, disk *proxyPkg.DiskMocks, params models.MockFilterParams, firstWindowStart time.Time, pool ...string) ([]*models.Mock, error) {
	if disk == nil {
		return resident, nil
	}

	var loaded []*models.Mock
	var err error
	// readPool reads the pool's mocks that neither resident nor loaded holds.
	poolRead := false
	readPool := func() ([]*models.Mock, error) {
		poolRead = true
		if len(pool) == 0 {
			return nil, nil
		}
		have := make(map[string]struct{}, len(resident)+len(loaded))
		for _, list := range [][]*models.Mock{resident, loaded} {
			for _, m := range list {
				if m != nil {
					have[m.Name] = struct{}{}
				}
			}
		}
		want := make([]string, 0, len(pool))
		for _, n := range pool {
			if _, ok := have[n]; !ok {
				want = append(want, n)
			}
		}
		if len(want) == 0 {
			return nil, nil
		}
		return disk.LoadByNames(want)
	}
	mode := perTestLoadMode(params)
	switch mode {
	case loadModeMapping:
		loaded, err = disk.LoadByNames(params.MockMapping)
		// Beside the mocks the mapping names, those placed at their answer
		// that this test's window holds (models.PlaceOf), whatever the
		// mapping lists (pkg.FilterTcsMocksMappingInWindow). A staging
		// call's BaseTime start is no test's window: a scope's staging
		// (stageScope) names the test's whole recording.
		if err == nil && models.AnswerPlacedRegistered() && pkg.IsTestWindow(params.AfterTime, params.BeforeTime) {
			var placed []*models.Mock
			if placed, err = disk.LoadAnswerPlaced(params.AfterTime, params.BeforeTime); err == nil {
				loaded = append(loaded, placed...)
			}
		}
	case loadModeStrictWindow:
		if cutoff, ok := stagingStartupCutoff(params); ok {
			loaded, err = disk.LoadBefore(cutoff)
			if err != nil {
				break
			}
			startup := len(loaded)
			// Beside the startup band, the mocks a parser declares it
			// needs staged within the staging's window
			// (models.StagingWithStartup): a server push it files when
			// the set is staged and releases at its own test window. They
			// are read from the band's cutoff to the end of the
			// staging's window, so the two never overlap.
			declared := 0
			if models.StagedAtStagingRegistered() {
				var staged []*models.Mock
				staged, err = disk.LoadStaged(cutoff, params.BeforeTime)
				if err != nil {
					break
				}
				loaded = append(loaded, staged...)
				declared = len(staged)
			}
			// And the exact staging's pool (stageMocks), what the band and
			// the declared mocks have not read already.
			var poolMocks []*models.Mock
			poolMocks, err = readPool()
			if err != nil {
				break
			}
			loaded = append(loaded, poolMocks...)
			a.logger.Info("staged the set's app-startup mocks from the on-disk store; each test reads its own when it runs",
				zap.Int("startupMocks", startup),
				zap.Int("declaredMocks", declared),
				zap.Int("poolMocks", len(poolMocks)),
				zap.Int("onDisk", disk.Len()),
				zap.String("firstRecordedTest", cutoff.UTC().Format(time.RFC3339Nano)))
			break
		}
		loaded, err = disk.LoadWindow(params.AfterTime, params.BeforeTime)
		if err == nil && !firstWindowStart.IsZero() {
			var startup []*models.Mock
			startup, err = disk.LoadBefore(firstWindowStart)
			if err == nil {
				loaded = append(loaded, startup...)
			}
		}
		// Beside the window's own, the calls in flight when its test began:
		// sent in the gap before it, answered at or after its start (a
		// consumer's long poll). The window loads by place, which is the
		// request time for a call the in-flight rule applies to, so they
		// are in no window's load otherwise.
		if from, ok := a.inFlightFrom(params); err == nil && ok {
			var inFlight []*models.Mock
			inFlight, err = disk.LoadInFlight(from, params.AfterTime)
			if err == nil {
				loaded = append(loaded, inFlight...)
			}
		}
	default:
		loaded, err = disk.LoadAll()
	}
	if err == nil && !poolRead {
		var poolMocks []*models.Mock
		if poolMocks, err = readPool(); err == nil {
			loaded = append(loaded, poolMocks...)
		}
	}
	if err != nil {
		return nil, err
	}
	a.logger.Debug("agent mock residency: loaded per-test set from disk",
		zap.String("mode", mode),
		zap.Int("loadedFromDisk", len(loaded)),
		zap.Int("residentIneligible", len(resident)))
	return dedupByName(resident, loaded), nil
}

// seedStatementCatalog hands the proxy the stored set's statement catalog
// (storage.statements): the first recorded mock of each statement its on-disk
// store holds. An app may prepare at start the statements it runs only inside
// tests, and the set's staging reads only the mocks recorded before the first
// test (stagingStartupCutoff): the parser answers those prepares from the
// catalog, describe-only, never serving it. It is seeded once per store, when
// the store is published, so no test's call and no scope staging reads it
// again, and a store with no on-disk store (whose per-test mocks are all
// resident, so the staging reaches them) replaces the previous set's catalog
// with none. A proxy that cannot take it is not seeded.
func (a *Agent) seedStatementCatalog(storage *ClientMockStorage) {
	seeder, ok := a.Proxy.(coreAgent.StatementCatalogSeeder)
	if !ok {
		return
	}
	seeder.SeedStatementCatalog(storage.statements)
	if storage.diskMocks != nil {
		a.logger.Info("seeded the set's statement catalog from the on-disk store: the first recording of each statement, to describe a statement the app prepares before the test that runs it, never served",
			zap.Int("statements", len(storage.statements)),
			zap.Int("onDisk", storage.diskMocks.Len()))
	}
}

// seedRecordedDestinations hands the proxy the stored set's recorded
// destinations: the first mock its on-disk store holds of each
// (DiskMocks.LoadDestinations). A set's staging reads only what was recorded
// before its first test, so a destination whose per-test mocks all lie inside
// tests would be unknown to the proxy's destination rules while the app
// starts: a non-default MySQL port would not be routed to the MySQL parser,
// and a TLS client would not be offered h2 for a destination recorded
// speaking HTTP/2. They are seeded once per store, when it is published, and
// replace the previous store's; the proxy keeps them, and every staging
// derives from them beside what it stages. A store with no on-disk store
// seeds none: its per-test mocks are all resident, so its staging reaches
// them. A read failure seeds none and is logged; the rules then read what is
// staged, as before.
func (a *Agent) seedRecordedDestinations(storage *ClientMockStorage) {
	seeder, ok := a.Proxy.(coreAgent.RecordedDestinationsSeeder)
	if !ok {
		return
	}
	var mocks []*models.Mock
	if storage.diskMocks != nil {
		var err error
		if mocks, err = storage.diskMocks.LoadDestinations(); err != nil {
			utils.LogError(a.logger, err, "failed to read the set's recorded destinations from the on-disk store; the proxy reads them from what is staged")
			mocks = nil
		}
	}
	seeder.SeedRecordedDestinations(mocks)
	if storage.diskMocks != nil {
		a.logger.Debug("seeded the set's recorded destinations from the on-disk store",
			zap.Int("destinations", len(mocks)), zap.Int("onDisk", storage.diskMocks.Len()))
	}
}

// stagingStartupCutoff returns where a set's staging call stops reading the
// on-disk store, and whether the call is one.
//
// The replayer stages a set before the app starts, with the window BaseTime to
// now (replay.go's SendMockFilterParamsToAgent at set-up, on retry and for a
// replacement agent). The proxy files everything it is staged with then as
// app-startup traffic, and the set's first test refiles it to what was recorded
// before that test (MockManager.setMocksWithWindowLocked). Reading the window
// itself therefore brought the whole recording into memory only to stage it as
// startup traffic, and a long recording OOM-killed the agent before its app
// started. The startup traffic is what was recorded before the set's earliest
// recorded test, which the staging call carries. Beside it the call stages
// the mocks a parser declares it needs staged within the call's window
// (models.StagingWithStartup, DiskMocks.LoadStaged), and only those, from the
// cutoff to the end of the call's window.
//
// A set whose tests carry no request time has no such test, so its staging
// call reads the window it names.
func stagingStartupCutoff(params models.MockFilterParams) (time.Time, bool) {
	if !params.AfterTime.Equal(models.BaseTime) || params.FirstRecordedTestStart.IsZero() {
		return time.Time{}, false
	}
	return params.FirstRecordedTestStart, true
}

const (
	loadModeMapping      = "mapping"
	loadModeStrictWindow = "strict-window"
	loadModeLaxAll       = "lax-all"
)

// perTestLoadMode is which per-test mocks loadPerTestMocks reads from disk for
// this call: mapping -> named mocks; strict -> window + startup band; lax -> all.
//
// Gate on the CALLER's flag, not the agent's own config. The two are
// independent: params.StrictMockWindow is what the replayer resolved for this
// run and is what the filter in UpdateMockParams uses, while a.config is the
// agent process's default. When they disagree, the windowed disk load would be
// picked while the filter that consumes its output ran lax (or the reverse) —
// the residency decision and the filtering decision have to come from one
// source. IsStrictMockWindow folds in the env override, so
// KEPLOY_STRICT_MOCK_WINDOW still opts out either way.
func perTestLoadMode(params models.MockFilterParams) string {
	switch {
	case params.UseMappingBased && (len(params.MockMapping) > 0 || params.ExactMapping):
		// An exact empty mapping names no per-test mock: nothing is read.
		return loadModeMapping
	case pkg.IsStrictMockWindow(params.StrictMockWindow) && !params.AfterTime.IsZero() && !params.BeforeTime.IsZero():
		return loadModeStrictWindow
	}
	return loadModeLaxAll
}

// loadCarryOverLookahead loads the carry-over mocks (models.IsCarryOver: a
// RegisterCarryOver kind's, a StagingRelease need's) that the proxy's
// carry-over tier needs for this window beyond the window's own mocks: the
// range CarryOverLoadRange names, which starts where the previous window's
// load ended and reaches models.CarryOverLookahead past what this window
// releases, or, for the set's staging, what the app's startup reaches. They go
// to the proxy in the filtered slice, and the manager files the out-of-window
// ones into the carry-over tier.
//
// Only for the windowed disk loads (strict-window, mapping). The lax load
// already holds every mock, and its filter makes the out-of-window ones session
// mocks; nothing loads when no kind registered a carry-over predicate or a
// StagingRelease need, the proxy has no planner, or there is no disk store.
func (a *Agent) loadCarryOverLookahead(disk *proxyPkg.DiskMocks, params models.MockFilterParams) ([]*models.Mock, error) {
	if disk == nil || !models.CarryOverRegistered() || perTestLoadMode(params) == loadModeLaxAll {
		return nil, nil
	}
	planner, ok := a.Proxy.(coreAgent.CarryOverPlanner)
	if !ok {
		return nil, nil
	}
	from, to, ok := planner.CarryOverLoadRange(params.AfterTime)
	if !ok {
		return nil, nil
	}
	carry, err := disk.LoadCarryOver(from, to)
	if err != nil {
		return nil, err
	}
	a.logger.Debug("agent mock residency: loaded carry-over mocks ahead of their window",
		zap.Time("from", from), zap.Time("to", to), zap.Int("count", len(carry)))
	return carry, nil
}

// appendCarryOver adds the lookahead mocks the filtered slice does not hold
// already (by name), marked per-test like the rest of it.
func appendCarryOver(filtered, carry []*models.Mock) []*models.Mock {
	if len(carry) == 0 {
		return filtered
	}
	have := make(map[string]struct{}, len(filtered))
	for _, m := range filtered {
		if m != nil {
			have[m.Name] = struct{}{}
		}
	}
	for _, m := range carry {
		if m == nil {
			continue
		}
		if _, dup := have[m.Name]; dup {
			continue
		}
		have[m.Name] = struct{}{}
		m.TestModeInfo.IsFiltered = true
		filtered = append(filtered, m)
	}
	return filtered
}

// dedupByName concatenates two slices, keeping the first occurrence per name
// (collapses a window/startup-band overlap on the same mock).
func dedupByName(a, b []*models.Mock) []*models.Mock {
	out := make([]*models.Mock, 0, len(a)+len(b))
	seen := make(map[string]struct{}, len(a)+len(b))
	add := func(src []*models.Mock) {
		for _, m := range src {
			if m == nil {
				continue
			}
			if _, dup := seen[m.Name]; dup {
				continue
			}
			seen[m.Name] = struct{}{}
			out = append(out, m)
		}
	}
	add(a)
	add(b)
	return out
}

// ResetVariables forgets every test variable the replay has bound: a test
// begins. Under the proven switch the agent's route calls it for every mock
// filter a keploy test sends (one per test case, before the test case is
// sent), and BeginScopeAt as a test of keploy mock replay begins.
func (a *Agent) ResetVariables() {
	if resetter, ok := a.Proxy.(coreAgent.VariableResetter); ok {
		resetter.ResetVariables()
	}
}

// StartTestVariables begins a test's variables: it forgets what the previous
// test bound and binds the values the test's own request gave, under one lock.
func (a *Agent) StartTestVariables(given []models.VariableBinding) error {
	binder, ok := a.Proxy.(coreAgent.VariableBinder)
	if !ok {
		if len(given) == 0 {
			a.ResetVariables()
			return nil
		}
		return errors.New("this keploy agent's proxy keeps no test variables")
	}
	if !binder.StartVariables(given) && len(given) > 0 {
		return errors.New("this keploy agent holds no mocks to bind the test's variables to")
	}
	return nil
}

// TestVariables returns the test variables the current test has bound, and
// false when this agent's proxy keeps none.
func (a *Agent) TestVariables() ([]models.VariableBinding, bool) {
	binder, ok := a.Proxy.(coreAgent.VariableBinder)
	if !ok {
		return nil, false
	}
	return binder.BoundVariables()
}

// UpdateMockParams applies filtering parameters and updates the agent's mock manager
func (a *Agent) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	// keploy test's exact serving, under the proven switch (exact_proven.go).
	if params.Exact != nil && provenswitch.On() {
		return a.serveKeployTestExactly(ctx, params, nil)
	}
	return a.stageMocks(ctx, params, nil, nil)
}

// opening is a test's variables, begun as the mock filter that opens the test
// applies (OpenTestCase): with given bound.
type opening struct{ given []models.VariableBinding }

// ReadsClientTests reports whether the learn step reads this agent's tests
// as keploy mock's client tests (coreAgent.ClientTestsReader), under the
// proven switch: what the agent answers a keploy mock session's MockOutgoing
// with (models.ClientTestsReadHeader).
func (a *Agent) ReadsClientTests() bool {
	c, ok := a.Proxy.(coreAgent.ClientTestsReader)
	return ok && provenswitch.On() && c.ClientTests()
}

// ReadsTestRequest reports whether OpenTestCase hands the learn step the
// test case's request its filter carries (coreAgent.TestRequestTaker), under
// the proven switch: what the agent answers the opening filter with
// (models.TestRequestReadHeader).
func (a *Agent) ReadsTestRequest() bool {
	_, ok := a.Proxy.(coreAgent.TestRequestTaker)
	return ok && provenswitch.On()
}

// OpenTestCase applies a keploy test case's mock filter that opens a test
// (models.MockFilterParams.BeginsTestVariables, under the proven switch) as
// UpdateMockParams does, and begins the test's variables with the values its
// request gave (the filter's Variables) bound, in the step that makes the
// test's mocks live (coreAgent.ExactTestOpener, coreAgent.WindowedTestOpener).
// No call the test's mocks answer binds before its variables begin: begun
// after, one of a set's first test answered between the two bound in the
// run's, which every later test of the set reads (a test answered with
// another's value, a false pass). A proxy that cannot open a test so opens
// one only when it keeps no variables, and none were given
// (opensWithoutVariables).
func (a *Agent) OpenTestCase(ctx context.Context, params models.MockFilterParams) error {
	if !provenswitch.On() || !params.BeginsTestVariables() {
		return errors.New("this mock filter opens no test case, and begins no test's variables")
	}
	// The test case's own request, for the learn step, before its mocks go
	// live: the replay sends the request only after the filter is answered.
	// A filter that carries none (a keploy older than its agent) is given
	// as such, and the learn step refuses what only the request could
	// account for.
	if taker, ok := a.Proxy.(coreAgent.TestRequestTaker); ok {
		taker.TakeTestRequest(params.TestRequest)
	}
	open := &opening{given: params.Variables}
	if params.Exact != nil {
		// Refused before anything of the test case is served or settled.
		if _, ok := a.Proxy.(coreAgent.ExactTestOpener); !ok {
			if err := a.opensWithoutVariables(open.given); err != nil {
				return err
			}
		}
		return a.serveKeployTestExactly(ctx, params, open)
	}
	return a.stageMocks(ctx, params, nil, open)
}

// stageMocks stages on the proxy what UpdateMockParams stages for params, and
// with it the per-test mocks named alsoLoad that the on-disk store holds,
// whatever params' own load reads (mocksForParams). With open, the staging
// opens a test, whose variables begin with it (OpenTestCase).
func (a *Agent) stageMocks(ctx context.Context, params models.MockFilterParams, alsoLoad []string, open *opening) error {
	// The window is opened in one step only through WindowedProxy's staging;
	// a proxy staged by SetMocks never is.
	_, windowed := a.Proxy.(coreAgent.WindowedProxy)
	opener, opensInOneStep := a.Proxy.(coreAgent.WindowedTestOpener)
	opensInOneStep = opensInOneStep && windowed
	if open != nil && !opensInOneStep {
		if err := a.opensWithoutVariables(open.given); err != nil {
			return err
		}
	}
	filteredMocks, unfilteredMocks, err := a.mocksForParams(ctx, params, alsoLoad...)
	if err != nil {
		return err
	}

	// Atomically update mocks AND the active test window when the proxy
	// supports the WindowedProxy extension. Otherwise fall back to the
	// stable SetMocks contract — third-party proxies without window
	// support keep working in legacy lax mode.
	if wp, ok := a.Proxy.(coreAgent.WindowedProxy); ok {
		if open != nil && opensInOneStep {
			if err := opener.OpenTestWithWindow(ctx, filteredMocks, unfilteredMocks, params.AfterTime, params.BeforeTime, open.given); err != nil {
				utils.LogError(a.logger, err, "failed to stage the test case's window on the proxy and begin its variables")
				return err
			}
			return nil
		}
		if err := wp.SetMocksWithWindow(ctx, filteredMocks, unfilteredMocks, params.AfterTime, params.BeforeTime); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks and test window on proxy; verify the proxy implements WindowedProxy and the agent/proxy versions are in sync, or retry via the plain SetMocks fallback path")
			return err
		}
	} else {
		if err := a.Proxy.SetMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks on proxy")
			return err
		}
	}
	return nil
}

// errOpensApart refuses a test's opening on a proxy that keeps the test
// variables a replay binds (coreAgent.VariableBinder,
// coreAgent.VariableResetter) but cannot begin them as the test's mocks go
// live (coreAgent.ExactTestOpener, coreAgent.WindowedTestOpener): begun
// apart, a call one of its mocks answers between the two binds in no scope
// of the test's, and before a set's first test in the run's, which every
// later test of the set reads.
var errOpensApart = errors.New("this keploy agent's proxy keeps test variables but cannot begin them as a test opens; run the keploy agent of this keploy version")

// opensWithoutVariables says why a proxy that cannot open a test with its
// variables in one step cannot open it, given given: one that keeps
// variables would begin them apart (errOpensApart), and one that keeps none
// cannot bind given. nil when it keeps none and none were given: the test
// opens with nothing to begin.
func (a *Agent) opensWithoutVariables(given []models.VariableBinding) error {
	_, binds := a.Proxy.(coreAgent.VariableBinder)
	_, resets := a.Proxy.(coreAgent.VariableResetter)
	if binds || resets {
		return errOpensApart
	}
	if len(given) > 0 {
		return errors.New("this keploy agent's proxy keeps no test variables")
	}
	return nil
}

// mocksForParams is the pool UpdateMockParams stages for params: the per-test
// and the session tier, filtered as params ask. It also applies what the
// staging call carries beside the pool (the startup cutoff, the recorded
// windows, the stateful cursors' reset), as UpdateMockParams always has.
// alsoLoad names per-test mocks the on-disk store holds to read as well,
// beside what params' own load reads.
func (a *Agent) mocksForParams(ctx context.Context, params models.MockFilterParams, alsoLoad ...string) ([]*models.Mock, []*models.Mock, error) {
	// Seed the startup-init cutoff from the set's earliest RECORDED test before
	// anything else touches the window. Without it the cutoff follows whichever
	// test fires FIRST, so a --test-sets selection, an ignored test or the
	// streaming deferral leaves it late and mocks belonging to a test that is
	// never run are classified as startup-init and served as bootstrap traffic.
	// Optional capability: a proxy that does not implement it keeps the
	// fired-window behaviour.
	if !params.FirstRecordedTestStart.IsZero() {
		if seeder, ok := a.Proxy.(coreAgent.StartupCutoffSeeder); ok {
			seeder.SeedStartupCutoff(params.FirstRecordedTestStart)
		}
	}
	// Same moment, same optional-capability style: the recorded window of every
	// test of the set, which lets the proxy release traffic the test windows do
	// not pace by themselves (server push) at the window it belongs to.
	if len(params.RecordedWindows) > 0 {
		if seeder, ok := a.Proxy.(coreAgent.RecordedWindowsSeeder); ok {
			seeder.SeedRecordedWindows(params.RecordedWindows)
		}
	}
	// A staging call (recordedSetShape populated: FirstRecordedTestStart and/or
	// RecordedWindows) marks a new test-set. Reset the stateful-dependency
	// cursors so each test-set replays its sequences (1,2,3…) from the start
	// rather than carrying a cursor across sets, while the per-test-case calls
	// (empty shape) leave them alone so a sequence spans the whole set. Optional
	// capability: a proxy without stateful cursors is a no-op.
	//
	// This reuses the same shape-non-empty staging heuristic as the startup
	// cutoff / recorded-window seeders above. A degenerate test-set whose cases
	// all carry a zero request timestamp produces an empty shape, so none of
	// these fire — the pre-existing limitation of that heuristic. Replacing it
	// with an explicit MockFilterParams.Staging flag (for all three) is a
	// tracked follow-up; real recordings always carry timestamps.
	if !params.FirstRecordedTestStart.IsZero() || len(params.RecordedWindows) > 0 {
		if resetter, ok := a.Proxy.(coreAgent.StatefulCursorResetter); ok {
			resetter.ResetStatefulCursors()
		}
	}

	a.logger.Debug("UpdateMockParams called",
		zap.Time("afterTime", params.AfterTime),
		zap.Time("beforeTime", params.BeforeTime),
		zap.Bool("useMappingBased", params.UseMappingBased),
		zap.Int("mockMappingCount", len(params.MockMapping)),
		zap.Bool("strictMockWindow", params.StrictMockWindow))

	// Strict mock-window is OPT-IN via either the per-call flag
	// (params.StrictMockWindow) or the process-wide KEPLOY_STRICT_MOCK_WINDOW
	// env override. Surface the EFFECTIVE state so operators searching
	// agent logs for "strict mock window enabled" find hits regardless of
	// which route they used — previously the log fired only on the per-call
	// flag, making env-only opt-ins invisible.
	if strictEnabled := pkg.IsStrictMockWindow(params.StrictMockWindow); strictEnabled {
		// Fire the activation message at most once per agent process
		// (sync.Once). Info (project logging policy disallows Warn for
		// expected-default state); the escape-hatch names are embedded
		// inline so operators hitting unexpected "missing mock" errors
		// after an upgrade can opt out without digging through docs.
		// Per-test diagnostics drop to Debug.
		a.strictLogOnce.Do(func() {
			a.logger.Info(
				"strict mock-window containment is ACTIVE for this session — per-test mocks whose request "+
					"timestamp falls outside the outer test window will be dropped rather than promoted "+
					"across tests. If your replays start reporting missing mocks after an upgrade, this "+
					"is likely why. To opt out: set KEPLOY_STRICT_MOCK_WINDOW=0 in the environment, OR "+
					"test.strictMockWindow: false in keploy.yaml. A recording whose recorder stated each "+
					"mock's lifetime (spec.metadata.lifetime) serves each test its own calls in every mode: "+
					"declare an HTTP call every test may make in keploy.yaml (sessionCalls), or record the "+
					"test set again.",
				zap.Bool("viaPerCallFlag", params.StrictMockWindow),
				zap.Bool("viaEnvOverride", !params.StrictMockWindow && strictEnabled),
				zap.String("escape_hatch_env", "KEPLOY_STRICT_MOCK_WINDOW=0"),
				zap.String("escape_hatch_config", "test.strictMockWindow: false"))
		})
		a.logger.Debug("strict mock window active for test",
			zap.Time("windowStart", params.AfterTime),
			zap.Time("windowEnd", params.BeforeTime))
	}

	// Get stored mocks for the client
	storageInterface, exists := a.clientMocks.Load(uint64(0))
	if !exists {
		return nil, nil, fmt.Errorf("no mocks stored for client ID")
	}
	storage := storageInterface.(*ClientMockStorage)

	storage.mu.RLock()
	residentFiltered := make([]*models.Mock, len(storage.filtered))
	originalUnfiltered := make([]*models.Mock, len(storage.unfiltered))
	copy(residentFiltered, storage.filtered)
	copy(originalUnfiltered, storage.unfiltered)
	disk := storage.diskMocks
	storage.mu.RUnlock()

	// On WindowedProxy the agent filter runs non-strict (SetMocksWithWindow does
	// the authoritative strict enforcement); env override still applies inside.
	_, isWindowedProxy := a.Proxy.(coreAgent.WindowedProxy)
	agentStrict := params.StrictMockWindow && !isWindowedProxy

	// firstWindowStart: earliest observed window start, so startup-init mocks
	// (req < it) are kept, not dropped as stale.
	//
	// On the FIRST window the proxy has not learned it yet: SetMocksWithWindow
	// records it (mockmanager.go:676-679) LATER in this same call, so the read
	// here returns the zero time and every startup-band guard downstream is
	// switched off for test #1 — loadPerTestMocks' disk.LoadBefore(...) never
	// fires, and the startup tier stays EMPTY for the one test where a slow
	// application bootstrap actually lands. That is why a bootstrap query issued
	// under load misses with candidates:0 while the mock sits unread on disk.
	//
	// Derive it from this call's own window start instead, applying exactly the
	// predicate mockmanager uses so the two cannot disagree: a zero or BaseTime
	// AfterTime is "no window", anything else is a real start.
	var firstWindowStart time.Time
	if reader, ok := a.Proxy.(coreAgent.FirstWindowStartReader); ok {
		firstWindowStart = reader.FirstTestWindowStart()
	}
	//
	// Guarded on isWindowedProxy on purpose. This derivation is only valid where
	// SetMocksWithWindow adopts this very same start as firstWindowStart later in
	// this call (see MockManager.SetMocksWithWindow). A proxy implementing only
	// the base agent.Proxy interface has neither the reader nor that adoption, so
	// its reader value is permanently zero — without the guard the fallback would
	// fire on EVERY window with the CURRENT window's start, collapsing
	// "req < firstWindowStart" into "req < afterTime" and preserving every stale
	// previous-test mock. That is exactly the cross-test bleed strict mode exists
	// to prevent, and agentStrict is true on that path.
	firstWindowStart = effectiveFirstWindowStart(isWindowedProxy, firstWindowStart, params.AfterTime)

	// Load only what the filter will keep for this call (see loadPerTestMocks).
	originalFiltered, err := a.loadPerTestMocks(residentFiltered, disk, params, firstWindowStart, alsoLoad...)
	if err != nil {
		utils.LogError(a.logger, err, "failed to load this test's per-test mocks from the agent's on-disk store; the temp file may be unreadable or the pool was superseded mid-test")
		return nil, nil, err
	}
	carryOver, err := a.loadCarryOverLookahead(disk, params)
	if err != nil {
		utils.LogError(a.logger, err, "failed to load the carry-over mocks for this test from the agent's on-disk store; the temp file may be unreadable or the pool was superseded mid-test")
		return nil, nil, err
	}

	a.logger.Debug("Original mocks before filtering",
		zap.Int("originalFiltered", len(originalFiltered)),
		zap.Int("originalUnfiltered", len(originalUnfiltered)))

	var filteredMocks, unfilteredMocks []*models.Mock

	// When the proxy supports the windowed extension, MockManager's
	// SetMocksWithWindow applies the authoritative per-test-window
	// filter (including startup-init promotion for req < firstWindowStart
	// — bootstrap traffic like HikariCP pool warm-up that fires before
	// any test request). Pre-dropping out-of-window per-test mocks at
	// the agent under strict mode would hide those startup-init mocks
	// from MockManager and break replay of app-bootstrap DB calls under
	// strict. So: agent-level strict pre-filtering runs only on the
	// legacy SetMocks fallback path. For WindowedProxy callers we
	// pass strict=false to FilterPerTestAndLaxPromoted and let
	// MockManager.SetMocksWithWindow enforce strict semantics.

	// Apply filtering based on parameters
	if params.UseMappingBased && (len(params.MockMapping) > 0 || params.ExactMapping) {
		filteredMocks = pkg.FilterTcsMocksMappingInWindow(ctx, a.logger, originalFiltered, params.MockMapping, params.AfterTime, params.BeforeTime)
		unfilteredMocks = pkg.FilterConfigMocksMapping(ctx, a.logger, originalUnfiltered, params.MockMapping)
	} else {
		// Lax-mode promotion restoration: filterByTimeStamp moves
		// out-of-window non-config per-test mocks into its "unfiltered"
		// return slice so they remain reusable across tests (the
		// pre-Phase-2 kind-fallback used to do this implicitly by
		// over-promoting anything MySQL/Postgres/... to LifetimeSession
		// at DeriveLifetime). Round-5 correctly narrowed DeriveLifetime
		// but FilterTcsMocks was still discarding the promoted slice —
		// so shared-fixture mocks (SELECT * FROM pet used by every
		// test in spring-petclinic) were lost at the agent. We now
		// drain those promoted-to-session mocks out of the per-test
		// filter and merge them into the session pool passed to the
		// proxy. Under strict mode this branch doesn't fire because
		// filterByTimeStamp drops rather than promotes.
		// The calls in flight when the window's test began are its own
		// per-test mocks in the user's window mode (params.StrictMockWindow),
		// not agentStrict's: on a windowed proxy the agent's filter is never
		// strict, and the mock manager filters what it is handed.
		inFlightFrom, _ := a.inFlightFrom(params)
		perTestIn, promotedToSession := pkg.FilterPerTestAndLaxPromotedInFlightAware(ctx, a.logger, originalFiltered, params.AfterTime, params.BeforeTime, agentStrict, firstWindowStart, inFlightFrom, params.StrictMockWindow)
		filteredMocks = perTestIn
		unfilteredMocks = pkg.FilterConfigMocksTierAware(ctx, a.logger, originalUnfiltered, params.AfterTime, params.BeforeTime, agentStrict, firstWindowStart)
		if len(promotedToSession) > 0 {
			unfilteredMocks = append(unfilteredMocks, promotedToSession...)
			// Ordering: both FilterConfigMocks and FilterPerTestAndLaxPromoted
			// already sort their outputs by ReqTimestampMock deterministically,
			// so the concatenated slice is stable across runs. We deliberately
			// DO NOT re-sort the merged pool here — the Mongo v2 matcher
			// relies on config-tagged session mocks preceding lax-promoted
			// per-test mocks in the unfiltered iteration order (config tier
			// wins handshake/ping ties before data-plane mocks get a chance
			// to steal the match and drop the driver connection mid-handshake).
			// Re-sorting by timestamp interleaves them and causes the
			// Mongo fuzzer cross-version replay to hang.
			a.logger.Debug("lax-promoted per-test mocks into session pool for cross-test reuse",
				zap.Int("count", len(promotedToSession)))
		}
	}

	// Count IsFiltered distribution for debugging
	var filteredCount, unfilteredCount int
	for _, m := range unfilteredMocks {
		if m.TestModeInfo.IsFiltered {
			filteredCount++
		} else {
			unfilteredCount++
		}
	}
	a.logger.Debug("After filtering",
		zap.Int("filteredMocks", len(filteredMocks)),
		zap.Int("unfilteredMocks", len(unfilteredMocks)),
		zap.Int("unfilteredWithIsFilteredTrue", filteredCount),
		zap.Int("unfilteredWithIsFilteredFalse", unfilteredCount))

	// The carry-over lookahead bypasses the filter on purpose: on a windowed
	// proxy the filter runs lax and would make these session mocks, reachable in
	// every window and never consumed out of the manager's carry-over tier.
	// They are per-test mocks outside this window, which is exactly what
	// SetMocksWithWindow files into that tier. filterOutDeleted below still
	// drops the ones already consumed.
	filteredMocks = appendCarryOver(filteredMocks, carryOver)

	// Filter out deleted mocks if totalConsumedMocks is provided
	if params.AgentOwnsConsumed {
		// Agent applies filterOutDeleted from its OWN persistent consumption
		// history — the client no longer re-sends the growing TotalConsumedMocks
		// map. Equivalent by construction: that persistent map is the exact
		// accumulation of the flagMockAsUsed states the client used to relay.
		if reader, ok := a.Proxy.(coreAgent.ConsumedStateReader); ok {
			filteredMocks = a.filterOutDeleted(filteredMocks, reader.GetPersistentConsumed())
		} else if params.TotalConsumedMocks != nil {
			// Proxy can't expose its persistent map — never silently skip the
			// filter; fall back to any client-sent map.
			a.logger.Warn("AgentOwnsConsumed set but proxy has no ConsumedStateReader; falling back to client-sent TotalConsumedMocks")
			filteredMocks = a.filterOutDeleted(filteredMocks, params.TotalConsumedMocks)
		}
	} else if params.TotalConsumedMocks != nil {
		filteredMocks = a.filterOutDeleted(filteredMocks, params.TotalConsumedMocks)
	}

	return filteredMocks, unfilteredMocks, nil
}

// ServedMocks reports the mocks served so far this session, keyed by mock name.
//
// It reads the proxy's never-drained persistent consumption map, NOT
// GetConsumedMocks: that one drains, so polling it mid-run would steal entries
// from the end-of-run outcome report and silently shrink both the consumed
// summary and the --strict verdict.
//
// Returns an empty map (not an error) when the proxy cannot expose the map, for
// the same reason ConsumedStateReader is an optional extension — a third-party
// Proxy without it must keep working, and a caller polling for progress should
// degrade to "nothing to show" rather than fail the run.
func (a *Agent) ServedMocks(_ context.Context) (map[string]models.MockState, error) {
	reader, ok := a.Proxy.(coreAgent.ConsumedStateReader)
	if !ok {
		a.logger.Debug("proxy has no ConsumedStateReader; no served-mock state to report")
		return map[string]models.MockState{}, nil
	}
	return reader.GetPersistentConsumed(), nil
}

// ReadsConsumedForPerTestOnly reports whether svc, the service the route
// serves, reads the consumed history the way this one does: only in
// filterOutDeleted, over the per-test mocks it stages, which all come from the
// filtered half of StoreMocks. The route then advertises it to the client in
// models.ConsumedScopeHeader, so the client can stop sending the entries of
// every other mock it served.
//
// It is true only when svc is this agent itself. A service that embeds *Agent
// inherits this method but may override UpdateMockParams to read the history
// differently, so it does not inherit the answer, and its clients keep
// sending the whole history.
func (a *Agent) ReadsConsumedForPerTestOnly(svc Service) bool {
	s, ok := svc.(*Agent)
	return ok && s == a
}

// filterOutDeleted filters out deleted mocks based on totalConsumedMocks.
//
// A surviving mock whose recorded consumption state differs is returned as a
// copy carrying that state; the input is never written. The filters hand it
// fresh copies, but with no test window they pass the stored mocks through, and
// a stored mock staged by an earlier call is in the proxy's pools, where
// matchers read it concurrently.
func (a *Agent) filterOutDeleted(mocks []*models.Mock, totalConsumedMocks map[string]models.MockState) []*models.Mock {
	filtered := make([]*models.Mock, 0, len(mocks))
	for _, m := range mocks {
		// treat empty/missing names as never consumed
		if m == nil || m.Name == "" {
			filtered = append(filtered, m)
			continue
		}
		// we are picking mocks that are not consumed till now (not present in map),
		// and, mocks that are updated.
		if k, ok := totalConsumedMocks[m.Name]; !ok || k.Usage != models.Deleted {
			if ok && (m.TestModeInfo.IsFiltered != k.IsFiltered || m.TestModeInfo.SortOrder != k.SortOrder) {
				m = m.ShallowCopy()
				m.TestModeInfo.IsFiltered = k.IsFiltered
				m.TestModeInfo.SortOrder = k.SortOrder
			}
			filtered = append(filtered, m)
		}
	}
	return filtered
}
