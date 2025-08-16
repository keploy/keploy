package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	secureSvc "go.keploy.io/server/v2/pkg/service/secure"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("secure", Secure)
}

func Secure(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "secure",
		Short:   "check security vulnerabilities against a given API url (--base-url)",
		Example: `keploy secure --base-url "http://localhost:8080/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			var secSvc secureSvc.Service
			var ok bool
			if secSvc, ok = svc.(secureSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy secure service interface")
				return nil
			}

			// Get flag values
			ruleSet, _ := cmd.Flags().GetString("rule-set")

			// Add flag values to context
			ctx = context.WithValue(ctx, "rule-set", ruleSet)

			_, err = secSvc.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to start secure")
				return nil
			}

			return nil
		},
	}

	// Add flags for the main secure command
	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add secure flags")
		return nil
	}

	if addCmd := AddCRCommand(cmd, ctx, logger, cfg, serviceFactory, cmdConfigurator); addCmd != nil {
		cmd.AddCommand(addCmd)
		// Add flags for the subcommand
		err := cmdConfigurator.AddFlags(addCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add secure add flags")
		}
	}

	if removeCmd := RemoveCRCommand(cmd, ctx, logger, cfg, serviceFactory, cmdConfigurator); removeCmd != nil {
		cmd.AddCommand(removeCmd)
		// Add flags for the subcommand
		err := cmdConfigurator.AddFlags(removeCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add secure remove flags")
		}
	}

	if updateCmd := UpdateCRCommand(cmd, ctx, logger, cfg, serviceFactory, cmdConfigurator); updateCmd != nil {
		cmd.AddCommand(updateCmd)
		// Add flags for the subcommand
		err := cmdConfigurator.AddFlags(updateCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add secure update flags")
		}
	}

	if listCmd := ListCRsCommand(cmd, ctx, logger, cfg, serviceFactory, cmdConfigurator); listCmd != nil {
		cmd.AddCommand(listCmd)
		// Add flags for the subcommand
		err := cmdConfigurator.AddFlags(listCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add secure list flags")
		}
	}

	return cmd
}

func AddCRCommand(cmd *cobra.Command, ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var addCmd = &cobra.Command{
		Use:     "add",
		Short:   "add a custom security check",
		Example: `keploy secure add --checks-path "./custom-checks.yaml"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "secure")
			if err != nil {
				utils.LogError(logger, err, "failed to get secure service")
				return nil
			}

			var secSvc secureSvc.Service
			var ok bool
			if secSvc, ok = svc.(secureSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy secure service interface")
				return nil
			}

			// Get flag values
			checksPath, _ := cmd.Flags().GetString("checks-path")

			// Add flag values to context
			ctx = context.WithValue(ctx, "checks-path", checksPath)

			err = secSvc.AddCustomCheck(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to add custom check")
				return nil
			}

			return nil
		},
	}

	// addCmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")
	// addCmd.Flags().String("checks-path", "keploy/secure/custom-checks.yaml", "Path to the custom checks file")

	return addCmd
}

func RemoveCRCommand(cmd *cobra.Command, ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var removeCmd = &cobra.Command{
		Use:     "remove",
		Short:   "remove a custom security check",
		Example: `keploy secure remove --id <check-id> --checks-path "./custom-checks.yaml"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "secure")
			if err != nil {
				utils.LogError(logger, err, "failed to get secure service")
				return nil
			}

			var secSvc secureSvc.Service
			var ok bool
			if secSvc, ok = svc.(secureSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy secure service interface")
				return nil
			}

			// Get flag values
			checksPath, _ := cmd.Flags().GetString("checks-path")
			id, _ := cmd.Flags().GetString("id")

			// Add flag values to context
			ctx = context.WithValue(ctx, "checks-path", checksPath)
			ctx = context.WithValue(ctx, "id", id)

			err = secSvc.RemoveCustomCheck(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to remove custom check")
				return nil
			}

			return nil
		},
	}

	// removeCmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")
	// removeCmd.Flags().String("checks-path", "keploy/secure/custom-checks.yaml", "Path to the custom checks file")
	// removeCmd.Flags().String("id", "", "ID of the custom check to remove")

	return removeCmd
}

func UpdateCRCommand(cmd *cobra.Command, ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var updateCmd = &cobra.Command{
		Use:     "update",
		Short:   "update a custom security check",
		Example: `keploy secure update --id <check-id> --checks-path "./custom-checks.yaml"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "secure")
			if err != nil {
				utils.LogError(logger, err, "failed to get secure service")
				return nil
			}

			var secSvc secureSvc.Service
			var ok bool
			if secSvc, ok = svc.(secureSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy secure service interface")
				return nil
			}

			// Get flag values
			checksPath, _ := cmd.Flags().GetString("checks-path")
			id, _ := cmd.Flags().GetString("id")

			// Add flag values to context
			ctx = context.WithValue(ctx, "checks-path", checksPath)
			ctx = context.WithValue(ctx, "id", id)

			err = secSvc.UpdateCustomCheck(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to update custom check")
				return nil
			}

			return nil
		},
	}

	// updateCmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")
	// updateCmd.Flags().String("checks-path", "keploy/secure/custom-checks.yaml", "Path to the custom checks file")
	// updateCmd.Flags().String("id", "", "ID of the custom check to update")

	return updateCmd
}

func ListCRsCommand(cmd *cobra.Command, ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var listCmd = &cobra.Command{
		Use:     "list",
		Short:   "list all built-in or custom security checks",
		Example: `keploy secure list --rule-set custom --checks-path "./custom-checks.yaml"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "secure")
			if err != nil {
				utils.LogError(logger, err, "failed to get secure service")
				return nil
			}

			var secSvc secureSvc.Service
			var ok bool
			if secSvc, ok = svc.(secureSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy secure service interface")
				return nil
			}

			// Get flag values
			ruleSet, _ := cmd.Flags().GetString("rule-set")
			checksPath, _ := cmd.Flags().GetString("checks-path")

			// Add flag values to context
			ctx = context.WithValue(ctx, "rule-set", ruleSet)
			ctx = context.WithValue(ctx, "checks-path", checksPath)

			err = secSvc.ListChecks(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to list checks")
				return nil
			}

			return nil
		},
	}

	// listCmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")
	// listCmd.Flags().String("rule-set", "basic", "Specify which checks to list: 'basic' (built-in), 'custom'")
	// listCmd.Flags().String("checks-path", "keploy/secure/custom-checks.yaml", "Path to the custom checks file")

	return listCmd
}
