package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	recordSvc "go.keploy.io/server/v2/pkg/service/record"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "mock",
		Short:   "Record and replay ougoung network traffic for the user application",
		Example: `keploy mock -c "/path/to/user/app" --delay 10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			record, err := cmd.Flags().GetBool("record")
			if err != nil {
				logger.Error("failed to read the record flag")
				return err
			}
			replay, err := cmd.Flags().GetBool("replay")
			if err != nil {
				logger.Error("failed to read the replay flag")
				return err
			}
			if !record && !replay {
				return errors.New("missing required --record or --replay flag")
			}
			if record && replay {
				return errors.New("both --record and --replay flags are set")
			}
			if record {
				svc, err := serviceFactory.GetService(ctx, "record", *cfg)
				if err != nil {
					logger.Error("failed to get service", zap.Error(err))
					return err
				}
				if recordService, ok := svc.(recordSvc.Service); ok {
					return recordService.StartMock(ctx)
				} else {
					logger.Error("service doesn't satisfy record service interface")
					return err
				}
			}
			if replay {
				svc, err := serviceFactory.GetService(ctx, "replay", *cfg)
				if err != nil {
					logger.Error("failed to get service", zap.Error(err))
					return err
				}
				if replayService, ok := svc.(replaySvc.Service); ok {
					return replayService.ProvideMocks(ctx)
				} else {
					logger.Error("service doesn't satisfy replay service interface")
					return err
				}
			}
			return nil

		},
	}
	if err := cmdConfigurator.AddFlags(cmd, cfg); err != nil {
		logger.Error("failed to add flags", zap.Error(err))
		return nil
	}
	return cmd
}
