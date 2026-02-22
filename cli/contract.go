package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	contractSvc "go.keploy.io/server/v3/pkg/service/contract"
	"go.keploy.io/server/v3/utils"
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
	cmd.AddCommand(Validate(ctx, logger, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
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
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return err
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				return errors.New("service doesn't satisfy contract service interface")
			}
			// Extract services from the flag

			err = contract.Generate(ctx, true)

			if err != nil {
				utils.LogError(logger, err, "failed to generate contract")
				return err
			}

			return nil
		},
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
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return err
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				return errors.New("service doesn't satisfy contract service interface")
			}
			err = contract.Download(ctx, true)

			if err != nil {
				utils.LogError(logger, err, "failed to download contract")
				return err
			}
			return nil
		},
	}

	return cmd
}

func Validate(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "test",
		Short:   "Validate contract for specified services",
		Example: `keploy contract test --service="email,notify" --path /local/path`,

		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return err
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				return errors.New("service doesn't satisfy contract service interface")
			}
			err = contract.Validate(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to validate contract")
				return err
			}
			return nil
		},
	}

	return cmd
}
