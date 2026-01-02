package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	keploymcp "go.keploy.io/server/v3/pkg/mcp"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mcp", MCP)
}

// MCP creates the mcp command and its subcommands.
func MCP(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mcp",
		Short: "MCP server for AI assistant integration",
		Long: `MCP (Model Context Protocol) server that exposes Keploy's mock recording 
and replay capabilities as tools for AI assistants.

This allows AI coding assistants to:
- Record outgoing calls (HTTP, databases, etc.) from your application
- Replay recorded mocks during testing
- Generate contextual names for mock files

The server communicates via stdio, making it compatible with VS Code, 
Claude Desktop, and other MCP-compatible AI assistants.`,
	}

	cmd.AddCommand(MCPServe(ctx, logger, cfg, serviceFactory, cmdConfigurator))

	return cmd
}

// MCPServe creates the serve subcommand that starts the MCP server.
func MCPServe(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server for mock recording and replay",
		Long: `Start the MCP server that exposes keploy_mock_record and keploy_mock_test tools.

The server runs on stdio transport and can be configured as an MCP server 
in your AI assistant's configuration.

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

  # The server will expose two tools:
  # - keploy_mock_record: Record outgoing calls from your application
  # - keploy_mock_test: Replay recorded mocks during testing`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger.Info("Initializing Keploy MCP server")

			// Get agent service for mock operations
			agentSvc, err := serviceFactory.GetService(ctx, "agent")
			if err != nil {
				utils.LogError(logger, err, "failed to get agent service")
				return err
			}

			agentService, ok := agentSvc.(agent.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			// Create mock record and replay services
			// Note: mockDB can be nil for now as the agent handles storage
			recorder := mockrecord.New(logger, cfg, &agentAdapter{agent: agentService}, nil)
			replayer := mockreplay.New(logger, cfg, &replayAgentAdapter{agent: agentService}, nil)

			// Create and start MCP server
			server := keploymcp.NewServer(&keploymcp.ServerOptions{
				Logger:       logger,
				MockRecorder: recorder,
				MockReplayer: replayer,
			})

			logger.Info("Starting Keploy MCP server on stdio transport")
			return server.Run(ctx)
		},
	}

	// Add flags for MCP server configuration
	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add flags to mcp serve command")
	}

	return cmd
}

// agentAdapter adapts the agent.Service to mockrecord.AgentService interface.
type agentAdapter struct {
	agent agent.Service
}

func (a *agentAdapter) Setup(ctx context.Context, startCh chan int) error {
	return a.agent.Setup(ctx, startCh)
}

func (a *agentAdapter) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	return a.agent.GetOutgoing(ctx, opts)
}

func (a *agentAdapter) StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return a.agent.StoreMocks(ctx, filtered, unFiltered)
}

// replayAgentAdapter adapts the agent.Service to mockreplay.AgentService interface.
type replayAgentAdapter struct {
	agent agent.Service
}

func (a *replayAgentAdapter) Setup(ctx context.Context, startCh chan int) error {
	return a.agent.Setup(ctx, startCh)
}

func (a *replayAgentAdapter) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	return a.agent.MockOutgoing(ctx, opts)
}

func (a *replayAgentAdapter) SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return a.agent.SetMocks(ctx, filtered, unFiltered)
}

func (a *replayAgentAdapter) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return a.agent.GetConsumedMocks(ctx)
}
