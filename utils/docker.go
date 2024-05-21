package utils

import (
	"context"
	"os"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

//func CheckPath(logger *zap.Logger, conf *config.Config, currDir string) error {
//	var err error
//	if strings.Contains(conf.Path, "..") || strings.HasPrefix(conf.Path, "/") {
//		conf.Path, err = filepath.Abs(filepath.Clean(conf.Path))
//		if err != nil {
//			return logError(logger, "failed to get the absolute path", conf.Path, err)
//		}
//
//		relativePath, err := filepath.Rel(currDir, conf.Path)
//		if err != nil {
//			return logError(logger, "failed to get the relative path", conf.Path, err)
//		}
//
//		if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
//			return logError(logger, "path provided is not a subdirectory of current directory", conf.Path, nil)
//		}
//
//		if strings.HasPrefix(conf.Path, "/") {
//			currentDir, err := getCurrentDirInDocker(curDir)
//			if err != nil {
//				return logError(logger, "failed to get the current directory path in docker", conf.Path, err)
//			}
//
//			if !strings.HasPrefix(conf.Path, currentDir) {
//				return logError(logger, "path provided is not a subdirectory of current directory", conf.Path, nil)
//			}
//
//			conf.Path, err = filepath.Rel(currentDir, conf.Path)
//			if err != nil {
//				return logError(logger, "failed to get the relative path for the subdirectory", conf.Path, err)
//			}
//		}
//	}
//	return nil
//}

// func logError(logger *zap.Logger, message, path string, err error) error {
// 	LogError(logger, err, message, zap.String("path:", path))
// 	return fmt.Errorf("%s: %v", message, err)
// }

// TODO: Use inbuilt functions rather than executing cmd whereever possible
//func getCurrentDirInDocker(curDir string) (string, error) {
//	getDir := fmt.Sprintf(`docker inspect keploy-v2 --format '{{ range .Mounts }}{{ if eq .Destination "%s" }}{{ .Source }}{{ end }}{{ end }}'`, curDir)
//	cmd := exec.Command("sh", "-c", getDir)
//	out, err := cmd.Output()
//	if err != nil {
//		return "", err
//	}
//	return strings.TrimSpace(string(out)), nil
//}

// StartInDocker will check if the docker command is provided as an input
// then start the Keploy as a docker container and run the command
// should also return a boolean if the execution is moved to docker
func StartInDocker(ctx context.Context, logger *zap.Logger, conf *config.Config) error {
	//Check if app command starts with docker or docker-compose.
	// If it does, then we would run the docker version of keploy and
	// pass the command and control to it.
	cmdType := FindDockerCmd(conf.Command)
	if conf.InDocker || !(IsDockerKind(cmdType)) {
		return nil
	}
	// pass the all the commands and args to the docker version of Keploy
	err := RunInDocker(ctx, logger)
	if err != nil {
		LogError(logger, err, "failed to run the command in docker")
		return err
	}
	// gracefully exit the current process
	logger.Info("exiting the current process as the command is moved to docker")
	os.Exit(0)
	return nil
}
