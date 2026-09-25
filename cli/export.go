package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
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
			provider.PrintLogo(log.PrimarySink(), disableAnsi)
			return cmd.Help()
		},
	}
	var postmanCmd = &cobra.Command{
		Use:     "postman",
		Short:   "export Keploy tests as Postman collection",
		Example: "keploy export postman",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(log.PrimarySink(), disableAnsi)
			// Failures arm a non-zero exit and return nil: they are already
			// logged, and an error returned to cobra would print usage over them.
			svc, err := serviceFactory.GetService(ctx, "export")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			err = tools.Export(ctx) // Assuming ExportPostmanCollection is a method in tools service
			if err != nil {
				utils.LogError(logger, err, "failed to export Postman collection")
				utils.SetFailureExitCode(utils.ExitCodeFor(err))
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
