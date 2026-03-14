package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/clilog"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"log/slog"
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
			l := clilog.FromContext(cmd.Context())
			svc, err := serviceFactory.GetService(cmd.Context(), cmd.Name())
			if err != nil {
				l.Error("failed to get service",
					slog.String("error", err.Error()),
				)
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				l.Error("service doesn't satisfy replay service interface")
				return nil
			}
			// defering the stop function to stop keploy in case of any error in test or in case of context cancellation
			defer func() {
				select {
				case <-cmd.Context().Done():
					break
				default:
					utils.ExecCancel()
				}
			}()
			err = replay.Start(cmd.Context())
			if err != nil {
				if cmd.Context().Err() != context.Canceled {
					l.Error("failed to replay",
						slog.String("error", err.Error()),
					)
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
