package cli

import (
	"context"
	"fmt"
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

	// Every binary (OSS and enterprise — enterprise builds its root through this
	// same Root) gets unknown-subcommand hardening, and its command-line errors
	// tagged as usage errors, in one place.
	HardenUnknownSubcommands(rootCmd)
	TagUsageErrors(rootCmd)

	return rootCmd
}

// HardenUnknownSubcommands makes every command GROUP under root reject an unknown
// verb instead of printing help and exiting 0. A cobra group with subcommands but
// no Run/RunE is not "runnable", so for a NON-root group `legacyArgs` passes the
// unmatched verb through and cobra falls back to Help() returning nil — so
// `keploy mock bogus` (and `ca`/`contract`/any future group) silently exited 0, a
// false success a CI job calling a mistyped verb would pass on. (The root command
// already errors on an unknown top-level command; only descendant groups need
// this.) A hardened group still shows help and exits 0 when given no verb; a
// valid subcommand still resolves to its own command, so this RunE never runs for
// it. The "unknown command" error maps to utils.ExitUsageError (8) in main's
// exitCodeForCmdErr. Exported so a downstream build can harden commands it adds
// after Root returns. (Design §P0b: an unknown mock verb is a usage error.)
func HardenUnknownSubcommands(root *cobra.Command) {
	for _, c := range root.Commands() {
		if len(c.Commands()) > 0 && c.Run == nil && c.RunE == nil {
			rejectUnknownSubcommand(c)
		}
		HardenUnknownSubcommands(c)
	}
}

// TagUsageErrors tags, as usage errors (utils.UsageError), the errors cobra
// raises while parsing the command line, at the one place each is raised: a
// command's flag-error func (an unknown flag or shorthand, a flag missing its
// value, a value of the wrong type) and its argument validator (the wrong
// number of arguments). Nothing else is tagged, so an error a command returns
// while running stays its own, even one of pflag's types (keploy reading a
// flag it never defined is keploy's bug, not the user's). A command's existing
// flag-error func still runs, and still prints; its error is only tagged.
// Exported so a downstream build can tag commands it adds after Root returns.
func TagUsageErrors(root *cobra.Command) {
	flagErr := root.FlagErrorFunc()
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return utils.UsageError(flagErr(cmd, err))
	})
	if args := root.Args; args != nil {
		root.Args = func(cmd *cobra.Command, a []string) error {
			return utils.UsageError(args(cmd, a))
		}
	}
	for _, c := range root.Commands() {
		TagUsageErrors(c)
	}
}

// rejectUnknownSubcommand installs the reject-on-unknown-verb RunE on one group.
func rejectUnknownSubcommand(c *cobra.Command) {
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
}
