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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type IngressEvent struct {
	PID     uint32
	Family  uint16
	Pub     uint16
	Backend uint16
	_       uint16 // Padding
}
type TestCaseCreator interface {
	Create(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error
	CreateGRPC(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, stream *pkg.HTTP2Stream) error
}

type ProxyDependencies struct {
	Logger    *zap.Logger
	Persister models.TestCasePersister
}

type proxyStop func() error

type IngressProxyManager struct {
	mu        sync.Mutex
	active    map[uint16]proxyStop
	logger    *zap.Logger
	deps      ProxyDependencies // Use the new dependency struct
	tcCreator TestCaseCreator   // The decoder remains the same

	tcChan       chan *models.TestCase
	ctx          context.Context
	incomingOpts models.IncomingOptions
}

func NewIngressProxyManager(ctx context.Context, logger *zap.Logger, deps ProxyDependencies, tcCreator TestCaseCreator, incomingOpts models.IncomingOptions) *IngressProxyManager {

	pm := &IngressProxyManager{
		logger:       logger,
		tcChan:       make(chan *models.TestCase, 100),
		active:       make(map[uint16]proxyStop),
		deps:         deps,
		tcCreator:    tcCreator,
		ctx:          ctx,
		incomingOpts: incomingOpts,
	}
	go pm.persistTestCases()
	return pm
}

// Ensure starts a new ingress proxy on the given public port if it's not already running.

func (pm *IngressProxyManager) Ensure(port, backend uint16) {
	pm.mu.Lock()
	_, ok := pm.active[port]
	pm.mu.Unlock()
	if ok {
		return
	}
	addrPub := "0.0.0.0:" + strconv.Itoa(int(port))
	addrBack := "127.0.0.1:" + strconv.Itoa(int(backend))
	// Start the basic TCP forwarder
	stop := runTCPForwarder(pm.logger, addrPub, addrBack, pm, pm.tcCreator, pm.incomingOpts)
	pm.mu.Lock()
	pm.active[port] = stop
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

func ListenForIngressEvents(ctx context.Context, h *hooks.Hooks, pm *IngressProxyManager) {
	rb, err := ringbuf.NewReader(h.Events)
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
			pm.logger.Info("Intercepted application bind event",
				zap.Uint32("pid", e.PID),
				zap.Uint16("public_port", e.Pub),
				zap.Uint16("backend_port", e.Backend))

			pm.Ensure(e.Pub, e.Backend)
		}
	}
}

// runTCPForwarder starts a basic proxy that forwards traffic and logs data.

func runTCPForwarder(logger *zap.Logger, listenAddr, upstreamAddr string, pm *IngressProxyManager, tcCreator TestCaseCreator, opts models.IncomingOptions) func() error {
	ln, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		logger.Error("Ingress proxy failed to listen", zap.String("addr", listenAddr), zap.Error(err))
		return func() error { return err }
	}
	tcpListener, ok := ln.(*net.TCPListener)
	if !ok {
		err := fmt.Errorf("listener was not a TCP listener, which is unexpected")
		logger.Error("Ingress proxy setup failed", zap.Error(err))
		ln.Close()
		return func() error { return err }
	}

	logger.Info("✅ Started ingress forwarder", zap.String("listening_on", listenAddr), zap.String("forwarding_to", upstreamAddr))
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
			_ = tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
			clientConn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				logger.Debug("Stopping ingress accept loop.", zap.Error(err))
				return
			}
			go handleConnection(pm.ctx, clientConn, upstreamAddr, logger, pm.tcChan, tcCreator, opts)
		}
	}()
	return func() error {
		cancel()
		_ = ln.Close()
		<-done
		return nil
	}
}

const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func handleConnection(ctx context.Context, clientConn net.Conn, upstreamAddr string, logger *zap.Logger, t chan *models.TestCase, tcCreator TestCaseCreator, opts models.IncomingOptions) {
	defer clientConn.Close()
	logger.Debug("Accepted ingress connection", zap.String("client", clientConn.RemoteAddr().String()))

	clientReader := bufio.NewReader(clientConn)
	preface, err := clientReader.Peek(len(clientPreface))
	if err != nil {
		logger.Debug("Could not peek for HTTP/2 preface, assuming HTTP/1.x", zap.Error(err))
		handleHttp1Connection(ctx, clientConn, clientReader, upstreamAddr, logger, t, tcCreator, opts)
		return
	}

	if bytes.Equal(preface, []byte(clientPreface)) {
		logger.Info("Detected HTTP/2 (gRPC) connection")
		handleHttp2ConnectionWithProxy(ctx, clientConn, upstreamAddr, logger, t, tcCreator)
	} else {
		logger.Info("Detected HTTP/1.x connection")
		handleHttp1Connection(ctx, clientConn, clientReader, upstreamAddr, logger, t, tcCreator, opts)
	}
}
func handleHttp1Connection(ctx context.Context, clientConn net.Conn, clientReader *bufio.Reader, upstreamAddr string, logger *zap.Logger, t chan *models.TestCase, tcCreator TestCaseCreator, opts models.IncomingOptions) {
	upConn, err := net.DialTimeout("tcp4", upstreamAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream backend", zap.String("backend", upstreamAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	upstreamReader := bufio.NewReader(upConn)

	for {
		reqTimestamp := time.Now()

		// 1. Read a single request from the client connection.
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			// io.EOF is the expected error when the client closes the keep-alive connection.
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
		logger.Info("Ingress Traffic Captured",
			zap.Int("request_bytes", len(reqData)),
			zap.Int("response_bytes", len(respData)),
			zap.String("request_preview", asciiPreview(reqData)),
		)

		go func() {
			err := tcCreator.Create(ctx, logger, t, reqData, respData, reqTimestamp, respTimestamp, opts)
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
				pm.deps.Logger.Error("Failed to persist captured test case", zap.Error(err))
			} else {
				pm.deps.Logger.Info("Successfully captured and persisted a test case.", zap.String("kind", string(tc.Kind)))
			}
		}
	}
}

func handleHttp2ConnectionWithProxy(ctx context.Context, clientConn net.Conn, upstreamAddr string, logger *zap.Logger, t chan *models.TestCase, tcCreator TestCaseCreator) {
	// Parse the upstream URL
	upstreamURL, err := url.Parse("http://" + upstreamAddr)
	if err != nil {
		logger.Error("Failed to parse upstream URL", zap.Error(err))
		return
	}

	// Create a reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	conn := &httpConn{
		Conn: clientConn,
		r:    bufio.NewReader(clientConn),
	}

	server := &http.Server{
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				conn.Close()
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				proxy.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Only gRPC requests are supported", http.StatusBadRequest)
		}),
	}

	go server.Serve(conn)

	select {
	case <-ctx.Done():
		server.Shutdown(context.Background())
	case <-conn.closed:
		// Connection closed naturally
	}
}

// httpConn wraps net.Conn to implement net.Listener interface
type httpConn struct {
	net.Conn
	r      *bufio.Reader
	closed chan struct{}
}

func (c *httpConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func (c *httpConn) Close() error {
	close(c.closed)
	return c.Conn.Close()
}

func (c *httpConn) Accept() (net.Conn, error) {
	if c.closed == nil {
		c.closed = make(chan struct{})
		return c, nil
	}
	<-c.closed
	return nil, io.EOF
}

func (c *httpConn) Addr() net.Addr {
	return c.Conn.LocalAddr()
}
