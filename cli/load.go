package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	loadSvc "go.keploy.io/server/v2/pkg/service/load"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("load", Load)
}

func Load(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "load",
		Short:   "load test a given testsuite.",
		Example: `keploy load -f test_suite.yaml --out json > report.json`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			var ltSvc loadSvc.Service
			var ok bool
			if ltSvc, ok = svc.(loadSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy load service interface")
				return nil
			}

			err = ltSvc.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to start the load tester")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add load flags")
		return nil
	}

	return cmd
}
