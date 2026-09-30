package cli

import (
	"context"
	"sync"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// removeAgentTokenOnce registers the control-plane token cleanup with cobra at
// most once, however many times Root is called.
var removeAgentTokenOnce sync.Once

func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()

	// A natively started agent is handed its control-plane token in a file in
	// a private temp directory (token.WriteFile), and that directory must not
	// outlive the command. Registered here, as a cobra finalizer, rather than
	// in a main: every keploy binary builds its commands with Root, but not
	// every one of them runs this repository's main — the enterprise binary
	// has its own — and a finalizer runs when the command finishes however it
	// finishes, error and panic included.
	removeAgentTokenOnce.Do(func() {
		cobra.OnFinalize(func() {
			if err := token.Cleanup(); err != nil {
				utils.LogError(logger, err, "failed to remove the agent control-plane token directory")
			}
		})
	})

	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: provider.RootExamples,
		Version: utils.Version,
		PreRun: func(cmd *cobra.Command, _ []string) {
			disableAnsi, _ := cmd.Flags().GetBool("disable-ansi")
			provider.PrintLogo(log.PrimarySink(), disableAnsi)
		},
	}

	defaultHelpFunc := rootCmd.HelpFunc()

	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		disableAnsi, _ := cmd.Flags().GetBool("disable-ansi")
		provider.PrintLogo(log.PrimarySink(), disableAnsi)

		// Use the default help function instead of calling the parent's HelpFunc
		defaultHelpFunc(cmd, args)
	})

	rootCmd.CompletionOptions.DisableDefaultCmd = true

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
