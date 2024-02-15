package cmd

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/config"
	"go.uber.org/zap"
)

func init() {
	Register("update", Update)
}

// Update retrieves the command to update Keploy
func Update(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	var updateCmd = &cobra.Command{
		Use:     "update",
		Short:   "Update Keploy ",
		Example: "keploy update",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logic to check and update the binary file using the updater
			return svc.Updater.Update(ctx)
		},
	}
	return updateCmd
}
