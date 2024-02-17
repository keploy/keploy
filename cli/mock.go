package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/config"
	"go.uber.org/zap"
)

func init() {
	// Register the command hook
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	// mock the keploy testcases/mocks for the user application
	var cmd = &cobra.Command{
		Use:     "mock",
		Short:   "Record and replay ougoung network traffic for the user application",
		Example: `keploy mock -c "/path/to/user/app" --delay 10`,
		RunE: func(cmd *cobra.Command, args []string) error {

			return nil
		},
	}

	cmd.Flags().BoolP("record", "r", false, "Record all outgoing network traffic")
	cmd.Flags().Lookup("record").NoOptDefVal = "true"

	cmd.Flags().BoolP("replay", "p", true, "Intercept all outgoing network traffic and replay the recorded traffic")
	cmd.Flags().Lookup("replay").NoOptDefVal = "true"

	cmd.Flags().StringP("name", "-n", "", "Name of the mock")
	cmd.MarkFlagRequired("name")

	return cmd
}
