package utils

import (
	"fmt"
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func CheckPath(logger *zap.Logger, conf *config.Config, curDir string) error {
	var err error
	if strings.Contains(conf.Test.Path, "..") || strings.HasPrefix(conf.Test.Path, "/") {
		conf.Test.Path, err = filepath.Abs(filepath.Clean(conf.Test.Path))
		if err != nil {
			return logError(logger, "failed to get the absolute path", conf.Test.Path, err)
		}

		relativePath, err := filepath.Rel(curDir, conf.Test.Path)
		if err != nil {
			return logError(logger, "failed to get the relative path", conf.Test.Path, err)
		}

		if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
			return logError(logger, "path provided is not a subdirectory of current directory", conf.Test.Path, nil)
		}

		if strings.HasPrefix(conf.Test.Path, "/") {
			currentDir, err := getCurrentDirInDocker(curDir)
			if err != nil {
				return logError(logger, "failed to get the current directory path in docker", conf.Test.Path, err)
			}

			if !strings.HasPrefix(conf.Test.Path, currentDir) {
				return logError(logger, "path provided is not a subdirectory of current directory", conf.Test.Path, nil)
			}

			conf.Test.Path, err = filepath.Rel(currentDir, conf.Test.Path)
			if err != nil {
				return logError(logger, "failed to get the relative path for the subdirectory", conf.Test.Path, err)
			}
		}
	}
	return nil
}

func logError(logger *zap.Logger, message, path string, err error) error {
	logger.Error(message, zap.Error(err), zap.String("path:", path))
	return fmt.Errorf("%s: %v", message, err)
}

func getCurrentDirInDocker(curDir string) (string, error) {
	getDir := fmt.Sprintf(`docker inspect keploy-v2 --format '{{ range .Mounts }}{{ if eq .Destination "%s" }}{{ .Source }}{{ end }}{{ end }}'`, curDir)
	cmd := exec.Command("sh", "-c", getDir)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// StartInDocker will check if the docker command is provided as an input
// then start the Keploy as a docker container and run the command
// should also return a boolean if the execution is moved to docker
func StartInDocker(logger *zap.Logger, conf *config.Config) error {
	//Check if app command starts with docker or  docker-compose.
	// If it does, then we would run the docker version of keploy and
	// pass the command and control to it.
	dockerRelatedCmd := IsDockerRelatedCmd(conf.Command)
	if conf.InDocker || !dockerRelatedCmd {
		return nil
	}
	// pass the all the commands and args to the docker version of Keploy
	err := RunInDocker(logger, strings.Join(os.Args[1:], " "))
	if err != nil {
		logger.Error("failed to run the test command in docker", zap.Error(err))
		return err
	}
	// gracefully exit the current process
	logger.Info("exiting the current process as the command is moved to docker")
	os.Exit(0)
	return nil
}
