package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/clilog"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"log/slog"
	"os"
)

func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()

	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: provider.RootExamples,
		Version: utils.Version,
		PreRun: func(cmd *cobra.Command, _ []string) {
			disableAnsi, _ := cmd.Flags().GetBool("disable-ansi")
			provider.PrintLogo(os.Stdout, disableAnsi)
		},
	}

	defaultHelpFunc := rootCmd.HelpFunc()

	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		disableAnsi, _ := cmd.Flags().GetBool("disable-ansi")
		provider.PrintLogo(os.Stdout, disableAnsi)

		// Use the default help function instead of calling the parent's HelpFunc
		defaultHelpFunc(cmd, args)
	})

	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.PersistentFlags().Bool("verbose", false, "Enable verbose debug logging")

	rootCmd.SetHelpTemplate(provider.RootCustomHelpTemplate)

	rootCmd.SetVersionTemplate(provider.VersionTemplate)

	err := cmdConfigurator.AddFlags(rootCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to set flags")
		return nil
	}

	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svcFactory, cmdConfigurator)
		wrapCommandContext(c)
		rootCmd.AddCommand(c)
	}
	return rootCmd
}

// fix2
func wrapCommandContext(c *cobra.Command) {
	originalPreRunE := c.PreRunE
	originalRunE := c.RunE

	c.PreRunE = func(cmd *cobra.Command, args []string) error {
		base := clilog.FromContext(cmd.Context())

		l := clilog.CommandLogger(
			base,
			cmd.Name(),
			utils.Version,
		)

		ctxWith := clilog.WithContext(cmd.Context(), l)
		cmd.SetContext(ctxWith)

		if originalPreRunE != nil {
			return originalPreRunE(cmd, args)
		}
		return nil
	}

	if originalRunE != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			return originalRunE(cmd, args)
		}
	}
}
