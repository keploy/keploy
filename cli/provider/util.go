package provider

import (
	"errors"

	"go.keploy.io/server/v2/utils"
)

func (c *CmdConfigurator) noCommandError() error {
	utils.LogError(c.logger, nil, "missing required -c flag or appCmd in config file")
	if c.cfg.InDocker {
		c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
	} else {
		c.logger.Info(LogExample(RootExamples))
	}
	return errors.New("missing required -c flag or appCmd in config file")
}

// alreadyRunning checks that during test mode, if user provides the basePath, then it implies that the application is already running somewhere.
func alreadyRunning(cmd, basePath string) bool {
	return (cmd == "test" && basePath != "")
}
