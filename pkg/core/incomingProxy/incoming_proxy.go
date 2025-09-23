//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/utils"

	grpc "go.keploy.io/server/v2/pkg/core/proxy/gRPCincoming"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type IngressEvent struct {
	PID         uint32
	Family      uint16
	OrigAppPort uint16
	NewAppPort  uint16
	_           uint16 // Padding
}

type TestCaseCreator interface {
	CreateHTTP(ctx context.Context, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error
}

type proxyStop func() error

type IngressProxyManager struct {
	mu           sync.Mutex
	active       map[uint16]proxyStop
	logger       *zap.Logger
	deps         models.ProxyDependencies // Use the new dependency struct
	tcCreator    TestCaseCreator          // The decoder remains the same
	hooks        *hooks.Hooks
	tcChan       chan *models.TestCase
	ctx          context.Context
	incomingOpts models.IncomingOptions
}

func NewIngressProxyManager(ctx context.Context, logger *zap.Logger, h *hooks.Hooks, deps models.ProxyDependencies, tcCreator TestCaseCreator, incomingOpts models.IncomingOptions) *IngressProxyManager {

	pm := &IngressProxyManager{
		logger:       logger,
		hooks:        h,
		tcChan:       make(chan *models.TestCase, 100),
		active:       make(map[uint16]proxyStop),
		deps:         deps,
		tcCreator:    tcCreator,
		ctx:          ctx,
		incomingOpts: incomingOpts,
	}
	return pm
}

// Ensure starts a new ingress proxy on the given original app pory if it's not already running.

func (pm *IngressProxyManager) StartIngressProxy(origAppPort, newAppPort uint16) {
	pm.mu.Lock()
	_, ok := pm.active[origAppPort]
	pm.mu.Unlock()
	if ok {
		return
	}
	origAppAddr := "0.0.0.0:" + strconv.Itoa(int(origAppPort))
	newAppAddr := "127.0.0.1:" + strconv.Itoa(int(newAppPort))
	// Start the basic TCP forwarder
	stop := runTCPForwarder(pm.logger, origAppAddr, newAppAddr, pm, pm.tcCreator, pm.incomingOpts)
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
func (pm *IngressProxyManager) UpdateDependencies(persister models.TestCasePersister, opts models.IncomingOptions) {
	pm.deps.Persister = persister
	pm.incomingOpts = opts
}
func (pm *IngressProxyManager) ListenForIngressEvents(ctx context.Context) {
	go pm.persistTestCases()
	rb, err := ringbuf.NewReader(pm.hooks.BindEvents)
	if err != nil {
		pm.logger.Error("Failed to create ringbuf reader for ingress events", zap.Error(err))
		return
	}

	defer rb.Close()
	pm.logger.Info("Listening for application bind events to start ingress proxies...")
	for {
		select {
		case <-ctx.Done():
			pm.logger.Info("Stopping ingress event listener.")
			return

		default:
			rec, err := rb.Read()
			if err != nil {
				if errors.Is(err, ringbuf.ErrClosed) {
					return
				}
				continue
			}
			var e IngressEvent
			if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
				pm.logger.Warn("Failed to decode ingress event", zap.Error(err))
				continue
			}
			pm.logger.Debug("Intercepted application bind event",
				zap.Uint32("pid", e.PID),
				zap.Uint16("Orig_App_Port", e.OrigAppPort),
				zap.Uint16("New_App_Port", e.NewAppPort))

			pm.StartIngressProxy(e.OrigAppPort, e.NewAppPort)
		}
	}
}

// runTCPForwarder starts a basic proxy that forwards traffic and logs data.

func runTCPForwarder(logger *zap.Logger, origAppAddr, newAppAddr string, pm *IngressProxyManager, tcCreator TestCaseCreator, opts models.IncomingOptions) func() error {
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
	ctx, cancel := context.WithCancel(pm.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
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
			go handleConnection(pm.ctx, clientConn, newAppAddr, logger, pm.tcChan, tcCreator, opts)
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

func handleConnection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, tcCreator TestCaseCreator, opts models.IncomingOptions) {
	defer clientConn.Close()
	logger.Debug("Accepted ingress connection", zap.String("client", clientConn.RemoteAddr().String()))

	preface, err := util.ReadInitialBuf(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "error reading initial bytes from client connection")
		return
	}
	if bytes.HasPrefix(preface, []byte(clientPreface)) {
		logger.Debug("Detected HTTP/2 connection")
		upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
		if err != nil {
			logger.Error("Failed to connect to upstream gRPC server",
				zap.String("New_App_Address", newAppAddr),
				zap.Error(err))
			clientConn.Close() // Close the client connection as we can't proceed
			return
		}

		grpc.RecordIncoming(ctx, logger, newReplayConn(preface, clientConn), upConn, t)
	} else {
		logger.Debug("Detected HTTP/1.x connection")
		handleHttp1Connection(ctx, newReplayConn(preface, clientConn), newAppAddr, logger, t, tcCreator, opts)
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

func handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, tcCreator TestCaseCreator, opts models.IncomingOptions) {
	upConn, err := net.DialTimeout("tcp4", newAppAddr, 3*time.Second)
	clientReader := bufio.NewReader(clientConn)
	if err != nil {
		logger.Warn("Failed to dial upstream new app port", zap.String("New_App_Port", newAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	upstreamReader := bufio.NewReader(upConn)

	for {
		reqTimestamp := time.Now()

		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				logger.Warn("Failed to read client request", zap.Error(err))
			}
			return // Exit the loop and close the connection.
		}
		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			return
		}

		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			return
		}

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}
		respTimestamp := time.Now()
		defer resp.Body.Close()

		respData, err := httputil.DumpResponse(resp, true)
		if err != nil {
			logger.Error("Failed to dump response for capturing", zap.Error(err))
			return
		}

		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			return
		}
		logger.Debug("Ingress Traffic Captured",
			zap.Int("request_bytes", len(reqData)),
			zap.Int("response_bytes", len(respData)),
			zap.String("request_preview", asciiPreview(reqData)),
		)

		go func() {
			err := tcCreator.CreateHTTP(ctx, t, reqData, respData, reqTimestamp, respTimestamp, opts)
			if err != nil {
				logger.Error("Failed to create test case from captured data", zap.Error(err))
			}
		}()
	}
}

// asciiPreview is a helper to visualize the buffer data.
func asciiPreview(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 32 && c < 127 || c == '\n' || c == '\r' || c == '\t' {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}

func (pm *IngressProxyManager) persistTestCases() {
	for {
		select {
		case <-pm.ctx.Done():
			pm.logger.Info("Stopping test case persister.")
			return
		case tc := <-pm.tcChan:
			if err := pm.deps.Persister(pm.ctx, tc); err != nil {
				pm.logger.Error("Failed to persist captured test case", zap.Error(err))
			} else {
				pm.logger.Debug("Successfully captured and persisted a test case.", zap.String("kind", string(tc.Kind)))
			}
		}
	}
}
