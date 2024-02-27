package cli

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/config"
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
				logger.Error("failed to read the customSetup flag")
				return err
			}
			if customSetup {
				fmt.Println(examples)
				return nil
			}
			fmt.Println(exampleOneClickInstall)
			fmt.Println(withoutexampleOneClickInstall)
			return nil
		},
	}
	cmd.SetHelpTemplate(customHelpTemplate)

	cmd.Flags().Bool("customSetup", customSetup, "Check if the user is using one click install")

	return cmd
}
