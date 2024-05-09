package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("normalize", Normalize)
}

// Normalize retrieves the command to normalize Keploy
func Normalize(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var normalizeCmd = &cobra.Command{
		Use:     "normalize",
		Short:   "Normalize Keploy",
		Example: "keploy normalize  --test-run testrun --tests test-set-1:test-case-1 test-case-2,test-set-2:test-case-1 test-case-2 ",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd)
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
			if err := replay.Normalize(ctx); err != nil {
				utils.LogError(logger, err, "failed to normalize test cases")
				return nil
			}
			return nil
		},
	}
	if err := cmdConfigurator.AddFlags(normalizeCmd); err != nil {
		utils.LogError(logger, err, "failed to add normalize cmd flags")
		return nil
	}
	return normalizeCmd
}
