package utils

import (
	"context"
	"os"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

// StartInDocker will check if the docker command is provided as an input
// then start the Keploy as a docker container and run the command
// should also return a boolean if the execution is moved to docker
func StartInDocker(ctx context.Context, logger *zap.Logger, conf *config.Config) error {
	//Check if app command starts with docker or docker-compose.
	// If it does, then we would run the docker version of keploy and
	// pass the command and control to it.
	cmdType := FindDockerCmd(conf.Command)
	if conf.InDocker || !(IsDockerCmd(cmdType)) {
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
