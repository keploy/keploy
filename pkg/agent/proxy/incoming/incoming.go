package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
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
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
	waitsMu  sync.Mutex
	waits    map[string]*appWait
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
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{} // closed when the accept loop exits
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
	// port until this bind (no hold: a kernel without sk_lookup), a call is
	// refused only before it. When keploy holds the port from inside the
	// app's bind (BPF sk_lookup), its park serves the port until this
	// forwarder is registered below; the bind finds at once a port another
	// listener holds already (not keploy's), and the hold yields it to that
	// listener.
	held := h.pm.ingressHeld()
	listener, err := h.bindIngress(ctx, origAppAddr)
	if err != nil {
		if held && errors.Is(err, syscall.EADDRINUSE) {
			return h.yieldIngressPort(origPort, newPort, "IPv4", err)
		}
		return err
	}
	// The forwarder listens on IPv4 only, so its bind does not find a
	// listener that holds the port on IPv6 alone ([::1]:P, or [::]:P with
	// IPV6_V6ONLY). The hold takes the port's IPv6 calls for the whole run
	// where the app's sockets take IPv6 (an IPv6 socket on :: or on an IPv6
	// address; not one on a v4-mapped address, which takes IPv4 only), so
	// check each of those addresses as the app's own bind did: without
	// keploy, that bind would have failed there.
	if held {
		for _, addr := range h.pm.appIPv6Binds(origPort) {
			if perr := ingressIPv6Probe(addr, origPort); errors.Is(perr, syscall.EADDRINUSE) {
				_ = listener.Close()
				return h.yieldIngressPort(origPort, newPort, "IPv6", fmt.Errorf("bind [%s]:%d: %w", addr, origPort, perr))
			} else if perr != nil {
				logger.Debug("could not check the application's port for an IPv6 listener; holding it", zap.Uint16("port", origPort), zap.Stringer("address", addr), zap.Error(perr))
			}
		}
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
		logger.Debug("Skipping ingress forwarder start; agent context canceled (shutting down)",
			zap.String("target", newAppAddr), zap.Error(ctxErr))
		return ctxErr
	}
	tcpListener := listener.(*net.TCPListener)

	// Steer the moved port's connections to this forwarder, now that it
	// listens, instead of to the park (a no-op when nothing is held). The
	// park stops seeing the port the moment this returns.
	h.pm.registerIngressForwarder(origPort, listener)

	logger.Debug("Started Ingress forwarder", zap.String("listening_on", origAppAddr), zap.String("forwarding_to", newAppAddr))
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		// Release the listener (and its bound port) whenever the accept loop exits,
		// even if StopIngress is never called for this forwarder (e.g. it raced with
		// shutdown and missed StopAll's snapshot). Close is idempotent — StopIngress
		// may also close it — and this runs before close(done) so a StopIngress
		// waiter observes the port freed. Prevents the port from lingering post-exit.
		defer func() { _ = listener.Close() }()
		// Shared with the park's calls to this port (portSem).
		sem := h.pm.portSem(origPort)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			err = tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
			if err != nil {
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
	}()

	h.mu.Lock()
	h.forwarders[origPort] = &tcpForwarderState{
		listener: listener,
		cancel:   cancel,
		done:     done,
	}
	h.mu.Unlock()

	return nil
}

// errIngressPortYielded is StartIngress's error for a port another listener
// (not keploy's) holds, which the hold left to it on purpose with a WARN: the
// manager does not log it as a failure.
var errIngressPortYielded = errors.New("another listener holds the application's port; keploy's hold yielded it")

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
	<-st.done
	return nil
}

// bindIngress binds the ingress forwarder's listener on addr: plain TCP (a Go
// listener is MPTCP by default on a kernel with MPTCP, and the lookup's
// sockhash cannot take an MPTCP socket), aborting if the context was canceled
// while it bound so a late bind does not leave the port held.
func (h *goTCPIngressHook) bindIngress(ctx context.Context, addr string) (net.Listener, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	listener, err := plaintcp.Listen(ctx, "tcp4", addr)
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

func (pm *IngressProxyManager) handleConnection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	defer clientConn.Close()

	preface, err := util.ReadRequiredBytes(ctx, logger, clientConn, len(clientPreface))
	if err == io.EOF && len(preface) == 0 {
		return
	}
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "error reading initial bytes from client connection")
		return
	}
	if bytes.HasPrefix(preface, []byte(clientPreface)) {
		finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, logger)

		// newAppAddr holds the port Keploy moved the application to, not the port
		// it advertises, so the test case records the caller-supplied appPort
		// (the original) instead.
		actualPort := appPort

		upConn, err := pm.dialApp(ctx, finalAppAddr, clientConn)
		if err != nil {
			pm.logAppDialFailure(logger, "Failed to connect to upstream gRPC server. Verify that the application is listening on the resolved address and port, and that ingress redirection is configured correctly.",
				err, zap.String("final_app_addr", finalAppAddr))
			clientConn.Close() // Close the client connection as we can't proceed
			return
		}

		// Pass replayConn so RecordIncoming sees the full byte stream including
		// the preface. RecordIncoming's forwardAndTee will forward everything
		// (including the preface) to the upstream app, which is what cmux needs
		// to see the original handshake. The context names the connection
		// for the count of the calls a pause of the memory guard costs
		// (hooksUtils.SkipGRPCUnderPressure).
		grpcCtx := syncMock.WithIngressConn(ctx, ingressConnName(clientConn, finalAppAddr))
		grpc.RecordIncoming(grpcCtx, logger, newReplayConn(preface, clientConn), upConn, t, actualPort, finalAppAddr, pm.synchronous, pm.mapping)
	} else {
		pm.handleHttp1Connection(ctx, newReplayConn(preface, clientConn), newAppAddr, logger, t, sem, appPort)
	}
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
