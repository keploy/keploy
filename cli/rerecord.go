package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/orchestrator"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("rerecord", ReRecord)
}

func ReRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "rerecord",
		Short:   "ReRecord new keploy testcases/mocks from the existing test cases for the given testset(s)",
		Example: `keploy rerecord -c "user app cmd" -t "test-set-1,teset-set-3"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			var orch orchestrator.Service
			var ok bool
			if orch, ok = svc.(orchestrator.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy orchestrator service interface")
				return nil
			}

			err = orch.ReRecord(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to re-record")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add rerecord flags")
		return nil
	}

	return cmd
}
