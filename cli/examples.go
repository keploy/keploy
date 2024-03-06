package cli

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"

	"github.com/spf13/cobra"
)

func init() {
	Register("example", Example)
}

func Example(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdcmdConfigurator CmdConfigurator) *cobra.Command {
	var customSetup bool
	var cmd = &cobra.Command{
		Use:   "example",
		Short: "Example to record and test via keploy",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.SetHelpTemplate(provider.CustomHelpTemplate)

	cmd.Flags().Bool("customSetup", customSetup, "Check if the user is using one click install")

	return cmd
}
