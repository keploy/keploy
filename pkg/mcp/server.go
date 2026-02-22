// Package mcp provides an MCP server for Keploy sandbox record/replay functionality.
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
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Server is the Keploy MCP server that exposes sandbox record/replay tools.
type Server struct {
	server       *sdkmcp.Server
	mockRecorder mockrecord.Service
	mockReplayer mockreplay.Service
	cfg          *config.Config
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
	// Config is the active Keploy config used by sandbox record/replay flows.
	Config *config.Config
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
		cfg:          opts.Config,
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
		Name:    "keploy-sandbox",
		Version: "v1.0.0",
	}, &sdkmcp.ServerOptions{
		Logger: slogLogger,
		Instructions: `Keploy Sandbox MCP Server for recording and replaying outgoing calls from applications.

Available tools:
1. keploy_manager - ALWAYS SELECT THIS TOOL FIRST for Keploy workflows.
2. keploy_prompt_test_command - Returns raw prompt text to derive the app test command.
3. keploy_prompt_dependency_start - Returns raw prompt text to detect/start required dependencies before record.
4. keploy_prompt_test_integration - Returns raw prompt text to instrument tests with sandbox scope hooks.
5. keploy_mock_record - Records sandbox files.
6. keploy_mock_test - Replays sandbox files.
7. keploy_prompt_pipeline_creation - Returns raw prompt text to generate CI pipeline config.

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
- If command cannot be decided confidently, pass command as empty. The server handles elicitation and returns the selected command.
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

Record outgoing calls (HTTP APIs, databases, message queues, etc.) made during command execution as sandbox files.

This tool captures all external dependencies while running the provided command, 
creating sandbox files that can be replayed during testing.

IMPORTANT: Before calling this tool, confirm the following with the user:
- The command to run. Prefer deterministic test commands over app run commands.
- For Go projects, default to go test commands first (for example 'go test -v -run "TestA|TestB"' or 'go test -v ./...').
- Never default to 'go run main.go' for recording.
- Search keploy.yml for the sandbox ref format (for example: sandbox: ref: anjupathak03/test-server:v1.0.0), extract the tag from ref, and use the next patch version for this record run (for example v1.0.0 -> v1.0.1); never reuse an existing tag value.
- Provide a semantic-version 'tag' argument (strict format like 'v1.0.0' or '1.0.0'); if the user does not provide one, AI must generate it.
- Avoid long-running/watch-mode/interactive commands and commands that do not terminate.
- If command is unknown, send command as empty; the server will resolve it via elicitation.
- The sandbox location directory (default: .)
- The sandbox file prefix (default: keploy, final file is <name>.sb.yaml)

The tool will show the configuration and ask for confirmation before starting.

Parameters:
- command (optional): Command to run (prefer test commands like 'go test -v ./...' or 'npm test'). If empty, server elicits command and uses that value.
- path (optional): Sandbox location directory (default: .)
- name (optional): Sandbox file prefix (default: keploy, final file is <name>.sb.yaml)
- tag (required): Semantic version tag with strict semver enforcement (for example 'v1.0.0'). Read keploy.yml sandbox ref in the format 'sandbox: ref: <org>/<app>:<tag>', increment the last digit (patch) by one (for example v1.0.0 -> v1.0.1), and use that for this run; otherwise AI should generate the starting tag.`,
		}, s.handleMockRecord)

	// Register mock replay/test tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolMockTest,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_mock_test".

Replay recorded sandbox files while running a command.

This tool intercepts outgoing calls and returns recorded responses, 
enabling isolated testing without external dependencies.

Show configuration to user and confirm before starting.

Parameters:
- command (required): Any command to run with sandbox replay (e.g., 'go test -v', 'npm test', './my-app')
- path (optional): Sandbox location directory (default: .)
- name (optional): Sandbox file prefix (default: keploy, final file is <name>.sb.yaml)
- local (optional): Local-only sandbox replay mode (skip cloud sync/upload, default: false)
- fallBackOnMiss (optional): Whether to make real calls when sandbox match is not found (default: false)`,
	}, s.handleMockReplay)

	// Register test integration prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptTestCommand,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_test_command".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: derive the best deterministic serialized app test command for Keploy.

If command cannot be decided confidently, the prompt result should return empty command.
Pass that empty command forward; the server will resolve it via elicitation in later execution tools.`,
	}, s.handlePromptTestCommand)

	// Register dependency startup prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptDependencyStart,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_dependency_start".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: identify dependencies required by the selected app/test command, validate readiness, and start dependencies that are not healthy before recording.

This tool does not execute dependency startup itself. It only returns the prompt text.
If command context is empty, pass it as empty; dependency prompt should infer best-effort from repository context.`,
	}, s.handlePromptDependencyStart)

	// Register test integration prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptTestIntegration,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_test_integration".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: automatically edit project test files to add Keploy sandbox scope integration hooks.

This tool does not edit files itself. It only returns the prompt text.
If command context is empty, pass it as empty; do not synthesize. Server-side elicitation in execution tools resolves it.`,
	}, s.handlePromptTestIntegration)

	// Register pipeline prompt tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolPromptPipelineCreation,
		Description: `EXPLICIT USE ONLY (STRICT).
Do not auto-select this tool under any circumstance.
Call this tool only when the user explicitly names "keploy_prompt_pipeline_creation".

Returns raw prompt text that client LLM must execute as a direct user task.
Purpose: generate/update CI/CD pipeline files to run keploy sandbox replay.
If command context is empty, keep it empty in prompt response; server-side elicitation resolves it before execution.`,
	}, s.handlePromptPipelineCreation)

	// Register unified manager tool
	sdkmcp.AddTool(s.server, &sdkmcp.Tool{
		Name: ToolManager,
		Description: `ALWAYS SELECT THIS TOOL FOR KEPLOY WORKFLOWS (STRICT DEFAULT).

This is an orchestration-first tool. The client LLM should execute this sequence:
1) Invoke keploy_prompt_test_command and execute returned prompt as direct user task to derive the app/test command.
2) If command is empty/unresolved, pass it as-is to execution tools; server-side elicitation will resolve it.
3) Invoke keploy_prompt_dependency_start and execute returned prompt as direct user task to ensure dependencies are ready. Continue only when the dependency result has ready=true.
4) Invoke keploy_prompt_test_integration and execute returned prompt as direct user task to edit tests.
5) Invoke keploy_mock_record with resolved command.
6) Invoke keploy_mock_test with resolved command (path/name optional; defaults are used when omitted).
7) Invoke keploy_prompt_pipeline_creation and execute returned prompt as direct user task for CI/CD pipeline edits.

CRITICAL: Execute ALL steps 1-7 in order. Do not stop after recording (step 5).
You must proceed immediately to step 6 (replay) and step 7 (pipeline) in the same session.
Do not ask for user confirmation between steps unless an error occurs.`,
	}, s.handleManager)

	s.logger.Info("Registered MCP tools",
		zap.Strings("tools", []string{
			ToolMockRecord,
			ToolMockTest,
			ToolPromptTestCommand,
			ToolPromptDependencyStart,
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
	return "KeployMCPServer{tools: [keploy_mock_record, keploy_mock_test, keploy_prompt_test_command, keploy_prompt_dependency_start, keploy_prompt_test_integration, keploy_prompt_pipeline_creation, keploy_manager]}"
}
