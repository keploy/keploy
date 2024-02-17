package cli

import (
	"context"
	"go.keploy.io/server/v2/config"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/service/generateConfig"
	"go.keploy.io/server/v2/utils"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func init() {
	Register("config", Config)
}

func Config(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "config",
		Short:   "manage keploy configuration file",
		Example: "keploy config --generate --path /path/to/localdir",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// override root command's persistent pre run
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				logger.Error("failed to read the config path")
				return err
			}

			filePath := filepath.Join(path, "keploy.yml")

			if utils.CheckFileExists(filePath) {
				override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
				if err != nil {
					logger.Fatal("Failed to ask for confirmation", zap.Error(err))
					return err
				}
				if !override {
					return nil
				}
			}

			generatorConfig.GenerateConfig(filePath, generateConfig.GenerateConfigOptions{})
			return nil
		},
	}
	return cmd
}
