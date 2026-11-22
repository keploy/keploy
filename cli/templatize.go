package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("Templatize", Templatize)
}

func Templatize(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "templatize",
		Short: "templatize the keploy testcases",
		Example: `keploy templatize -t "test-set-1,test-set-3"   # these test sets
keploy templatize                              # every test set`,
		// An error RunE returns is a failure of the run, which the log has
		// said: not a command-line mistake, so no usage after it.
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Get the replay service.
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}
			// Templatize logged what stopped it, and named the sets it did
			// not reach; the error makes the command exit non-zero.
			return tools.Templatize(ctx)
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add templatize flags")
		return nil
	}

	return cmd
}
