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
	var updateBinaryCmd = &cobra.Command{
		Use:     "update",
		Short:   "update the Keploy binary file",
		Example: "keploy update --path /path/to/localdir",
		RunE: func(cmd *cobra.Command, args []string) error {

			binaryPath, err := cmd.Flags().GetString("path")
			fmt.Println("binaryPath is" + binaryPath) //prints . is that a good thing?
			if err != nil {
				u.logger.Error("failed to read the binary path")
				return err
			}

			binaryFilePath := filepath.Join(binaryPath, "keploy-binary")

			// Logic to check and update the binary file using the updater
			u.updater.UpdateBinary(binaryFilePath)

			return nil
		},
	}

	updateBinaryCmd.Flags().StringP("path", "p", ".", "Path to the local directory where Keploy binary file will be stored")

	return updateBinaryCmd
}
