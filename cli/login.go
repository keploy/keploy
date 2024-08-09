package cli

import (
	"context"

	"github.com/spf13/cobra"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("login", Login)
}

func Login(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, _ CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "login",
		Short:   "login to keploy via github",
		Example: `keploy login`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				return nil
			}
			tools.Login(ctx)
			return nil
		},
	}

	return cmd
}
