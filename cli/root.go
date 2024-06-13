package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()

	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: provider.RootExamples,
		Version: utils.Version,
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.SetHelpTemplate(provider.RootCustomHelpTemplate)

	rootCmd.SetVersionTemplate(provider.VersionTemplate)

	err := cmdConfigurator.AddFlags(rootCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to set flags")
		return nil
	}

	// fmt.Println("This is the value of registered", Registered)
	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svcFactory, cmdConfigurator)
		rootCmd.AddCommand(c)
	}
	return rootCmd
}
