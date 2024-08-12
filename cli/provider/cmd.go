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

	"gopkg.in/yaml.v3"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/tools"
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
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --container-name "<containerName>" --buildDelay 60

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 10 --buildDelay 60

  Config:
	keploy config --generate -p "/path/to/localdir"
`

var VersionTemplate = `{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`
var IsConfigFileFound = true

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
		PrintLogo(true)
		color.Red(fmt.Sprintf("❌ error: %v", err))
		fmt.Println()
		return err
	})

	//add flags
	var err error
	cmd.Flags().SetNormalizeFunc(aliasNormalizeFunc)
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
	case "templatize":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().StringSliceP("testsets", "t", c.cfg.Templatize.TestSets, "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
	case "gen":
		cmd.Flags().String("source-file-path", "", "Path to the source file.")
		cmd.Flags().String("test-file-path", "", "Path to the input test file.")
		cmd.Flags().String("coverage-report-path", "coverage.xml", "Path to the code coverage report file.")
		cmd.Flags().String("test-command", "", "The command to run tests and generate coverage report.")
		cmd.Flags().String("coverage-format", "cobertura", "Type of coverage report.")
		cmd.Flags().Int("expected-coverage", 100, "The desired coverage percentage.")
		cmd.Flags().Int("max-iterations", 5, "The maximum number of iterations.")
		cmd.Flags().String("test-dir", "", "Path to the test directory.")
		cmd.Flags().String("llm-base-url", "", "Base URL for the AI model.")
		cmd.Flags().String("model", "gpt-4o", "Model to use for the AI.")
		cmd.Flags().String("llm-api-version", "", "API version of the llm")
		err := cmd.MarkFlagRequired("test-command")
		if err != nil {
			errMsg := "failed to mark testCommand as required flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

	case "record", "test", "rerecord":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Uint32("proxy-port", c.cfg.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().Uint32("dns-port", c.cfg.DNSPort, "Port used by the Keploy DNS server to intercept the DNS queries")
		cmd.Flags().StringP("command", "c", c.cfg.Command, "Command to start the user application")

		cmd.Flags().String("cmd-type", c.cfg.CommandType, "Type of command to start the user application (native/docker/docker-compose)")
		cmd.Flags().Uint64P("build-delay", "b", c.cfg.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().String("container-name", c.cfg.ContainerName, "Name of the application's docker container")
		cmd.Flags().StringP("network-name", "n", c.cfg.NetworkName, "Name of the application's docker network")
		cmd.Flags().UintSlice("pass-through-ports", config.GetByPassPorts(c.cfg), "Ports to bypass the proxy server and ignore the traffic")
		cmd.Flags().Uint64P("app-id", "a", c.cfg.AppID, "A unique name for the user's application")
		cmd.Flags().String("app-name", c.cfg.AppName, "Name of the user's application")
		cmd.Flags().Bool("generate-github-actions", c.cfg.GenerateGithubActions, "Generate Github Actions workflow file")
		cmd.Flags().Bool("in-ci", c.cfg.InCi, "is CI Running or not")
		//add rest of the uncommon flags for record, test, rerecord commands
		c.AddUncommonFlags(cmd)

	case "keploy":
		cmd.PersistentFlags().Bool("debug", c.cfg.Debug, "Run in debug mode")
		cmd.PersistentFlags().Bool("disable-tele", c.cfg.DisableTele, "Run in telemetry mode")
		cmd.PersistentFlags().Bool("disable-ansi", c.cfg.DisableANSI, "Disable ANSI color in logs")
		err = cmd.PersistentFlags().MarkHidden("disable-tele")
		if err != nil {
			errMsg := "failed to mark telemetry as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		cmd.PersistentFlags().Bool("enable-testing", c.cfg.EnableTesting, "Enable testing keploy with keploy")
		err = cmd.PersistentFlags().MarkHidden("enable-testing")
		if err != nil {
			errMsg := "failed to mark enableTesting as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	default:
		return errors.New("unknown command name")
	}
	cmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")

	return nil
}

func (c *CmdConfigurator) AddUncommonFlags(cmd *cobra.Command) {
	switch cmd.Name() {
	case "record":
		cmd.Flags().Uint64("record-timer", 0, "User provided time to record its application")
	case "test", "rerecord":
		cmd.Flags().StringSliceP("test-sets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
		cmd.Flags().String("host", c.cfg.Test.Host, "Custom host to replace the actual host in the testcases")
		cmd.Flags().Uint32("port", c.cfg.Test.Port, "Custom port to replace the actual port in the testcases")
		if cmd.Name() == "test" {
			cmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
			cmd.Flags().Uint64("api-timeout", c.cfg.Test.APITimeout, "User provided timeout for calling its application")
			cmd.Flags().String("mongo-password", c.cfg.Test.MongoPassword, "Authentication password for mocking MongoDB conn")
			cmd.Flags().String("coverage-report-path", c.cfg.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")
			cmd.Flags().VarP(&c.cfg.Test.Language, "language", "l", "Application programming language")
			cmd.Flags().Bool("ignore-ordering", c.cfg.Test.IgnoreOrdering, "Ignore ordering of array in response")
			cmd.Flags().Bool("skip-coverage", c.cfg.Test.SkipCoverage, "skip code coverage computation while running the test cases")
			cmd.Flags().Bool("remove-unused-mocks", c.cfg.Test.RemoveUnusedMocks, "Clear the unused mocks for the passed test-sets")
			cmd.Flags().Bool("fallBack-on-miss", c.cfg.Test.FallBackOnMiss, "Enable connecting to actual service if mock not found during test mode")
			cmd.Flags().String("jacoco-agent-path", c.cfg.Test.JacocoAgentPath, "Only applicable for test coverage for Java projects. You can override the jacoco agent jar by proving its path")
			cmd.Flags().String("base-path", c.cfg.Test.BasePath, "Custom api basePath/origin to replace the actual basePath/origin in the testcases; App flag is ignored and app will not be started & instrumented when this is set since the application running on a different machine")
			cmd.Flags().Bool("update-temp", c.cfg.Test.UpdateTemp, "Update the template with the result of the testcases.")
			cmd.Flags().Bool("mocking", true, "enable/disable mocking for the testcases")
			cmd.Flags().Bool("disable-line-coverage", c.cfg.Test.DisableLineCoverage, "Disable line coverage generation.")
		}
	}
}

func aliasNormalizeFunc(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	var flagNameMapping = map[string]string{
		"testsets":              "test-sets",
		"delay":                 "delay",
		"apiTimeout":            "api-timeout",
		"mongoPassword":         "mongo-password",
		"coverageReportPath":    "coverage-report-path",
		"language":              "language",
		"ignoreOrdering":        "ignore-ordering",
		"coverage":              "coverage",
		"removeUnusedMocks":     "remove-unused-mocks",
		"goCoverage":            "go-coverage",
		"fallBackOnMiss":        "fallBack-on-miss",
		"basePath":              "base-path",
		"updateTemp":            "update-temp",
		"mocking":               "mocking",
		"sourceFilePath":        "source-file-path",
		"testFilePath":          "test-file-path",
		"testCommand":           "test-command",
		"coverageFormat":        "coverage-format",
		"expectedCoverage":      "expected-coverage",
		"maxIterations":         "max-iterations",
		"testDir":               "test-dir",
		"llmBaseUrl":            "llm-base-url",
		"model":                 "model",
		"llmApiVersion":         "llm-api-version",
		"configPath":            "config-path",
		"path":                  "path",
		"port":                  "port",
		"proxyPort":             "proxy-port",
		"dnsPort":               "dns-port",
		"command":               "command",
		"cmdType":               "cmd-type",
		"buildDelay":            "build-delay",
		"containerName":         "container-name",
		"networkName":           "network-name",
		"passThroughPorts":      "pass-through-ports",
		"appId":                 "app-id",
		"appName":               "app-name",
		"generateGithubActions": "generate-github-actions",
		"disableTele":           "disable-tele",
		"disableANSI":           "disable-ansi",
		"selectedTests":         "selected-tests",
		"testReport":            "test-report",
		"enableTesting":         "enable-testing",
		"inDocker":              "in-docker",
		"keployContainer":       "keploy-container",
		"keployNetwork":         "keploy-network",
		"recordTimer":           "record-timer",
		"urlMethods":            "url-methods",
		"inCi":                  "in-ci",
	}

	if newName, ok := flagNameMapping[name]; ok {
		name = newName
	}
	return pflag.NormalizedName(name)
}

func (c *CmdConfigurator) Validate(ctx context.Context, cmd *cobra.Command) error {
	err := isCompatible(c.logger)
	if err != nil {
		return err
	}
	defaultCfg := *c.cfg
	err = c.PreProcessFlags(cmd)
	if err != nil {
		c.logger.Error("failed to preprocess flags", zap.Error(err))
		return err
	}
	err = c.ValidateFlags(ctx, cmd)
	if err != nil {
		c.logger.Error("failed to validate flags", zap.Error(err))
		return err
	}
	if !IsConfigFileFound {
		err := c.CreateConfigFile(ctx, defaultCfg)
		if err != nil {
			c.logger.Error("failed to create config file", zap.Error(err))
			return err
		}
	}
	return nil
}

func (c *CmdConfigurator) PreProcessFlags(cmd *cobra.Command) error {
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
		IsConfigFileFound = false
		c.logger.Info("config file not found; proceeding with flags only")
	}

	if err := viper.Unmarshal(c.cfg); err != nil {
		errMsg := "failed to unmarshal the config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	c.cfg.ConfigPath = configPath
	return nil
}
func (c *CmdConfigurator) ValidateFlags(ctx context.Context, cmd *cobra.Command) error {
	disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
	PrintLogo(disableAnsi)
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
		models.IsAnsiDisabled = true
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
	case "record", "test", "rerecord":

		// handle the app command
		if c.cfg.Command == "" {
			if !alreadyRunning(cmd.Name(), c.cfg.Test.BasePath) {
				return c.noCommandError()
			}
		}

		// set the command type
		c.cfg.CommandType = string(utils.FindDockerCmd(c.cfg.Command))

		// empty the command if base path is provided, because no need of command even if provided
		if c.cfg.Test.BasePath != "" {
			c.cfg.CommandType = string(utils.Empty)
			c.cfg.Command = ""
		}

		if c.cfg.GenerateGithubActions && utils.CmdType(c.cfg.CommandType) != utils.Empty {
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
					return errors.New("missing required --container-name flag or containerName in config file")
				}
			}
		}
		err := StartInDocker(ctx, c.logger, c.cfg)
		if err != nil {
			return err
		}

		absPath, err := utils.GetAbsPath(c.cfg.Path)
		if err != nil {
			utils.LogError(c.logger, err, "error while getting absolute path")
			return errors.New("failed to get the absolute path")
		}
		c.cfg.Path = absPath + "/keploy"

		bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
		if err != nil {
			errMsg := "failed to read the ports of outgoing calls to be ignored"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetByPassPorts(c.cfg, bypassPorts)

		if cmd.Name() == "test" || cmd.Name() == "rerecord" {
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

			if cmd.Name() == "rerecord" {
				c.cfg.Test.SkipCoverage = true
				host, err := cmd.Flags().GetString("host")
				if err != nil {
					errMsg := "failed to get the provided host"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				c.cfg.ReRecord.Host = host
				port, err := cmd.Flags().GetUint32("port")
				if err != nil {
					errMsg := "failed to get the provided port"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				c.cfg.ReRecord.Port = port
				return nil
			}

			// skip coverage by default if command is of type docker
			if utils.CmdType(c.cfg.CommandType) != "native" && !cmd.Flags().Changed("skip-coverage") {
				c.cfg.Test.SkipCoverage = true
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
		c.cfg.Path = utils.ConvertToAbs(c.logger, c.cfg.Path)
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

	case "templatize":
		c.cfg.Path = utils.ConvertToAbs(c.logger, c.cfg.Path)
	case "gen":
		if os.Getenv("API_KEY") == "" {
			utils.LogError(c.logger, nil, "API_KEY is not set")
			return errors.New("API_KEY is not set")
		}
		if (c.cfg.Gen.SourceFilePath == "" && c.cfg.Gen.TestFilePath != "") || c.cfg.Gen.SourceFilePath != "" && c.cfg.Gen.TestFilePath == "" {
			utils.LogError(c.logger, nil, "One of the SourceFilePath and TestFilePath is mentioned. Either provide both or neither")
			return errors.New("sourceFilePath and testFilePath misconfigured")
		} else if c.cfg.Gen.SourceFilePath == "" && c.cfg.Gen.TestFilePath == "" {
			if c.cfg.Gen.TestDir == "" {
				utils.LogError(c.logger, nil, "TestDir is not set, Please specify the test directory")
				return errors.New("TestDir is not set")
			}
		}
	}

	return nil
}

func (c *CmdConfigurator) CreateConfigFile(ctx context.Context, defaultCfg config.Config) error {
	defaultCfg = c.UpdateConfigData(defaultCfg)
	toolSvc := tools.NewTools(c.logger, nil)
	configData := defaultCfg
	configDataBytes, err := yaml.Marshal(configData)
	if err != nil {
		utils.LogError(c.logger, err, "failed to marshal config data")
		return errors.New("failed to marshal config data")
	}
	err = toolSvc.CreateConfig(ctx, c.cfg.ConfigPath+"/keploy.yml", string(configDataBytes))
	if err != nil {
		utils.LogError(c.logger, err, "failed to create config file")
		return errors.New("failed to create config file")
	}
	c.logger.Info("Generated config file based on the flags that are used")
	return nil
}

func (c *CmdConfigurator) UpdateConfigData(defaultCfg config.Config) config.Config {
	defaultCfg.Command = c.cfg.Command
	defaultCfg.Test.Delay = c.cfg.Test.Delay
	defaultCfg.AppName = c.cfg.AppName
	defaultCfg.Test.APITimeout = c.cfg.Test.APITimeout
	defaultCfg.ContainerName = c.cfg.ContainerName
	defaultCfg.Test.IgnoreOrdering = c.cfg.Test.IgnoreOrdering
	defaultCfg.Test.Language = c.cfg.Test.Language
	defaultCfg.DisableANSI = c.cfg.DisableANSI
	defaultCfg.Test.SkipCoverage = c.cfg.Test.SkipCoverage
	defaultCfg.Test.Mocking = c.cfg.Test.Mocking
	defaultCfg.Test.DisableLineCoverage = c.cfg.Test.DisableLineCoverage
	return defaultCfg
}
