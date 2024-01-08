package updateBinary

import (
	"fmt"
	"os"
	"os/exec"

	"go.uber.org/zap"
)

type updater struct {
	logger *zap.Logger
}

func NewUpdater(logger *zap.Logger) Updater {
	return &updater{
		logger: logger,
	}
}

type Updater interface {
	UpdateBinary(binaryFilePath string)
}

func (u *updater) UpdateBinary(binaryFilePath string) {
	// Your logic to update the binary file
	// For example:
	// ...

	// Simulated logic to update the binary file
	_, err := os.Create(binaryFilePath)
	if err != nil {
		u.logger.Fatal("Failed to create/update binary file", zap.Error(err))
		return
	}

	cmd := exec.Command("sudo", "chmod", "-R", "777", binaryFilePath)
	err = cmd.Run()
	if err != nil {
		u.logger.Error("Failed to set the permission of binary file", zap.Error(err))
		return
	}
	u.logger.Info(fmt.Sprintf("Binary file updated successfully at %s", binaryFilePath))
	u.logger.Info("Binary file updated successfully")
	// ...
}
