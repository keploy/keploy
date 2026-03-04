package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/processlock"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()
	var cliLock *processlock.Lock
	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: provider.RootExamples,
		Version: utils.Version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			lockInstance, err := processlock.Acquire()
			if err != nil {
				return err
			}

			cliLock = lockInstance
			return nil
		},
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
	rootCmd.PersistentPostRunE = func(cmd *cobra.Command, _ []string) error {
		if cliLock != nil {
			if err := cliLock.Release(); err != nil {
				return err
			}
		}
		return nil
	}
	rootCmd.SetHelpTemplate(provider.RootCustomHelpTemplate)

	rootCmd.SetVersionTemplate(provider.VersionTemplate)

	err := cmdConfigurator.AddFlags(rootCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to set flags")
		return nil
	}

	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svcFactory, cmdConfigurator)
		rootCmd.AddCommand(c)
	}
	return rootCmd
}
