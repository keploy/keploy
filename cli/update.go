package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("update", Update)
}

// Update retrieves the command to tools Keploy
func Update(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var updateCmd = &cobra.Command{
		Use:     "update",
		Short:   "Update Keploy ",
		Example: "keploy update",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			svc, err := serviceFactory.GetService(ctx, "update")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}
			err = tools.Update(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to update")
			}
			return nil
		},
	}
	if err := cmdConfigurator.AddFlags(updateCmd); err != nil {
		utils.LogError(logger, err, "failed to add update cmd flags")
		return nil
	}
	return updateCmd
}
