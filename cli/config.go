package cli

import (
	"context"
	"errors"
	"io"
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
		Use:     "config",
		Short:   "manage keploy configuration file",
		Example: "keploy config --generate --path /path/to/localdir",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			isGenerate, err := cmd.Flags().GetBool("generate")
			if err != nil {
				utils.LogError(logger, err, "failed to get generate flag")
				return err
			}
			force, err := cmd.Flags().GetBool("force")
			if err != nil {
				utils.LogError(logger, err, "failed to get force flag")
				return err
			}

			if isGenerate {
				filePath := filepath.Join(cfg.Path, "keploy.yml")
				if !force && !cfg.InCi && utils.CheckFileExists(filePath) {
					override, err := utils.AskForConfirmation(ctx, "Config file already exists. Do you want to override it?")
					if err != nil {
						// EOF means nothing answered the prompt — no terminal and
						// nothing piped in. That is every scripted invocation: CI,
						// Makefile, `docker run` without -i, systemd, cron. Failing
						// there blames the caller for something they cannot fix, and
						// overwriting unasked would silently discard a hand-edited
						// config. Keep the file, say so, and point at --force.
						if errors.Is(err, io.EOF) {
							logger.Warn("config file already exists and nothing answered the override prompt, so it was left untouched",
								zap.String("path", filePath),
								zap.String("hint", "re-run with --force to overwrite it non-interactively"))
							return nil
						}
						utils.LogError(logger, err, "failed to ask for confirmation")
						return err
					}
					if !override {
						logger.Info("Skipping config file override")
						return nil
					}
				}
				svc, err := servicefactory.GetService(ctx, cmd.Name())
				if err != nil {
					utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
					return err
				}
				var tools toolsSvc.Service
				var ok bool
				if tools, ok = svc.(toolsSvc.Service); !ok {
					err = errors.New("service doesn't satisfy tools service interface")
					utils.LogError(logger, err, "failed to generate config")
					return err
				}
				if err := tools.CreateConfig(ctx, filePath, ""); err != nil {
					utils.LogError(logger, err, "failed to create config")
					return err
				}
				logger.Info("Config file generated successfully")
				return nil
			}
			return errors.New("only generate flag is supported in the config command")
		},
	}
	if err := cmdConfigurator.AddFlags(cmd); err != nil {
		utils.LogError(logger, err, "failed to add flags")
		return nil
	}
	return cmd
}
