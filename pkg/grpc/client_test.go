package grpc_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v2/pkg/grpc"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap/zaptest"
	googleGrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

// MockKeployInstanceServer is a mock implementation of the KeployInstanceServer.
type MockKeployInstanceServer struct {
	models.UnimplementedKeployInstanceServer
	RecordFunc func(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error)
	MockFunc   func(ctx context.Context, req *models.MockRequest) (*models.MockReply, error)
}

func (m *MockKeployInstanceServer) Record(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error) {
	if m.RecordFunc != nil {
		return m.RecordFunc(ctx, req)
	}
	return &models.RecordReply{Success: true}, nil
}

func (m *MockKeployInstanceServer) Mock(ctx context.Context, req *models.MockRequest) (*models.MockReply, error) {
	if m.MockFunc != nil {
		return m.MockFunc(ctx, req)
	}
	return &models.MockReply{Success: true}, nil
}

func startTestGrpcClientServer(t *testing.T, mockServer *MockKeployInstanceServer) (*bufconn.Listener, func()) {
	listener := bufconn.Listen(bufSize)
	s := googleGrpc.NewServer()
	models.RegisterKeployInstanceServer(s, mockServer)

	go func() {
		if err := s.Serve(listener); err != nil {
			t.Logf("Mock googleGrpc server failed to start: %v", err)
		}
	}()

	stop := func() {
		s.Stop()
		listener.Close()
	}
	return listener, stop
}

// clientDialer is a helper for client tests to dial via bufconn.
func clientDialer(listener *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return listener.Dial()
	}
}

func TestNewClient_Success(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockServer := &MockKeployInstanceServer{}
	listener, stop := startTestGrpcClientServer(t, mockServer)
	defer stop()

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 5*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}
	if client == nil {
		t.Fatal("Expected client to be non-nil")
	}
	defer client.Close()
}

func TestNewClient_Failure(t *testing.T) {
	logger := zaptest.NewLogger(t)
	// Attempt to connect to a port that is very unlikely to be open.
	// Using localhost:1 to hopefully get a quick "connection refused".
	client, err := grpc.NewClient(logger, "localhost:1", 200*time.Millisecond) // Increased timeout slightly
	if err == nil {
		// If NewClient itself doesn't error, the first RPC call should.
		// This can happen if DialContext returns before the connection fully fails,
		// even with WithBlock, under certain conditions.
		if client != nil { // Should not be nil if err is nil
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond) // Short timeout for RPC
			defer cancel()
			_, rpcErr := client.SendRecord(ctx, &models.RecordRequest{AppId: "probe"})
			if rpcErr == nil {
				t.Fatal("NewClient succeeded and subsequent RPC also succeeded for an invalid address")
			} else {
				t.Logf("NewClient returned no error, but subsequent RPC failed as expected: %v", rpcErr)
			}
		} else {
			// This case should ideally not be reached if err is nil.
			t.Fatal("NewClient returned no error but client is nil, which is unexpected")
		}
	} else {
		t.Logf("NewClient failed as expected: %v", err)
	}
}

func TestClient_SendRecord_Success(t *testing.T) {
	logger := zaptest.NewLogger(t)
	expectedReply := &models.RecordReply{Success: true, ErrorMessage: "recorded"}
	mockServer := &MockKeployInstanceServer{
		RecordFunc: func(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error) {
			return expectedReply, nil
		},
	}
	listener, stop := startTestGrpcClientServer(t, mockServer)
	defer stop()

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 5*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}
	defer client.Close()

	req := &models.RecordRequest{AppId: "test-app"}
	reply, err := client.SendRecord(context.Background(), req)
	if err != nil {
		t.Fatalf("SendRecord failed: %v", err)
	}
	if !reply.Success || reply.ErrorMessage != expectedReply.ErrorMessage {
		t.Errorf("SendRecord returned %+v, expected %+v", reply, expectedReply)
	}
}

func TestClient_SendRecord_Error(t *testing.T) {
	logger := zaptest.NewLogger(t)
	expectedError := status.Error(codes.Unavailable, "server unavailable")
	mockServer := &MockKeployInstanceServer{
		RecordFunc: func(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error) {
			return nil, expectedError
		},
	}
	listener, stop := startTestGrpcClientServer(t, mockServer)
	defer stop()

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 5*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}
	defer client.Close()

	req := &models.RecordRequest{AppId: "test-app-err"}
	_, err = client.SendRecord(context.Background(), req) // err is already declared by NewClientWithDialer
	if err == nil {
		t.Fatal("SendRecord was expected to fail but succeeded")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable || st.Message() != "server unavailable" {
		t.Errorf("SendRecord returned error %v, expected %v", err, expectedError)
	}
}

func TestClient_SendMock_Success(t *testing.T) {
	logger := zaptest.NewLogger(t)
	expectedReply := &models.MockReply{Success: true, ErrorMessage: "mocked"}
	mockServer := &MockKeployInstanceServer{
		MockFunc: func(ctx context.Context, req *models.MockRequest) (*models.MockReply, error) {
			return expectedReply, nil
		},
	}
	listener, stop := startTestGrpcClientServer(t, mockServer)
	defer stop()

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 5*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}
	defer client.Close()

	req := &models.MockRequest{AppId: "test-app-mock-succ"}
	reply, err := client.SendMock(context.Background(), req)
	if err != nil {
		t.Fatalf("SendMock failed: %v", err)
	}
	if !reply.Success || reply.ErrorMessage != expectedReply.ErrorMessage {
		t.Errorf("SendMock returned %+v, expected %+v", reply, expectedReply)
	}
}

func TestClient_SendMock_Error(t *testing.T) {
	logger := zaptest.NewLogger(t)
	expectedError := status.Error(codes.Internal, "internal mock error")
	mockServer := &MockKeployInstanceServer{
		MockFunc: func(ctx context.Context, req *models.MockRequest) (*models.MockReply, error) {
			return nil, expectedError
		},
	}
	listener, stop := startTestGrpcClientServer(t, mockServer)
	defer stop()

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 5*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}
	defer client.Close()

	req := &models.MockRequest{AppId: "test-app-mock-err"}
	_, err = client.SendMock(context.Background(), req) // err is already declared
	if err == nil {
		t.Fatal("SendMock was expected to fail but succeeded")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal || st.Message() != "internal mock error" {
		t.Errorf("SendMock returned error %v, expected %v", err, expectedError)
	}
}

func TestClient_Close(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockServer := &MockKeployInstanceServer{}
	listener, stop := startTestGrpcClientServer(t, mockServer)

	client, err := grpc.NewClientWithDialer(logger, "passthrough:///bufnet_test_server", 1*time.Second, clientDialer(listener))
	if err != nil {
		t.Fatalf("NewClientWithDialer failed: %v", err)
	}

	err = client.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// After closing, operations should ideally fail.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.SendRecord(ctx, &models.RecordRequest{}) 
	if err == nil {
		t.Error("SendRecord succeeded on a closed client, expected error")
	} else {
		st, ok := status.FromError(err)
		if !ok {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "transport is closing") && !strings.Contains(err.Error(), "connection closed") {
				t.Logf("Received non-gRPC error after close: %v. This might be okay if it indicates closure.", err)
			}
		} else if st.Code() != codes.Canceled && st.Code() != codes.Unavailable && st.Code() != codes.DeadlineExceeded {
			t.Errorf("Expected context/gRPC error indicating closed connection, but got: %v code: %v", err, st.Code())
		}
	}
	stop() 
}
