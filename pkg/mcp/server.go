// Package mcp provides an MCP server for Keploy mock recording and replay functionality.
// It exposes tools that allow AI assistants to record outgoing calls from applications
// and replay them during testing.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.uber.org/zap"
)

// Server is the Keploy MCP server that exposes mock recording and replay tools.
type Server struct {
	server       *sdkmcp.Server
	mockRecorder mockrecord.Service
	mockReplayer mockreplay.Service
	logger       *zap.Logger

	mu            sync.RWMutex
	activeSession *sdkmcp.ServerSession
}

// ServerOptions contains configuration options for the MCP server.
type ServerOptions struct {
	// Logger is the zap logger for logging.
	Logger *zap.Logger
	// MockRecorder is the service for recording mocks.
	MockRecorder mockrecord.Service
	// MockReplayer is the service for replaying mocks.
	MockReplayer mockreplay.Service
}

// NewServer creates a new Keploy MCP server.
func NewServer(opts *ServerOptions) *Server {
	if opts == nil {
		opts = &ServerOptions{}
	}

	s := &Server{
		mockRecorder: opts.MockRecorder,
		mockReplayer: opts.MockReplayer,
		logger:       opts.Logger,
	}

	if s.logger == nil {
		// Create a default logger
		zapCfg := zap.NewProductionConfig()
		zapCfg.OutputPaths = []string{"stderr"}
		logger, _ := zapCfg.Build()
		s.logger = logger
	}

	return s
}

// Run starts the MCP server and blocks until the context is cancelled or an error occurs.
func (s *Server) Run(ctx context.Context) error {
	// Create slog logger from zap
	slogHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slogLogger := slog.New(slogHandler)

	// Initialize MCP server
	s.server = sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "keploy-mock",
		Version: "v1.0.0",
	}, &sdkmcp.ServerOptions{
		Logger:       slogLogger,
		Instructions: "Keploy Mock MCP Server for recording and replaying outgoing calls from applications. Use keploy_mock_record to capture external API calls, database queries, and other outgoing requests. Use keploy_mock_test to replay recorded mocks during testing.",
		InitializedHandler: func(ctx context.Context, req *sdkmcp.InitializedRequest) {
			s.mu.Lock()
			s.activeSession = req.Session
			s.mu.Unlock()
			s.logger.Info("MCP client connected and initialized")
		},
	})

	// Register tools
	s.registerTools()

	s.logger.Info("Starting Keploy MCP server on stdio transport")

	// Run the server on stdio transport
	return s.server.Run(ctx, &sdkmcp.StdioTransport{})
}

// registerTools registers all MCP tools with the server.
func (s *Server) registerTools() {
	// Register mock record tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name:        "keploy_mock_record",
		Description: "Record outgoing calls (HTTP APIs, databases, message queues, etc.) from your application. This captures all external dependencies while running your application command, creating mock files that can be replayed during testing.",
	}, s.handleMockRecord)

	// Register mock replay/test tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name:        "keploy_mock_test",
		Description: "Replay recorded mocks while running your application. This intercepts outgoing calls and returns the recorded responses, enabling isolated testing without external dependencies.",
	}, s.handleMockReplay)

	s.logger.Info("Registered MCP tools",
		zap.Strings("tools", []string{"keploy_mock_record", "keploy_mock_test"}),
	)
}

// getActiveSession returns the currently active server session.
func (s *Server) getActiveSession() *sdkmcp.ServerSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeSession
}

// Close gracefully shuts down the server.
func (s *Server) Close() error {
	s.logger.Info("Shutting down Keploy MCP server")
	return nil
}

// String returns a string representation of the server.
func (s *Server) String() string {
	return fmt.Sprintf("KeployMCPServer{tools: [keploy_mock_record, keploy_mock_test]}")
}
