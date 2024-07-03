package cli

import (
	"context"

	"go.keploy.io/server/v2/utils"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

func init() {
	Register("test", Test)
}

func Test(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `keploy test -c "/path/to/user/app" --delay 6`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}
			// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					err = utils.Stop(logger, replaySvc.StopReason)
					if err != nil {
						utils.LogError(logger, err, "failed to stop replaying")
					}
				}
			}()
			err = replay.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to replay")
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(testCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add test flags")
		return nil
	}

	return testCmd
}
