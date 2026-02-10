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
type IngressProxyManager struct {
	mu           sync.Mutex
	active       map[uint16]proxyStop
	logger       *zap.Logger
	hooks        agent.Hooks
	tcChan       chan *models.TestCase
	incomingOpts models.IncomingOptions
	synchronous  bool
}

func New(logger *zap.Logger, h agent.Hooks, cfg *config.Config) *IngressProxyManager {
	pm := &IngressProxyManager{
		logger:      logger,
		hooks:       h,
		tcChan:      make(chan *models.TestCase, 100),
		active:      make(map[uint16]proxyStop),
		synchronous: cfg.Agent.Synchronous,
	}
	return pm
}

func (pm *IngressProxyManager) Start(ctx context.Context, opts models.IncomingOptions) chan *models.TestCase {
	pm.incomingOpts = opts
	go pm.ListenForIngressEvents(ctx)
	return pm.tcChan
}

// Ensure starts a new ingress proxy on the given original app pory if it's not already running.
func (pm *IngressProxyManager) StartIngressProxy(ctx context.Context, origAppPort, newAppPort uint16) {
	pm.mu.Lock()
	_, ok := pm.active[origAppPort]
	pm.mu.Unlock()
	if ok {
		return
	}

	// TODO : We will change this interface implementation to the IP
	origAppAddr := "0.0.0.0:" + strconv.Itoa(int(origAppPort))
	newAppAddr := "127.0.0.1:" + strconv.Itoa(int(newAppPort))
	// Start the basic TCP forwarder
	stop := pm.runTCPForwarder(ctx, pm.logger, origAppAddr, newAppAddr, origAppPort)
	pm.mu.Lock()
	pm.active[origAppPort] = stop
	pm.mu.Unlock()
}

// StopAll gracefully shuts down all active ingress proxies.
func (pm *IngressProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for p, s := range pm.active {
		if err := s(); err != nil {
			pm.logger.Warn("Failed to stop ingress proxy", zap.Uint16("port", p), zap.Error(err))

		}
		delete(pm.active, p)
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

// runTCPForwarder starts a basic proxy that forwards traffic and logs data.
func (pm *IngressProxyManager) runTCPForwarder(ctx context.Context, logger *zap.Logger, origAppAddr, newAppAddr string, appPort uint16) func() error {
	listener, err := net.Listen("tcp4", origAppAddr)
	if err != nil {
		logger.Error("Ingress proxy failed to listen", zap.String("original_addr", origAppAddr), zap.Error(err))
		return func() error { return err }
	}
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		err := fmt.Errorf("listener was not a TCP listener, which is unexpected")
		logger.Error("Ingress proxy setup failed", zap.Error(err))
		listener.Close()
		return func() error { return err }
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
				pm.handleConnection(ctx, cc, newAppAddr, logger, pm.tcChan, sem, appPort)
			}(clientConn)
		}
	}()
	return func() error {
		cancel()
		_ = listener.Close()
		<-done
		return nil
	}
}

const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func (pm *IngressProxyManager) handleConnection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	defer clientConn.Close()
	logger.Debug("Accepted ingress connection", zap.String("client", clientConn.RemoteAddr().String()))

	preface, err := util.ReadInitialBuf(ctx, logger, clientConn)
	if err != nil {
		//if not EOF then log
		if err != io.EOF {
			utils.LogError(logger, err, "error reading initial bytes from client connection")
		}
		return
	}
	if bytes.HasPrefix(preface, []byte(clientPreface)) {
		logger.Debug("Detected HTTP/2 connection")

		// Get the actual destination for gRPC on Windows
		finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, logger)

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
			logger.Error("Failed to connect to upstream gRPC server",
				zap.String("Final_App_Address", finalAppAddr),
				zap.Error(err))
			clientConn.Close() // Close the client connection as we can't proceed
			return
		}

		grpc.RecordIncoming(ctx, logger, newReplayConn(preface, clientConn), upConn, t, actualPort)
	} else {
		logger.Debug("Detected HTTP/1.x connection")
		pm.handleHttp1Connection(ctx, newReplayConn(preface, clientConn), newAppAddr, logger, t, sem, appPort)
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
