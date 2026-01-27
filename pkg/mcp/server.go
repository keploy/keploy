// Package mcp provides an MCP server for Keploy mock recording and replay functionality.
// It exposes tools that allow AI assistants to record outgoing calls from applications
// and replay them during testing.
//
// IMPORTANT: MCP Protocol Rules for JSON-RPC 2.0 compliance:
// 1. Stdio Transport - JSON-RPC Only: Every line sent to stdout must be valid JSON-RPC
// 2. Logging Must Go to Stderr: All logs (even with emojis, colors, banners) must go to stderr
// 3. No ANSI Codes in Output: Color codes corrupt JSON and break the protocol
package mcp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Server is the Keploy MCP server that exposes mock recording and replay tools.
type Server struct {
	server       *sdkmcp.Server
	mockRecorder mockrecord.Service
	mockReplayer mockreplay.Service
	logger       *zap.Logger

	// stdout is the writer for MCP JSON-RPC output. This is captured from os.Stdout
	// before any other code runs, to prevent other code from corrupting the stream.
	stdout io.Writer

	mu            sync.RWMutex
	activeSession *sdkmcp.ServerSession
}

// ServerOptions contains configuration options for the MCP server.
type ServerOptions struct {
	// Logger is the zap logger for logging. MUST output to stderr only with no ANSI codes.
	Logger *zap.Logger
	// MockRecorder is the service for recording mocks.
	MockRecorder mockrecord.Service
	// MockReplayer is the service for replaying mocks.
	MockReplayer mockreplay.Service
	// Stdout is the writer for MCP JSON-RPC output. If nil, uses os.Stdout.
	// IMPORTANT: This should be the original os.Stdout captured before any
	// stdout redirection, to ensure the MCP protocol can communicate properly.
	Stdout io.Writer
}

// newMCPLogger creates a logger suitable for MCP server use.
// It outputs to stderr only, with no ANSI color codes, to ensure
// JSON-RPC 2.0 protocol compliance on stdout.
func newMCPLogger() *zap.Logger {
	// Create encoder config without colors (no ANSI codes)
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder, // No colors
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// Create core that writes to stderr only
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig), // JSON format for structured logs
		zapcore.AddSync(os.Stderr),            // stderr only - never stdout
		zapcore.InfoLevel,
	)

	return zap.New(core)
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
		stdout:       opts.Stdout,
	}

	if s.logger == nil {
		// Create a MCP-safe logger (stderr only, no ANSI codes)
		s.logger = newMCPLogger()
	}

	if s.stdout == nil {
		// Use os.Stdout if no custom writer provided
		s.stdout = os.Stdout
	}

	return s
}

// Run starts the MCP server and blocks until the context is cancelled or an error occurs.
func (s *Server) Run(ctx context.Context) error {
	// Create slog logger that outputs to stderr only (MCP protocol requirement)
	slogHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slogLogger := slog.New(slogHandler)

	// Initialize MCP server with proper instructions for LLM
	s.server = sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "keploy-mock",
		Version: "v1.0.0",
	}, &sdkmcp.ServerOptions{
		Logger: slogLogger,
		Instructions: `Keploy Mock MCP Server for recording and replaying outgoing calls from applications.

Available tools:
1. keploy_list_mocks - List all available recorded mock sets. Use this first to see what mocks exist.
2. keploy_mock_record - Record outgoing calls (HTTP APIs, databases, etc.) from your application.
3. keploy_mock_test - Replay recorded mocks during testing.

Workflow:
- For recording: Ask user for the command to run their application, then use keploy_mock_record
- For testing: First use keploy_list_mocks to show available mocks, then use keploy_mock_test

Important: Always confirm configuration with user before starting record/test operations.`,
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

	// Use IOTransport with the captured stdout to ensure MCP messages go to
	// the correct output, even if os.Stdout has been redirected.
	// We use os.Stdin for input (unchanged) and the captured stdout for output.
	transport := &sdkmcp.IOTransport{
		Reader: os.Stdin,
		Writer: &nopWriteCloser{s.stdout},
	}

	// Run the server on stdio transport
	return s.server.Run(ctx, transport)
}

// nopWriteCloser wraps an io.Writer and provides a no-op Close method.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

// registerTools registers all MCP tools with the server.
func (s *Server) registerTools() {
	// Register list mocks tool (for mock discovery)
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: "keploy_list_mocks",
		Description: `List all available recorded mock sets in the keploy directory.

Use this tool to:
- Discover what mocks are available before running mock test
- Show the user available options
- Help user choose which mock set to use for testing

Returns a list of mock set names/IDs that can be used with keploy_mock_test.`,
	}, s.handleListMocks)

	// Register mock record tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: "keploy_mock_record",
		Description: `Record outgoing calls (HTTP APIs, databases, message queues, etc.) made during command execution.

This tool captures all external dependencies while running the provided command, 
creating mock files that can be replayed during testing.

IMPORTANT: Before calling this tool, confirm the following with the user:
- The command to run (typically the command to start your application, like 'go run main.go', 'npm start', './my-app', etc.)
- The path to store mocks (default: ./keploy)

The tool will show the configuration and ask for confirmation before starting.

Parameters:
- command (required): Any command to run (e.g., 'go run main.go', 'npm start', './my-app', 'python app.py')
- path (optional): Path to store mock files (default: ./keploy)`,
	}, s.handleMockRecord)

	// Register mock replay/test tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: "keploy_mock_test",
		Description: `Replay recorded mocks while running a command.

This tool intercepts outgoing calls and returns recorded responses, 
enabling isolated testing without external dependencies.

IMPORTANT workflow:
1. First use keploy_list_mocks to show available mocks to the user
2. If no mockName specified, the latest mock set will be used
3. Show configuration to user and confirm before starting

Parameters:
- command (required): Any command to run with mocks (e.g., 'go test -v', 'npm test', 'go run main.go', './my-app')
- mockName (optional): Name of the mock set to use. If not provided, uses the latest available mock.
- fallBackOnMiss (optional): Whether to make real calls when mock not found (default: false)`,
	}, s.handleMockReplay)

	s.logger.Info("Registered MCP tools",
		zap.Strings("tools", []string{"keploy_list_mocks", "keploy_mock_record", "keploy_mock_test"}),
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
	return "KeployMCPServer{tools: [keploy_list_mocks, keploy_mock_record, keploy_mock_test]}"
}
