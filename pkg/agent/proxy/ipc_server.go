package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
)

// Message Types
const (
	MsgTypeGetDest      = 1
	MsgTypeGetDestRes   = 2
	MsgTypeData         = 3
	MsgTypeClose        = 4
	MsgTypeStartIngress = 5
	MsgTypeIngressData  = 6
	MsgTypeIngressClose = 7
	MsgTypeGetCert      = 8
	MsgTypeGetCertRes   = 9
	MsgTypeNotifyConn   = 10
	MsgTypeBatchData    = 11
)

// Stream type identifiers for the handshake protocol
const (
	StreamTypeCtrl = 1 // request/response (GetDest)
	StreamTypeData = 2 // fire-and-forget (Data/Close/IngressData/IngressClose)
	StreamTypeCmd  = 3 // Go→Rust commands (StartIngress)
)

// SimulatedConn represents a mock network connection fed by the Rust proxy via IPC
type SimulatedConn struct {
	readCh     chan []byte
	currentBuf []byte
	closed     bool
	mu         sync.Mutex
	localAddr  net.Addr
	remoteAddr net.Addr
}

func NewSimulatedConn(local, remote net.Addr) *SimulatedConn {
	return &SimulatedConn{
		readCh:     make(chan []byte, 1000), // generous buffer
		localAddr:  local,
		remoteAddr: remote,
	}
}

func (c *SimulatedConn) Push(b []byte) {
	if len(b) == 0 {
		return
	}
	buf := make([]byte, len(b))
	copy(buf, b)
	c.readCh <- buf
}

func (c *SimulatedConn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	if len(c.currentBuf) > 0 {
		n = copy(b, c.currentBuf)
		c.currentBuf = c.currentBuf[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	buf, ok := <-c.readCh
	if !ok {
		return 0, io.EOF
	}

	n = copy(b, buf)
	c.mu.Lock()
	if n < len(buf) {
		c.currentBuf = buf[n:]
	}
	c.mu.Unlock()
	return n, nil
}

func (c *SimulatedConn) Write(b []byte) (n int, err error) {
	// discard writes because the real server connection handles it
	return len(b), nil
}

func (c *SimulatedConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.readCh)
	}
	c.mu.Unlock()
	return nil
}

func (c *SimulatedConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *SimulatedConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *SimulatedConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *SimulatedConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *SimulatedConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type ConnectionPair struct {
	ClientConn *SimulatedConn
	ServerConn *SimulatedConn
}

// IPCServer handles Unix Domain Socket communication with the Rust forwarder
type IPCServer struct {
	logger      *zap.Logger
	proxy       *Proxy
	socket      string
	connections sync.Map // map[string]*ConnectionPair  (egress connections)

	// Command stream: Go → Rust (for StartIngress commands)
	cmdConn net.Conn
	cmdMu   sync.Mutex

	// Ingress connection tracking
	ingressConns sync.Map // map[string]*IngressConnection

	// Callback for ingress test case capture
	ingressDataHandler  func(connID string, direction byte, origPort uint16, data []byte)
	ingressCloseHandler func(connID string)
}

// NewIPCServer creates a new IPCServer instance
func NewIPCServer(logger *zap.Logger, proxy *Proxy) *IPCServer {
	socketPath := filepath.Join(os.TempDir(), "keploy-rust-proxy.sock")
	return &IPCServer{
		logger: logger.With(zap.String("component", "ipc-server")),
		proxy:  proxy,
		socket: socketPath,
	}
}

// Start listens for incoming IPC connections from the Rust proxy
func (s *IPCServer) Start(ctx context.Context) error {
	s.logger.Info("Starting IPC Server for Rust Proxy", zap.String("socket", s.socket))

	// Clean up existing socket if it exists
	if err := os.RemoveAll(s.socket); err != nil {
		s.logger.Error("Failed to remove existing socket", zap.Error(err))
	}

	listener, err := net.Listen("unix", s.socket)
	if err != nil {
		s.logger.Error("Failed to listen on UDS", zap.Error(err))
		return err
	}

	go func() {
		<-ctx.Done()
		listener.Close()
		os.RemoveAll(s.socket)
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Error("Failed to accept IPC connection", zap.Error(err))
			continue
		}

		// Read 1-byte stream type handshake
		typeBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, typeBuf); err != nil {
			s.logger.Error("Failed to read stream type handshake", zap.Error(err))
			conn.Close()
			continue
		}

		switch typeBuf[0] {
		case StreamTypeCtrl:
			s.logger.Info("Accepted ctrl stream from Rust proxy")
			go s.handleCtrlStream(ctx, conn)
		case StreamTypeData:
			s.logger.Info("Accepted data stream from Rust proxy")
			go s.handleDataStream(ctx, conn)
		case StreamTypeCmd:
			s.logger.Info("Accepted cmd stream from Rust proxy (Go→Rust)")
			s.cmdMu.Lock()
			s.cmdConn = conn
			s.cmdMu.Unlock()
			// No read loop — this is a write-only stream from Go's perspective
		default:
			s.logger.Warn("Unknown stream type in handshake", zap.Uint8("type", typeBuf[0]))
			conn.Close()
		}
	}
}

// handleCtrlStream processes the request/response ctrl stream (GetDest)
func (s *IPCServer) handleCtrlStream(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s.processMessages(ctx, conn, true)
}

// handleDataStream processes the fire-and-forget data stream (Data/Close/IngressData/IngressClose)
func (s *IPCServer) handleDataStream(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s.processMessages(ctx, conn, false)
}

// processMessages reads framed messages from a UDS connection and dispatches them.
// isCtrl distinguishes the ctrl stream (which needs to write responses) from the data stream.
func (s *IPCServer) processMessages(ctx context.Context, conn net.Conn, isCtrl bool) {
	for {
		// Read length prefix (4 bytes)
		var length uint32
		if err := binary.Read(conn, binary.LittleEndian, &length); err != nil {
			if err != io.EOF {
				s.logger.Error("Failed to read message length", zap.Error(err))
			}
			return
		}

		// Read message type (1 byte)
		var msgType uint8
		if err := binary.Read(conn, binary.LittleEndian, &msgType); err != nil {
			s.logger.Error("Failed to read message type", zap.Error(err))
			return
		}

		// Read payload
		payload := make([]byte, length-1) // Length includes the msgType byte
		if _, err := io.ReadFull(conn, payload); err != nil {
			if err != io.EOF {
				s.logger.Error("Failed to read message payload", zap.Error(err))
			}
			return
		}

		switch msgType {
		case MsgTypeGetDest:
			s.handleGetDest(ctx, conn, payload)
		case MsgTypeGetCert:
			s.handleGetCert(ctx, conn, payload)
		case MsgTypeData:
			s.handleData(payload)
		case MsgTypeClose:
			s.handleClose(payload)
		case MsgTypeNotifyConn:
			s.handleNotifyConn(ctx, payload)
		case MsgTypeBatchData:
			s.handleBatchData(payload)
		case MsgTypeIngressData:
			s.handleIngressData(payload)
		case MsgTypeIngressClose:
			s.handleIngressClose(payload)
		default:
			s.logger.Warn("Unknown message type received", zap.Uint8("type", msgType))
		}
	}
}

// handleGetDest handles requests to resolve original destination using eBPF maps
func (s *IPCServer) handleGetDest(ctx context.Context, conn net.Conn, payload []byte) {
	var req struct {
		SourcePort uint16 `json:"source_port"`
		ConnID     string `json:"conn_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("Failed to unmarshal GET_DEST request", zap.Error(err))
		return
	}

	destInfo, err := s.proxy.DestInfo.Get(ctx, req.SourcePort)
	if err != nil {
		s.logger.Warn("Untracked connection", zap.Uint16("source_port", req.SourcePort), zap.Error(err))
		s.sendGetDestRes(conn, false, "", 0)
		return
	}

	_ = s.proxy.DestInfo.Delete(ctx, req.SourcePort)

	// Format destination address
	var destIP string
	if destInfo.Version == 4 {
		destIP = fmt.Sprintf("%d.%d.%d.%d", byte(destInfo.IPv4Addr>>24), byte(destInfo.IPv4Addr>>16), byte(destInfo.IPv4Addr>>8), byte(destInfo.IPv4Addr))
	} else {
		destIP = "::1" // IPv6 simplified for now
	}

	s.sendGetDestRes(conn, true, destIP, uint16(destInfo.Port))

	// Setup connection pair
	clientAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(req.SourcePort)}
	serverAddr := &net.TCPAddr{IP: net.ParseIP(destIP), Port: int(destInfo.Port)}

	pair := &ConnectionPair{
		ClientConn: NewSimulatedConn(clientAddr, serverAddr),
		ServerConn: NewSimulatedConn(serverAddr, clientAddr),
	}
	s.connections.Store(req.ConnID, pair)

	// Identify protocol and route to parser
	// Checking for specific ports or applying protocol matching logic from Keploy
	// (simplified integration for mysql, postgres, etc.)
	go s.routeToParser(ctx, req.ConnID, pair, destInfo.Port)
}

// handleNotifyConn is the fire-and-forget equivalent of handleGetDest.
// Used when Rust resolves the destination directly from the pinned eBPF map.
// No response is sent back — we just set up the SimulatedConn pair and route to the parser.
func (s *IPCServer) handleNotifyConn(ctx context.Context, payload []byte) {
	var req struct {
		ConnID   string `json:"conn_id"`
		DestIP   string `json:"dest_ip"`
		DestPort uint16 `json:"dest_port"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("Failed to unmarshal NOTIFY_CONN", zap.Error(err))
		return
	}

	s.logger.Debug("NOTIFY_CONN from Rust", zap.String("conn_id", req.ConnID),
		zap.String("dest_ip", req.DestIP), zap.Uint16("dest_port", req.DestPort))

	// Setup connection pair (same as handleGetDest but no IPC response)
	clientAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	serverAddr := &net.TCPAddr{IP: net.ParseIP(req.DestIP), Port: int(req.DestPort)}

	pair := &ConnectionPair{
		ClientConn: NewSimulatedConn(clientAddr, serverAddr),
		ServerConn: NewSimulatedConn(serverAddr, clientAddr),
	}
	s.connections.Store(req.ConnID, pair)

	go s.routeToParser(ctx, req.ConnID, pair, uint32(req.DestPort))
}

func (s *IPCServer) routeToParser(ctx context.Context, connID string, pair *ConnectionPair, destPort uint32) {
	rule, ok := s.proxy.sessions.Get(0)
	if !ok {
		s.logger.Error("Failed to fetch session rule")
		return
	}

	mgr := syncMock.Get()
	mgr.SetOutputChannel(rule.MC)

	var matchedIntegration integrations.IntegrationType
	switch destPort {
	case 3306:
		matchedIntegration = integrations.MYSQL
	case 5432:
		matchedIntegration = integrations.POSTGRES_V2
	case 27017:
		matchedIntegration = integrations.MONGO_V2
	default:
		// Try generic/http
		matchedIntegration = integrations.GENERIC
	}

	if integration, exists := s.proxy.Integrations[matchedIntegration]; exists {
		parserCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		// RecordOutgoing requires an errgroup in the context to spawn its
		// internal goroutines (handshake, TeeForwardConn pipeline, etc.).
		g, parserCtx := errgroup.WithContext(parserCtx)
		parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, g)
		parserCtx = context.WithValue(parserCtx, models.ClientConnectionIDKey, connID)
		parserCtx = context.WithValue(parserCtx, models.DestConnectionIDKey, connID)

		s.logger.Debug("Routing connection to parser", zap.String("integration", string(matchedIntegration)), zap.Uint32("port", destPort))
		err := integration.RecordOutgoing(parserCtx, pair.ClientConn, pair.ServerConn, rule.MC, rule.OutgoingOptions)
		if err != nil && err != io.EOF {
			if parserCtx.Err() != nil {
				s.logger.Debug("RecordOutgoing stopped due to context cancellation", zap.String("integration", string(matchedIntegration)))
			} else {
				s.logger.Error("Error during RecordOutgoing", zap.Error(err), zap.String("integration", string(matchedIntegration)))
			}
		}

		// Wait for parser goroutines to complete cleanly.
		if err := g.Wait(); err != nil {
			s.logger.Debug("Parser errgroup finished", zap.Error(err))
		}
	} else {
		s.logger.Warn("No parser found for destination port", zap.Uint32("port", destPort))
	}
}

func (s *IPCServer) sendGetDestRes(conn net.Conn, success bool, ip string, port uint16) {
	res := struct {
		Success bool   `json:"success"`
		IP      string `json:"ip"`
		Port    uint16 `json:"port"`
	}{
		Success: success,
		IP:      ip,
		Port:    port,
	}

	resBytes, _ := json.Marshal(res)
	length := uint32(1 + len(resBytes))

	binary.Write(conn, binary.LittleEndian, length)
	binary.Write(conn, binary.LittleEndian, uint8(MsgTypeGetDestRes))
	conn.Write(resBytes)
}

// handleGetCert handles certificate generation requests from the Rust proxy.
// The Rust proxy sends this when it detects a TLS ClientHello and needs a
// per-host certificate signed by the embedded CA for MITM.
func (s *IPCServer) handleGetCert(ctx context.Context, conn net.Conn, payload []byte) {
	var req struct {
		ServerName string `json:"server_name"`
		SourcePort uint16 `json:"source_port"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("Failed to unmarshal GET_CERT request", zap.Error(err))
		s.sendGetCertRes(conn, false, nil, nil)
		return
	}

	s.logger.Debug("GET_CERT request", zap.String("server_name", req.ServerName), zap.Uint16("source_port", req.SourcePort))

	certPEM, keyPEM, err := pTls.CertForHost(s.logger, req.ServerName, time.Now())
	if err != nil {
		s.logger.Error("Failed to generate cert for host", zap.String("host", req.ServerName), zap.Error(err))
		s.sendGetCertRes(conn, false, nil, nil)
		return
	}

	s.sendGetCertRes(conn, true, certPEM, keyPEM)
}

func (s *IPCServer) sendGetCertRes(conn net.Conn, success bool, certPEM, keyPEM []byte) {
	res := struct {
		Success bool   `json:"success"`
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}{
		Success: success,
		CertPEM: string(certPEM),
		KeyPEM:  string(keyPEM),
	}

	resBytes, _ := json.Marshal(res)
	length := uint32(1 + len(resBytes))

	binary.Write(conn, binary.LittleEndian, length)
	binary.Write(conn, binary.LittleEndian, uint8(MsgTypeGetCertRes))
	conn.Write(resBytes)
}

// handleData processes raw network data forwarded by the Rust proxy
func (s *IPCServer) handleData(payload []byte) {
	// Custom binary format:
	// [conn_id_len u8] [conn_id bytes] [direction u8: 0=req, 1=res] [data bytes]
	if len(payload) < 2 {
		s.logger.Error("DATA payload too short")
		return
	}
	connIDLen := int(payload[0])
	if len(payload) < 1+connIDLen+1 {
		s.logger.Error("DATA payload invalid length")
		return
	}
	connID := string(payload[1 : 1+connIDLen])
	direction := payload[1+connIDLen] // 0 or 1
	data := payload[1+connIDLen+1:]

	v, ok := s.connections.Load(connID)
	if !ok {
		// Log softly, it might be an unhandled bypassed connection or close race condition
		return
	}
	pair := v.(*ConnectionPair)

	if direction == 0 { // request
		pair.ClientConn.Push(data)
	} else { // response
		pair.ServerConn.Push(data)
	}
}

// handleBatchData processes batched capture data sent by the Rust proxy.
// Binary format: [conn_id_len u8][conn_id bytes][chunks...]
// Each chunk: [direction u8: 0=req, 1=res][data_len u32_le][data bytes]
func (s *IPCServer) handleBatchData(payload []byte) {
	if len(payload) < 2 {
		return
	}
	connIDLen := int(payload[0])
	if len(payload) < 1+connIDLen {
		return
	}
	connID := string(payload[1 : 1+connIDLen])
	data := payload[1+connIDLen:]

	v, ok := s.connections.Load(connID)
	if !ok {
		return
	}
	pair := v.(*ConnectionPair)

	// Parse and push each chunk
	offset := 0
	for offset < len(data) {
		if offset+5 > len(data) {
			break
		}
		direction := data[offset]
		dataLen := int(binary.LittleEndian.Uint32(data[offset+1 : offset+5]))
		offset += 5
		if offset+dataLen > len(data) {
			break
		}
		chunk := data[offset : offset+dataLen]
		offset += dataLen

		if direction == 0 {
			pair.ClientConn.Push(chunk)
		} else {
			pair.ServerConn.Push(chunk)
		}
	}
}

func (s *IPCServer) handleClose(payload []byte) {
	var req struct {
		ConnID string `json:"conn_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("Failed to unmarshal CLOSE message", zap.Error(err))
		return
	}

	s.logger.Debug("Connection closed by Rust Proxy", zap.String("conn_id", req.ConnID))

	if v, ok := s.connections.Load(req.ConnID); ok {
		pair := v.(*ConnectionPair)
		pair.ClientConn.Close()
		pair.ServerConn.Close()
		s.connections.Delete(req.ConnID)
	}
}

// ---------- Ingress (incoming) IPC handling ----------

// IngressConnection tracks a single ingress connection being forwarded by Rust.
type IngressConnection struct {
	ClientConn *SimulatedConn // request data from external client
	ServerConn *SimulatedConn // response data from app
	OrigPort   uint16
}

// handleIngressData processes ingress data teed by the Rust proxy.
// Binary format: [conn_id_len u8] [conn_id bytes] [direction u8] [orig_port u16 LE] [data bytes]
func (s *IPCServer) handleIngressData(payload []byte) {
	if len(payload) < 2 {
		s.logger.Error("INGRESS_DATA payload too short")
		return
	}
	connIDLen := int(payload[0])
	if len(payload) < 1+connIDLen+1+2 {
		s.logger.Error("INGRESS_DATA payload invalid length")
		return
	}
	connID := string(payload[1 : 1+connIDLen])
	direction := payload[1+connIDLen]
	origPort := binary.LittleEndian.Uint16(payload[1+connIDLen+1 : 1+connIDLen+3])
	data := payload[1+connIDLen+3:]

	// Forward to registered handler if set
	if s.ingressDataHandler != nil {
		s.ingressDataHandler(connID, direction, origPort, data)
		return
	}

	s.logger.Warn("Received ingress data but no handler registered", zap.String("conn_id", connID))
}

// handleIngressClose processes ingress connection close notifications from Rust.
func (s *IPCServer) handleIngressClose(payload []byte) {
	var req struct {
		ConnID string `json:"conn_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("Failed to unmarshal INGRESS_CLOSE message", zap.Error(err))
		return
	}

	s.logger.Debug("Ingress connection closed by Rust Proxy", zap.String("conn_id", req.ConnID))

	if s.ingressCloseHandler != nil {
		s.ingressCloseHandler(req.ConnID)
	}
}

// SendStartIngress sends a StartIngress command to the Rust proxy via the cmd stream.
func (s *IPCServer) SendStartIngress(origPort, newPort uint16) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	if s.cmdConn == nil {
		return fmt.Errorf("cmd stream not connected yet")
	}

	cmd := struct {
		OrigPort uint16 `json:"orig_port"`
		NewPort  uint16 `json:"new_port"`
	}{
		OrigPort: origPort,
		NewPort:  newPort,
	}

	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("failed to marshal StartIngress command: %w", err)
	}

	length := uint32(1 + len(cmdBytes))
	if err := binary.Write(s.cmdConn, binary.LittleEndian, length); err != nil {
		return fmt.Errorf("failed to write StartIngress length: %w", err)
	}
	if err := binary.Write(s.cmdConn, binary.LittleEndian, uint8(MsgTypeStartIngress)); err != nil {
		return fmt.Errorf("failed to write StartIngress msg type: %w", err)
	}
	if _, err := s.cmdConn.Write(cmdBytes); err != nil {
		return fmt.Errorf("failed to write StartIngress payload: %w", err)
	}

	s.logger.Info("Sent StartIngress command to Rust proxy",
		zap.Uint16("orig_port", origPort), zap.Uint16("new_port", newPort))
	return nil
}

// SetIngressHandlers registers callbacks for ingress data/close events from the Rust proxy.
func (s *IPCServer) SetIngressHandlers(
	dataHandler func(connID string, direction byte, origPort uint16, data []byte),
	closeHandler func(connID string),
) {
	s.ingressDataHandler = dataHandler
	s.ingressCloseHandler = closeHandler
}
