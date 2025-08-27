package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/mcp"
	embedSvc "go.keploy.io/server/v2/pkg/service/embed"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mcp", McpCommand)
}

// mockTelemetry is a no-op implementation for the MCP server's needs.
type mockTelemetry struct{}

func (m *mockTelemetry) GenerateEmbedding() {}

// mockAuth is a no-op implementation for the MCP server's needs.
type mockAuth struct{}

func (m *mockAuth) GetToken(ctx context.Context) (string, error) {
	return "", nil
}

func (m *mockAuth) Login(ctx context.Context) bool {
	return true
}

func McpCommand(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "mcp",
		Short:   "Starts the Keploy MCP server for AI-powered conversations.",
		Example: `keploy mcp`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The MCP server has minimal dependencies. We create a custom embed service
			// instance with mock auth and telemetry to avoid unnecessary setup (like API key checks).
			mockTel := &mockTelemetry{}
			mockAuth := &mockAuth{}

			// Override config with environment variables for MCP server
			if dbURL := os.Getenv("KEPLOY_EMBED_DATABASE_URL"); dbURL != "" {
				cfg.Embed.DatabaseURL = dbURL
			}
			if embedSvcURL := os.Getenv("KEPLOY_EMBEDDING_SERVICE_URL"); embedSvcURL != "" {
				cfg.Embed.EmbeddingServiceURL = embedSvcURL
			}

			// The service will pick up the database and embedding service URLs from the config/env.
			embedService, err := embedSvc.NewEmbedService(cfg, mockTel, mockAuth, logger)
			if err != nil {
				utils.LogError(logger, err, "failed to create a dedicated embed service for mcp")
				return err
			}

			// Create a new MCP server instance, passing the embed service.
			server := mcp.NewServer(embedService, cfg, logger)

			logger.Info("Starting MCP server...")
			// Start the server. This will block until the server is stopped.
			if err := server.Start(ctx); err != nil {
				utils.LogError(logger, err, "failed to start MCP server")
				return err
			}

			return nil
		},
	}

	var pathCmd = &cobra.Command{
		Use:     "path",
		Short:   "Prints the path of the keploy binary.",
		Example: `keploy mcp path`,
		RunE: func(cmd *cobra.Command, args []string) error {
			executable, err := os.Executable()
			if err != nil {
				utils.LogError(logger, err, "failed to get executable path")
				return err
			}
			fmt.Println(executable)
			return nil
		},
	}

	cmd.AddCommand(pathCmd)

	return cmd
}
