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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/plaintcp"

	"go.keploy.io/server/v3/pkg/agent"
	grpc "go.keploy.io/server/v3/pkg/agent/proxy/incoming/gRPC"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"golang.org/x/net/http/httpguts"
)

type proxyStop func() error

// IngressHook defines the interface for ingress forwarding implementations.
// Both the default Go TCP forwarder and external components (e.g. an
// enterprise ingress handler) implement this interface.
type IngressHook interface {
	// StartIngress begins ingress forwarding for the given port pair.
	// The provided context should be used for lifetime management of the
	// forwarding goroutines.
	StartIngress(ctx context.Context, origPort, newPort uint16) error
	// StopIngress tears down the ingress forwarder for the given original port.
	StopIngress(origPort uint16) error
}

type IngressProxyManager struct {
	// stopping: the recording it serves is stopping, and its app is
	// stopped on purpose (SetStopping, stopping.go).
	stopping atomic.Bool
	// waits is the shared wait of the calls that found an app address
	// refusing, by address (appdial.go), guarded by waitsMu.
	waitsMu sync.Mutex
	waits   map[string]*appWait
	// listensAt is where the socket table found the app listening, by the
	// address that refused (appdial.go), guarded by waitsMu.
	listensAt map[string]string
	// pollControl is the Control of the dialer a wait's poll dials the app
	// with (pollApp): nil but in tests, which hold a poll's dial where its
	// context no longer stops it, between the dialer's look at the context
	// and its connect.
	pollControl func(network, address string, c syscall.RawConn) error

	mu       sync.Mutex
	active   map[uint16]proxyStop
	logger   *zap.Logger
	hooks    agent.Hooks
	tcChan   chan *models.TestCase
	mockMode bool
	requests bool
	// incomingOpts is read by ingress capture goroutines on every
	// captured request (CaptureHook call sites in http.go) and written
	// by IngressProxyManager.Start on every recorder (re)connect. Pre-
	// atomic this was a plain struct field guarded by nothing on the
	// read path, which is a real data race the moment a recorder
	// reconnects with different filter/sampling settings while ingress
	// traffic is in flight. atomic.Pointer gives a lock-free,
	// pointer-sized swap that the readers can Load without contending.
	// Always stored as a heap copy of the caller's value so writes
	// don't tear the struct in-place.
	incomingOpts atomic.Pointer[models.IncomingOptions]
	synchronous  bool
	// mapping mirrors !cfg.DisableMapping at construction time. Forwarded
	// to CaptureHook so the synchronous-record path can decide whether
	// SyncMockManager.ResolveRange should emit a TestMockMapping entry
	// onto the agent's mappingChan. Without this plumbing the mapping
	// arg was hardcoded false at the OSS Capture call site, which
	// silently disabled mappings.yaml production during record (#3905).
	mapping     bool
	sampling    bool
	samplingSem chan struct{}

	// captures counts the capture goroutines the HTTP/1 handlers started
	// that have not returned yet. Done is each one's last step, so Wait says
	// every capture they started is over (tests wait on it before they
	// swap a hook or a semaphore those goroutines read).
	captures sync.WaitGroup

	ingressHook IngressHook
	// requestTaker is given each HTTP/1 request before it is forwarded,
	// while it wants them (SetRequestTaker, takeRequest).
	requestTaker RequestTaker

	// portHolder is the hooks, when they hold the application's moved ports
	// from inside their bind (ingress_lookup.c): the manager serves what its
	// park accepts, and registers each forwarder it starts for the lookup.
	// nil when the hooks do not implement it or hold nothing, and the
	// forwarder covers the window as it did before (shrunk by binding the
	// port as soon as the bind event arrives).
	portHolder IngressPortHolder
	// parkedServed and parkedServedV6 count the parked IPv4 and IPv6
	// connections the manager forwarded (serveParked), for the run's summary.
	parkedServed   atomic.Uint64
	parkedServedV6 atomic.Uint64
	// steeredServed counts the calls the park took while the hooks steer
	// (serveSteered): every call to the app's ports then.
	steeredServed atomic.Uint64
	// portSems are the synchronous mode's per-port semaphores, which a
	// port's forwarder and the park share (portSem). Guarded by semMu.
	semMu    sync.Mutex
	portSems map[uint16]chan struct{}

	// relocated maps an app port whose bind the record hooks moved (keploy's
	// ingress forwarder then holds the port) to the port the app was moved
	// to, for each forwarder that started with a known one. Guarded by mu.
	relocated map[uint16]uint16

	// startOnce gates the ListenForIngressEvents goroutine so that
	// repeated Start() calls (a reconnecting recorder opening a new
	// /agent/incoming stream while the agent process keeps running)
	// don't leak additional watcher goroutines that all consume from
	// the same eventChan. The IncomingOptions on subsequent calls
	// still take effect — Start always atomic-swaps incomingOpts via
	// atomic.Pointer.Store regardless of whether the watcher actually
	// (re)spawns — so callers that intentionally reconnect with
	// different filtering get the new behavior immediately on the
	// next captured request.
	startOnce sync.Once

	// logKeys is the session the ingress serves now (SetLogKeySession):
	// each connection it takes names its calls' path tags under its key
	// (handleConnection). nil: the process's key.
	logKeys atomic.Pointer[models.LogKeySession]
}

func New(logger *zap.Logger, h agent.Hooks, cfg *config.Config) *IngressProxyManager {
	pm := &IngressProxyManager{
		logger:      logger,
		hooks:       h,
		tcChan:      make(chan *models.TestCase, 100),
		active:      make(map[uint16]proxyStop),
		mockMode:    cfg.Agent.MockMode,
		requests:    cfg.Agent.RecordRequests,
		synchronous: cfg.Agent.Synchronous,
		mapping:     !cfg.DisableMapping,
		sampling:    false,
		samplingSem: make(chan struct{}, func() int {
			if cfg.Agent.EnableSampling > 0 {
				return cfg.Agent.EnableSampling
			}
			return 5
		}()),
	}
	if cfg.Agent.EnableSampling > 0 {
		pm.sampling = true
	}
	// Default to the Go TCP forwarder; can be replaced via SetIngressHook.
	pm.ingressHook = newGoTCPIngressHook(pm)
	if holder, ok := h.(IngressPortHolder); ok {
		pm.portHolder = holder
	}
	return pm
}

// loadIncomingOpts returns the current IncomingOptions snapshot, or a
// zero-value struct if Start has never been called. CaptureHook readers
// in http.go call this on every captured request — the atomic.Pointer
// Load is wait-free, so this stays cheap on the hot path.
func (pm *IngressProxyManager) loadIncomingOpts() models.IncomingOptions {
	if p := pm.incomingOpts.Load(); p != nil {
		return *p
	}
	return models.IncomingOptions{}
}

// IncomingOptions returns the options the recorder last started ingress
// with (filters, sampling): zero before Start. It is how a capture path that
// does not go through this manager's forwarder (an enterprise build's
// observe-only capture) applies the same recording options.
func (pm *IngressProxyManager) IncomingOptions() models.IncomingOptions {
	return pm.loadIncomingOpts()
}

// SetIngressHook replaces the default Go TCP forwarder with an external
// ingress handler supplied by an enterprise build.
func (pm *IngressProxyManager) SetIngressHook(h IngressHook) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.ingressHook = h
}

func (pm *IngressProxyManager) Start(ctx context.Context, opts models.IncomingOptions) chan *models.TestCase {
	// Always update incomingOpts — a reconnecting recorder may pass
	// different filtering / sampling settings that should take effect
	// immediately. Atomic swap of a pointer-sized field means the
	// CaptureHook readers in http.go can Load it on every captured
	// request without contending on a mutex.
	optsCopy := opts
	pm.incomingOpts.Store(&optsCopy)

	// Gate the ListenForIngressEvents goroutine so subsequent Start
	// invocations don't spawn duplicate watchers. The ctx passed in
	// here is now process-lifetime (see pkg/service/agent/agent.go),
	// so the goroutine survives /agent/incoming stream teardowns;
	// without the gate, each reconnect would leak another watcher.
	pm.startOnce.Do(func() {
		go pm.ListenForIngressEvents(ctx)
		if park := pm.ingressPark(); park != nil {
			go pm.serveParkedConnections(ctx, park)
		}
	})
	return pm.tcChan
}

// SetLogKeySession makes s the session the ingress serves from now: every
// connection it takes from here names its calls by path tags under s's key
// (models.LogKeySession), unless its context carries a session of its own.
// The agent sets it as each session it serves begins (a recording, a
// replay's session); a connection taken before keeps the session it was taken
// in, as its calls are that session's.
func (pm *IngressProxyManager) SetLogKeySession(s *models.LogKeySession) {
	pm.logKeys.Store(s)
}

// withLogKeys is ctx carrying the session the ingress serves now
// (SetLogKeySession), unless ctx carries one already.
func (pm *IngressProxyManager) withLogKeys(ctx context.Context) context.Context {
	if models.LogKeys(ctx) != nil {
		return ctx
	}
	return models.WithLogKeySession(ctx, pm.logKeys.Load())
}

// TCChan returns the test case channel for direct use by external consumers
// (e.g., the enterprise proxyless agent) without going through Start().
func (pm *IngressProxyManager) TCChan() chan *models.TestCase {
	return pm.tcChan
}

// StartIngressProxy starts a new ingress proxy on the given original app port if it's not already running.
// It delegates to the registered IngressHook (default: Go TCP forwarder).
func (pm *IngressProxyManager) StartIngressProxy(ctx context.Context, origAppPort, newAppPort uint16) {
	// If the agent is already shutting down, do not arm a new forwarder.
	// WatchBindEvents delivers bind events over a buffered channel, so
	// ListenForIngressEvents can still drain late/stale events after the context
	// is canceled. Binding then would leave a listener holding the app's port
	// (commonly 8000) for the next record run ("address already in use") and would
	// also surface as a spurious ERROR log. Silently skip during teardown.
	if ctx.Err() != nil {
		return
	}
	pm.mu.Lock()
	if _, ok := pm.active[origAppPort]; ok {
		pm.mu.Unlock()
		if origAppPort == newAppPort && pm.ingressSteers() {
			// Another socket of the app's on a port keploy steers
			// already (on another address, or over IPv6), which the
			// hooks noted as they sent this event: kept as the first
			// was, so an agent restarted in the pod steers it again.
			pm.portHolder.KeepSteerTable()
		}
		return
	}
	if pm.ingressSteers() {
		// The app listens on its own port, and the lookup steers the calls
		// its sockets take to the park, which forwards each to the app
		// (serveSteered): no forwarder binds the app's port, so nothing of
		// keploy's stands on it when keploy goes.
		pm.active[origAppPort] = func() error { return nil }
		pm.mu.Unlock()
		// Kept for an agent restarted in the pod, which steers it again.
		pm.portHolder.KeepSteerTable()
		pm.logger.Info("Started ingress forwarding",
			zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", origAppPort),
			zap.String("how", "steered: the application keeps its port"))
		return
	}
	hook := pm.ingressHook
	startDone := make(chan struct{})
	started := false
	// Reserve the slot so concurrent callers see this port as active.
	pm.active[origAppPort] = func() error {
		<-startDone
		if !started {
			return nil
		}
		return hook.StopIngress(origAppPort)
	}
	pm.mu.Unlock()

	if err := hook.StartIngress(ctx, origAppPort, newAppPort); err != nil {
		close(startDone)
		if ctx.Err() != nil {
			// The agent is shutting down (context canceled or deadline exceeded); a
			// start failure here is expected teardown, not a real error. Keep it out
			// of ERROR so it doesn't trip error-grep gates in CI record output.
			pm.logger.Debug("Ingress forwarder start aborted; agent shutting down",
				zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort), zap.Error(err))
		} else if errors.Is(err, errIngressPortYielded) {
			// keploy left the port to the listener that holds it, on
			// purpose, and its WARN said so: not a failure.
			pm.logger.Debug("Ingress forwarder not started: the port is another listener's",
				zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort), zap.Error(err))
		} else {
			pm.logger.Error("Ingress hook failed to start; verify hook configuration/permissions and required kernel features, or disable the custom ingress hook",
				zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort), zap.Error(err))
		}
		pm.mu.Lock()
		delete(pm.active, origAppPort)
		pm.mu.Unlock()
		return
	}
	started = true
	close(startDone)
	if newAppPort != 0 {
		pm.mu.Lock()
		if _, ok := pm.active[origAppPort]; ok { // not stopped in the meantime
			if pm.relocated == nil {
				pm.relocated = make(map[uint16]uint16)
			}
			pm.relocated[origAppPort] = newAppPort
		}
		pm.mu.Unlock()
	}
	pm.logger.Info("Started ingress forwarding",
		zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort))
}

// AppListenPort is the port the app's own socket listens on for its port orig.
// That is orig, unless the record hooks moved the app's bind elsewhere: then
// it is the port the app was moved to. keploy's forwarder holds orig once it
// listens, and before that the hooks hold it from the app's bind (the park
// serves its calls, and records them as tests on orig): so a test can be on
// orig while the forwarder is still starting, or before its bind event is
// read, and the hold's own record of the move says where the app went. ok is
// false while the forwarder holds orig and that port is not known (no hold,
// and the forwarder is still starting or the bind event did not carry it).
func (pm *IngressProxyManager) AppListenPort(orig uint16) (port uint16, ok bool) {
	if pm.ingressSteers() {
		return orig, true // no bind is moved
	}
	pm.mu.Lock()
	_, forwarded := pm.active[orig]
	moved, relocated := pm.relocated[orig]
	pm.mu.Unlock()
	if relocated {
		return moved, true
	}
	if pm.portHolder != nil {
		if moved, ok := pm.portHolder.MovedPort(orig); ok {
			return moved, true
		}
	}
	if !forwarded {
		return orig, true
	}
	return 0, false
}

// StopAll gracefully shuts down all active ingress proxies.
func (pm *IngressProxyManager) StopAll() {
	pm.mu.Lock()
	stops := make(map[uint16]proxyStop, len(pm.active))
	for p, s := range pm.active {
		stops[p] = s
	}
	pm.active = make(map[uint16]proxyStop)
	pm.relocated = nil
	pm.mu.Unlock()

	for p, s := range stops {
		if err := s(); err != nil {
			pm.logger.Error("Failed to stop ingress proxy; verify ingress hook stop implementation and permissions, then restart the agent if needed",
				zap.Uint16("port", p), zap.Error(err))
		}
	}
}

func (pm *IngressProxyManager) ListenForIngressEvents(ctx context.Context) {
	eventChan, err := pm.hooks.WatchBindEvents(ctx)
	if err != nil && pm.mockMode {
		pm.logger.Debug("not watching app binds: the tests' calls to the app are found when they connect", zap.Error(err))
		return
	}
	if err != nil {
		pm.logger.Error("Failed to start watching for ingress events", zap.Error(err))
		return
	}

	pm.logger.Debug("Listening for application bind events to start ingress proxies...")

	for e := range eventChan {

		pm.logger.Debug("Intercepted application bind event",
			zap.Uint32("pid", e.PID),
			zap.Uint16("Orig_App_Port", e.OrigAppPort),
			zap.Uint16("New_App_Port", e.NewAppPort))

		pm.StartIngressProxy(ctx, e.OrigAppPort, e.NewAppPort)
	}
	pm.logger.Debug("Stopping ingress event listener as the event channel was closed.")
	pm.logIngressHoldEnd()
	pm.StopAll()
}

// goTCPIngressHook is the default IngressHook implementation that uses a
// Go-based TCP forwarder for ingress traffic capture.
type goTCPIngressHook struct {
	pm         *IngressProxyManager
	mu         sync.Mutex
	forwarders map[uint16]*tcpForwarderState
}

type tcpForwarderState struct {
	listener  net.Listener // at 0.0.0.0, the port's IPv4 calls
	listener6 net.Listener // at [::], the port's IPv6 calls; nil with the hold (listenIngressIPv6)
	cancel    context.CancelFunc
	done      chan struct{} // closed when the accept loops exit
}

// ingressTargetPollInterval is how often a call that waits for the app to
// listen looks for its listener (appdial.go).
const ingressTargetPollInterval = 25 * time.Millisecond

func newGoTCPIngressHook(pm *IngressProxyManager) *goTCPIngressHook {
	return &goTCPIngressHook{
		pm:         pm,
		forwarders: make(map[uint16]*tcpForwarderState),
	}
}

func (h *goTCPIngressHook) StartIngress(ctx context.Context, origPort, newPort uint16) error {
	// TODO : We will change this interface implementation to the IP
	origAppAddr := "0.0.0.0:" + strconv.Itoa(int(origPort))
	newAppAddr := "127.0.0.1:" + strconv.Itoa(int(newPort))
	logger := h.pm.logger

	// Bind the app's port and start forwarding at once, whether or not the
	// app listens on its moved port yet: a call that comes before it does
	// waits for its listener in its dial (dialApp), which no time ends. (The
	// forwarder used to wait up to 3 s here, dialing the app on 25 ms tries:
	// it only delayed the forwarder, a loaded host timed those tries out
	// against an app that listened and logged a false WARN, and each start
	// opened and closed a connection to the app.) When nothing holds the
	// port until this bind (no hold: a kernel without sk_lookup, and the
	// macOS and Windows agents), a call is refused only before it. When
	// keploy holds the port from inside the app's bind (BPF sk_lookup), its
	// park serves the port until this forwarder is registered below; the
	// bind finds at once a port another listener holds already (not
	// keploy's), and the hold yields it to that listener.
	held := h.pm.ingressHeld()
	listener, err := h.bindIngress(ctx, "tcp4", origAppAddr)
	if err != nil {
		if held && neterr.IsAddrInUse(err) {
			return h.yieldIngressPort(origPort, newPort, "IPv4", err)
		}
		return err
	}
	// With the hold, the forwarder listens on IPv4 only, so its bind does
	// not find a listener that holds the port on IPv6 alone ([::1]:P, or
	// [::]:P with IPV6_V6ONLY). The hold takes the port's IPv6 calls for the
	// whole run where the app's sockets take IPv6 (an IPv6 socket on :: or on
	// an IPv6 address; not one on a v4-mapped address, which takes IPv4
	// only), so check each of those addresses as the app's own bind did:
	// without keploy, that bind would have failed there.
	if held {
		for _, addr := range h.pm.appIPv6Binds(origPort) {
			if perr := ingressIPv6Probe(addr, origPort); neterr.IsAddrInUse(perr) {
				_ = listener.Close()
				return h.yieldIngressPort(origPort, newPort, "IPv6", fmt.Errorf("bind [%s]:%d: %w", addr, origPort, perr))
			} else if perr != nil {
				logger.Debug("could not check the application's port for an IPv6 listener; holding it", zap.Uint16("port", origPort), zap.Stringer("address", addr), zap.Error(perr))
			}
		}
	}
	var listener6 net.Listener
	if !held {
		listener6 = h.listenIngressIPv6(ctx, origPort, newPort)
	}

	// If the agent is shutting down (the context was canceled as it bound),
	// release the port instead of starting the forwarder. A forwarder
	// started during teardown (e.g. from a late/stale bind event) can
	// outlive StopAll's snapshot and linger holding the app's port (commonly
	// 8000), so the next record run's application fails to bind it with
	// "address already in use" — the flaky port-8000 reuse failure. A
	// canceled context means no ingress traffic will be served anyway.
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = listener.Close()
		if listener6 != nil {
			_ = listener6.Close()
		}
		logger.Debug("Skipping ingress forwarder start; agent context canceled (shutting down)",
			zap.String("target", newAppAddr), zap.Error(ctxErr))
		return ctxErr
	}

	// Steer the moved port's connections to this forwarder, now that it
	// listens, instead of to the park (a no-op when nothing is held). The
	// park stops seeing the port the moment this returns.
	h.pm.registerIngressForwarder(origPort, listener)

	listeningOn := []string{origAppAddr}
	if listener6 != nil {
		listeningOn = append(listeningOn, listener6.Addr().String())
	}
	logger.Debug("Started Ingress forwarder", zap.Strings("listening_on", listeningOn), zap.String("forwarding_to", newAppAddr))
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	// Shared with the park's calls to this port (portSem).
	sem := h.pm.portSem(origPort)
	var loops sync.WaitGroup
	for _, l := range []net.Listener{listener, listener6} {
		if l != nil {
			loops.Add(1)
			go func() {
				defer loops.Done()
				h.acceptIngress(ctx, l, newAppAddr, sem, origPort)
			}()
		}
	}
	go func() {
		loops.Wait()
		close(done)
	}()

	h.mu.Lock()
	h.forwarders[origPort] = &tcpForwarderState{
		listener:  listener,
		listener6: listener6,
		cancel:    cancel,
		done:      done,
	}
	h.mu.Unlock()

	return nil
}

// acceptIngress is the accept loop of one of a forwarder's listeners: each
// call it accepts is forwarded to the app at newAppAddr, until ctx ends or
// the listener is closed.
func (h *goTCPIngressHook) acceptIngress(ctx context.Context, listener net.Listener, newAppAddr string, sem chan struct{}, origPort uint16) {
	logger := h.pm.logger
	tcpListener := listener.(*net.TCPListener)
	// Release the listener (and its bound port) whenever the accept loop exits,
	// even if StopIngress is never called for this forwarder (e.g. it raced with
	// shutdown and missed StopAll's snapshot). Close is idempotent — StopIngress
	// may also close it — and this runs before the forwarder's done is closed
	// so a StopIngress waiter observes the port freed. Prevents the port from
	// lingering post-exit.
	defer func() { _ = listener.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ingressAcceptTurn()
		err := tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			// StopIngress cancels ctx, then closes the listener: a stop
			// that lands between the check above and this call is a
			// stop, not a fault (an ERROR here was logged for a forwarder
			// stopped as it started).
			if ctx.Err() != nil {
				logger.Debug("Stopping ingress accept loop.", zap.Error(err))
				return
			}
			logger.Error("Failed to set deadline on ingress listener", zap.Error(err))
			return
		}
		clientConn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			logger.Debug("Stopping ingress accept loop.", zap.Error(err))
			return
		}

		go func(cc net.Conn) {
			h.pm.handleConnection(ctx, cc, newAppAddr, logger, h.pm.tcChan, sem, origPort)
		}(clientConn)
	}
}

// errIngressPortYielded is StartIngress's error for a port another listener
// (not keploy's) holds, which the hold left to it on purpose with a WARN: the
// manager does not log it as a failure.
var errIngressPortYielded = errors.New("another listener holds the application's port; keploy's hold yielded it")

// ingressAcceptTurn runs at each turn of an accept loop, after its check of
// its context; a test stands a stop there.
var ingressAcceptTurn = func() {}

// ingressIPv6Probe is StartIngress's IPv6 check of the app's port
// (probeIPv6Port); a variable so a test can make it fail as on a host
// without IPv6.
var ingressIPv6Probe = probeIPv6Port

// yieldIngressPort leaves the whole of the app's port orig to the listener
// that holds it on family (keploy's bind there failed with err): the hold
// stops taking any of the port's calls, which are then the kernel's, as
// without keploy, and no forwarder starts. One WARN names the port and the
// family.
func (h *goTCPIngressHook) yieldIngressPort(orig, newPort uint16, family string, err error) error {
	h.pm.yieldIngressPort(orig)
	h.pm.logger.Warn("another listener, not keploy's, holds the application's port, so keploy leaves the port's calls to it and records none of them; stop that listener to record this port",
		zap.Uint16("port", orig), zap.String("family", family), zap.Uint16("application_listens_on", newPort), zap.Error(err))
	return fmt.Errorf("%w: %w", errIngressPortYielded, err)
}

// appIPv6Binds is the IPv6 addresses the app's binds of orig take calls on,
// which the hold takes the port's IPv6 calls on (IngressPortHolder's
// AppIPv6Binds). When the hooks cannot say, it is :: (all of IPv6): a hold
// must never take calls from a listener keploy does not own.
func (pm *IngressProxyManager) appIPv6Binds(orig uint16) []netip.Addr {
	if pm.portHolder == nil {
		return nil
	}
	addrs, err := pm.portHolder.AppIPv6Binds(orig)
	if err != nil {
		pm.logger.Debug("could not read which IPv6 addresses the application's binds of the port take; checking all of IPv6", zap.Uint16("port", orig), zap.Error(err))
		return []netip.Addr{netip.IPv6Unspecified()}
	}
	return addrs
}

func (h *goTCPIngressHook) StopIngress(origPort uint16) error {
	h.mu.Lock()
	st, ok := h.forwarders[origPort]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("no TCP forwarder for port %d", origPort)
	}
	delete(h.forwarders, origPort)
	h.mu.Unlock()

	// Take this forwarder out of the lookup's sockhash before closing it (a
	// no-op when nothing is held; a closed socket leaves the sockhash by
	// itself, so this only tidies the entry).
	h.pm.unregisterIngressForwarder(origPort)
	st.cancel()
	_ = st.listener.Close()
	if st.listener6 != nil {
		_ = st.listener6.Close()
	}
	<-st.done
	return nil
}

// listenIngressIPv6 is, without the hold, the forwarder's listener for the
// port's IPv6 calls: at [::]:P, IPv6 only (Go sets IPV6_V6ONLY for "tcp6"),
// beside its IPv4 one at 0.0.0.0:P. With the hold there is none: the hold
// takes the port's IPv6 calls, from the application's bind on, for the
// whole run (its lookup can hand an IPv6 call only to an IPv6 socket, the
// park's). Without it (a kernel without sk_lookup, and the macOS and Windows
// agents) nothing else would take them: the application's bind on :: was
// moved off the port, so its IPv6 calls were refused for the whole run, and
// on a network with no IPv4 (a single-stack IPv6 Pod) every call is one. So
// the forwarder takes them, whichever family the application listens on:
// one that listens on IPv4 alone is reached over IPv6 too while keploy
// records.
//
// Two listeners, not one on both families: the IPv4 one is the forwarder's
// listener as it always was, so which other sockets can bind the port's
// IPv4 beside it, and which the forwarder can bind beside, are unchanged on
// every platform. A socket on both families does not keep others off the
// port's IPv4 everywhere: on Windows a later bind of 0.0.0.0:P succeeds
// beside one, and takes the port's IPv4 calls; on macOS one binds beside a
// listener at 0.0.0.0:P.
//
// When another listener holds the port's IPv6 (at [::]:P, IPv6 only; on
// Linux also at one IPv6 address), it keeps the port's IPv6 calls, as it did
// before this change; when the bind fails for another reason (no IPv6
// here), nothing takes them, as before. Either way the forwarder takes the
// port's IPv4 calls alone, as it always did, and says why at debug level:
// nothing is lost compared with before, as with the hold for an
// application bound on IPv4 alone. nil in both cases.
func (h *goTCPIngressHook) listenIngressIPv6(ctx context.Context, origPort, newPort uint16) net.Listener {
	ln, err := h.bindIngress(ctx, "tcp6", net.JoinHostPort("::", strconv.Itoa(int(origPort))))
	switch {
	case err == nil:
		return ln
	case neterr.IsAddrInUse(err):
		// On Windows the bind fails with WSAEADDRINUSE, which
		// syscall.EADDRINUSE does not match.
		h.pm.logger.Debug("another listener holds the application's port on IPv6, so keploy leaves the port's IPv6 calls to it and takes only the port's IPv4 calls",
			zap.Uint16("port", origPort), zap.String("family", "IPv6"), zap.Uint16("application_listens_on", newPort), zap.Error(err))
	default:
		h.pm.logger.Debug("keploy cannot listen on the application's port over IPv6, so it takes only the port's IPv4 calls",
			zap.Uint16("port", origPort), zap.Error(err))
	}
	return nil
}

// bindIngress binds the ingress forwarder's listener on addr: plain TCP (a Go
// listener is MPTCP by default on a kernel with MPTCP, and the lookup's
// sockhash cannot take an MPTCP socket), aborting if the context was canceled
// while it bound so a late bind does not leave the port held.
func (h *goTCPIngressHook) bindIngress(ctx context.Context, network, addr string) (net.Listener, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	listener, err := plaintcp.Listen(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("ingress proxy failed to listen on %s: %w", addr, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = listener.Close()
		return nil, ctxErr
	}
	if _, ok := listener.(*net.TCPListener); !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("listener on %s was not a TCP listener", addr)
	}
	return listener, nil
}

const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// prefaceKind is what a connection's first bytes say it carries.
type prefaceKind int

const (
	prefaceUndecided prefaceKind = iota // the bytes so far begin more than one of the kinds below
	prefaceTLS                          // a TLS handshake record: relayed to the app's TLS (relayTLS)
	prefaceH2                           // HTTP/2's client preface: gRPC (grpc.RecordIncoming)
	prefaceHTTP1                        // an HTTP/1 request line's method and its space
	prefaceOther                        // none of them: relayed to the app unread (relayUnread)
)

// undecidedPrefaceWait is how long readPreface waits for more client bytes
// once at least one client byte has arrived that is still undecided (such as
// JDWP's 14-byte "JDWP-Handshake", all HTTP token characters with no space)
// before relaying the connection unread. While no client byte has arrived, no
// timer runs (both sides silent waits without timing out).
var undecidedPrefaceWait = 250 * time.Millisecond

// serverClosedGrace is how long a server's 0-byte close waits before waking
// an idle client read, so already-buffered client bytes (such as a port probe
// followed by EOF) finish reading without racing the server's close.
var serverClosedGrace = 50 * time.Millisecond

const h2FrameHeaderLen = 9

// isH2SettingsFrame reports whether b begins with a valid 9-byte HTTP/2
// server SETTINGS frame header (RFC 9113 §4.1, §6.5): type 0x04, flags 0x00
// (not ACK), stream ID 0, and payload length a multiple of 6 within the
// default max frame size (16384).
func isH2SettingsFrame(b []byte) bool {
	if len(b) < h2FrameHeaderLen {
		return false
	}
	length := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	return length <= 16384 && length%6 == 0 &&
		b[3] == 0x04 && b[4] == 0x00 &&
		b[5]&0x7f == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// isH2SettingsPrefix reports whether b (fewer than 9 bytes) could be the
// start of an HTTP/2 server SETTINGS frame header.
func isH2SettingsPrefix(b []byte) bool {
	if len(b) == 0 || len(b) >= h2FrameHeaderLen {
		return false
	}
	if b[0] != 0x00 {
		return false
	}
	if len(b) >= 2 && b[1] > 0x40 {
		return false
	}
	if len(b) >= 3 {
		length := int(b[1])<<8 | int(b[2])
		if length > 16384 || length%6 != 0 {
			return false
		}
	}
	if len(b) >= 4 && b[3] != 0x04 {
		return false
	}
	if len(b) >= 5 && b[4] != 0x00 {
		return false
	}
	if len(b) >= 6 && b[5]&0x7f != 0 {
		return false
	}
	if len(b) >= 7 && b[6] != 0 {
		return false
	}
	if len(b) >= 8 && b[7] != 0 {
		return false
	}
	return true
}

type serverFirstOutcome int

const (
	serverNone serverFirstOutcome = iota
	serverSpoke
	serverH2Settings
	serverClosed
)

// prefaceWatch coordinates deadlines between the client preface reader
// (readPreface) and the concurrent app-first peek (peekServerFirst) so
// neither can race with the other or touch deadlines after sniffing ends.
type prefaceWatch struct {
	mu           sync.Mutex
	sniffing     bool
	stopClient   bool
	clientDoneCh chan struct{}
}

func newPrefaceWatch() *prefaceWatch {
	return &prefaceWatch{
		sniffing:     true,
		clientDoneCh: make(chan struct{}),
	}
}

func (w *prefaceWatch) setClientDeadline(conn net.Conn, d time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sniffing || w.stopClient {
		return false
	}
	_ = conn.SetReadDeadline(d)
	return true
}

func (w *prefaceWatch) wakeClient(conn net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sniffing {
		return
	}
	w.stopClient = true
	_ = conn.SetReadDeadline(time.Unix(1, 0))
}

func (w *prefaceWatch) finishClient(upConn net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.clientDoneCh:
	default:
		close(w.clientDoneCh)
	}
	if w.sniffing {
		_ = upConn.SetReadDeadline(time.Unix(1, 0))
	}
}

func (w *prefaceWatch) cancelAll(clientConn, upConn net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sniffing {
		return
	}
	w.stopClient = true
	_ = clientConn.SetReadDeadline(time.Unix(1, 0))
	_ = upConn.SetReadDeadline(time.Unix(1, 0))
}

func (w *prefaceWatch) endSniff() {
	w.mu.Lock()
	w.sniffing = false
	w.mu.Unlock()
}

// peekServerFirst reads the app's first bytes on upConn concurrently with
// readPreface. Non-HTTP/2 server-first greetings (MySQL, SMTP, SSH) wake the
// client reader at once so the connection is relayed unread; an HTTP/2
// SETTINGS frame lets the client reader wait for HTTP/2's client preface so
// gRPC recording is preserved.
func peekServerFirst(ctx context.Context, clientConn, upConn net.Conn, w *prefaceWatch) ([]byte, serverFirstOutcome) {
	buf := make([]byte, 0, 64)
	for {
		n, err := upConn.Read(buf[len(buf):cap(buf)])
		if n > 0 {
			buf = buf[:len(buf)+n]
			if isH2SettingsFrame(buf) {
				return buf, serverH2Settings
			}
			if isH2SettingsPrefix(buf) && err == nil {
				continue
			}
			w.wakeClient(clientConn)
			return buf, serverSpoke
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return buf, serverNone
			}
			if len(buf) > 0 {
				w.wakeClient(clientConn)
				return buf, serverSpoke
			}
			t := time.NewTimer(serverClosedGrace)
			select {
			case <-w.clientDoneCh:
			case <-ctx.Done():
			case <-t.C:
				w.wakeClient(clientConn)
			}
			t.Stop()
			return nil, serverClosed
		}
	}
}

// readPreface reads a connection's first bytes as they come, until they say
// what it carries (kindOf), and at most len(clientPreface). It used to wait
// for all of those 24 bytes, whatever they were. So a client whose first
// message is shorter and that waits for the answer before sending more
// waited for ever: an HTTP/1.0 request with no header (18 bytes), or a
// binary protocol's hello (PostgreSQL's 8-byte SSLRequest, to a database
// the tests started). Every kind is told well within the bytes such a
// client sends first: a TLS record by its 5-byte header, an HTTP/1 request
// by its method, a binary protocol by its first byte that no method has.
// A connection that ends first is what it read (kindOf of the end).
//
// While no client byte has arrived yet, no timer runs (both sides silent
// waits without timing out). Once at least one byte has arrived and kindOf
// is still prefaceUndecided (e.g. JDWP's "JDWP-Handshake"), undecidedPrefaceWait
// bounds the wait so the connection is relayed unread instead of deadlocking.
func readPreface(ctx context.Context, logger *zap.Logger, conn net.Conn, w *prefaceWatch) ([]byte, prefaceKind, error) {
	// Invariant: buf's capacity MUST be exactly len(clientPreface) (24 bytes).
	// A larger capacity lets a single write containing HTTP/2's 24-byte
	// preface and its first SETTINGS frame read past byte 24, so kindOf would
	// not match strings.HasPrefix(clientPreface, ...) and would misclassify
	// HTTP/2 as HTTP/1 ("PRI ").
	buf := make([]byte, 0, len(clientPreface))
	var deadline time.Time
	for {
		if len(buf) > 0 && deadline.IsZero() {
			deadline = time.Now().Add(undecidedPrefaceWait)
		}
		if !deadline.IsZero() {
			if w != nil {
				if !w.setClientDeadline(conn, deadline) {
					return buf, prefaceOther, nil
				}
			} else {
				_ = conn.SetReadDeadline(deadline)
			}
		}
		got, err := util.AppendAvailableBytes(ctx, logger, conn, buf)
		buf = got
		if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			if w == nil {
				_ = conn.SetReadDeadline(time.Time{})
			}
			if ctx.Err() != nil {
				return buf, prefaceOther, ctx.Err()
			}
			// Either the server spoke first (or closed), or the client's
			// bytes stayed undecided past undecidedPrefaceWait (JDWP):
			// relay unread without error.
			return buf, prefaceOther, nil
		}
		full := len(buf) == cap(buf)
		// At the end kindOf is never prefaceUndecided.
		if k := kindOf(buf, full || err != nil); k != prefaceUndecided {
			if !deadline.IsZero() && w == nil {
				_ = conn.SetReadDeadline(time.Time{})
			}
			return buf, k, err
		}
	}
}

// kindOf is what b, a connection's first bytes, says it carries; at the end
// (the connection's end, or all len(clientPreface) bytes read) never
// prefaceUndecided. HTTP/2's preface ("PRI * HTTP/2.0...") also begins an
// HTTP/1 request line, whose method is PRI, so it is told only by all of
// it, as before. An HTTP/1 request line begins with its method, a token
// (RFC 9110 5.6.2), and a space: a byte that is neither, or a token that
// does not end in a space within the bytes read, is not one. Such a
// connection, and one that ends before it says what it is, is relayed to
// the app unread, as without keploy (prefaceOther): read as an HTTP/1
// request, its bytes were lost, the ingress closed it, and its client
// failed (Gradle's, to the daemon it forks: "The first result from the
// daemon was empty").
func kindOf(b []byte, end bool) prefaceKind {
	if len(b) == 0 {
		if end {
			return prefaceOther
		}
		return prefaceUndecided
	}
	if b[0] == tlsHandshakeRecord {
		switch {
		case pTls.IsTLSHandshake(b):
			return prefaceTLS
		case !end && tlsRecordPrefix(b):
			return prefaceUndecided
		}
		return prefaceOther
	}
	if strings.HasPrefix(clientPreface, string(b)) {
		switch {
		case len(b) == len(clientPreface):
			return prefaceH2
		case !end:
			return prefaceUndecided
		}
	}
	method := 0
	for method < len(b) && httpguts.IsTokenRune(rune(b[method])) {
		method++
	}
	switch {
	case method == len(b) && !end:
		return prefaceUndecided
	case method > 0 && method < len(b) && b[method] == ' ':
		return prefaceHTTP1
	}
	return prefaceOther
}

// tlsHandshakeRecord is the first byte of a TLS handshake record, and so of
// every ClientHello.
const tlsHandshakeRecord = 0x16

// tlsRecordPrefix is whether b, shorter than a TLS record header, begins one
// that pTls.IsTLSHandshake takes: 0x16, then major version 3, then a minor
// version up to 3.
func tlsRecordPrefix(b []byte) bool {
	return len(b) < 5 && b[0] == tlsHandshakeRecord && (len(b) < 2 || b[1] == 0x03) && (len(b) < 3 || b[2] <= 0x03)
}

func (pm *IngressProxyManager) handleConnection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	defer clientConn.Close()
	// The connection's calls are the session's the ingress serves as it is
	// taken: its lines tag their paths under that session's key.
	ctx = pm.withLogKeys(ctx)

	// Dial the application eagerly as soon as the connection is accepted so
	// server-first protocols (MySQL, SMTP, SSH) can speak without deadlocking
	// behind a client-first preface read, and empty connections connect and
	// close at the app just as without keploy.
	finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, logger)
	upConn, err := pm.dialApp(ctx, finalAppAddr, clientConn)
	if err != nil {
		pm.logAppDialFailure(logger, "Failed to connect to the application. Verify that the application is listening on the resolved address and port.",
			err, zap.String("final_app_addr", finalAppAddr))
		return
	}
	defer func() {
		_ = upConn.Close()
	}()

	w := newPrefaceWatch()
	stopCancel := context.AfterFunc(ctx, func() {
		w.cancelAll(clientConn, upConn)
	})
	type serverRes struct {
		bytes   []byte
		outcome serverFirstOutcome
	}
	serverCh := make(chan serverRes, 1)
	go func() {
		b, out := peekServerFirst(ctx, clientConn, upConn, w)
		serverCh <- serverRes{bytes: b, outcome: out}
	}()

	preface, kind, err := readPreface(ctx, logger, clientConn, w)
	w.finishClient(upConn)
	srv := <-serverCh
	w.endSniff()
	stopCancel()
	_ = clientConn.SetReadDeadline(time.Time{})
	_ = upConn.SetReadDeadline(time.Time{})

	if len(srv.bytes) > 0 {
		upConn = newReplayConn(srv.bytes, upConn)
	}
	if srv.outcome == serverSpoke {
		pm.relayUnreadConn(clientConn, preface, upConn, finalAppAddr, logger)
		return
	}
	if err == io.EOF && len(preface) == 0 && len(srv.bytes) == 0 {
		return
	}
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "error reading initial bytes from client connection")
		return
	}
	switch kind {
	case prefaceTLS:
		pm.relayTLSConn(ctx, clientConn, preface, upConn, logger, appPort)
		return
	case prefaceOther:
		pm.relayUnreadConn(clientConn, preface, upConn, finalAppAddr, logger)
		return
	}
	if kind == prefaceH2 {
		// newAppAddr holds the port Keploy moved the application to, not the port
		// it advertises, so the test case records the caller-supplied appPort
		// (the original) instead.
		actualPort := appPort

		// Pass replayConn so RecordIncoming sees the full byte stream including
		// the preface. RecordIncoming's forwardAndTee will forward everything
		// (including the preface) to the upstream app, which is what cmux needs
		// to see the original handshake. The context names the connection
		// for the count of the calls a pause of the memory guard costs
		// (hooksUtils.SkipGRPCUnderPressure).
		grpcCtx := syncMock.WithIngressConn(ctx, ingressConnName(clientConn, finalAppAddr))
		if err := grpc.RecordIncoming(grpcCtx, logger, newReplayConn(preface, clientConn), upConn, t, actualPort, finalAppAddr, pm.synchronous, pm.mapping, pm.loadIncomingOpts); err != nil {
			utils.LogError(logger, err, "failed to record the gRPC connection")
		}
	} else {
		pm.handleHttp1WithConn(ctx, newReplayConn(preface, clientConn), upConn, finalAppAddr, logger, t, sem, appPort)
	}
}

// tlsRelayedWarning is what the ingress says of a TLS connection it relayed
// to the app unread (relayTLSConn): at WARN for a recording session's first to
// each app port, at Debug for the rest.
const tlsRelayedWarning = "a TLS connection to the app was relayed to it unread, so the app's own certificate answers its client: keploy records the app's plain HTTP and gRPC calls as test cases, and NONE of the calls on a TLS connection was recorded"

// relayTLSConn relays a connection whose client opened with a TLS ClientHello
// (preface, read from clientConn) over upConn to the app, both ways, unread:
// the app's TLS answers it, with the app's own certificate, as it would
// without keploy. Nothing on the connection is recorded: its calls are
// encrypted, so each is counted on the recording's manager, and the session's
// first to each app port is said at WARN, the rest at Debug.
func (pm *IngressProxyManager) relayTLSConn(ctx context.Context, clientConn net.Conn, preface []byte, upConn net.Conn, logger *zap.Logger, appPort uint16) {
	if syncMock.FromContextOrGlobal(ctx).NoteTLSRelayedConn(appPort) {
		logger.Warn(tlsRelayedWarning, zap.Uint16("port", appPort),
			zap.String("note", "this recording's further TLS connections to this port are logged at debug, and its summary counts them all"))
	} else {
		logger.Debug(tlsRelayedWarning, zap.Uint16("port", appPort))
	}
	util.RelayRawPassthrough(newReplayConn(preface, clientConn), upConn)
}

// relayUnreadConn relays a connection that is not HTTP, HTTP/2 or TLS
// (prefaceOther, or server-first) over upConn to the app, both ways, unread,
// as it would reach the app without keploy: nothing keploy records as a test
// case is on it.
func (pm *IngressProxyManager) relayUnreadConn(clientConn net.Conn, preface []byte, upConn net.Conn, finalAppAddr string, logger *zap.Logger) {
	logger.Debug("a connection to the app that is not HTTP, HTTP/2 or TLS was relayed to it unread: keploy records HTTP and gRPC calls as test cases, and none is on it",
		zap.String("final_app_addr", finalAppAddr))
	util.RelayRawPassthrough(newReplayConn(preface, clientConn), upConn)
}

type replayConn struct {
	net.Conn
	reader io.Reader
}

func newReplayConn(initial []byte, c net.Conn) net.Conn {
	return &replayConn{
		Conn:   c,
		reader: util.NewPrefixReader(initial, c),
	}
}

func (r *replayConn) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

// CloseWrite half-closes the connection underneath: net.Conn, embedded as an
// interface, does not promote it. The HTTP/1 ingress ends what it sends with
// it before it closes a connection whose client may still be sending
// (lingerClose).
func (r *replayConn) CloseWrite() error {
	return util.CloseWriteIfPossible(r.Conn)
}

func (pm *IngressProxyManager) ServeLive(ctx context.Context, conn net.Conn, upstream string, port uint16) {
	if !pm.requests {
		up, err := pm.dialApp(ctx, upstream, conn)
		if err != nil {
			pm.logger.Debug("the app is not reachable", zap.String("upstream", upstream), zap.Error(err))
			return
		}
		defer up.Close()
		util.RelayRawPassthrough(conn, up)
		return
	}
	pm.handleConnection(ctx, conn, upstream, pm.logger, pm.tcChan, make(chan struct{}, 1), port)
}
