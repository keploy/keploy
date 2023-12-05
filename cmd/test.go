package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

func (t *Test) getTestConfig(path *string, proxyPort *uint32, appCmd *string, tests *map[string][]string, appContainer, networkName *string, Delay *uint64, passThorughPorts *[]uint, apiTimeout *uint64, globalNoise *models.GlobalNoise, testSetNoise *models.TestsetNoise, configPath string) error {
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
	if len(*passThorughPorts) == 0 {
		*passThorughPorts = confTest.PassThroughPorts
	}
	if *apiTimeout == 5 {
		*apiTimeout = confTest.ApiTimeout
	}
	noiseJSON, err := test.UnmarshallJson(confTest.GlobalNoise, t.logger)
	if err != nil {
		return fmt.Errorf("failed to unmarshall the noise flag due to error: %s", err)
	}

	globalScopeVal := noiseJSON.(map[string]interface{})["global"]

	bodyOrHeaderVal := globalScopeVal.(map[string]interface{})

	(*globalNoise)["body"] = map[string][]string{}
	for field, regexArr := range bodyOrHeaderVal["body"].(map[string]interface{}) {
		(*globalNoise)["body"][field] = []string{}
		for _, val := range regexArr.([]interface{}) {
			(*globalNoise)["body"][field] = append((*globalNoise)["body"][field], val.(string))
		}
	}

	(*globalNoise)["header"] = map[string][]string{}
	for field, regexArr := range bodyOrHeaderVal["header"].(map[string]interface{}) {
		(*globalNoise)["header"][field] = []string{}
		for _, val := range regexArr.([]interface{}) {
			(*globalNoise)["header"][field] = append((*globalNoise)["header"][field], val.(string))
		}
	}

	testSetScopeVal := noiseJSON.(map[string]interface{})["test-sets"]

	for testset := range testSetScopeVal.(map[string]interface{}) {
		(*testSetNoise)[testset] = map[string]map[string][]string{}

		bodyOrHeaderVal := testSetScopeVal.(map[string]interface{})[testset].(map[string]interface{})

		(*testSetNoise)[testset]["body"] = map[string][]string{}
		for field, regexArr := range bodyOrHeaderVal["body"].(map[string]interface{}) {
			(*testSetNoise)[testset]["body"][field] = []string{}
			for _, val := range regexArr.([]interface{}) {
				(*testSetNoise)[testset]["body"][field] = append((*testSetNoise)[testset]["body"][field], val.(string))
			}
		}

		(*testSetNoise)[testset]["header"] = map[string][]string{}
		for field, regexArr := range bodyOrHeaderVal["header"].(map[string]interface{}) {
			(*testSetNoise)[testset]["header"][field] = []string{}
			for _, val := range regexArr.([]interface{}) {
				(*testSetNoise)[testset]["header"][field] = append((*testSetNoise)[testset]["header"][field], val.(string))
			}
		}
	}
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

			err = t.getTestConfig(&path, &proxyPort, &appCmd, &tests, &appContainer, &networkName, &delay, &ports, &apiTimeout, &globalNoise, &testsetNoise, configPath)
			if err != nil {
				if err == errFileNotFound {
					t.logger.Info("continuing without configuration file because file not found")
				} else {
					t.logger.Error("", zap.Error(err))
				}
			}

			if appCmd == "" {
				fmt.Println("Error: missing required -c flag or appCmd in config file")
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6\n`)
				}
				fmt.Println("Example usage:\n", cmd.Example)

				return errors.New("missing required -c flag or appCmd in config file")
			}

			if delay <= 5 {
				fmt.Printf("Warning: delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay\n", delay)
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6\n`)
				} else {
					fmt.Println("Example usage:\n", cmd.Example)
				}
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
					fmt.Println("Error: missing required --containerName flag or containerName in config file")
					fmt.Println("\nExample usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6`)
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

			t.tester.Test(path, testReportPath, appCmd, test.TestOptions{
				Tests:            tests,
				AppContainer:     appContainer,
				AppNetwork:       networkName,
				MongoPassword:    mongoPassword,
				Delay:            delay,
				PassThroughPorts: ports,
				ApiTimeout:       apiTimeout,
				ProxyPort:        proxyPort,
				GlobalNoise:      globalNoise,
				TestsetNoise:     testsetNoise,
			})

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

	testCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	testCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	testCmd.Flags().String("config-path", ".", "Path to the local directory where keploy configuration file is stored")

	testCmd.Flags().String("mongoPassword", "default123", "Authentication password for mocking MongoDB connection")

	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}
