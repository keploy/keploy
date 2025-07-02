package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("reverse-proxy", ReverseProxy)
}


func ReverseProxy(ctx context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var proxyCmd = &cobra.Command{
		Use:     "reverse-proxy",
		Short:   "Run Keploy as a reverse proxy to record frontend → backend HTTP calls as mocks",
		Example: `keploy reverse-proxy --proxy-port 16789 --forward-to localhost:5001`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			proxyPort, _ := cmd.Flags().GetInt("proxy-port")
			forwardTo, _ := cmd.Flags().GetString("forward-to")
			return provider.StartReverseProxy(context.Background(), proxyPort, forwardTo)
		},
	}

	proxyCmd.Flags().Int("proxy-port", 16789, "Port to listen for incoming HTTP requests (default 16789)")
	proxyCmd.Flags().String("forward-to", "localhost:5001", "Backend address to forward all requests to (e.g., localhost:5001)")

	if err := cmdConfigurator.AddFlags(proxyCmd); err != nil {
		utils.LogError(logger, err, "failed to add reverse-proxy flags")
		return nil
	}

	return proxyCmd
}
