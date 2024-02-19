package fs

import (
	"os"
	"runtime"
)

func FetchHomeDirectory(isNewConfigPath bool) string {
	var configFolder = "/.keploy-config"

	if isNewConfigPath {
		configFolder = "/.keploy"
	}

	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + configFolder
	}

	return os.Getenv("HOME") + configFolder
}
