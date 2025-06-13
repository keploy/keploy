//go:build linux

package grpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func init() {
	integrations.Register(integrations.GRPC, &integrations.Parsers{
		Initializer: NewFixed,
		Priority:    101, // Higher priority than the original
	})
}

type FixedGrpc struct {
	logger *zap.Logger
}

func NewFixed(logger *zap.Logger) integrations.Integrations {
	return &FixedGrpc{
		logger: logger,
	}
}

// MatchType determines if the outgoing network call is gRPC
func (g *FixedGrpc) MatchType(_ context.Context, reqBuf []byte) bool {
	// Check for HTTP/2 connection preface
	if len(reqBuf) >= 24 {
		preface := string(reqBuf[:24])
		isGrpc := preface == "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
		g.logger.Debug("gRPC match check",
			zap.Bool("is_grpc", isGrpc),
			zap.String("preface", preface))
		return isGrpc
	}
	return false
}

// RecordOutgoing records gRPC calls by proxying through a gRPC server
func (g *FixedGrpc) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := g.logger.With(
		zap.String("client_conn_id", getStringFromContext(ctx, models.ClientConnectionIDKey)),
		zap.String("dest_conn_id", getStringFromContext(ctx, models.DestConnectionIDKey)),
		zap.String("client_ip", src.RemoteAddr().String()))

	logger.Info("starting gRPC recording session")

	// Create recording proxy
	proxy := &recordingProxy{
		logger:     logger,
		clientConn: src,
		serverAddr: dst.RemoteAddr().String(),
		mocks:      mocks,
		opts:       opts,
	}

	return proxy.handle(ctx)
}

// MockOutgoing serves mocked gRPC responses
func (g *FixedGrpc) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := g.logger.With(
		zap.String("client_conn_id", getStringFromContext(ctx, models.ClientConnectionIDKey)),
		zap.String("dest_conn_id", getStringFromContext(ctx, models.DestConnectionIDKey)),
		zap.String("client_ip", src.RemoteAddr().String()))

	logger.Info("starting gRPC mocking session")

	// Create mock server
	mockServer := &mockingServer{
		logger: logger,
		conn:   src,
		mockDb: mockDb,
		dstCfg: dstCfg,
		opts:   opts,
	}

	return mockServer.handle(ctx)
}

// recordingProxy handles recording of gRPC traffic
type recordingProxy struct {
	logger     *zap.Logger
	clientConn net.Conn
	serverAddr string
	mocks      chan<- *models.Mock
	opts       models.OutgoingOptions
}

func (r *recordingProxy) handle(ctx context.Context) error {
	// Create a pipe to handle the connection
	clientReader, clientWriter := io.Pipe()
	serverReader, serverWriter := io.Pipe()

	// Copy data between connections and pipes
	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// Client to pipe
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer clientWriter.Close()
		if _, err := io.Copy(clientWriter, r.clientConn); err != nil && err != io.EOF {
			r.logger.Error("error copying from client", zap.Error(err))
			errCh <- err
		}
	}()

	// Pipe to client
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(r.clientConn, serverReader); err != nil && err != io.EOF {
			r.logger.Error("error copying to client", zap.Error(err))
			errCh <- err
		}
	}()

	// Create gRPC server to handle client requests
	server := grpc.NewServer(
		grpc.UnknownServiceHandler(r.handleUnknownService),
	)

	// Create listener from pipe
	listener := &pipeListener{
		reader: clientReader,
		writer: serverWriter,
		addr:   r.clientConn.LocalAddr(),
	}

	// Start server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(listener); err != nil {
			r.logger.Error("gRPC server error", zap.Error(err))
			errCh <- err
		}
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		r.logger.Info("context cancelled, stopping recording")
		server.GracefulStop()
		clientReader.Close()
		serverWriter.Close()
		wg.Wait()
		return ctx.Err()
	case err := <-errCh:
		server.GracefulStop()
		return err
	}
}

func (r *recordingProxy) handleUnknownService(srv interface{}, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "failed to get method")
	}

	r.logger.Info("recording gRPC call", zap.String("method", method))

	// Extract metadata
	md, _ := metadata.FromIncomingContext(stream.Context())

	// Connect to actual server
	conn, err := grpc.DialContext(stream.Context(), r.serverAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		r.logger.Error("failed to connect to server", zap.Error(err))
		return err
	}
	defer conn.Close()

	// Create client stream
	outCtx := metadata.NewOutgoingContext(stream.Context(), md)
	clientStream, err := conn.NewStream(outCtx, &grpc.StreamDesc{
		StreamName:    method,
		ServerStreams: true,
		ClientStreams: true,
	}, method)
	if err != nil {
		return err
	}

	// Record the interaction
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "grpc-mock",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"method": method,
				"connID": getStringFromContext(stream.Context(), models.ClientConnectionIDKey),
			},
			GRPCReq: &models.GrpcReq{
				Headers: models.GrpcHeaders{
					PseudoHeaders:   extractPseudoHeaders(md),
					OrdinaryHeaders: extractOrdinaryHeaders(md),
				},
			},
			GRPCResp: &models.GrpcResp{
				Headers: models.GrpcHeaders{
					PseudoHeaders:   make(map[string]string),
					OrdinaryHeaders: make(map[string]string),
				},
				Trailers: models.GrpcHeaders{
					PseudoHeaders:   make(map[string]string),
					OrdinaryHeaders: make(map[string]string),
				},
			},
			ReqTimestampMock: time.Now(),
		},
	}

	// Handle streaming
	return r.handleStreaming(stream, clientStream, mock)
}

func (r *recordingProxy) handleStreaming(serverStream grpc.ServerStream, clientStream grpc.ClientStream, mock *models.Mock) error {
	errCh := make(chan error, 2)

	// Forward requests
	go func() {
		defer clientStream.CloseSend()
		for {
			msg := make([]byte, 1024*1024) // 1MB buffer
			if err := serverStream.RecvMsg(&msg); err != nil {
				if err == io.EOF {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}

			// Record request
			mock.Spec.GRPCReq.Body = models.GrpcLengthPrefixedMessage{
				CompressionFlag: 0,
				MessageLength:   uint32(len(msg)),
				DecodedData:     string(msg),
			}

			if err := clientStream.SendMsg(&msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Forward responses
	go func() {
		for {
			msg := make([]byte, 1024*1024) // 1MB buffer
			if err := clientStream.RecvMsg(&msg); err != nil {
				if err == io.EOF {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}

			// Record response
			mock.Spec.GRPCResp.Body = models.GrpcLengthPrefixedMessage{
				CompressionFlag: 0,
				MessageLength:   uint32(len(msg)),
				DecodedData:     string(msg),
			}
			mock.Spec.ResTimestampMock = time.Now()

			if err := serverStream.SendMsg(&msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for completion
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			return err
		}
	}

	// Send mock
	r.mocks <- mock
	return nil
}

// mockingServer handles serving mocked responses
type mockingServer struct {
	logger *zap.Logger
	conn   net.Conn
	mockDb integrations.MockMemDb
	dstCfg *models.ConditionalDstCfg
	opts   models.OutgoingOptions
}

func (m *mockingServer) handle(ctx context.Context) error {
	// Create gRPC server for mocking
	server := grpc.NewServer(
		grpc.UnknownServiceHandler(m.handleMockService),
	)

	// Create listener from connection
	listener := &connListener{
		conn: m.conn,
		done: make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		if err := server.Serve(listener); err != nil {
			m.logger.Error("mock server error", zap.Error(err))
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		m.logger.Info("context cancelled, stopping mock server")
		server.GracefulStop()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (m *mockingServer) handleMockService(srv interface{}, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "failed to get method")
	}

	m.logger.Info("serving mock", zap.String("method", method))

	// Extract request
	md, _ := metadata.FromIncomingContext(stream.Context())

	msg := make([]byte, 1024*1024)
	if err := stream.RecvMsg(&msg); err != nil {
		return err
	}

	// Create request model
	grpcReq := models.GrpcReq{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   extractPseudoHeaders(md),
			OrdinaryHeaders: extractOrdinaryHeaders(md),
		},
		Body: models.GrpcLengthPrefixedMessage{
			CompressionFlag: 0,
			MessageLength:   uint32(len(msg)),
			DecodedData:     string(msg),
		},
	}

	// Find mock
	mock, err := m.findMock(stream.Context(), grpcReq)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if mock == nil {
		return status.Error(codes.NotFound, "no mock found")
	}

	// Send response
	respData := []byte(mock.Spec.GRPCResp.Body.DecodedData)
	return stream.SendMsg(&respData)
}

func (m *mockingServer) findMock(ctx context.Context, req models.GrpcReq) (*models.Mock, error) {
	mocks, err := m.mockDb.GetFilteredMocks()
	if err != nil {
		return nil, err
	}

	// Filter gRPC mocks
	var grpcMocks []*models.Mock
	for _, mock := range mocks {
		if mock != nil && mock.Kind == models.GRPC_EXPORT &&
			mock.Spec.GRPCReq != nil && mock.Spec.GRPCResp != nil {
			grpcMocks = append(grpcMocks, mock)
		}
	}

	if len(grpcMocks) == 0 {
		return nil, nil
	}

	// Simple matching - can be enhanced
	for _, mock := range grpcMocks {
		if m.matchRequest(req, *mock.Spec.GRPCReq) {
			if m.mockDb.DeleteFilteredMock(*mock) {
				return mock, nil
			}
		}
	}

	return nil, nil
}

func (m *mockingServer) matchRequest(req, mockReq models.GrpcReq) bool {
	// Simple content-based matching
	return req.Body.DecodedData == mockReq.Body.DecodedData
}

// Helper types
type pipeListener struct {
	reader io.Reader
	writer io.Writer
	addr   net.Addr
}

func (p *pipeListener) Accept() (net.Conn, error) {
	return &pipeConn{
		reader: p.reader,
		writer: p.writer,
		addr:   p.addr,
	}, nil
}

func (p *pipeListener) Close() error   { return nil }
func (p *pipeListener) Addr() net.Addr { return p.addr }

type pipeConn struct {
	reader io.Reader
	writer io.Writer
	addr   net.Addr
}

func (p *pipeConn) Read(b []byte) (n int, err error)   { return p.reader.Read(b) }
func (p *pipeConn) Write(b []byte) (n int, err error)  { return p.writer.Write(b) }
func (p *pipeConn) Close() error                       { return nil }
func (p *pipeConn) LocalAddr() net.Addr                { return p.addr }
func (p *pipeConn) RemoteAddr() net.Addr               { return p.addr }
func (p *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type connListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (c *connListener) Accept() (net.Conn, error) {
	select {
	case <-c.done:
		return nil, net.ErrClosed
	default:
		c.once.Do(func() { close(c.done) })
		return c.conn, nil
	}
}

func (c *connListener) Close() error {
	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
		return c.conn.Close()
	}
}

func (c *connListener) Addr() net.Addr { return c.conn.LocalAddr() }

// Helper functions
func extractPseudoHeaders(md metadata.MD) map[string]string {
	pseudo := make(map[string]string)
	for k, v := range md {
		if strings.HasPrefix(k, ":") && len(v) > 0 {
			pseudo[k] = v[0]
		}
	}
	return pseudo
}

func extractOrdinaryHeaders(md metadata.MD) map[string]string {
	ordinary := make(map[string]string)
	for k, v := range md {
		if !strings.HasPrefix(k, ":") && len(v) > 0 {
			ordinary[k] = v[0]
		}
	}
	return ordinary
}

func getStringFromContext(ctx context.Context, key interface{}) string {
	if val := ctx.Value(key); val != nil {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

// Alternative approach using HTTP/2 directly but safely
type safeGrpcHandler struct {
	logger *zap.Logger
	conn   net.Conn
	mockDb integrations.MockMemDb
}

func (s *safeGrpcHandler) handleHTTP2Connection(ctx context.Context) error {
	s.logger.Info("handling HTTP/2 connection safely")

	// Read the connection preface
	reader := bufio.NewReader(s.conn)
	preface := make([]byte, 24)
	if _, err := io.ReadFull(reader, preface); err != nil {
		return fmt.Errorf("failed to read preface: %w", err)
	}

	if string(preface) != "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" {
		return fmt.Errorf("invalid HTTP/2 preface")
	}

	// Send connection preface response
	if _, err := s.conn.Write([]byte("HTTP/2.0 200 Connection established\r\n\r\n")); err != nil {
		return fmt.Errorf("failed to send preface response: %w", err)
	}

	// Handle the rest with simple byte copying for now
	// This avoids the frame parsing issues
	return s.handleRawCopy(ctx)
}

func (s *safeGrpcHandler) handleRawCopy(ctx context.Context) error {
	// Simple approach: just echo back for mocking
	buffer := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			s.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, err := s.conn.Read(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if err == io.EOF {
					return nil
				}
				return err
			}

			// Echo back (simple mock)
			if _, err := s.conn.Write(buffer[:n]); err != nil {
				return err
			}
		}
	}
}
