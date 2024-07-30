package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("agent", GenerateUT)
}

func Agent(_ context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, _ CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:    "example",
		Short:  "Example to record and test via keploy",
		Hidden: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// validate the flags

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			customSetup, err := cmd.Flags().GetBool("customSetup")
			if err != nil {
				utils.LogError(logger, nil, "failed to read the customSetup flag")
				return err
			}
			if customSetup {
				fmt.Println(provider.Examples)
				return nil
			}
			fmt.Println(provider.ExampleOneClickInstall)
			fmt.Println(provider.WithoutexampleOneClickInstall)
			return nil
		},
	}

	return cmd
}
