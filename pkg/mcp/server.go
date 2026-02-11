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
1. keploy_manager - ALWAYS SELECT THIS TOOL FIRST for Keploy workflows.
2. keploy_prompt_test_integration - Returns raw prompt text to instrument tests with start-session hooks.
3. keploy_mock_record - Records mocks.
4. keploy_mock_test - Replays mocks.
5. keploy_prompt_pipeline_creation - Returns raw prompt text to generate CI pipeline config.

Tool routing rule (STRICT):
- Always call keploy_manager for any Keploy-related request.
- Never auto-select any non-manager tool.
- Call a non-manager tool only if the user explicitly names that exact tool (for example: "use keploy_mock_test").
- If user intent is generic (record/test/pipeline without exact tool name), you must still call keploy_manager.

Command selection policy for recording:
- Prefer test commands over run commands.
- For Go projects, default to go test commands unless there is a strong reason not to.
- Do not default to 'go run main.go' for recording. Treat long-running server commands as disallowed defaults.
- Use repository signals to choose commands: go.mod, *_test.go, Makefile, CI workflows, README scripts, and known test names.
- Go-specific decision order:
  - If user provided explicit tests/patterns, use them directly (for example 'go test -v -run "TestA|TestB"').
  - Else if integration/e2e style tests exist, prefer a focused -run pattern over full suite.
  - Else if tests exist but no target, use 'go test -v ./...'.
  - Only if no useful tests exist, use an app run command as last resort.
- Disallowed by default: watch/dev servers, interactive/manual-step commands, commands that do not terminate, and overly broad commands when focused tests are available.
- If command cannot be decided confidently, leave command empty and use elicitation to ask the user.
- Prefer stable, deterministic, terminating commands that trigger the intended outbound interactions.
`,
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
	// Register mock record tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolMockRecord,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_mock_record".

Record outgoing calls (HTTP APIs, databases, message queues, etc.) made during command execution.

This tool captures all external dependencies while running the provided command, 
creating mock files that can be replayed during testing.

IMPORTANT: Before calling this tool, confirm the following with the user:
- The command to run. Prefer deterministic test commands over app run commands.
- For Go projects, default to go test commands first (for example 'go test -v -run "TestA|TestB"' or 'go test -v ./...').
- Never default to 'go run main.go' for recording.
- Avoid long-running/watch-mode/interactive commands and commands that do not terminate.
- If command is unknown, pass command as empty and the server will elicit it from the user.
- The path to store mocks (default: ./keploy)

The tool will show the configuration and ask for confirmation before starting.

Parameters:
- command (optional): Command to run (prefer test commands like 'go test -v ./...' or 'npm test'). If empty, server elicits command.
- path (optional): Path to store mock files (default: ./keploy)`,
	}, s.handleMockRecord)

	// Register mock replay/test tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolMockTest,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_mock_test".

Replay recorded mocks while running a command.

This tool intercepts outgoing calls and returns recorded responses, 
enabling isolated testing without external dependencies.

Show configuration to user and confirm before starting.

Parameters:
- command (required): Any command to run with mocks (e.g., 'go test -v', 'npm test', 'go run main.go', './my-app')
- path (optional): Path to the mock directory to replay (e.g., './keploy/mock-set-3'). Omit (send empty) unless user explicitly asks for a specific path. If omitted, latest mock set is used.
- fallBackOnMiss (optional): Whether to make real calls when mock not found (default: false)`,
	}, s.handleMockReplay)

	// Register test integration prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptTestIntegration,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_test_integration".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: automatically edit project test files to add Keploy start-session integration hooks.

This tool does not edit files itself. It only returns the prompt text.`,
	}, s.handlePromptTestIntegration)

	// Register pipeline prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptPipelineCreation,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_pipeline_creation".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: generate/update CI/CD pipeline files to run keploy mock test.`,
	}, s.handlePromptPipelineCreation)

	// Register unified manager tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolManager,
		Description: `ALWAYS SELECT THIS TOOL FOR KEPLOY WORKFLOWS (STRICT DEFAULT).

This is an orchestration-first tool. The client LLM should execute this sequence:
1) Determine app/test command. If missing, use elicitation to ask the user.
2) Invoke keploy_prompt_test_integration and execute returned prompt as direct user task to edit tests.
3) Invoke keploy_mock_record with resolved command.
4) Invoke keploy_mock_test with resolved command (path optional; omit to use latest).
5) Invoke keploy_prompt_pipeline_creation and execute returned prompt as direct user task for CI/CD pipeline edits.

Do not skip the sequence unless user explicitly asks to skip steps.
Do not auto-select non-manager tools directly from generic intent.
Non-manager tools are allowed only when user explicitly names them.

Manager handler returns workflow guidance and does not perform direct record/test/pipeline side effects.`,
	}, s.handleManager)

	s.logger.Info("Registered MCP tools",
		zap.Strings("tools", []string{
			ToolMockRecord,
			ToolMockTest,
			ToolPromptTestIntegration,
			ToolPromptPipelineCreation,
			ToolManager,
		}),
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
	return "KeployMCPServer{tools: [keploy_mock_record, keploy_mock_test, keploy_prompt_test_integration, keploy_prompt_pipeline_creation, keploy_manager]}"
}
