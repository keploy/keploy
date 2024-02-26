package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var rootCustomHelpTemplate = `{{.Short}}

Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableLocalFlags}}

Guided Commands:{{range .Commands}}{{if and (not .IsAvailableCommand) (not .Hidden)}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Examples:
{{.Example}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

var rootExamples = `
  Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --containerName "<containerName>" --delay 1 --buildDelay 1m

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 1 --buildDelay 1m

  Generate-Config:
	keploy generate-config -p "/path/to/localdir"
`

var versionTemplate = `{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`

type cmdConfigurator struct {
	logger *zap.Logger
}

func NewCmdConfigurator(logger *zap.Logger) *cmdConfigurator {
	return &cmdConfigurator{
		logger: logger,
	}
}

func (c *cmdConfigurator) AddFlags(cmd *cobra.Command, cfg *config.Config) error {
	var configPath string
	var err error
	switch cmd.Name() {
	case "update", "config":
		return nil
	case "record", "test":
		cmd.Flags().Bool("debug", cfg.Debug, "Run in debug mode")
		cmd.Flags().Bool("telemetry", cfg.Telemetry, "Run in telemetry mode")
		cmd.Flags().String("configPath", cfg.ConfigPath, "Path to the local directory where keploy configuration file is stored")
		cmd.Flags().StringP("path", "p", cfg.Path, "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Uint32("port", cfg.Port, "GraphQL server port used for executing testcases in unit test library integration")
		cmd.Flags().Uint32("proxyPort", cfg.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().StringP("command", "c", cfg.Command, "Command to start the user application")
		cmd.Flags().DurationP("buildDelay", "bd", cfg.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().String("containerName", cfg.ContainerName, "Name of the application's docker container")
		cmd.Flags().StringP("networkName", "n", cfg.NetworkName, "Name of the application's docker network")
		cmd.Flags().UintSlice("bypassPorts", config.GetByPassPorts(cfg), "Ports to bypass the proxy server and ignore the traffic")
		configPath, err = cmd.Flags().GetString("config-path")
		if err != nil {
			c.logger.Error("failed to read the config path")
			return err
		}
		err = cmd.Flags().MarkHidden("telemetry")
		if err != nil {
			errMsg := "failed to mark telemetry as hidden flag"
			c.logger.Error(errMsg, zap.Error(err))
			return errors.New(errMsg)
		}
		err = cmd.Flags().MarkHidden("port")
		if err != nil {
			errMsg := "failed to mark port as hidden flag"
			c.logger.Error(errMsg, zap.Error(err))
			return errors.New(errMsg)
		}
		err = viper.BindPFlags(cmd.Flags())
		if err != nil {
			errMsg := "failed to bind flags to config"
			c.logger.Error(errMsg, zap.Error(err))
			return errors.New(errMsg)
		}
		if cmd.Name() == "test" {
			cmd.Flags().StringSliceP("testsets", "t", utils.Keys(cfg.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
			cmd.Flags().Uint64P("delay", "d", cfg.Test.Delay, "User provided time to run its application")
			cmd.Flags().Uint64("apiTimeout", cfg.Test.ApiTimeout, "User provided timeout for calling its application")
			cmd.Flags().String("mongoPassword", cfg.Test.MongoPassword, "Authentication password for mocking MongoDB conn")
			cmd.Flags().String("coverageReportPath", cfg.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")
			cmd.Flags().StringP("language", "l", cfg.Test.Language, "application programming language")
			cmd.Flags().Bool("ignoreOrdering", cfg.Test.IgnoreOrdering, "Ignore ordering of array in response")
			cmd.Flags().Bool("coverage", cfg.Test.Coverage, "Enable coverage reporting for the testcases. for golang please set language flag to golang, ref https://keploy.io/docs/server/sdk-installation/go/")
			utils.BindFlagsToViper(c.logger, cmd, "")
		}
		return nil
	case "keploy":
		cmd.PersistentFlags().Bool("debug", cfg.Debug, "Run in debug mode")
		viper.BindPFlag("debug", cmd.PersistentFlags().Lookup("debug"))
	default:
		return errors.New("unknown command name")
	}
	if cmd.Name() != "keploy" && configPath != "" {
		viper.SetConfigName("keploy-config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(configPath)
		if err := viper.ReadInConfig(); err != nil {
			errMsg := "failed to read config file"
			c.logger.Error(errMsg, zap.Error(err))
			return errors.New(errMsg)
		}
		if err := viper.Unmarshal(&cfg); err != nil {
			errMsg := "failed to unmarshal the config"
			c.logger.Error(errMsg, zap.Error(err))
			return errors.New(errMsg)
		}
	}
	return nil
}

func (c *cmdConfigurator) GetHelpTemplate() string {
	return rootCustomHelpTemplate
}

func (c *cmdConfigurator) GetExampleTemplate() string {
	return rootExamples
}

func (c *cmdConfigurator) GetVersionTemplate() string {
	return versionTemplate
}

func (c cmdConfigurator) ValidateFlags(cmd *cobra.Command, cfg *config.Config) error {
	switch cmd.Name() {
	case "record", "test":

		bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
		if err != nil {
			c.logger.Error("failed to read the ports of outgoing calls to be ignored")
			return err
		}
		config.SetByPassPorts(cfg, bypassPorts)

		if cfg.Command == "" {
			c.logger.Error("Couldn't find the application command to test")
			if cfg.InDocker {
				c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
			} else {
				c.logger.Info(LogExample(c.GetExampleTemplate()))
			}
			return errors.New("missing required -c flag or appCmd in config file")
		}

		if cfg.InDocker {
			if len(cfg.Path) > 0 {
				curDir, err := os.Getwd()
				if err != nil {
					c.logger.Error("failed to get current working directory", zap.Error(err))
					return err
				}
				if strings.Contains(cfg.Path, "..") {
					cfg.Path, err = filepath.Abs(filepath.Clean(cfg.Path))
					if err != nil {
						c.logger.Error("failed to get the absolute path from relative path", zap.Error(err), zap.String("path:", cfg.Path))
						return err
					}
					relativePath, err := filepath.Rel(curDir, cfg.Path)
					if err != nil {
						c.logger.Error("failed to get the relative path from absolute path", zap.Error(err), zap.String("path:", cfg.Path))
						return err
					}
					if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
						c.logger.Error("path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories", zap.String("path:", cfg.Path))
						return err
					}
				}
			}
			if cfg.BuildDelay <= 30*time.Second {
				c.logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", cfg.BuildDelay))
				c.logger.Info(`Example usage: keploy record -c "docker-compose up --build" --buildDelay 35s`)
			}
			if strings.Contains(cfg.Command, "--name") {
				if cfg.ContainerName == "" {
					c.logger.Error("Couldn't find containerName")
					c.logger.Info(`Example usage: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}

		}

		err = utils.StartInDocker(c.logger, cfg)
		if err != nil {
			return err
		}

		absPath, err := filepath.Abs(cfg.Path)
		if err != nil {
			c.logger.Error("failed to get the absolute path from relative path", zap.String("path received", cfg.Path), zap.Error(err))
			return err
		}
		cfg.Path = absPath + "/keploy"
		if cmd.Name() == "test" {
			testSets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				c.logger.Error("failed to get the testsets", zap.Error(err))
				return err
			}
			config.SetSelectedTests(cfg, testSets)
			if cfg.Test.Delay <= 5 {
				c.logger.Warn(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", cfg.Test.Delay))
				if cfg.InDocker {
					c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					c.logger.Info("Example usage: " + cmd.Example)
				}
			}
		}
	}
	return nil
}
