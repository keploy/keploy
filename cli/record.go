package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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
			// print usage on top of it. What must not happen is what used to:
			// `return nil` alone, so a recording that never started -- or
			// whose application died -- exited 0, and a CI job wrapping it
			// went green with nothing recorded.
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			var record recordSvc.Service
			var ok bool
			if record, ok = svc.(recordSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}

			// Start returns nil for a recording that was stopped -- Ctrl+C,
			// SIGTERM, --record-timer -- or whose application exited 0, and an
			// error only for one that failed. ctx cannot tell the two apart
			// here: Start's teardown cancels the root context either way.
			err = record.Start(ctx)

			if err != nil {
				code, fromApp := recordExitCode(err)
				// Nothing is armed when a signal ended this run: what Start
				// returned is then part of the stop, not a failure. Start
				// cannot tell that on every path -- a stop that lands during
				// setup can surface as the failure it caused, and an
				// application the same signal killed can be reported before
				// the stop is -- so the signal, not the error, decides. One
				// that lands once the run is over, while Start tears down,
				// ended nothing, and does not count (utils.NewCtx).
				armed := utils.SetFailureExitCode(code)
				var fields []zap.Field
				if fromApp && armed {
					// The code can be any at all, Keploy's own 3/4/6 included:
					// say whose it is -- when it is the one keploy exits with.
					fields = append(fields, zap.Int("appExitCode", code))
				}
				utils.LogError(logger, err, "failed to record", fields...)
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

// recordExitCode is the exit code of a recording that failed with err, and
// whether that code is the user's application's own.
//
// When the user's application ended it with a code -- exited non-zero, was
// killed, or the shell could not run it -- the code is the application's own,
// the contract `keploy mock record` keeps for its test command
// (utils/exitcodes.go): whoever reads it learns what the application did,
// where a Keploy code would report a Keploy failure that did not happen.
//
// Every other failure is Keploy's to report: the specific code it carries
// (utils.ExitCodeFor), else 1. That includes an application that ended the
// recording with no code to mirror -- its command never became a process, or
// the wait for its container failed -- where a tag on the cause survives the
// AppError it crossed, as a tag must through every layer (utils/exitcodes.go).
// `keploy mock record` gives a flat 1 there; it has no tag to read.
func recordExitCode(err error) (code int, fromApp bool) {
	var app models.AppError
	if errors.As(err, &app) && (app.AppErrorType == models.ErrUnExpected || app.AppErrorType == models.ErrCommandError) && app.ExitCode > 0 {
		return app.ExitCode, true
	}
	return utils.ExitCodeFor(err), false
}
