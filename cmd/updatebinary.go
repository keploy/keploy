package cmd

import (
	"fmt"
	"path/filepath"

	"go.keploy.io/server/pkg/service/updateBinary" // Import the updateBinary package

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdUpdateBinary(logger *zap.Logger) *UpdateBinary {
	updater := updateBinary.NewUpdater(logger)
	return &UpdateBinary{
		updater: updater,
		logger:  logger,
	}
}

type UpdateBinary struct {
	updater updateBinary.Updater
	logger  *zap.Logger
}

func (u *UpdateBinary) GetCmd() *cobra.Command {
	var binaryPath string // declare binaryPath outside of the RunE function scope

	var updateBinaryCmd = &cobra.Command{
		Use:     "update",
		Short:   "update the Keploy binary file",
		Example: "keploy update --path /path/to/localdir",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Access the binaryPath value from the parent scope
			binaryFilePath := filepath.Join(binaryPath, "keploybin")

			// Logic to check and update the binary file using the updater
			u.updater.UpdateBinary(binaryFilePath)

			return nil
		},
	}

	updateBinaryCmd.Flags().StringVarP(&binaryPath, "path", "p", ".", "Path to the local directory where Keploy binary file will be stored")
	updateBinaryCmd.MarkFlagRequired("path") // Mark the path flag as required

	// Validate the flag before executing the command
	updateBinaryCmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if binaryPath == "" {
			return fmt.Errorf("path is required")
		}
		return nil
	}

	return updateBinaryCmd
}
