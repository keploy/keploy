package cmd

import (
	"path/filepath"

	"go.keploy.io/server/pkg/service/generateConfig"
	"go.keploy.io/server/utils"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdGenerateConfig(logger *zap.Logger) *GenerateConfig {
	generatorConfig := generateConfig.NewGeneratorConfig(logger)
	return &GenerateConfig{
		generatorConfig: generatorConfig,
		logger:          logger,
	}
}

type GenerateConfig struct {
	generatorConfig generateConfig.GeneratorConfig
	logger          *zap.Logger
}

func (g *GenerateConfig) GetCmd() *cobra.Command {
	// create keploy configuration file
	var generateConfigCmd = &cobra.Command{
		Use:     "generate-config",
		Short:   "generate the keploy configuration file",
		Example: "keploy generate-config --path /path/to/localdir",
		RunE: func(cmd *cobra.Command, args []string) error {

			configPath, err := cmd.Flags().GetString("path")
			if err != nil {
				g.logger.Error("failed to read the config path")
				return err
			}

			filePath := filepath.Join(configPath, "keploy-config.yaml")

			if utils.CheckFileExists(filePath) {
				override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
				if err != nil {
					g.logger.Fatal("Failed to ask for confirmation", zap.Error(err))
					return err
				}
				if !override {
					return nil
				}
			}

			g.generatorConfig.GenerateConfig(filePath)
			return nil
		},
	}

	generateConfigCmd.Flags().StringP("path", "p", ".", "Path to the local directory where keploy configuration file will be stored")

	return generateConfigCmd
}
