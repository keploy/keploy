package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	reportSvc "go.keploy.io/server/v3/pkg/service/report"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("report", Report)
}

func Report(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "report",
		Short:   "report the keploy test results from the API calls",
		Example: `keploy report -t "test-set-id"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var report reportSvc.Service
			var ok bool
			if report, ok = svc.(reportSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy report service interface")
				return nil
			}

			err = report.GenerateReport(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to generate report")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add report flags")
		return nil
	}

	return cmd
}
