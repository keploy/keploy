package grpc_test

import (
	"context"
	"log"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v2/pkg/grpc"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	googleGrpc "google.golang.org/grpc" // Renamed import alias
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// const bufSize = 1024 * 1024 // Removed, as it's defined in client_test.go

var lis *bufconn.Listener

func startTestGrpcServer(logger *zap.Logger) *grpc.Server {
	lis = bufconn.Listen(bufSize)
	s := grpc.NewServer(logger, "") // Use default port, though it won't be used with bufconn
	
	go func() {
		// models.RegisterKeployInstanceServer is called within s.StartViaListener
		if err := s.StartViaListener(lis); err != nil {
			// Since this runs in a goroutine, log fatal to make test fail
			log.Fatalf("Test gRPC server failed to start: %v", err)
		}
	}()
	return s
}

// dialer is a helper function to create a gRPC client connection to the in-memory server.
// The address string is ignored by bufconn but required by the grpc.WithContextDialer interface.
func dialer(ctx context.Context, _ string) (net.Conn, error) {
	return lis.Dial()
}

func TestRecordMethod(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_ = startTestGrpcServer(logger) // Server starts in a goroutine

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := googleGrpc.DialContext(ctx, "", googleGrpc.WithContextDialer(dialer), googleGrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}
	defer conn.Close()

	client := models.NewKeployInstanceClient(conn)

	req := &models.RecordRequest{
		AppId:        "test-app",
		TestCasePath: "/path/to/tests",
		MockPath:     "/path/to/mocks",
		TestSetId:    "test-set-1",
		GrpcSpec: &models.GrpcSpec{
			Metadata: map[string]string{"header1": "value1"},
		},
	}

	reply, err := client.Record(ctx, req)
	if err != nil {
		t.Fatalf("Record RPC failed: %v", err)
	}

	if !reply.Success {
		t.Errorf("Expected RecordReply.Success to be true, got false. Error: %s", reply.ErrorMessage)
	}

	// TODO: Assert logging. This requires either injecting a test logger with log capture capabilities
	// or using a global logger hook if zap supports it for testing. For now, manual inspection of logs
	// or more advanced logger setup would be needed.
}

func TestMockMethod(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_ = startTestGrpcServer(logger) // Server starts in a goroutine, new listener for each test if run in parallel

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := googleGrpc.DialContext(ctx, "", googleGrpc.WithContextDialer(dialer), googleGrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}
	defer conn.Close()

	client := models.NewKeployInstanceClient(conn)

	req := &models.MockRequest{
		AppId:     "test-app-mock",
		MockPath:  "/path/to/mocks-for-mocking",
		TestSetId: "test-set-mock-1",
		GrpcSpec: &models.GrpcSpec{
			Metadata: map[string]string{"mockHeader": "mockValue"},
		},
	}

	reply, err := client.Mock(ctx, req)
	if err != nil {
		t.Fatalf("Mock RPC failed: %v", err)
	}

	if !reply.Success {
		t.Errorf("Expected MockReply.Success to be true, got false. Error: %s", reply.ErrorMessage)
	}

	// TODO: Assert logging, similar to TestRecordMethod.
}

// We need a way for the server to accept a listener directly for bufconn.
// Modify server.go to add StartViaListener method.
// For now, this test file assumes such a method exists or Start can be adapted.
// If Start() always creates its own net.Listen, bufconn won't work directly without server modification.

// Let's assume we add this to pkg/grpc/server.go:
/*
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
*/
// This comment will be removed once server.go is updated.
