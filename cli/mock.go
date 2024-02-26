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
	// Register the command hook
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	// mock the keploy testcases/mocks for the user application
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
				logger.Error("Couldn't find the record or replay flag")
				return errors.New("missing required --record or --replay flag")
			}
			if record && replay {
				logger.Error("Both record and replay flags are set")
				return errors.New("both --record and --replay flags are set")
			}
			if record {
				svc, err := serviceFactory.GetService("record", *cfg)
				if err != nil {
					logger.Error("failed to get the record service")
					return err
				}
				if recordService, ok := svc.(recordSvc.Service); ok {
					return recordService.StartMock(ctx)
				}
			}
			if replay {
				svc, err := serviceFactory.GetService("replay", *cfg)
				if err != nil {
					logger.Error("failed to get the replay service")
					return err
				}
				if recordService, ok := svc.(replaySvc.Service); ok {
					return recordService.ProvideMocks(ctx)
				}
			}
			return nil

		},
	}
	cmdConfigurator.AddFlags(cmd, cfg)
	return cmd
}
