//go:build linux

package grpc

import (
	"context"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func init() {
	integrations.Register(integrations.GRPC, &integrations.Parsers{
		Initializer: NewSimplified,
		Priority:    100,
	})
}

type SimplifiedGrpc struct {
	logger *zap.Logger
	mu     sync.RWMutex
	mocks  map[string]*models.Mock
}

func NewSimplified(logger *zap.Logger) integrations.Integrations {
	return &SimplifiedGrpc{
		logger: logger,
		mocks:  make(map[string]*models.Mock),
	}
}

// MatchType determines if the outgoing network call is gRPC
func (g *SimplifiedGrpc) MatchType(ctx context.Context, reqBuf []byte) bool {
	isGrpc := len(reqBuf) >= 24 && string(reqBuf[:24]) == "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	g.logger.Debug("gRPC match check",
		zap.Bool("is_grpc", isGrpc),
		zap.Int("buffer_size", len(reqBuf)))
	return isGrpc
}

// RecordOutgoing records gRPC calls using the standard gRPC client
func (g *SimplifiedGrpc) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := g.logger.With(
		zap.String("client_conn_id", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.String("dest_conn_id", ctx.Value(models.DestConnectionIDKey).(string)),
		zap.String("client_ip", src.RemoteAddr().String()))

	logger.Info("starting gRPC recording session")

	// Create a proxy server that intercepts and records gRPC calls
	proxy := &grpcProxy{
		logger:     logger,
		clientConn: src,
		serverConn: dst,
		mocks:      mocks,
		opts:       opts,
	}

	return proxy.serve(ctx)
}

// MockOutgoing serves mocked gRPC responses
func (g *SimplifiedGrpc) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := g.logger.With(
		zap.String("client_conn_id", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.String("dest_conn_id", ctx.Value(models.DestConnectionIDKey).(string)),
		zap.String("client_ip", src.RemoteAddr().String()))

	logger.Info("starting gRPC mocking session")

	mockServer := &grpcMockServer{
		logger: logger,
		conn:   src,
		mockDb: mockDb,
		dstCfg: dstCfg,
		opts:   opts,
	}

	return mockServer.serve(ctx)
}

// grpcProxy handles recording of gRPC traffic
type grpcProxy struct {
	logger     *zap.Logger
	clientConn net.Conn
	serverConn net.Conn
	mocks      chan<- *models.Mock
	opts       models.OutgoingOptions
}

func (p *grpcProxy) serve(ctx context.Context) error {
	// Create a gRPC server that will handle client connections
	server := grpc.NewServer(
		grpc.UnknownServiceHandler(p.handleUnknownService),
		grpc.UnaryInterceptor(p.unaryInterceptor),
		grpc.StreamInterceptor(p.streamInterceptor),
	)

	// Create listener from the client connection
	listener := &singleConnListener{
		conn: p.clientConn,
		done: make(chan struct{}),
	}

	// Serve the client connection
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		if err := server.Serve(listener); err != nil {
			p.logger.Error("gRPC server error", zap.Error(err))
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		p.logger.Info("context cancelled, stopping gRPC proxy")
		server.GracefulStop()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (p *grpcProxy) handleUnknownService(srv interface{}, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		p.logger.Error("failed to get method from stream")
		return status.Error(codes.Internal, "failed to get method")
	}

	p.logger.Info("handling unknown service", zap.String("method", method))

	// Extract metadata
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		md = metadata.New(nil)
	}

	// Create client connection to actual server
	conn, err := grpc.DialContext(stream.Context(), p.serverConn.RemoteAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())
	if err != nil {
		p.logger.Error("failed to connect to server", zap.Error(err))
		return err
	}
	defer conn.Close()

	// Forward the call and record it
	return p.forwardAndRecord(stream, conn, method, md)
}

func (p *grpcProxy) forwardAndRecord(stream grpc.ServerStream, conn *grpc.ClientConn, method string, md metadata.MD) error {
	// Create context with metadata
	ctx := metadata.NewOutgoingContext(stream.Context(), md)

	// Create client stream
	clientStream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    method,
		ServerStreams: true,
		ClientStreams: true,
	}, method)
	if err != nil {
		p.logger.Error("failed to create client stream", zap.Error(err))
		return err
	}

	// Record the request/response
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "grpc-mock",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"method": method,
				"connID": stream.Context().Value(models.ClientConnectionIDKey).(string),
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

	// Handle request/response streaming
	errCh := make(chan error, 2)

	// Forward requests
	go func() {
		defer clientStream.CloseSend()
		for {
			var req anypb.Any
			if err := stream.RecvMsg(&req); err != nil {
				if err == io.EOF {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}

			// Record request
			mock.Spec.GRPCReq.Body = p.recordMessage(&req)

			if err := clientStream.SendMsg(&req); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Forward responses
	go func() {
		for {
			var resp anypb.Any
			if err := clientStream.RecvMsg(&resp); err != nil {
				if err == io.EOF {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}

			// Record response
			mock.Spec.GRPCResp.Body = p.recordMessage(&resp)
			mock.Spec.ResTimestampMock = time.Now()

			if err := stream.SendMsg(&resp); err != nil {
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

	// Send mock to channel
	p.mocks <- mock
	p.logger.Info("recorded gRPC call", zap.String("method", method))

	return nil
}

func (p *grpcProxy) recordMessage(msg proto.Message) models.GrpcLengthPrefixedMessage {
	data, err := proto.Marshal(msg)
	if err != nil {
		p.logger.Error("failed to marshal message", zap.Error(err))
		return models.GrpcLengthPrefixedMessage{}
	}

	return models.GrpcLengthPrefixedMessage{
		CompressionFlag: 0, // No compression
		MessageLength:   uint32(len(data)),
		DecodedData:     string(data),
	}
}

func (p *grpcProxy) unaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	p.logger.Debug("unary interceptor", zap.String("method", info.FullMethod))
	return handler(ctx, req)
}

func (p *grpcProxy) streamInterceptor(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	p.logger.Debug("stream interceptor", zap.String("method", info.FullMethod))
	return handler(srv, stream)
}

// grpcMockServer handles serving mocked gRPC responses
type grpcMockServer struct {
	logger *zap.Logger
	conn   net.Conn
	mockDb integrations.MockMemDb
	dstCfg *models.ConditionalDstCfg
	opts   models.OutgoingOptions
}

func (m *grpcMockServer) serve(ctx context.Context) error {
	server := grpc.NewServer(
		grpc.UnknownServiceHandler(m.handleMockService),
	)

	listener := &singleConnListener{
		conn: m.conn,
		done: make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		if err := server.Serve(listener); err != nil {
			m.logger.Error("mock gRPC server error", zap.Error(err))
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		m.logger.Info("context cancelled, stopping mock gRPC server")
		server.GracefulStop()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (m *grpcMockServer) handleMockService(srv interface{}, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		m.logger.Error("failed to get method from stream")
		return status.Error(codes.Internal, "failed to get method")
	}

	m.logger.Info("handling mock service", zap.String("method", method))

	// Extract request metadata
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		md = metadata.New(nil)
	}

	// Receive request message
	var req anypb.Any
	if err := stream.RecvMsg(&req); err != nil {
		m.logger.Error("failed to receive request", zap.Error(err))
		return err
	}

	// Create gRPC request model using existing structure
	grpcReq := models.GrpcReq{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   extractPseudoHeaders(md),
			OrdinaryHeaders: extractOrdinaryHeaders(md),
		},
		Body: m.createMessageFromProto(&req),
	}

	// Find matching mock using the function from match.go
	mock, err := m.filterMocksBasedOnGrpcRequest(stream.Context(), grpcReq)
	if err != nil {
		m.logger.Error("failed to find mock", zap.Error(err))
		return status.Error(codes.Internal, "failed to find mock")
	}

	if mock == nil {
		m.logger.Error("no mock found for request", zap.String("method", method))
		return status.Error(codes.NotFound, "no mock found")
	}

	// Send mock response
	respMsg, err := m.createProtoFromMessage(mock.Spec.GRPCResp.Body)
	if err != nil {
		m.logger.Error("failed to create response message", zap.Error(err))
		return status.Error(codes.Internal, "failed to create response")
	}

	// Set response headers
	if len(mock.Spec.GRPCResp.Headers.OrdinaryHeaders) > 0 {
		respMd := metadata.New(mock.Spec.GRPCResp.Headers.OrdinaryHeaders)
		if err := stream.SendHeader(respMd); err != nil {
			m.logger.Error("failed to send response headers", zap.Error(err))
		}
	}

	if err := stream.SendMsg(respMsg); err != nil {
		m.logger.Error("failed to send response", zap.Error(err))
		return err
	}

	// Set trailers
	if len(mock.Spec.GRPCResp.Trailers.OrdinaryHeaders) > 0 {
		trailerMd := metadata.New(mock.Spec.GRPCResp.Trailers.OrdinaryHeaders)
		stream.SetTrailer(trailerMd)
	}

	m.logger.Info("served mock response", zap.String("method", method))
	return nil
}

// filterMocksBasedOnGrpcRequest implements the mock filtering logic locally
func (m *grpcMockServer) filterMocksBasedOnGrpcRequest(ctx context.Context, grpcReq models.GrpcReq) (*models.Mock, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			mocks, err := m.mockDb.GetFilteredMocks()
			if err != nil {
				return nil, err
			}

			grpcMocks := m.filterMocksRelatedToGrpc(mocks)
			if len(grpcMocks) == 0 {
				m.logger.Debug("No grpc mocks found in the db")
				return nil, nil
			}

			m.logger.Debug("Found grpc mocks in db", zap.Int("count", len(grpcMocks)))

			schemaMatched, err := m.schemaMatch(ctx, grpcReq, grpcMocks)
			if err != nil {
				return nil, err
			}

			if len(schemaMatched) == 0 {
				m.logger.Debug("No mock found with schema match")
				return nil, nil
			}

			// Exact body match
			if ok, matchedMock := m.exactBodyMatch(grpcReq.Body, schemaMatched); ok {
				m.logger.Debug("Exact body match found")
				if !m.mockDb.DeleteFilteredMock(*matchedMock) {
					continue
				}
				return matchedMock, nil
			}

			// Fuzzy match
			if isMatched, bestMatch := m.fuzzyMatch(schemaMatched, grpcReq.Body.DecodedData); isMatched {
				m.logger.Debug("Fuzzy match found")
				if !m.mockDb.DeleteFilteredMock(*bestMatch) {
					continue
				}
				return bestMatch, nil
			}

			return nil, nil
		}
	}
}

// Helper methods for mock filtering
func (m *grpcMockServer) filterMocksRelatedToGrpc(mocks []*models.Mock) []*models.Mock {
	var res []*models.Mock
	for _, mock := range mocks {
		if mock != nil && mock.Kind == models.GRPC_EXPORT && mock.Spec.GRPCReq != nil && mock.Spec.GRPCResp != nil {
			res = append(res, mock)
		}
	}
	return res
}

func (m *grpcMockServer) schemaMatch(ctx context.Context, req models.GrpcReq, mocks []*models.Mock) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	for _, mock := range mocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		mockReq := mock.Spec.GRPCReq

		// Compare pseudo headers
		if !m.compareMap(mockReq.Headers.PseudoHeaders, req.Headers.PseudoHeaders) {
			continue
		}

		// Compare ordinary header keys
		if !m.compareMapKeys(mockReq.Headers.OrdinaryHeaders, req.Headers.OrdinaryHeaders) {
			continue
		}

		// Compare content type
		if mockReq.Headers.OrdinaryHeaders["content-type"] != req.Headers.OrdinaryHeaders["content-type"] {
			continue
		}

		// Compare compression flag
		if mockReq.Body.CompressionFlag != req.Body.CompressionFlag {
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

func (m *grpcMockServer) exactBodyMatch(body models.GrpcLengthPrefixedMessage, schemaMatched []*models.Mock) (bool, *models.Mock) {
	for _, mock := range schemaMatched {
		if mock.Spec.GRPCReq.Body.MessageLength == body.MessageLength &&
			mock.Spec.GRPCReq.Body.DecodedData == body.DecodedData {
			return true, mock
		}
	}
	return false, nil
}

func (m *grpcMockServer) fuzzyMatch(mocks []*models.Mock, reqData string) (bool, *models.Mock) {
	// Simple fuzzy matching - can be enhanced with more sophisticated algorithms
	for _, mock := range mocks {
		if strings.Contains(mock.Spec.GRPCReq.Body.DecodedData, reqData) ||
			strings.Contains(reqData, mock.Spec.GRPCReq.Body.DecodedData) {
			return true, mock
		}
	}
	return false, nil
}

func (m *grpcMockServer) compareMap(m1, m2 map[string]string) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v := range m1 {
		if v2, ok := m2[k]; !ok || v != v2 {
			return false
		}
	}
	return true
}

func (m *grpcMockServer) compareMapKeys(m1, m2 map[string]string) bool {
	if len(m1) > len(m2) {
		for k := range m2 {
			if _, ok := m1[k]; !ok {
				return false
			}
		}
	} else {
		for k := range m1 {
			if _, ok := m2[k]; !ok {
				return false
			}
		}
	}
	return true
}

func (m *grpcMockServer) createMessageFromProto(msg proto.Message) models.GrpcLengthPrefixedMessage {
	data, err := proto.Marshal(msg)
	if err != nil {
		m.logger.Error("failed to marshal proto message", zap.Error(err))
		return models.GrpcLengthPrefixedMessage{}
	}

	return models.GrpcLengthPrefixedMessage{
		CompressionFlag: 0,
		MessageLength:   uint32(len(data)),
		DecodedData:     string(data),
	}
}

func (m *grpcMockServer) createProtoFromMessage(msg models.GrpcLengthPrefixedMessage) (proto.Message, error) {
	var any anypb.Any
	if err := proto.Unmarshal([]byte(msg.DecodedData), &any); err != nil {
		m.logger.Error("failed to unmarshal message data", zap.Error(err))
		return nil, err
	}
	return &any, nil
}

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

// singleConnListener implements net.Listener for a single connection
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
		l.once.Do(func() {
			close(l.done)
		})
		return l.conn, nil
	}
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.done:
		return nil
	default:
		close(l.done)
		return l.conn.Close()
	}
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}
