// Package provider provides functionality for the keploy provider.\
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/moby/moby/pkg/parsers/kernel"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	"go.uber.org/zap"
)

func LogExample(example string) string {
	return fmt.Sprintf("Example usage: %s", example)
}

var CustomHelpTemplate = `
{{if .Example}}Examples:
{{.Example}}
{{end}}
{{if .HasAvailableSubCommands}}Guided Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{end}}
{{if .HasAvailableFlags}}Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}
{{end}}
Use "{{.CommandPath}} [command] --help" for more information about a command.
`

var WithoutexampleOneClickInstall = `
Note: If installed keploy without One Click Install, use "keploy example --customSetup true"
`
var Examples = `
Golang Application
	Record:
	sudo -E env PATH=$PATH keploy record -c "/path/to/user/app/binary"

	Test:
	sudo -E env PATH=$PATH keploy test -c "/path/to/user/app/binary" --delay 10

Node Application
	Record:
	sudo -E env PATH=$PATH keploy record -c “npm start --prefix /path/to/node/app"

	Test:
	sudo -E env PATH=$PATH keploy test -c “npm start --prefix /path/to/node/app" --delay 10

Java
	Record:
	sudo -E env PATH=$PATH keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	sudo -E env PATH=$PATH keploy test -c "java -jar /path/to/java-project/target/jar" --delay 10

Docker
	Alias:
	alias keploy='sudo docker run --name keploy-ebpf -p 16789:16789 --privileged --pid=host -it -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup
	-v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'

	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 60

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 10 --buildDelay 60

`

var ExampleOneClickInstall = `
Golang Application
	Record:
	keploy record -c "/path/to/user/app/binary"

	Test:
	keploy test -c "/path/to/user/app/binary" --delay 10

Node Application
	Record:
	keploy record -c “npm start --prefix /path/to/node/app"

	Test:
	keploy test -c “npm start --prefix /path/to/node/app" --delay 10

Java
	Record:
	keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	keploy test -c "java -jar /path/to/java-project/target/jar" --delay 10

Docker
	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 60

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 60
`

var RootCustomHelpTemplate = `{{.Short}}

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

var RootExamples = `
  Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --containerName "<containerName>" --buildDelay 60

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 10 --buildDelay 60

  Config:
	keploy config --generate -p "/path/to/localdir"
`

var VersionTemplate = `{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`

type CmdConfigurator struct {
	logger *zap.Logger
	cfg    *config.Config
}

func NewCmdConfigurator(logger *zap.Logger, config *config.Config) *CmdConfigurator {
	return &CmdConfigurator{
		logger: logger,
		cfg:    config,
	}
}

func (c *CmdConfigurator) AddFlags(cmd *cobra.Command) error {
	//sets the displayment of flag-related errors
	cmd.SilenceErrors = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		color.Red(fmt.Sprintf("❌ error: %v", err))
		fmt.Println()
		return err
	})

	//add flags
	var err error
	switch cmd.Name() {
	case "update":
		return nil
	case "normalize":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks/reports are stored")
		cmd.Flags().String("test-run", "", "Test Run to be normalized")
		cmd.Flags().String("tests", "", "Test Sets to be normalized")
	case "config":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated config is stored")
		cmd.Flags().Bool("generate", false, "Generate a new keploy configuration file")
	case "mock":
		cmd.Flags().StringP("path", "p", c.cfg.Path, "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Bool("record", false, "Record all outgoing network traffic")
		cmd.Flags().Bool("replay", false, "Intercept all outgoing network traffic and replay the recorded traffic")
		cmd.Flags().StringP("name", "n", "mocks", "Name of the mock")
		cmd.Flags().Uint32("pid", 0, "Process id of your application.")
		err := cmd.MarkFlagRequired("pid")
		if err != nil {
			errMsg := "failed to mark pid as required flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	case "record", "test":
		cmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")
		cmd.Flags().StringP("rerecord", "r", c.cfg.ReRecord, "Rerecord the testcases/mocks for the given testset(s)")
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Uint32("port", c.cfg.Port, "GraphQL server port used for executing testcases in unit test library integration")
		cmd.Flags().Uint32("proxyPort", c.cfg.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().Uint32("dnsPort", c.cfg.DNSPort, "Port used by the Keploy DNS server to intercept the DNS queries")
		cmd.Flags().StringP("command", "c", c.cfg.Command, "Command to start the user application")
		cmd.Flags().String("cmdType", c.cfg.CommandType, "Type of command to start the user application (native/docker/docker-compose)")
		cmd.Flags().Uint64P("buildDelay", "b", c.cfg.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().String("containerName", c.cfg.ContainerName, "Name of the application's docker container")
		cmd.Flags().StringP("networkName", "n", c.cfg.NetworkName, "Name of the application's docker network")
		cmd.Flags().UintSlice("passThroughPorts", config.GetByPassPorts(c.cfg), "Ports to bypass the proxy server and ignore the traffic")
		cmd.Flags().StringP("appId", "a", c.cfg.AppID, "A unique name for the user's application")
		cmd.Flags().Bool("generateGithubActions", c.cfg.GenerateGithubActions, "Generate Github Actions workflow file")
		err = cmd.Flags().MarkHidden("port")
		if err != nil {
			errMsg := "failed to mark port as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		if cmd.Name() == "test" {
			cmd.Flags().StringSliceP("testsets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
			cmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
			cmd.Flags().Uint64("apiTimeout", c.cfg.Test.APITimeout, "User provided timeout for calling its application")
			cmd.Flags().String("mongoPassword", c.cfg.Test.MongoPassword, "Authentication password for mocking MongoDB conn")
			cmd.Flags().String("coverageReportPath", c.cfg.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")
			cmd.Flags().StringP("language", "l", c.cfg.Test.Language, "application programming language")
			cmd.Flags().Bool("ignoreOrdering", c.cfg.Test.IgnoreOrdering, "Ignore ordering of array in response")
			cmd.Flags().Bool("coverage", c.cfg.Test.Coverage, "Enable coverage reporting for the testcases. for golang please set language flag to golang, ref https://keploy.io/docs/server/sdk-installation/go/")
			cmd.Flags().Bool("removeUnusedMocks", c.cfg.Test.RemoveUnusedMocks, "Clear the unused mocks for the passed test-sets")
			cmd.Flags().Bool("goCoverage", c.cfg.Test.GoCoverage, "Enable go coverage reporting for the testcases")
			cmd.Flags().Bool("fallBackOnMiss", c.cfg.Test.FallBackOnMiss, "Enable connecting to actual service if mock not found during test mode")
		} else {
			cmd.Flags().Uint64("recordTimer", 0, "User provided time to record its application")
		}
	case "keploy":
		cmd.PersistentFlags().Bool("debug", c.cfg.Debug, "Run in debug mode")
		cmd.PersistentFlags().Bool("disableTele", c.cfg.DisableTele, "Run in telemetry mode")
		cmd.PersistentFlags().Bool("disableANSI", c.cfg.DisableANSI, "Disable ANSI color in logs")
		err = cmd.PersistentFlags().MarkHidden("disableTele")
		if err != nil {
			errMsg := "failed to mark telemetry as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		cmd.PersistentFlags().Bool("enableTesting", c.cfg.EnableTesting, "Enable testing keploy with keploy")
		err = cmd.PersistentFlags().MarkHidden("enableTesting")
		if err != nil {
			errMsg := "failed to mark enableTesting as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	default:
		return errors.New("unknown command name")
	}
	return nil
}

func (c *CmdConfigurator) Validate(ctx context.Context, cmd *cobra.Command) error {
	//check if the version of the kernel is above 5.15 for eBPF support
	isValid := kernel.CheckKernelVersion(5, 15, 0)
	if !isValid {
		errMsg := "Kernel version is below 5.15. Keploy requires kernel version 5.15 or above"
		utils.LogError(c.logger, nil, errMsg)
		return errors.New(errMsg)
	}

	return c.ValidateFlags(ctx, cmd)
}

func (c *CmdConfigurator) ValidateFlags(ctx context.Context, cmd *cobra.Command) error {
	// used to bind common flags for commands like record, test. For eg: PATH, PORT, COMMAND etc.
	err := viper.BindPFlags(cmd.Flags())
	if err != nil {
		errMsg := "failed to bind flags to config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	// used to bind flags with environment variables
	viper.AutomaticEnv()
	viper.SetEnvPrefix("KEPLOY")

	//used to bind flags specific to the command for eg: testsets, delay, recordTimer etc. (nested flags)
	err = utils.BindFlagsToViper(c.logger, cmd, "")
	if err != nil {
		errMsg := "failed to bind cmd specific flags to viper"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}
	if cmd.Name() == "test" || cmd.Name() == "record" {
		configPath, err := cmd.Flags().GetString("configPath")
		if err != nil {
			utils.LogError(c.logger, nil, "failed to read the config path")
			return err
		}
		viper.SetConfigName("keploy")
		viper.SetConfigType("yml")
		viper.AddConfigPath(configPath)
		if err := viper.ReadInConfig(); err != nil {
			var configFileNotFoundError viper.ConfigFileNotFoundError
			if !errors.As(err, &configFileNotFoundError) {
				errMsg := "failed to read config file"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.logger.Info("config file not found; creating one and proceeding with flags.")
		}
	}
	if err := viper.Unmarshal(c.cfg); err != nil {
		errMsg := "failed to unmarshal the config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}
	if c.cfg.Debug {
		logger, err := log.ChangeLogLevel(zap.DebugLevel)
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to change log level"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	}

	if c.cfg.EnableTesting {
		// Add mode to logger to debug the keploy during testing
		logger, err := log.AddMode(cmd.Name())
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to add mode to logger"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.DisableTele = true
	}

	if c.cfg.DisableANSI {
		logger, err := log.ChangeColorEncoding()
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to change color encoding"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.logger.Info("Color encoding is disabled")
	}

	c.logger.Debug("config has been initialised", zap.Any("for cmd", cmd.Name()), zap.Any("config", c.cfg))

	switch cmd.Name() {
	case "record", "test":
		bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
		if err != nil {
			errMsg := "failed to read the ports of outgoing calls to be ignored"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetByPassPorts(c.cfg, bypassPorts)

		if c.cfg.Command == "" {
			utils.LogError(c.logger, nil, "missing required -c flag or appCmd in config file")
			if c.cfg.InDocker {
				c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
			} else {
				c.logger.Info(LogExample(RootExamples))
			}
			return errors.New("missing required -c flag or appCmd in config file")
		}

		// set the command type
		c.cfg.CommandType = string(utils.FindDockerCmd(c.cfg.Command))

		if c.cfg.GenerateGithubActions {
			defer utils.GenerateGithubActions(c.logger, c.cfg.Command)
		}
		if c.cfg.InDocker {
			c.logger.Info("detected that Keploy is running in a docker container")
			if len(c.cfg.Path) > 0 {
				curDir, err := os.Getwd()
				if err != nil {
					errMsg := "failed to get current working directory"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				if strings.Contains(c.cfg.Path, "..") {

					c.cfg.Path, err = utils.GetAbsPath(filepath.Clean(c.cfg.Path))
					if err != nil {
						return fmt.Errorf("failed to get the absolute path from relative path: %w", err)
					}

					relativePath, err := filepath.Rel(curDir, c.cfg.Path)
					if err != nil {
						errMsg := "failed to get the relative path from absolute path"
						utils.LogError(c.logger, err, errMsg)
						return errors.New(errMsg)
					}
					if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
						errMsg := "path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories"
						utils.LogError(c.logger, err, errMsg, zap.String("path:", c.cfg.Path))
						return errors.New(errMsg)
					}
				}
			}
			// check if the buildDelay is less than 30 seconds
			if time.Duration(c.cfg.BuildDelay)*time.Second <= 30*time.Second {
				c.logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", c.cfg.BuildDelay))
				c.logger.Info(`Example usage: keploy record -c "docker-compose up --build" --buildDelay 35`)
			}
			if utils.CmdType(c.cfg.Command) == utils.DockerCompose {
				if c.cfg.ContainerName == "" {
					utils.LogError(c.logger, nil, "Couldn't find containerName")
					c.logger.Info(`Example usage: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}
		}
		err = utils.StartInDocker(ctx, c.logger, c.cfg)
		if err != nil {
			return err
		}

		absPath, err := utils.GetAbsPath(c.cfg.Path)
		if err != nil {
			utils.LogError(c.logger, err, "error while getting absolute path")
			return errors.New("failed to get the absolute path")
		}

		c.cfg.Path = absPath + "/keploy"
		if cmd.Name() == "test" {
			//check if the keploy folder exists
			if _, err := os.Stat(c.cfg.Path); os.IsNotExist(err) {
				recordCmd := models.HighlightGrayString("keploy record")
				errMsg := fmt.Sprintf("No test-sets found. Please record testcases using %s command", recordCmd)
				utils.LogError(c.logger, nil, errMsg)
				return errors.New(errMsg)
			}

			testSets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				errMsg := "failed to get the testsets"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			config.SetSelectedTests(c.cfg, testSets)

			if utils.CmdType(c.cfg.CommandType) == utils.Native && c.cfg.Test.GoCoverage {
				goCovPath, err := utils.SetCoveragePath(c.logger, c.cfg.Test.CoverageReportPath)
				if err != nil {
					utils.LogError(c.logger, err, "failed to set go coverage path")
					return errors.New("failed to set go coverage path")
				}
				c.cfg.Test.CoverageReportPath = goCovPath
			}

			if c.cfg.Test.Delay <= 5 {
				c.logger.Warn(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", c.cfg.Test.Delay))
				if c.cfg.InDocker {
					c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					c.logger.Info("Example usage: " + cmd.Example)
				}
			}
		}
	case "normalize":
		path := c.cfg.Path
		//if user provides relative path
		if len(path) > 0 && path[0] != '/' {
			absPath, err := filepath.Abs(path)
			if err != nil {
				utils.LogError(c.logger, err, "failed to get the absolute path from relative path")
			}
			path = absPath
		} else if len(path) == 0 { // if user doesn't provide any path
			cdirPath, err := os.Getwd()
			if err != nil {
				utils.LogError(c.logger, err, "failed to get the path of current directory")
			}
			path = cdirPath
		}
		path += "/keploy"
		c.cfg.Path = path
		tests, err := cmd.Flags().GetString("tests")
		if err != nil {
			errMsg := "failed to read tests to be normalized"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		err = config.SetSelectedTestsNormalize(c.cfg, tests)
		if err != nil {
			errMsg := "failed to normalize the selected tests"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	}
	return nil
}
