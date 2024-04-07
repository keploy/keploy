package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("normalise", Normalise)
}

// Normalise retrieves the command to normalise Keploy
func Normalise(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var normaliseCmd = &cobra.Command{
		Use:     "normalise",
		Short:   "Normalise Keploy",
		Example: "keploy normalise  --test-run testrun --test-sets testsets --test-cases testcases",
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
			fmt.Println("Normalising test cases")
			if err := replay.Normalise(ctx, cfg); err != nil {
				utils.LogError(logger, err, "failed to normalise test cases")
				return err
			}
			return nil
		},
	}
	if err := cmdConfigurator.AddFlags(normaliseCmd); err != nil {
		utils.LogError(logger, err, "failed to add nornalise cmd flags")
		return nil
	}
	return normaliseCmd
}
