package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("tools", Update)
}

// Update retrieves the command to tools Keploy
func Update(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var updateCmd = &cobra.Command{
		Use:     "update",
		Short:   "Update Keploy ",
		Example: "keploy tools",
		RunE: func(_ *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "tools", *conf)
			if err != nil {
				return err
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				return fmt.Errorf("svc is not of type tools")
			}
			return tools.Update(ctx)
		},
	}
	if err := cmdConfigurator.AddFlags(updateCmd, conf); err != nil {
		utils.LogError(logger, err, "failed to add update cmd flags")
		return nil
	}
	return updateCmd
}
