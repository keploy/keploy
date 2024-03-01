package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "tools", *conf)
			if err != nil {
				return err
			}
			if tools, ok := svc.(toolsSvc.Service); !ok {
				return fmt.Errorf("svc is not of type tools")
			} else {
				return tools.Update(ctx)
			}
		},
	}
	cmdConfigurator.AddFlags(updateCmd, conf)
	return updateCmd
}
