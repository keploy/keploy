package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	diagSvc "go.keploy.io/server/v3/pkg/service/diagnose"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("diagnose", Diagnose)
}

func Diagnose(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "diagnose",
		Short: "diagnose mock mismatches and suggest fixes",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var diag diagSvc.Service
			var ok bool
			if diag, ok = svc.(diagSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy diagnose service interface")
				return nil
			}

			if err := diag.Diagnose(ctx); err != nil {
				utils.LogError(logger, err, "failed to diagnose")
				return nil
			}
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add diagnose flags")
		return nil
	}

	return cmd
}
