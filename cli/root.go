package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	armForcedCrash(rootCmd)

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
//
// A tagged error is also left to the binary's main to say (sayUsageError):
// cobra followed it with the command's whole usage text, so `keploy record
// --delay 10` printed every flag record has, some 80 lines, under the one line
// that said what was wrong, and that line scrolled away. Each main says the
// error once, then where the command's help is (`Run 'keploy record --help'
// for usage.`). An error a command returns while it runs is not touched
// here: cobra prints it, and its usage, as it did. A refusal of its PreRunE
// (CmdConfigurator.Validate) is said with no usage text under it, which
// Validate sees to.
func TagUsageErrors(root *cobra.Command) {
	flagErr := root.FlagErrorFunc()
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return sayUsageError(cmd, utils.UsageError(flagErr(cmd, err)))
	})
	if args := root.Args; args != nil {
		root.Args = func(cmd *cobra.Command, a []string) error {
			return sayUsageError(cmd, utils.UsageError(args(cmd, a)))
		}
	}
	for _, c := range root.Commands() {
		TagUsageErrors(c)
	}
}

// sayUsageError hands a usage error raised on cmd to the binary's main to say:
// cobra prints neither the error nor the usage text it would print under it.
// Both mains say an error cobra was silenced for -- this repository's, unless
// the command's flag-error func said it already; the enterprise binary's, for
// a command cobra was silenced for -- and point at `<command> --help`, which
// is where the flags cobra would have listed are. Set on cmd only once the
// error is raised, so nothing else the command does is silenced.
func sayUsageError(cmd *cobra.Command, err error) error {
	if err != nil {
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
	}
	return err
}

// rejectUnknownSubcommand installs the reject-on-unknown-verb RunE on one group,
// and the same refusal for an unknown verb followed by flags: cobra parses the
// group's own flags first, so `keploy cloud request-changes --branch x` failed
// on --branch ("unknown flag: --branch"), which sent a coding agent looking for
// a flag when what it had wrong was the command.
func rejectUnknownSubcommand(c *cobra.Command) {
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return UnknownSubcommand(cmd, args[0])
	}
	// The func c answered flag errors with before: its own, or the one it
	// inherits. It still says every flag error that is not an unknown verb.
	prev := c.FlagErrorFunc()
	c.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		// pflag keeps the arguments it read before the flag it failed on.
		if args := cmd.Flags().Args(); cmd == c && len(args) > 0 {
			return UnknownSubcommand(cmd, args[0])
		}
		// An error raised on a command under c, which reaches this func by
		// inheritance, is not an unknown verb of c's.
		return prev(cmd, err)
	})
}

// maxSubcommandHints bounds the commands an unknown verb's error names.
const maxSubcommandHints = 5

// suggestionDistance is the edit distance cobra suggests within when a
// command sets none (cobra's findSuggestions uses the same 2).
const suggestionDistance = 2

// UnknownSubcommand is group's refusal of verb, naming the commands under
// group at any depth whose name is verb or close to it (cobra's own
// suggestions, made at each level), exact names first: `keploy cloud
// request-changes` points at `keploy cloud review request-changes`, which
// cobra's suggestions, one level deep and made only for the root, never
// reached. None when suggestions are disabled. Its message starts as cobra's
// own does (`unknown command "verb" for "path"`), which utils.IsUsageError
// reads as a usage error.
func UnknownSubcommand(group *cobra.Command, verb string) error {
	type hint struct {
		path  string
		exact bool
	}
	var hints []hint
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.DisableSuggestions {
			return
		}
		// SuggestionsFor reads the distance from the command; at 0 it matches
		// only an exact name, a prefix or an explicit SuggestFor. Set as cobra
		// would, then put back: the commands are shared.
		if d := c.SuggestionsMinimumDistance; d <= 0 {
			c.SuggestionsMinimumDistance = suggestionDistance
			defer func() { c.SuggestionsMinimumDistance = d }()
		}
		for _, s := range c.SuggestionsFor(verb) {
			hints = append(hints, hint{c.CommandPath() + " " + s, s == verb})
		}
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() {
				walk(sub)
			}
		}
	}
	if !group.Root().DisableSuggestions {
		walk(group)
	}
	sort.SliceStable(hints, func(i, j int) bool { return hints[i].exact && !hints[j].exact })
	if len(hints) > maxSubcommandHints {
		hints = hints[:maxSubcommandHints]
	}
	msg := fmt.Sprintf("unknown command %q for %q", verb, group.CommandPath())
	if len(hints) > 0 {
		paths := make([]string, len(hints))
		for i, h := range hints {
			paths[i] = h.path
		}
		msg += "\n\nDid you mean this?\n\t" + strings.Join(paths, "\n\t")
	}
	return errors.New(msg)
}

// unknownRootCommand is cobra's refusal of an unknown top-level command:
// `unknown command "verb" for "keploy"`, then its own one-level suggestions.
var unknownRootCommand = regexp.MustCompile(`^unknown command ("(?:[^"\\]|\\.)*") for ("(?:[^"\\]|\\.)*")`)

// HintUnknownCommand is err, or, when err is cobra's refusal of an unknown
// top-level command of root, the same refusal naming where a command of that
// name, or a near one, lives at any depth (UnknownSubcommand): `keploy
// generate` points at `keploy contract generate`. cobra raises it while it
// finds the command, before any of root's funcs run, so a binary's main calls
// this on the error ExecuteC returns.
func HintUnknownCommand(root *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	m := unknownRootCommand.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	// cobra quoted both with %q.
	verb, verr := strconv.Unquote(m[1])
	path, perr := strconv.Unquote(m[2])
	if verr != nil || perr != nil || path != root.CommandPath() {
		return err
	}
	return UnknownSubcommand(root, verb)
}
