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

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
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
	sudo -E env PATH=$PATH keploy test -c "/path/to/user/app/binary" --delay 2

Node Application
	Record:
	sudo -E env PATH=$PATH keploy record -c “npm start --prefix /path/to/node/app"
	
	Test:
	sudo -E env PATH=$PATH keploy test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	sudo -E env PATH=$PATH keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	sudo -E env PATH=$PATH keploy test -c "java -jar /path/to/java-project/target/jar" --delay 2

Docker
	Alias:
	alias keploy='sudo docker run --name keploy-ebpf -p 16789:16789 --privileged --pid=host -it -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup
	-v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'

	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 1m

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 1m

`

var ExampleOneClickInstall = `
Golang Application
	Record:
	keploy record -c "/path/to/user/app/binary"
	
	Test:
	keploy test -c "/path/to/user/app/binary" --delay 2

Node Application
	Record:
	keploy record -c “npm start --prefix /path/to/node/app"
	
	Test:
	keploy test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	keploy test -c "java -jar /path/to/java-project/target/jar" --delay 2

Docker
	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 1m

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 1m
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
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --containerName "<containerName>" --delay 1 --buildDelay 1m

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 1 --buildDelay 1m

  Config:
	keploy config --generate -p "/path/to/localdir"
`

var VersionTemplate = `{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`

type CmdConfigurator struct {
	logger *zap.Logger
}

func NewCmdConfigurator(logger *zap.Logger) *CmdConfigurator {
	return &CmdConfigurator{
		logger: logger,
	}
}

func (c *CmdConfigurator) AddFlags(cmd *cobra.Command, cfg *config.Config) error {
	var err error
	switch cmd.Name() {
	case "update":
		return nil
	case "config":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated config is stored")
		cmd.Flags().Bool("generate", false, "Generate a new keploy configuration file")
	case "mock":
		cmd.Flags().StringP("path", "p", cfg.Path, "Path to local directory where generated testcases/mocks are stored")
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
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Uint32("port", cfg.Port, "GraphQL server port used for executing testcases in unit test library integration")
		cmd.Flags().Uint32("proxyPort", cfg.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().Uint32("dnsPort", cfg.DNSPort, "Port used by the Keploy DNS server to intercept the DNS queries")
		cmd.Flags().StringP("command", "c", cfg.Command, "Command to start the user application")
		cmd.Flags().DurationP("buildDelay", "b", cfg.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().String("containerName", cfg.ContainerName, "Name of the application's docker container")
		cmd.Flags().StringP("networkName", "n", cfg.NetworkName, "Name of the application's docker network")
		cmd.Flags().UintSlice("passThroughPorts", config.GetByPassPorts(cfg), "Ports to bypass the proxy server and ignore the traffic")
		err = cmd.Flags().MarkHidden("port")
		if err != nil {
			errMsg := "failed to mark port as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		if cmd.Name() == "test" {
			cmd.Flags().StringSliceP("testsets", "t", utils.Keys(cfg.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
			cmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
			cmd.Flags().Uint64("apiTimeout", cfg.Test.APITimeout, "User provided timeout for calling its application")
			cmd.Flags().String("mongoPassword", cfg.Test.MongoPassword, "Authentication password for mocking MongoDB conn")
			cmd.Flags().String("coverageReportPath", cfg.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")
			cmd.Flags().StringP("language", "l", cfg.Test.Language, "application programming language")
			cmd.Flags().Bool("ignoreOrdering", cfg.Test.IgnoreOrdering, "Ignore ordering of array in response")
			cmd.Flags().Bool("coverage", cfg.Test.Coverage, "Enable coverage reporting for the testcases. for golang please set language flag to golang, ref https://keploy.io/docs/server/sdk-installation/go/")
			cmd.Flags().Bool("clearUnusedMocks", false, "Clear the unused mocks for the passed test-sets")
			cmd.Flags().Lookup("clearUnusedMocks").NoOptDefVal = "true"
		} else {
			cmd.Flags().Uint64("recordTimer", 0, "User provided time to record its application")
		}
	case "keploy":
		cmd.PersistentFlags().Bool("debug", cfg.Debug, "Run in debug mode")
		cmd.PersistentFlags().Bool("disableTele", cfg.DisableTele, "Run in telemetry mode")
		err = cmd.PersistentFlags().MarkHidden("disableTele")
		if err != nil {
			errMsg := "failed to mark telemetry as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		err := viper.BindPFlag("debug", cmd.PersistentFlags().Lookup("debug"))
		if err != nil {
			errMsg := "failed to bind flag to config"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	default:
		return errors.New("unknown command name")
	}
	return nil
}

func (c CmdConfigurator) ValidateFlags(ctx context.Context, cmd *cobra.Command, cfg *config.Config) error {
	err := viper.BindPFlags(cmd.Flags())
	utils.BindFlagsToViper(c.logger, cmd, "")
	if err != nil {
		errMsg := "failed to bind flags to config"
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
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				errMsg := "failed to read config file"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.logger.Info("config file not found; proceeding with flags only")
		}
	}
	if err := viper.Unmarshal(cfg); err != nil {
		errMsg := "failed to unmarshal the config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}
	if cfg.Debug {
		logger, err := log.ChangeLogLevel(zap.DebugLevel)
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to change log level"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	}
	c.logger.Debug("config has been initialised", zap.Any("for cmd", cmd.Name()), zap.Any("config", cfg))

	switch cmd.Name() {
	case "record", "test":
		bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
		if err != nil {
			errMsg := "failed to read the ports of outgoing calls to be ignored"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetByPassPorts(cfg, bypassPorts)

		if cfg.Command == "" {
			utils.LogError(c.logger, nil, "missing required -c flag or appCmd in config file")
			if cfg.InDocker {
				c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
			} else {
				c.logger.Info(LogExample(RootExamples))
			}
			return errors.New("missing required -c flag or appCmd in config file")
		}

		if cfg.InDocker {
			if len(cfg.Path) > 0 {
				curDir, err := os.Getwd()
				if err != nil {
					errMsg := "failed to get current working directory"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				if strings.Contains(cfg.Path, "..") {
					cfg.Path, err = filepath.Abs(filepath.Clean(cfg.Path))
					if err != nil {
						errMsg := "failed to get the absolute path from relative path"
						utils.LogError(c.logger, err, errMsg)
						return errors.New(errMsg)
					}
					relativePath, err := filepath.Rel(curDir, cfg.Path)
					if err != nil {
						errMsg := "failed to get the relative path from absolute path"
						utils.LogError(c.logger, err, errMsg)
						return errors.New(errMsg)
					}
					if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
						errMsg := "path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories"
						utils.LogError(c.logger, err, errMsg, zap.String("path:", cfg.Path))
						return errors.New(errMsg)
					}
				}
			}
			if cfg.BuildDelay <= 30*time.Second {
				c.logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", cfg.BuildDelay))
				c.logger.Info(`Example usage: keploy record -c "docker-compose up --build" --buildDelay 35s`)
			}
			if strings.Contains(cfg.Command, "--name") {
				if cfg.ContainerName == "" {
					utils.LogError(c.logger, nil, "Couldn't find containerName")
					c.logger.Info(`Example usage: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}

		}

		err = utils.StartInDocker(ctx, c.logger, cfg)
		if err != nil {
			return err
		}

		absPath, err := filepath.Abs(cfg.Path)
		if err != nil {
			errMsg := "failed to get the absolute path from relative path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		cfg.Path = absPath + "/keploy"
		if cmd.Name() == "test" {
			testSets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				errMsg := "failed to get the testsets"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
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
