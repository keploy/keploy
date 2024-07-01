package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	contractSvc "go.keploy.io/server/v2/pkg/service/contract"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("contract", Contract)
}

func Contract(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "contract",
		Short: "Manage keploy contracts",
	}

	cmd.AddCommand(Generate(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(Download(ctx, logger, serviceFactory, cmdConfigurator))

	return cmd
}

func Generate(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "generate",
		Short:   "Generate contract for specified services",
		Example: `keploy contract generate --service="email,notify"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				return nil
			}
			// Extract services from the flag
			serviceStr, _ := cmd.Flags().GetString("service")
			if serviceStr == "" {
				err = contract.Generate(ctx, false)
			} else {
				err = contract.Generate(ctx, true)
			}

			if err != nil {
				utils.LogError(logger, err, "failed to generate contract")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add generate flags")
		return nil
	}

	return cmd
}

func Download(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "download",
		Short:   "Download contract for specified services",
		Example: `keploy contract download --service="email,notify" --path /local/path`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				return nil
			}
			err = contract.Download(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to download contract")
			}
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add download flags")
		return nil
	}

	return cmd
}
