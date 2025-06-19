package grpc

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Server is a gRPC server that implements the KeployInstanceServer interface.
type Server struct {
	models.UnimplementedKeployInstanceServer
	logger *zap.Logger
	port   string
}

// NewServer creates a new gRPC server.
func NewServer(logger *zap.Logger, port string) *Server {
	if port == "" {
		port = "8081" // Default port
	}
	return &Server{
		logger: logger,
		port:   port,
	}
}

// Record implements the Record RPC method.
func (s *Server) Record(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error) {
	s.logger.Info("Received Record request", zap.Any("request", req))
	// TODO: Implement actual recording logic
	return &models.RecordReply{Success: true}, nil
}

// Mock implements the Mock RPC method.
func (s *Server) Mock(ctx context.Context, req *models.MockRequest) (*models.MockReply, error) {
	s.logger.Info("Received Mock request", zap.Any("request", req))
	// TODO: Implement actual mocking logic
	return &models.MockReply{Success: true}, nil
}

// Start starts the gRPC server.
func (s *Server) Start() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", s.port))
	if err != nil {
		s.logger.Error("Failed to listen on port", zap.String("port", s.port), zap.Error(err))
		return err
	}
	s.logger.Info("gRPC server listening on port", zap.String("port", s.port))

	grpcServer := grpc.NewServer()
	models.RegisterKeployInstanceServer(grpcServer, s)

	if err := grpcServer.Serve(lis); err != nil {
		s.logger.Error("Failed to start gRPC server", zap.Error(err))
		return err
	}
	return nil
}

// StartViaListener starts the gRPC server using the provided net.Listener.
// This is useful for testing with in-memory listeners like bufconn.
func (s *Server) StartViaListener(lis net.Listener) error {
	s.logger.Info("gRPC server starting with provided listener")
	grpcServer := grpc.NewServer()
	models.RegisterKeployInstanceServer(grpcServer, s)
	if err := grpcServer.Serve(lis); err != nil {
		s.logger.Error("Failed to start gRPC server with listener", zap.Error(err))
		return err
	}
	return nil
}
