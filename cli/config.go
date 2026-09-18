package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/config"

	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func init() {
	Register("config", Config)
}

func Config(ctx context.Context, logger *zap.Logger, cfg *config.Config, servicefactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "config",
		Short: "manage keploy configuration file",
		// The root silences cobra's errors, so a returned error reached the
		// user as a flag dump and nothing else -- including the one that
		// explains why Keploy would not write through their symlink.
		SilenceUsage: true,
		Example: `  keploy config --generate                  # write keploy.yml with only what differs from the defaults
  keploy config --generate --path ./svc     # ...somewhere else
  keploy config --generate --force          # ...over one that already exists
  keploy config defaults                    # print every setting and its default
  keploy config defaults -o defaults.yml    # save them all, to read or edit`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			isGenerate, err := cmd.Flags().GetBool("generate")
			if err != nil {
				utils.LogError(logger, err, "failed to get generate flag")
				return err
			}

			if isGenerate {
				// The flag, not cfg.Path. Two config.Config values exist --
				// root.go allocates its own and hands it to every command
				// constructor, while ValidateFlags writes into the other --
				// so cfg.Path here is permanently "", and `--path ./svc`
				// wrote to the current directory instead. That is a silent
				// overwrite of the wrong file, so read the flag directly.
				where := cfg.Path
				if flagPath, ferr := cmd.Flags().GetString("path"); ferr == nil && flagPath != "" {
					where = flagPath
				}
				filePath := filepath.Join(where, "keploy.yml")
				force, ferr := cmd.Flags().GetBool("force")
				if ferr != nil {
					// Not silently "not forced": a flag that has been renamed
					// or lost its registration would quietly start refusing
					// every regenerate, and the message would blame the file.
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", ferr)
					return ferr
				}
				// WHICH file is about to be replaced -- resolved the same way
				// the writer resolves it, so consent is asked about the file
				// that will actually be opened. Asking about the path as
				// typed is how a keploy.yml that is a link to the
				// developer's hand-written config was overwritten without a
				// word: the link was treated as "not a file anyone wants
				// kept", while the writer followed it and replaced what was
				// on the other end.
				target, terr := toolsSvc.ResolveConfigTarget(filePath)
				if terr != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", terr)
					return terr
				}
				// Consent to overwrite, and the three ways it can be settled.
				//
				// The gate used to read cfg.InCi, which is the SAME dead
				// config.Config the --path fix above documents: root.go
				// allocates one and ValidateFlags writes into the other, so
				// InCi is permanently false -- and `config` never registered
				// --in-ci anyway, so there was no way to set it. In CI the
				// prompt was therefore always asked, stdin answered EOF,
				// AskForConfirmation declined, and the command logged
				// "Skipping" and exited 0. A pipeline step that regenerates
				// the config recorded a success for having done nothing.
				//
				// So: --force settles it outright; a human's "no" is a real
				// answer and a real success; and an EOF -- nobody there to
				// ask -- REFUSES out loud and names the flag that would have
				// let it through. Silence is the one answer it must not give.
				// EOF is the signal, not a TTY check: CI runners that
				// allocate a pty look interactive and still answer EOF.
				if !force && utils.CheckFileExists(target) {
					override, err := utils.AskForConfirmation(ctx, "Config file already exists. Do you want to override it?")
					if errors.Is(err, utils.ErrNoAnswer) {
						err = fmt.Errorf("%s already exists and there was nobody to confirm overwriting it; re-run with --force", target)
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
						return err
					}
					if err != nil {
						utils.LogError(logger, err, "failed to ask for confirmation")
						return err
					}
					if !override {
						logger.Info("Skipping config file override")
						return nil
					}
				}
				// The service lookup stays, and not for the service: it is
				// where every command pings telemetry, and `config
				// --generate` is an onboarding step. Dropping it made this
				// the one command in the CLI that reported nothing. The
				// write itself needs no dependencies -- the tools service
				// was resolved only to reach a method that never used its
				// own receiver -- so a lookup that fails is not fatal here.
				if _, svcErr := servicefactory.GetService(ctx, cmd.Name()); svcErr != nil {
					logger.Debug("could not resolve the service for telemetry", zap.Error(svcErr))
				}
				// Only what a developer decided reaches the file; the rest
				// is a default, and `keploy config defaults` prints it.
				// The RESOLVED path, not the one typed. Handing the writer
				// filePath made it resolve a second time, so the file the
				// user was asked about was not guaranteed to be the file
				// written -- and it doubled the window a link planted between
				// the two could slip through.
				if err := toolsSvc.WriteMinimalConfig(logger, target, ""); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
					return err
				}
				logger.Info("Config file generated successfully")
				return nil
			}
			err = errors.New("only generate flag is supported in the config command")
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n\n", err)
			_ = cmd.Usage()
			return err
		},
	}
	cmd.AddCommand(configDefaults(logger))

	if err := cmdConfigurator.AddFlags(cmd); err != nil {
		utils.LogError(logger, err, "failed to add flags")
		return nil
	}
	return cmd
}

// configDefaults builds `keploy config defaults`.
//
// A generated keploy.yml now carries only what a developer chose, which is
// the point -- but the defaults it leaves out must stay READABLE, or the
// short file just moves the question somewhere nobody can answer it. This
// prints them, and pipes or saves them for anyone who wants the long form:
//
//	keploy config defaults                  # read them
//	keploy config defaults -o keploy.yml    # start from them
//	keploy config defaults | grep -i proxy  # find one
func configDefaults(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "defaults",
		Short: "Print Keploy's default configuration",
		Long: `Print the settings Keploy applies when your keploy.yml does not mention them.

The internally-managed ` + "`agent`" + ` block is not included: Keploy resolves it per
run, and a copy of it in a repository would only go stale.

A generated keploy.yml holds only the settings that differ from these, so this
is where to look for the rest -- and where to start if you would rather edit a
complete file by hand.`,
		Example: `  keploy config defaults
  keploy config defaults -o keploy.defaults.yml
  keploy config defaults | grep -i port`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defaults, err := config.DefaultsYAML()
			if err != nil {
				utils.LogError(logger, err, "failed to read the default config")
				return err
			}
			// The version comment, not the generated-config header: this is
			// the full document, so the header's "only what differs" would
			// be false about the very thing it tops.
			doc := utils.GetVersionAsComment() + defaults
			savedHeader := `# Every setting Keploy applies when your keploy.yml does not mention it.
#
# Keeping all of this in a repository FREEZES today's defaults there: a value
# Keploy improves later will not reach you. A generated keploy.yml carries
# only what you changed (keploy config --generate) for that reason. Copy the
# lines you want to change, not the file.
`

			out, err := cmd.Flags().GetString("output")
			if err != nil {
				utils.LogError(logger, err, "failed to get the output flag")
				return err
			}
			force, err := cmd.Flags().GetBool("force")
			if err != nil {
				utils.LogError(logger, err, "failed to get the force flag")
				return err
			}
			if out == "" {
				_, err = fmt.Fprint(cmd.OutOrStdout(), doc)
				return err
			}
			// Only on the SAVED copy: a printed one is being read, and a
			// piped one is being grepped.
			doc = utils.GetVersionAsComment() + savedHeader + defaults
			// Never over an existing file. The obvious target is keploy.yml,
			// and 134 lines of defaults written over a developer's own
			// config would take their command with it -- silently, exit 0.
			// `keploy config --generate` asks before it overwrites; this is
			// pipeable and scriptable, so it refuses and names the way past.
			// A symlink, EVER. --force means "overwrite the file I named",
			// not "write through a link to one I did not"; the non-force
			// branch refused a dangling link and --force wrote straight
			// through it to a path outside the project.
			if info, lErr := os.Lstat(out); lErr == nil && info.Mode()&os.ModeSymlink != 0 {
				err := fmt.Errorf("%s is a symbolic link; Keploy will not write through it -- remove the link, or choose another name", out)
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
				return err
			}
			// Lstat, not Stat: Stat follows a symlink, so `-o link.yml` wrote
			// through a DANGLING link to a file the user never named, with
			// no refusal at all.
			if _, statErr := os.Lstat(out); statErr == nil && !force {
				err := fmt.Errorf("%s already exists; pass --force to overwrite it, or choose another name", out)
				// Printed, not only returned: the root silences cobra's own
				// error output, so a returned error alone reaches the user
				// as a bare rule and a non-zero exit.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
				return err
			}
			// 0644, and chmod after the write: os.WriteFile does not change
			// the mode of a file that already exists, so `-o keploy.yml
			// --force` left a world-writable config world-writable -- on the
			// path this command's own help recommends.
			if err := os.WriteFile(out, []byte(doc), 0644); err != nil {
				utils.LogError(logger, err, "failed to write the default config",
					zap.String("path", out),
					zap.String("next_step", "check that the directory exists and is writable, then run the command again"))
				return err
			}
			// The group and world WRITE bits only: forcing 0644 would widen a
			// file somebody had deliberately restricted. And a failure here
			// is the failure: os.WriteFile leaves an existing file's mode
			// alone, so reporting success over it defeats the hardening on
			// exactly the upgrade path it targets.
			if err := toolsSvc.RestrictConfigMode(out); err != nil {
				utils.LogError(logger, err, "failed to set the permission of the defaults file", zap.String("path", out))
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: wrote %s but could not set its permissions: %v\n", out, err)
				return err
			}
			utils.RestoreFileOwnership(logger, out)
			logger.Info("Default configuration written", zap.String("path", out))
			return nil
		},
	}
	cmd.Flags().StringP("output", "o", "", "write to this file instead of standard output")
	cmd.Flags().Bool("force", false, "overwrite the output file if it already exists")
	return cmd
}
