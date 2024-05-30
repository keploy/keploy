package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	utgenSvc "go.keploy.io/server/v2/pkg/service/utgen"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
)

func init() {
	Register("utGen", GenerateUT)
}

func GenerateUT(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "utGen",
		Short:   "generate unit tests using AI",
		Example: `keploy utGen"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var utg utgenSvc.Service
			var ok bool
			if utg, ok = svc.(utgenSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy unit test generation service interface")
				return nil
			}

			err = utg.Start()
			if err != nil {
				utils.LogError(logger, err, "failed to generate unit tests")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add unit test generation flags")
		return nil
	}

	return cmd
}
