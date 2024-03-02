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
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {

			isGenerate, err := cmd.Flags().GetBool("generate")
			if err != nil {
				logger.Error("Failed to get generate flag", zap.Error(err))
				return err
			}

			if isGenerate {
				filePath := filepath.Join(cfg.Path, "keploy.yml")
				if utils.CheckFileExists(filePath) {
					override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
					if err != nil {
						logger.Error("Failed to ask for confirmation", zap.Error(err))
						return err
					}
					if !override {
						return nil
					}
				}
				svc, err := servicefactory.GetService(ctx, cmd.Name(), *cfg)
				if err != nil {
					logger.Error("failed to get service", zap.Error(err))
					return err
				}
				if tools, ok := svc.(toolsSvc.Service); !ok {
					logger.Error("service doesn't satisfy tools service interface")
					return err
				} else {
					if err := tools.CreateConfig(ctx, filePath, ""); err != nil {
						logger.Error("failed to create config", zap.Error(err))
						return err
					}
				}
				return nil
			} else {
				return errors.New("only generate flag is supported in the config command")
			}
		},
	}
	if err := cmdConfigurator.AddFlags(cmd, cfg); err != nil {
		logger.Error("Failed to add flags", zap.Error(err))
		return nil
	}
	return cmd
}
