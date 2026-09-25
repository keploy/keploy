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
		Short:   "Generate OpenAPI contract from recorded test cases",
		Example: `keploy contract generate --path .`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger.Info("`keploy contract generate` is a community-maintained OpenAPI spec generator; a fully managed, production-grade version is available as part of Keploy Enterprise")

			// Failures arm a non-zero exit and return nil: they are already
			// logged, and an error returned to cobra would print usage over them.
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}

			infer, _ := cmd.Flags().GetBool("infer")
			if infer {
				err = contract.GenerateFromTests(ctx)
			} else {
				err = contract.Generate(ctx, true)
			}

			if err != nil {
				utils.LogError(logger, err, "failed to generate contract")
				utils.SetFailureExitCode(utils.ExitCodeFor(err))
				return nil
			}

			return nil
		},
	}

	cmd.Flags().Bool("infer", false, "Infer OpenAPI contract from recorded traffic (opt-in; the default path is service-mapping based generation)")

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
			// Failures arm a non-zero exit and return nil: they are already
			// logged, and an error returned to cobra would print usage over them.
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			err = contract.Download(ctx, true)

			if err != nil {
				utils.LogError(logger, err, "failed to download contract")
				utils.SetFailureExitCode(utils.ExitCodeFor(err))
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
			// Failures arm a non-zero exit and return nil: they are already
			// logged, and an error returned to cobra would print usage over them.
			svc, err := serviceFactory.GetService(ctx, "contract")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			var contract contractSvc.Service
			var ok bool
			if contract, ok = svc.(contractSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy contract service interface")
				utils.SetFailureExitCode(utils.ExitKeployError)
				return nil
			}
			err = contract.Validate(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to validate contract")
				utils.SetFailureExitCode(utils.ExitCodeFor(err))
			}
			return nil
		},
	}

	return cmd
}
