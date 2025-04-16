package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("Templatize", Templatize)
}

func Templatize(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "templatize",
		Short:   "templatize the keploy testcases for re-record",
		Example: `keploy templatize -t "test-set-1,teset-set-3" for particular testsets and keploy templatize for all testsets`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Get the replay service.
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}
			if err := tools.Templatize(ctx); err != nil {
				utils.LogError(logger, err, "failed to templatize test cases")
				return nil
			}
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add templatize flags")
		return nil
	}

	return cmd
}
