package cmd

import (
	"go.keploy.io/server/pkg/service/generateConfig"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdGenerateConfig(logger *zap.Logger) *GenerateConfig {
	generatorConfig := generateConfig.NewGeneratorConfig(logger)
	return &GenerateConfig{
		generatorConfig: generatorConfig,
		logger:     logger,
	}
}

type GenerateConfig struct {
	generatorConfig generateConfig.GeneratorConfig
	logger   *zap.Logger
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
			
			g.generatorConfig.GenerateConfig(configPath)
			return nil
		},
	}

	generateConfigCmd.Flags().StringP("path", "p", ".", "Path to the local directory where keploy configuration file is stored")

	return generateConfigCmd
}
