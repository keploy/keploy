package updateBinary

import (
	"os"
	"os/exec"

	"go.keploy.io/server/utils"
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
	currentVersion := utils.KeployVersion
	latestVersion := "1.2.3" // Replace this with your logic to fetch the latest version

	if currentVersion == latestVersion {
		u.logger.Info("No updates available. Version " + latestVersion + " is the latest.")
		return
	}

	// Execute the curl command to download keploy.sh and run it with bash
	curlCommand := `curl -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && bash keploy.sh`
	//Maybe dont use the one click installation, seems to be buggy
	//Either way you will have to ask user to confirm update
	//How will we get the version of the newly downloaded file btw?
	//Can we download it somewhere else, then check and update, if update not required just delete
	//WE HAVE TO GET THIS PR DONE BY TONIGHT
	// Execute the combined curl command to download and execute keploy.sh with bash
	cmd := exec.Command("sh", "-c", curlCommand)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		u.logger.Error("Failed to download and execute keploy.sh", zap.Error(err))
		return
	}

	u.logger.Info("Upadated Keploy binary to version " + latestVersion + ".")
	// Add logic here if necessary after sourcing keploy.sh
}
