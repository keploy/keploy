package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	recordSvc "go.keploy.io/server/v2/pkg/service/record"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "mock",
		Short:   "Record and replay outgoing network traffic for the user application",
		Example: `keploy mock -c "/path/to/user/app" --delay 10`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			record, err := cmd.Flags().GetBool("record")
			if err != nil {
				utils.LogError(logger, nil, "failed to read the record flag")
				return err
			}
			replay, err := cmd.Flags().GetBool("replay")
			if err != nil {
				utils.LogError(logger, nil, "failed to read the replay flag")
				return err
			}
			if !record && !replay {
				return errors.New("missing required --record or --replay flag")
			}
			if record && replay {
				return errors.New("both --record and --replay flags are set")
			}
			if record {
				svc, err := serviceFactory.GetService(ctx, "record")
				if err != nil {
					utils.LogError(logger, err, "failed to get service")
					return err
				}
				var recordService recordSvc.Service
				var ok bool
				if recordService, ok = svc.(recordSvc.Service); ok {
					return recordService.StartMock(ctx)
				}
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				return err

			}
			if replay {
				svc, err := serviceFactory.GetService(ctx, "replay")
				if err != nil {
					utils.LogError(logger, err, "failed to get service")
					return err
				}
				var replayService replaySvc.Service
				var ok bool
				if replayService, ok = svc.(replaySvc.Service); ok {
					return replayService.ProvideMocks(ctx)
				}
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return err
			}
			return nil

		},
	}
	if err := cmdConfigurator.AddFlags(cmd); err != nil {
		utils.LogError(logger, err, "failed to add flags")
		return nil
	}
	return cmd
}
