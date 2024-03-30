package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// func NewCmdNormalise(logger *zap.Logger) *Normalise {
// 	normaliser := normalise.NewNormaliser(logger)
// 	return &Normalise{
// 		normaliser: normaliser,
// 		logger:     logger,
// 	}
// }

//	type Normalise struct {
//		normaliser normalise.Normaliser
//		logger     *zap.Logger
//	}
func init() {
	Register("normalise", Normalise)
}

// Normalise retrieves the command to normalise Keploy
func Normalise(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var normaliseCmd = &cobra.Command{
		Use:     "normalise",
		Short:   "Normalise Keploy",
		Example: "keploy normalise --path /path/to/localdir --test-set testset --test-cases testcases",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd)
		},
		RunE: func(_ *cobra.Command, _ []string) error {

			svc, err := serviceFactory.GetService(ctx, "normalise")
			if err != nil {
				return err
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy normalise service interface")
				return nil
			}
			if err := tools.Normalise(ctx); err != nil {
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
