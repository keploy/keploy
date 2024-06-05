package provider

import (
	"errors"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/utils"
)

func isTest(cmd string) bool {
	return cmd == "test"
}

func isRecord(cmd string) bool {
	return cmd == "record"
}

func hasBasePath(path string) bool {
	return path != ""
}

func hasCommand(command string) bool {
	return command != ""
}

func (c *CmdConfigurator) basePathError() error {
	errMsg := "basepath flag is not allowed with the command flag during test mode"
	utils.LogError(c.logger, nil, errMsg)
	return errors.New(errMsg)
}

func (c *CmdConfigurator) noCommandError() error {
	utils.LogError(c.logger, nil, "missing required -c flag or appCmd in config file")
	if c.cfg.InDocker {
		c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
	} else {
		c.logger.Info(LogExample(RootExamples))
	}
	return errors.New("missing required -c flag or appCmd in config file")
}

func (c *CmdConfigurator) handleRunCmd(cmd *cobra.Command) error {
	if isTest(cmd.Name()) {
		if hasBasePath(c.cfg.Test.BasePath) && hasCommand(c.cfg.Command) {
			return c.basePathError()
		}
		if !hasBasePath(c.cfg.Test.BasePath) && !hasCommand(c.cfg.Command) {
			return c.noCommandError()
		}
		return nil
	}

	if isRecord(cmd.Name()) && !hasCommand(c.cfg.Command) {
		return c.noCommandError()
	}
	return nil
}
