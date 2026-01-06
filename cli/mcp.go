package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	keploymcp "go.keploy.io/server/v3/pkg/mcp"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func init() {
	Register("mcp", MCP)
}

// newMCPLogger creates a logger suitable for MCP server use.
// MCP Protocol Requirements:
// 1. All logs MUST go to stderr (stdout is reserved for JSON-RPC messages)
// 2. NO ANSI color codes (they corrupt JSON-RPC parsing)
// 3. Use structured JSON format for machine readability
func newMCPLogger() *zap.Logger {
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

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(os.Stderr), // stderr only
		zapcore.InfoLevel,
	)

	return zap.New(core)
}

// MCP creates the mcp command and its subcommands.
func MCP(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mcp",
		Short: "MCP server for AI assistant integration",
		Long: `MCP (Model Context Protocol) server that exposes Keploy's mock recording 
and replay capabilities as tools for AI assistants.

This allows AI coding assistants to:
- List available recorded mocks
- Record outgoing calls (HTTP, databases, etc.) from your application
- Replay recorded mocks during testing

The server communicates via stdio using JSON-RPC 2.0, making it compatible 
with VS Code, Claude Desktop, and other MCP-compatible AI assistants.

IMPORTANT: The MCP server outputs JSON-RPC messages on stdout and logs on stderr.
Do not pipe stdout to other commands when using as an MCP server.`,
		// Set MCP stdio mode early to prevent logo from printing
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			utils.SetMCPStdio(true)
		},
	}

	cmd.AddCommand(MCPServe(ctx, logger, cfg, serviceFactory, cmdConfigurator))

	return cmd
}

// MCPServe creates the serve subcommand that starts the MCP server.
func MCPServe(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server for mock recording and replay",
		Long: `Start the MCP server that exposes Keploy mock tools.

The server runs on stdio transport using JSON-RPC 2.0 protocol and can be 
configured as an MCP server in your AI assistant's configuration.

Available tools:
- keploy_list_mocks: List all available recorded mock sets
- keploy_mock_record: Record outgoing calls from your application
- keploy_mock_test: Replay recorded mocks during testing

Example Claude Desktop configuration (claude_desktop_config.json):
{
  "mcpServers": {
    "keploy": {
      "command": "keploy",
      "args": ["mcp", "serve"]
    }
  }
}

Example VS Code configuration:
{
  "mcp.servers": {
    "keploy": {
      "command": "keploy",
      "args": ["mcp", "serve"]
    }
  }
}`,
		Example: `  # Start the MCP server
  keploy mcp serve

  # The server will expose three tools:
  # - keploy_list_mocks: List available mock sets
  # - keploy_mock_record: Record outgoing calls from your application
  # - keploy_mock_test: Replay recorded mocks during testing`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			utils.SetMCPStdio(true)
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			utils.SetMCPStdio(true)
			// CRITICAL: Capture the original stdout FIRST, before any redirection.
			// MCP uses stdio transport - stdout is reserved for JSON-RPC messages.
			// We save the original stdout for MCP communication, then redirect
			// os.Stdout to /dev/null to prevent any banners/logs/colors from
			// corrupting the JSON-RPC stream.
			originalStdout := os.Stdout

			// Redirect os.Stdout to /dev/null so any code that writes to os.Stdout
			// (banners, colored logs, etc.) doesn't corrupt the JSON-RPC stream.
			devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer devNull.Close()
			os.Stdout = devNull

			// Create MCP-safe logger (stderr only, no ANSI codes)
			mcpLogger := newMCPLogger()

			mcpLogger.Info("Initializing Keploy MCP server")

			recordSvc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(mcpLogger, err, "failed to get record service")
				return err
			}

			runner, ok := recordSvc.(mockrecord.RecordRunner)
			if !ok {
				utils.LogError(mcpLogger, nil, "service doesn't satisfy record runner interface")
				return nil
			}

			replaySvc, err := serviceFactory.GetService(ctx, "test")
			if err != nil {
				utils.LogError(mcpLogger, err, "failed to get replay service")
				return err
			}

			replayRuntime, ok := replaySvc.(mockreplay.Runtime)
			if !ok {
				utils.LogError(mcpLogger, nil, "service doesn't satisfy replay runtime interface")
				return nil
			}

			// Create mock record and replay services
			recorder := mockrecord.New(mcpLogger, cfg, runner, nil)
			replayer := mockreplay.New(mcpLogger, cfg, replayRuntime)

			// Create and start MCP server with MCP-safe logger
			// IMPORTANT: Pass the original stdout for MCP JSON-RPC communication
			server := keploymcp.NewServer(&keploymcp.ServerOptions{
				Logger:       mcpLogger,
				MockRecorder: recorder,
				MockReplayer: replayer,
				Stdout:       originalStdout, // Use the original stdout for MCP
			})

			mcpLogger.Info("Starting Keploy MCP server on stdio transport")
			return server.Run(ctx)
		},
	}
	cmd.Annotations = map[string]string{provider.MCPStdioAnnotationKey: "true"}
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)

	// Add flags for MCP server configuration
	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add flags to mcp serve command")
	}

	return cmd
}
