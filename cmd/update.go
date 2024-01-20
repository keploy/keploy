package cmd

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/update" // Import the updateBinary package
	"go.uber.org/zap"
)

// NewCmdUpdateBinary initializes a new command to update the Keploy binary file.
func NewCmdUpdateBinary(logger *zap.Logger) *UpdateBinary {
	updater := updateBinary.NewUpdater(logger)
	return &UpdateBinary{
		updater: updater,
		logger:  logger,
	}
}

// UpdateBinary holds the updater instance for managing binary updates.
type UpdateBinary struct {
	updater updateBinary.Updater
	logger  *zap.Logger
}

// GetCmd retrieves the command to update the Keploy binary file.
func (u *UpdateBinary) GetCmd() *cobra.Command {
	var updateBinaryCmd = &cobra.Command{
		Use:     "update",
		Short:   "update the Keploy binary file",
		Example: "keploy update",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logic to check and update the binary file using the updater
			u.updater.UpdateBinary()
			return nil
		},
	}
	return updateBinaryCmd
}
