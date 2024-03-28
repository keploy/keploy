package cli

import (
	"context"
	"errors"
	"path/filepath"

	"go.keploy.io/server/v2/config"

	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func init() {
	Register("config", Config)
}

func Config(ctx context.Context, logger *zap.Logger, cfg *config.Config, servicefactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "config",
		Short:   "manage keploy configuration file",
		Example: "keploy config --generate --path /path/to/localdir",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmdConfigurator.ValidateFlags(ctx, cmd); err != nil {
				utils.LogError(logger, err, "failed to validate flags")
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			isGenerate, err := cmd.Flags().GetBool("generate")
			if err != nil {
				utils.LogError(logger, err, "failed to get generate flag")
				return err
			}

			if isGenerate {
				filePath := filepath.Join(cfg.Path, "keploy.yml")
				if utils.CheckFileExists(filePath) {
					override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
					if err != nil {
						utils.LogError(logger, err, "failed to ask for confirmation")
						return err
					}
					if !override {
						return nil
					}
				}
				svc, err := servicefactory.GetService(ctx, cmd.Name())
				if err != nil {
					utils.LogError(logger, err, "failed to get service")
					return err
				}
				var tools toolsSvc.Service
				var ok bool
				if tools, ok = svc.(toolsSvc.Service); !ok {
					utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
					return err
				}
				if err := tools.CreateConfig(ctx, filePath, ""); err != nil {
					utils.LogError(logger, err, "failed to create config")
					return err
				}
				return nil
			}
			return errors.New("only generate flag is supported in the config command")
		},
	}
	if err := cmdConfigurator.AddFlags(cmd); err != nil {
		utils.LogError(logger, err, "failed to add flags")
		return nil
	}
	return cmd
}
