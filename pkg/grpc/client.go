package grpc

import (
	"context"
	"net" // Added import for net.Conn
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a gRPC client for the KeployInstance service.
type Client struct {
	conn    *grpc.ClientConn
	client  models.KeployInstanceClient
	logger  *zap.Logger
	timeout time.Duration
}

// NewClient creates a new gRPC client.
// It attempts to connect to the server at the given address.
func NewClient(logger *zap.Logger, serverAddr string, timeout time.Duration) (*Client, error) {
	if serverAddr == "" {
		serverAddr = "localhost:8081" // Default server address
	}
	if timeout == 0 {
		timeout = 5 * time.Second // Default timeout
	}

	logger.Info("Connecting to gRPC server", zap.String("address", serverAddr))

	// Set up a connection to the server.
	conn, err := grpc.NewClient(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("Failed to connect to gRPC server", zap.String("address", serverAddr), zap.Error(err))
		return nil, err
	}

	client := models.NewKeployInstanceClient(conn)
	return &Client{
		conn:    conn,
		client:  client,
		logger:  logger,
		timeout: timeout,
	}, nil
}

// NewClientWithDialer creates a new gRPC client with a custom dialer.
// This is primarily useful for testing with in-memory listeners like bufconn.
func NewClientWithDialer(logger *zap.Logger, serverAddr string, timeout time.Duration, dialerFunc func(context.Context, string) (net.Conn, error)) (*Client, error) {
	if serverAddr == "" {
		serverAddr = "localhost:8081" // Default server address, though may not be used by dialer
	}
	if timeout == 0 {
		timeout = 5 * time.Second // Default timeout
	}

	logger.Info("Connecting to gRPC server with custom dialer", zap.String("address", serverAddr))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, serverAddr, // serverAddr here is the target string
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialerFunc), // Use the custom dialer
		grpc.WithBlock(), // Adding WithBlock to make DialContext synchronous for testing setup
	)
	if err != nil {
		logger.Error("Failed to connect to gRPC server with custom dialer", zap.String("address", serverAddr), zap.Error(err))
		return nil, err
	}

	client := models.NewKeployInstanceClient(conn)
	return &Client{
		conn:    conn,
		client:  client,
		logger:  logger,
		timeout: timeout,
	}, nil
}

// SendRecord sends a RecordRequest to the gRPC server.
func (c *Client) SendRecord(ctx context.Context, req *models.RecordRequest) (*models.RecordReply, error) {
	c.logger.Info("Sending Record request", zap.Any("request", req))

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.client.Record(ctx, req)
	if err != nil {
		c.logger.Error("Failed to send Record request", zap.Error(err))
		return nil, err
	}

	c.logger.Info("Received Record reply", zap.Any("reply", resp))
	return resp, nil
}

// SendMock sends a MockRequest to the gRPC server.
func (c *Client) SendMock(ctx context.Context, req *models.MockRequest) (*models.MockReply, error) {
	c.logger.Info("Sending Mock request", zap.Any("request", req))

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.client.Mock(ctx, req)
	if err != nil {
		c.logger.Error("Failed to send Mock request", zap.Error(err))
		return nil, err
	}

	c.logger.Info("Received Mock reply", zap.Any("reply", resp))
	return resp, nil
}

// Close closes the client's connection to the gRPC server.
func (c *Client) Close() error {
	if c.conn != nil {
		c.logger.Info("Closing gRPC client connection")
		return c.conn.Close()
	}
	return nil
}
