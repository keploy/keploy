package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"

	"go.keploy.io/server/v3/pkg/agent"
	grpc "go.keploy.io/server/v3/pkg/agent/proxy/incoming/gRPC"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type proxyStop func() error

// IngressHook defines the interface for ingress forwarding implementations.
// Both the default Go TCP forwarder and external components (e.g. enterprise
// sockmap proxy) implement this interface.
type IngressHook interface {
	// StartIngress begins ingress forwarding for the given port pair.
	// The provided context should be used for lifetime management of the
	// forwarding goroutines.
	StartIngress(ctx context.Context, origPort, newPort uint16) error
	// StopIngress tears down the ingress forwarder for the given original port.
	StopIngress(origPort uint16) error
}

type IngressProxyManager struct {
	mu           sync.Mutex
	active       map[uint16]proxyStop
	logger       *zap.Logger
	hooks        agent.Hooks
	tcChan       chan *models.TestCase
	incomingOpts models.IncomingOptions
	synchronous  bool
	sampling     bool
	samplingSem  chan struct{}

	ingressHook IngressHook
}

func New(logger *zap.Logger, h agent.Hooks, cfg *config.Config) *IngressProxyManager {
	pm := &IngressProxyManager{
		logger:      logger,
		hooks:       h,
		tcChan:      make(chan *models.TestCase, 100),
		active:      make(map[uint16]proxyStop),
		synchronous: cfg.Agent.Synchronous,
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
	return pm
}

// SetIngressHook replaces the default Go TCP forwarder with an external
// ingress handler (e.g. enterprise sockmap proxy).
func (pm *IngressProxyManager) SetIngressHook(h IngressHook) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.ingressHook = h
}

func (pm *IngressProxyManager) Start(ctx context.Context, opts models.IncomingOptions) chan *models.TestCase {
	pm.incomingOpts = opts
	go pm.ListenForIngressEvents(ctx)
	return pm.tcChan
}

// TCChan returns the test case channel for direct use by external consumers
// (e.g., the enterprise sockmap proxy) without going through Start().
func (pm *IngressProxyManager) TCChan() chan *models.TestCase {
	return pm.tcChan
}

// StartIngressProxy starts a new ingress proxy on the given original app port if it's not already running.
// It delegates to the registered IngressHook (default: Go TCP forwarder).
func (pm *IngressProxyManager) StartIngressProxy(ctx context.Context, origAppPort, newAppPort uint16) {
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
		pm.logger.Error("Ingress hook failed to start; verify hook configuration/permissions and required kernel features, or disable the custom ingress hook",
			zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort), zap.Error(err))
		pm.mu.Lock()
		delete(pm.active, origAppPort)
		pm.mu.Unlock()
		return
	}
	started = true
	close(startDone)
	pm.logger.Info("Started ingress forwarding",
		zap.Uint16("orig_port", origAppPort), zap.Uint16("new_port", newAppPort))
}

// StopAll gracefully shuts down all active ingress proxies.
func (pm *IngressProxyManager) StopAll() {
	pm.mu.Lock()
	stops := make(map[uint16]proxyStop, len(pm.active))
	for p, s := range pm.active {
		stops[p] = s
	}
	pm.active = make(map[uint16]proxyStop)
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

	listener, err := net.Listen("tcp4", origAppAddr)
	if err != nil {
		return fmt.Errorf("ingress proxy failed to listen on %s: %w", origAppAddr, err)
	}
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		listener.Close()
		return fmt.Errorf("listener on %s was not a TCP listener", origAppAddr)
	}

	logger.Debug("Started Ingress forwarder", zap.String("listening_on", origAppAddr), zap.String("forwarding_to", newAppAddr))
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		sem := make(chan struct{}, 1)
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

func (h *goTCPIngressHook) StopIngress(origPort uint16) error {
	h.mu.Lock()
	st, ok := h.forwarders[origPort]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("no TCP forwarder for port %d", origPort)
	}
	delete(h.forwarders, origPort)
	h.mu.Unlock()

	st.cancel()
	_ = st.listener.Close()
	<-st.done
	return nil
}

const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func (pm *IngressProxyManager) handleConnection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	defer clientConn.Close()
	connLogger := logger.With(
		zap.String("client_addr", clientConn.RemoteAddr().String()),
		zap.String("proxy_addr", clientConn.LocalAddr().String()),
		zap.String("upstream_fallback_addr", newAppAddr),
		zap.Uint16("orig_app_port", appPort),
	)

	preface, err := util.ReadInitialBuf(ctx, connLogger, clientConn)
	if err != nil {
		//if not EOF then log
		if err != io.EOF {
			utils.LogError(connLogger, err, "error reading initial bytes from client connection")
		}
		return
	}
	if bytes.HasPrefix(preface, []byte(clientPreface)) {
		// Get the actual destination for gRPC on Windows
		finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, connLogger)

		// Determine the correct port for the test case:
		// On Windows, getActualDestination resolves the real destination dynamically,
		// so we extract the port from the resolved address.
		// On non-Windows (Linux/Docker), getActualDestination returns the fallback (newAppAddr)
		// which contains the eBPF-redirected port, NOT the original app port.
		// In that case, we use the passed-in appPort which carries the correct OrigAppPort.
		actualPort := appPort
		if finalAppAddr != newAppAddr {
			// Destination was dynamically resolved (Windows) — extract port from resolved address
			actualPort = extractPortFromAddr(finalAppAddr, appPort)
		}

		upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
		if err != nil {
			connLogger.Error("Failed to connect to upstream gRPC server",
				zap.String("Final_App_Address", finalAppAddr),
				zap.Error(err))
			clientConn.Close() // Close the client connection as we can't proceed
			return
		}
		grpc.RecordIncoming(ctx, connLogger, newReplayConn(preface, clientConn), upConn, t, actualPort, finalAppAddr)
	} else {
		pm.handleHttp1Connection(ctx, newReplayConn(preface, clientConn), newAppAddr, connLogger, t, sem, appPort)
	}
}

type replayConn struct {
	net.Conn
	buf *bytes.Reader
}

func newReplayConn(initial []byte, c net.Conn) net.Conn {
	return &replayConn{
		Conn: c,
		buf:  bytes.NewReader(initial),
	}
}

func (r *replayConn) Read(p []byte) (int, error) {
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	return r.Conn.Read(p)
}

// extractPortFromAddr extracts the port from an address string (host:port).
// If extraction fails, it returns the fallback port.
// This is needed because on Windows, the actual destination port is obtained
// dynamically and may differ from the originally passed appPort.
func extractPortFromAddr(addr string, fallback uint16) uint16 {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fallback
	}
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return fallback
	}
	return uint16(port64)
}
