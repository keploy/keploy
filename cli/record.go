package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	recordSvc "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Every failure below arms a non-zero exit and returns nil: the
			// failure is already logged, and a returned error would have cobra
			// print usage on top of it. `return nil` alone exited 0, so a
			// recording that never started read as a clean one to CI (#4772).
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
			var record recordSvc.Service
			var ok bool
			if record, ok = svc.(recordSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				utils.SetExitCodeOnce(utils.ExitKeployError)
				return nil
			}

			// Start arms the exit code of a recording that failed itself:
			// only it can tell a failure from a stop (a Ctrl+C, the record
			// timer), because it ends ctx as it returns.
			err = record.Start(ctx)

			if err != nil {
				utils.LogError(logger, err, "failed to record")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add record flags")
		return nil
	}

	return cmd
}
