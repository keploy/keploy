package cmd

import (
	"github.com/spf13/cobra"
	update "go.keploy.io/server/pkg/service/update"
	"go.uber.org/zap"
)

// NewCmdUpdate initializes a new command to update the Keploy binary file.
func NewCmdUpdateBinary(logger *zap.Logger) *Update {
	updater := update.NewUpdater(logger)
	return &Update{
		updater: updater,
		logger:  logger,
	}
}

// Update holds the updater instance for managing binary updates.
type Update struct {
	updater update.Updater
	logger  *zap.Logger
}

// GetCmd retrieves the command to update Keploy
func (u *Update) GetCmd() *cobra.Command {
	var updateBinaryCmd = &cobra.Command{
		Use:     "update",
		Short:   "Update Keploy ",
		Example: "keploy update",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logic to check and update the binary file using the updater
			u.updater.Update()
			return nil
		},
	}
	return updateBinaryCmd
}
