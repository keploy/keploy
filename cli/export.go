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
	Register("export", Export)
}

func Export(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {

	var exportCmd = &cobra.Command{
		Use:     "export",
		Short:   "export Keploy tests as postman collection",
		Example: "keploy export",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			return cmd.Help()
		},
	}
	var postmanCmd = &cobra.Command{
		Use:     "postman",
		Short:   "export Keploy tests as Postman collection",
		Example: "keploy export postman",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			svc, err := serviceFactory.GetService(ctx, "export")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}
			err = tools.Export(ctx) // Assuming ExportPostmanCollection is a method in tools service
			if err != nil {
				utils.LogError(logger, err, "failed to export Postman collection")
			}
			return nil
		},
	}
	exportCmd.AddCommand(postmanCmd)

	if err := cmdConfigurator.AddFlags(exportCmd); err != nil {
		utils.LogError(logger, err, "failed to add export cmd flags")
		return nil
	}
	return exportCmd
}
