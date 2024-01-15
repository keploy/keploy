package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/test"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewCmdTest(logger *zap.Logger) *Test {
	tester := test.NewTester(logger)
	return &Test{
		tester: tester,
		logger: logger,
	}
}

func readTestConfig(configPath string) (*models.Test, error) {
	file, err := os.OpenFile(configPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.Config
	err = decoder.Decode(&doc)
	if err != nil {
		return nil, err
	}
	return &doc.Test, nil
}

func (t *Test) getTestConfig(path *string, proxyPort *uint32, appCmd *string, tests *map[string][]string, appContainer, networkName *string, Delay *uint64, buildDelay *time.Duration, passThorughPorts *[]uint, apiTimeout *uint64, globalNoise *models.GlobalNoise, testSetNoise *models.TestsetNoise, coverageReportPath *string, withCoverage *bool, configPath string) error {
	configFilePath := filepath.Join(configPath, "keploy-config.yaml")
	if isExist := utils.CheckFileExists(configFilePath); !isExist {
		return errFileNotFound
	}
	confTest, err := readTestConfig(configFilePath)
	if err != nil {
		return fmt.Errorf("failed to get the test config from config file due to error: %s", err)
	}
	if len(*path) == 0 {
		*path = confTest.Path
	}
	if *proxyPort == 0 {
		*proxyPort = confTest.ProxyPort
	}
	if *appCmd == "" {
		*appCmd = confTest.Command
	}
	for testset, testcases := range confTest.Tests {
		if _, ok := (*tests)[testset]; !ok {
			(*tests)[testset] = testcases
		}
	}
	if *appContainer == "" {
		*appContainer = confTest.ContainerName
	}
	if *networkName == "" {
		*networkName = confTest.NetworkName
	}
	if *Delay == 5 {
		*Delay = confTest.Delay
	}
	if *buildDelay == 30*time.Second && confTest.BuildDelay != 0 {
		*buildDelay = confTest.BuildDelay
	}
	if len(*passThorughPorts) == 0 {
		*passThorughPorts = confTest.PassThroughPorts
	}
	if len(*coverageReportPath) == 0 {
		*coverageReportPath = confTest.CoverageReportPath
	}
	*withCoverage = *withCoverage || confTest.WithCoverage
	if *apiTimeout == 5 {
		*apiTimeout = confTest.ApiTimeout
	}
	*globalNoise = confTest.GlobalNoise.Global
	*testSetNoise = confTest.GlobalNoise.Testsets
	return nil
}

type Test struct {
	tester test.Tester
	logger *zap.Logger
}

func (t *Test) GetCmd() *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `sudo -E env PATH=$PATH keploy test -c "/path/to/user/app" --delay 6`,
		RunE: func(cmd *cobra.Command, args []string) error {
			isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				t.logger.Error("failed to read the testcase path input")
				return err
			}
			withCoverage, err := cmd.Flags().GetBool("withCoverage")
			if err != nil {
				t.logger.Error("failed to read the go coverage binary", zap.Error(err))
				return err
			}
			coverageReportPath, err := cmd.Flags().GetString("coverageReportPath")
			if err != nil {
				t.logger.Error("failed to read the go coverage directory path", zap.Error(err))
				return err
			}

			appCmd, err := cmd.Flags().GetString("command")
			if err != nil {
				t.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
				return err
			}

			appContainer, err := cmd.Flags().GetString("containerName")
			if err != nil {
				t.logger.Error("Failed to get the application's docker container name", zap.Error((err)))
				return err
			}

			networkName, err := cmd.Flags().GetString("networkName")
			if err != nil {
				t.logger.Error("Failed to get the application's docker network name", zap.Error((err)))
				return err
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if err != nil {
				t.logger.Error("Failed to get the delay flag", zap.Error((err)))
				return err
			}

			buildDelay, err := cmd.Flags().GetDuration("buildDelay")
			if err != nil {
				t.logger.Error("Failed to get the build-delay flag", zap.Error((err)))
				return err
			}

			apiTimeout, err := cmd.Flags().GetUint64("apiTimeout")
			if err != nil {
				t.logger.Error("Failed to get the apiTimeout flag", zap.Error((err)))
				return err
			}

			ports, err := cmd.Flags().GetUintSlice("passThroughPorts")
			if err != nil {
				t.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				t.logger.Error("failed to read the proxyport")
				return err
			}

			configPath, err := cmd.Flags().GetString("config-path")
			if err != nil {
				t.logger.Error("failed to read the config path")
				return err
			}

			enableTele, err := cmd.Flags().GetBool("enableTele")
			if err != nil {
				t.logger.Error("failed to read the disable telemetry flag")
				return err
			}

			tests := map[string][]string{}

			testsets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				t.logger.Error("Failed to read the testsets")
				return err
			}

			for _, testset := range testsets {
				tests[testset] = []string{}
			}

			globalNoise := make(models.GlobalNoise)
			testsetNoise := make(models.TestsetNoise)

			err = t.getTestConfig(&path, &proxyPort, &appCmd, &tests, &appContainer, &networkName, &delay, &buildDelay, &ports, &apiTimeout, &globalNoise, &testsetNoise, &coverageReportPath, &withCoverage, configPath)
			if err != nil {
				if err == errFileNotFound {
					t.logger.Info("continuing without configuration file because file not found")
				} else {
					t.logger.Error("", zap.Error(err))
				}
			}

			if appCmd == "" {
				t.logger.Error("Couldn't find appCmd")
				if isDockerCmd {
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					t.logger.Info(fmt.Sprintf("Example usage: %s", cmd.Example))
				}
				return errors.New("missing required -c flag or appCmd in config file")
			}

			if delay <= 5 {
				t.logger.Warn(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", delay))
				if isDockerCmd {
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					t.logger.Info("Example usage: " + cmd.Example)
				}
			}

			if isDockerCmd && buildDelay <= 30*time.Second {
				t.logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", buildDelay))
				t.logger.Info(`Example usage:keploy test -c "docker-compose up --build" --buildDelay 35s`)
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					t.logger.Error("failed to get the absolute path from relative path", zap.Error(err))
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				cdirPath, err := os.Getwd()
				if err != nil {
					t.logger.Error("failed to get the path of current directory", zap.Error(err))
				}
				path = cdirPath
			} else {
				// user provided the absolute path
			}

			path += "/keploy"

			testReportPath := path + "/testReports"

			t.logger.Info("", zap.Any("keploy test and mock path", path), zap.Any("keploy testReport path", testReportPath))

			var hasContainerName bool
			if isDockerCmd {
				if strings.Contains(appCmd, "--name") {
					hasContainerName = true
				}
				if !hasContainerName && appContainer == "" {
					t.logger.Error("Couldn't find containerName")
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}

			t.logger.Debug("the ports are", zap.Any("ports", ports))

			mongoPassword, err := cmd.Flags().GetString("mongoPassword")
			if err != nil {
				t.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}
			t.logger.Debug("the configuration for mocking mongo connection", zap.Any("password", mongoPassword))

			if !t.tester.Test(path, testReportPath, appCmd, test.TestOptions{
				Tests:              tests,
				AppContainer:       appContainer,
				AppNetwork:         networkName,
				MongoPassword:      mongoPassword,
				Delay:              delay,
				BuildDelay:         buildDelay,
				PassThroughPorts:   ports,
				ApiTimeout:         apiTimeout,
				ProxyPort:          proxyPort,
				GlobalNoise:        globalNoise,
				TestsetNoise:       testsetNoise,
				WithCoverage:       withCoverage,
				CoverageReportPath: coverageReportPath,
			}, enableTele) {
				t.logger.Error("failed to run the test")
				return errors.New("failed to run the test")
			}

			return nil
		},
	}

	testCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")

	testCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")

	testCmd.Flags().StringP("command", "c", "", "Command to start the user application")

	testCmd.Flags().StringSliceP("testsets", "t", []string{}, "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")

	testCmd.Flags().String("containerName", "", "Name of the application's docker container")

	testCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")
	testCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")

	testCmd.Flags().DurationP("buildDelay", "", 30*time.Second, "User provided time to wait docker container build")

	testCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	testCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	testCmd.Flags().String("config-path", ".", "Path to the local directory where keploy configuration file is stored")

	testCmd.Flags().String("mongoPassword", "default123", "Authentication password for mocking MongoDB connection")

	testCmd.Flags().String("coverageReportPath", "", "Write a go coverage profile to the file in the given directory.")

	testCmd.Flags().Bool("enableTele", true, "Switch for telemetry")
	testCmd.Flags().MarkHidden("enableTele")

	testCmd.Flags().Bool("withCoverage", false, "Capture the code coverage of the go binary in the command flag.")
	testCmd.Flags().Lookup("withCoverage").NoOptDefVal = "true"
	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}
