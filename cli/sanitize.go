package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	sanitizeSvc "go.keploy.io/server/v2/pkg/service/sanitize"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("Sanitize", Sanitize)
}

func Sanitize(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "sanitize",
		Short:   "sanitize the keploy testcases to remove the sensitive data",
		Example: `keploy sanitize -t "test-set-id"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var sanitizeService sanitizeSvc.Service
			var ok bool
			if sanitizeService, ok = svc.(sanitizeSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy sanitize service interface")
				return nil
			}

			err = sanitizeService.Sanitize(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to sanitize test cases")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add report flags")
		return nil
	}

	return cmd
}
