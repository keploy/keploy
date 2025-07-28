package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	sekcheckerSvc "go.keploy.io/server/v2/pkg/service/sekchecker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("sekchecker", SekChecker)
}

func SekChecker(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "sekchecker",
		Short:   "check security vulnerabilities against a given API url (--base-url)",
		Example: `keploy sekchecker --base-url "http://localhost:8080/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			var sekSvc sekcheckerSvc.Service
			var ok bool
			if sekSvc, ok = svc.(sekcheckerSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy sekchecker service interface")
				return nil
			}

			err = sekSvc.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to start sekchecker")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add sekchecker flags")
		return nil
	}

	cmd.AddCommand(AddCRCommand(cmd, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(RemoveCRCommand(cmd, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(UpdateCRCommand(cmd, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(ListCRsCommand(cmd, logger, cfg, serviceFactory, cmdConfigurator))

	return cmd
}

func AddCRCommand(cmd *cobra.Command, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return nil
}
func RemoveCRCommand(cmd *cobra.Command, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return nil
}
func UpdateCRCommand(cmd *cobra.Command, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return nil
}
func ListCRsCommand(cmd *cobra.Command, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return nil
}
