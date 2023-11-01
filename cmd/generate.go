package cmd

import (
	"go.keploy.io/server/pkg/service/generate"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdGenerate(logger *zap.Logger) *Generate {
	generator := generate.NewGenerator(logger)
	return &Generate{
		generator: generator,
		logger:     logger,
	}
}

type Generate struct {
	generator generate.Generator
	logger   *zap.Logger
}

func (g *Generate) GetCmd() *cobra.Command {
	// create keploy configuration file
	var generateCmd = &cobra.Command{
		Use:     "generate-keploy-config",
		Short:   "generate the keploy configuration file",
		Example: "keploy-generate-config --config-path /path/to/localdir",
		RunE: func(cmd *cobra.Command, args []string) error {

			configPath, err := cmd.Flags().GetString("config-path")
			if err != nil {
				g.logger.Error("failed to read the config path")
				return err
			}
			
			g.generator.Generate(configPath)
			return nil
		},
	}

	generateCmd.Flags().String("config-path", ".", "Path to the local directory where keploy configuration file is stored")

	return generateCmd
}
