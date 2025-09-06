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
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// --- Structs (must match BPF) ---
// --- Structs (must match BPF) ---
type IngressEvent struct {
	PID     uint32
	Family  uint16
	Pub     uint16
	Backend uint16
	_       uint16 // Padding
}
type TestCaseCreator interface {
	// The signature now includes the channel and other context.
	Create(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error
}

// type TestCasePersister func(ctx context.Context, testCase *models.TestCase) error

// ProxyDependencies holds everything the proxy needs to run and record.
type ProxyDependencies struct {
	Logger    *zap.Logger
	Persister models.TestCasePersister
}

// --- Ingress Proxy Manager ---

type proxyStop func() error

// IngressProxyManager manages the lifecycle of individual ingress proxies.

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

// NewIngressProxyManager creates a new manager for ingress proxies.

func NewIngressProxyManager(ctx context.Context, logger *zap.Logger, deps ProxyDependencies, tcCreator TestCaseCreator, incomingOpts models.IncomingOptions) *IngressProxyManager {

	pm := &IngressProxyManager{
		// active: make(map[uint16]proxyStop),
		logger: logger,
		// tcChan:    tcChan,
		// tcCreator: tcCreator,
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

// --- TCP Forwarder Logic ---

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
			go handleConnection(pm.ctx, clientConn, upstreamAddr, logger, pm.tcChan, pm, tcCreator, opts)
		}
	}()
	return func() error {
		cancel()
		_ = ln.Close()
		<-done
		return nil
	}
}

func handleConnection(ctx context.Context, clientConn net.Conn, upstreamAddr string, logger *zap.Logger, t chan *models.TestCase, pm *IngressProxyManager, tcCreator TestCaseCreator, opts models.IncomingOptions) {
	defer clientConn.Close()
	logger.Debug("Accepted ingress connection", zap.String("client", clientConn.RemoteAddr().String()))

	upConn, err := net.DialTimeout("tcp4", upstreamAddr, 3*time.Second)
	if err != nil {
		logger.Warn("Failed to dial upstream backend", zap.String("backend", upstreamAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	// 1. Capture Request
	// reqTimestamp := time.Now()

	// reqData, err := captureAndForward(upConn, clientConn, 500*time.Millisecond) // 500ms timeout to detect end of request
	// if err != nil {
	// 	logger.Warn("Error capturing request data", zap.Error(err))
	// 	return
	// }
	// if len(reqData) == 0 {
	// 	logger.Debug("Client connected but sent no data.")
	// 	return
	// }

	// // 2. Capture Response

	// respData, err := captureAndForward(clientConn, upConn, 500*time.Millisecond) // 500ms timeout for response
	// if err != nil {
	// 	logger.Warn("Error capturing response data", zap.Error(err))
	// 	// We still have the request, so we could potentially proceed if that's desired.
	// }
	// var reqCapture bytes.Buffer
	// var respCapture bytes.Buffer

	// // Use a WaitGroup to wait for both forwarding goroutines to finish.
	// var wg sync.WaitGroup
	// wg.Add(2)

	// // Goroutine 1: Forward data from client to upstream, capturing it along the way.
	// go func() {
	// 	defer wg.Done()
	// 	// TeeReader captures all data read from clientConn into reqCapture.
	// 	tee := io.TeeReader(clientConn, &reqCapture)
	// 	// Copy forwards the data to the upstream connection.
	// 	// This will block until the client closes the connection or an error occurs.
	// 	io.Copy(upConn, tee)
	// 	// Signal the upstream connection that we are done writing.
	// 	if tcpConn, ok := upConn.(*net.TCPConn); ok {
	// 		tcpConn.CloseWrite()
	// 	}
	// }()

	// // Goroutine 2: Forward data from upstream to client, capturing it along the way.
	// go func() {
	// 	defer wg.Done()
	// 	// TeeReader captures all data read from upConn into respCapture.
	// 	tee := io.TeeReader(upConn, &respCapture)
	// 	// Copy forwards the data to the client.
	// 	// This will block until the upstream server closes the connection or an error occurs.
	// 	io.Copy(clientConn, tee)
	// 	// Signal the client connection that we are done writing.
	// 	if tcpConn, ok := clientConn.(*net.TCPConn); ok {
	// 		tcpConn.CloseWrite()
	// 	}
	// }()
	// respTimestamp := time.Now()
	// // Wait for both directions to be closed.
	// wg.Wait()

	// reqData := reqCapture.Bytes()
	// respData := respCapture.Bytes()
	// if len(reqData) == 0 && len(respData) == 0 {
	// 	logger.Debug("Connection closed without any data exchanged.")
	// 	return
	// }
	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	// Loop to handle multiple request-response cycles on the same connection.
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

		// 2. Dump the parsed request back into a byte slice for your test case creator.
		// `httputil.DumpRequest` is perfect for this. The `false` means don't dump the body.
		// We handle the body separately to ensure it can be re-read.
		reqData, err := httputil.DumpRequest(req, true)
		if err != nil {
			logger.Error("Failed to dump request for capturing", zap.Error(err))
			return
		}

		// 3. Forward the request to the upstream server.
		if err := req.Write(upConn); err != nil {
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			return
		}

		// 4. Read the corresponding response from the upstream server.
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}
		respTimestamp := time.Now()
		defer resp.Body.Close()

		// 5. Dump the response to a byte slice.
		respData, err := httputil.DumpResponse(resp, true)
		if err != nil {
			logger.Error("Failed to dump response for capturing", zap.Error(err))
			return
		}

		// 6. Forward the response back to the client.
		if err := resp.Write(clientConn); err != nil {
			logger.Error("Failed to forward response to client", zap.Error(err))
			return
		}
		logger.Info("Ingress Traffic Captured",
			zap.Int("request_bytes", len(reqData)),
			zap.Int("response_bytes", len(respData)),
			zap.String("request_preview", asciiPreview(reqData)),
		)

		// 3. Create TestCase using the provided decoder
		// err = tcCreator.Create(ctx, logger, t, reqData, respData, reqTimestamp, respTimestamp, opts)
		// if err != nil {
		// 	logger.Error("Failed to create test case from captured data", zap.Error(err))
		// 	return
		// }
		go func() {
			err := tcCreator.Create(ctx, logger, t, reqData, respData, reqTimestamp, respTimestamp, opts)
			if err != nil {
				logger.Error("Failed to create test case from captured data", zap.Error(err))
			}
		}()
	}
}

// func captureAndForward(dst io.Writer, src net.Conn, readTimeout time.Duration) ([]byte, error) {
// 	var capturedData bytes.Buffer
// 	buf := make([]byte, 32*1024)
// 	for {
// 		// Set a deadline to detect the end of a message (e.g., end of an HTTP request)
// 		err := src.SetReadDeadline(time.Now().Add(readTimeout))
// 		if err != nil {
// 			return nil, fmt.Errorf("failed to set read deadline: %w", err)
// 		}

// 		n, err := src.Read(buf)
// 		if n > 0 {
// 			// Write captured chunk to the destination
// 			if _, werr := dst.Write(buf[:n]); werr != nil {
// 				return capturedData.Bytes(), werr
// 			}
// 			// Append captured chunk to our buffer
// 			capturedData.Write(buf[:n])
// 		}
// 		if err != nil {
// 			if ne, ok := err.(net.Error); ok && ne.Timeout() {
// 				// Timeout is expected, it means the client/server stopped sending data.
// 				break
// 			}
// 			if err == io.EOF {
// 				break
// 			}
// 			return capturedData.Bytes(), err
// 		}
// 	}
// 	// Clear the deadline for subsequent operations
// 	_ = src.SetReadDeadline(time.Time{})
// 	return capturedData.Bytes(), nil
// }

// copyAndLog copies data from src to dst and logs it.
func copyAndLog(tag string, dst io.Writer, src io.Reader, logger *zap.Logger) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// Using zap.String for structured logging
			logger.Info("Ingress Traffic",
				zap.String("direction", tag),
				zap.Int("bytes", n),
				zap.String("data", asciiPreview(buf[:n])),
			)
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			return nil
		}
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
