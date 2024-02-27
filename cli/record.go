package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	recordSvc "go.keploy.io/server/v2/pkg/service/record"
	"go.uber.org/zap"
)

func init() {
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(cmd.Name(), *cfg)
			if err != nil {
				logger.Error("failed to get service", zap.Error(err))
				return err
			}
			if record, ok := svc.(recordSvc.Service); !ok {
				logger.Error("service doesn't satisfy record service interface")
				return err
			} else {
				record.Start(ctx)
			}
			return nil
		},
	}

	cmdConfigurator.AddFlags(cmd, cfg)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd
}
