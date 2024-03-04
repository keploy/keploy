package cli

import (
	"context"
	"fmt"
	_ "net/http/pprof"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func LogExample(example string) string {
	return fmt.Sprintf("Example usage: %s", example)
}

// TODO return error
func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()

	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: rootExamples,
		Version: utils.Version,
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.SetHelpTemplate(rootCustomHelpTemplate)

	rootCmd.SetVersionTemplate(versionTemplate)

	err := cmdConfigurator.AddFlags(rootCmd, conf)
	if err != nil {
		logger.Error("failed to set flags", zap.Error(err))
		return nil
	}

	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svcFactory, cmdConfigurator)
		rootCmd.AddCommand(c)
	}
	return rootCmd
}
