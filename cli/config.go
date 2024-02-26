package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v2/config"

	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func init() {
	Register("config", Config)
}

func Config(ctx context.Context, logger *zap.Logger, cfg *config.Config, servicefactory ServiceFactory, cmdConfiguration CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "config",
		Short:   "manage keploy configuration file",
		Example: "keploy config --generate --path /path/to/localdir",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// override root command's persistent pre run
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {

			isGenerate, err := cmd.Flags().GetBool("generate")
			if err != nil {
				logger.Fatal("Failed to get generate flag", zap.Error(err))
				return err
			}

			if isGenerate {
				filePath := filepath.Join(cfg.Path, "keploy.yml")
				if utils.CheckFileExists(filePath) {
					override, err := utils.AskForConfirmation("Config file already exists. Do you want to override it?")
					if err != nil {
						logger.Fatal("Failed to ask for confirmation", zap.Error(err))
						return err
					}
					if !override {
						return nil
					}
				}
				svc, err := servicefactory.GetService(cmd.Name(), *cfg)
				if err != nil {
					return err
				}
				if tools, ok := svc.(toolsSvc.Service); !ok {
					return fmt.Errorf("record is not of type MyInterface")
				} else {
					tools.CreateConfig(ctx, cfg.Path, "")
				}
			}
			return nil
		},
	}
	cmdConfiguration.AddFlags(cmd, cfg)
	return cmd
}
