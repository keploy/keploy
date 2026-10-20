package cli

import (
	"context"

	"go.keploy.io/server/v3/utils"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
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
			// A failure before the replay starts arms a non-zero exit and
			// returns nil, as the replay's own do (Replayer.Start): it is
			// logged already, and a returned error would have cobra print
			// usage on top of it. `return nil` alone exited 0 with no test run.
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				// A Ctrl+C while the service was being built is a stop, not
				// a failure.
				if ctx.Err() == nil {
					utils.SetExitCodeOnce(utils.ExitCodeFor(err))
				}
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				utils.SetExitCodeOnce(utils.ExitKeployError)
				return nil
			}
			// defering the stop function to stop keploy in case of any error in test or in case of context cancellation
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					utils.ExecCancel()
				}
			}()
			err = replay.Start(ctx)
			if err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogContentError(logger, err, "failed to replay")
				}
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
