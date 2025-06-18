//go:build linux

package grpc

import (
	"context"
	"io"
	"math"
	"net"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockOutgoing starts a gRPC server to mock responses for an incoming connection.
func mockOutgoing(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mockDb integrations.MockMemDb) error {
	// Always close the socket when we return.
	defer func() {
		if err := clientConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error("failed to close client connection in mock mode", zap.Error(err))
		}
	}()

	mockServer := &grpcMockServer{
		logger: logger,
		mockDb: mockDb,
	}

	// Create a gRPC server that uses our raw codec and unknown service handler.
	srv := grpc.NewServer(
		grpc.UnknownServiceHandler(mockServer.handler),
		grpc.ForceServerCodec(new(rawCodec)),
	)

	// Use a single-connection listener to serve the mock on the given connection.
	lis := newSingleConnListener(clientConn)
	logger.Info("starting mock gRPC server")
	// Run Serve in its own goroutine so we can stop it when the context is done.
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(lis) }()

	select {
	case <-ctx.Done():
		// Parent context cancelled (Ctrl-C, timeout, etc.).
		go srv.GracefulStop()
		<-srvErr // wait for Serve to return
		return ctx.Err()

	case err := <-srvErr:
		// Serve returned on its own (connection closed, reset, etc.).
		switch {
		case err == nil,
			err == io.EOF,
			strings.Contains(err.Error(), "use of closed network connection"):
			logger.Debug("client connection closed (EOF)")
			return nil
		case strings.Contains(err.Error(), "connection reset by peer"):
			logger.Warn("client connection was reset by peer")
			return nil
		default:
			logger.Error("mock gRPC server failed", zap.Error(err))
			return err
		}
	}
}

// grpcMockServer implements the gRPC unknown service handler to mock responses.
type grpcMockServer struct {
	logger *zap.Logger
	mockDb integrations.MockMemDb
}

func (s *grpcMockServer) handler(_ interface{}, stream grpc.ServerStream) error {
	// 1. Extract request details
	fullMethod, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		s.logger.Error("failed to get method from stream")
		return status.Errorf(codes.Internal, "failed to get method from stream")
	}

	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		s.logger.Warn("failed to get metadata from context")
	}

	s.logger.Info("received gRPC request to mock", zap.String("method", fullMethod), zap.Any("metadata", md))

	// Read the request body.
	var requestBody []byte
	reqMsg := new(rawMessage)
	if err := stream.RecvMsg(reqMsg); err != nil && err != io.EOF {
		s.logger.Error("failed to receive request message from stream", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to receive request message: %v", err)
	}
	requestBody = reqMsg.data
	s.logger.Debug("fully received request body", zap.Int("size", len(requestBody)))

	grpcReq := &models.GrpcReq{
		Headers: s.grpcMetadataToHeaders(md, fullMethod),
		Body:    createLengthPrefixedMessage(requestBody),
	}

	// 2. Find a matching mock
	s.logger.Debug("finding mock for gRPC request", zap.Any("request", grpcReq))
	mock, err := FilterMocksBasedOnGrpcRequest(stream.Context(), s.logger, *grpcReq, s.mockDb)
	if err != nil {
		s.logger.Error("failed to find mock", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to find mock: %v", err)
	}
	if mock == nil {
		s.logger.Error("no matching gRPC mock found", zap.String("method", fullMethod))
		return status.Errorf(codes.NotFound, "no matching keploy mock found for %s", fullMethod)
	}

	s.logger.Info("found matching mock", zap.String("mock.name", mock.Name), zap.String("mock.kind", string(mock.Kind)))

	// 3. Send the mocked response
	grpcResp := mock.Spec.GRPCResp

	// --- Determine final gRPC status --------------------------------------
	scStr := grpcResp.Trailers.OrdinaryHeaders["grpc-status"]
	scInt, err := strconv.Atoi(scStr) // bad/missing ⇒ 0 (codes.OK)
	var finalCode codes.Code
	if err != nil || scInt < 0 || scInt > math.MaxUint32 {
		s.logger.Warn("invalid grpc-status value, defaulting to codes.OK", zap.String("grpc-status", scStr))
		finalCode = codes.OK
	} else {
		finalCode = codes.Code(uint32(scInt)) // 0 ⇒ OK
	}
	finalMsg := grpcResp.Trailers.OrdinaryHeaders["grpc-message"]
	// Send headers
	respMd := s.headersToGrpcMetadata(grpcResp.Headers)
	if err := stream.SendHeader(respMd); err != nil {
		s.logger.Error("failed to send response headers", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to send headers: %v", err)
	}

	// Send body **only when grpc-status == OK**
	if finalCode == codes.OK {
		respBody, err := createPayloadFromLengthPrefixedMessage(grpcResp.Body)
		if err != nil {
			s.logger.Error("failed to create payload from length-prefixed message", zap.Error(err))
			return status.Errorf(codes.Internal, "failed to create response payload: %v", err)
		}
		if err := stream.SendMsg(&rawMessage{data: respBody}); err != nil {
			s.logger.Error("failed to send response message", zap.Error(err))
			return status.Errorf(codes.Internal, "failed to send response message: %v", err)
		}
		s.logger.Debug("sent mocked response body", zap.Int("size", len(respBody)))
	}

	// For non-OK results return a status.Error – this makes the runtime
	// write the required grpc-status / grpc-message trailers for us.
	if finalCode != codes.OK {
		return status.Error(finalCode, finalMsg)
	}

	// Success path – attach any extra trailers and finish.
	trailerMd := s.headersToGrpcMetadata(grpcResp.Trailers)
	stream.SetTrailer(trailerMd)
	s.logger.Debug("sent mocked response trailers", zap.Any("trailers", trailerMd))
	return nil
}

// grpcMetadataToHeaders converts gRPC metadata to Keploy's header format.
func (s *grpcMockServer) grpcMetadataToHeaders(md metadata.MD, fullMethod string) models.GrpcHeaders {
	hdr := models.GrpcHeaders{
		PseudoHeaders:   make(map[string]string),
		OrdinaryHeaders: make(map[string]string),
	}
	for k, v := range md {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			hdr.PseudoHeaders[k] = val
		} else {
			hdr.OrdinaryHeaders[k] = val
		}
	}
	// Stabilise the header set so it matches what was recorded
	hdr.OrdinaryHeaders["te"] = "trailers"

	// The grpc server framework consumes pseudo-headers, so we must add them back.
	if method, ok := hdr.PseudoHeaders[":method"]; !ok || method == "" {
		hdr.PseudoHeaders[":method"] = "POST"
	}
	if scheme, ok := hdr.PseudoHeaders[":scheme"]; !ok || scheme == "" {
		hdr.PseudoHeaders[":scheme"] = "http"
	}
	if path, ok := hdr.PseudoHeaders[":path"]; !ok || path == "" {
		hdr.PseudoHeaders[":path"] = fullMethod
	}
	return hdr
}

// headersToGrpcMetadata converts Keploy's header format to gRPC metadata.
func (s *grpcMockServer) headersToGrpcMetadata(headers models.GrpcHeaders) metadata.MD {
	md := metadata.New(nil)
	for k, v := range headers.PseudoHeaders {
		md.Set(k, v)
	}
	for k, v := range headers.OrdinaryHeaders {
		md.Set(k, v)
	}
	return md
}
