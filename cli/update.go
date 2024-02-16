package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/config"
	"go.uber.org/zap"
)

func init() {
	Register("tools", Update)
}

// Update retrieves the command to tools Keploy
func Update(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	var updateCmd = &cobra.Command{
		Use:     "tools",
		Short:   "Update Keploy ",
		Example: "keploy tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logic to check and tools the binary file using the updater
			return svc.Updater.Update(ctx)
		},
	}
	return updateCmd
}
