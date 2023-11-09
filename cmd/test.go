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

func readTestConfig() (*models.Test, error) {
	file, err := os.OpenFile(filepath.Join(".", "keploy-config.yaml"), os.O_RDONLY, os.ModePerm)
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

func (t *Test) getTestConfig(path *string, proxyPort *uint32, appCmd *string, testsets *[]string, appContainer, networkName *string, Delay *uint64, passThorughPorts *[]uint, apiTimeout *uint64, noiseConfig *map[string]interface{}) {
	if isExist := utils.CheckFileExists(filepath.Join(".", "keploy-config.yaml")); !isExist {
		t.logger.Info("keploy configuration file not found")
		return
	}
	confTest, err := readTestConfig()
	if err != nil {
		t.logger.Error("failed to get the test config from config file")
		return
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
	if len(*testsets) == 0 {
		*testsets = confTest.TestSets
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
	noiseJSON, err := test.UnmarshallJson(confTest.Noise, t.logger)
	if err != nil {
		t.logger.Error("Failed to unmarshall the noise flag", zap.Error((err)))
	}
	*noiseConfig = map[string]interface{}{}
	(*noiseConfig)["body"] = noiseJSON.(map[string]interface{})["body"]
	(*noiseConfig)["header"] = noiseJSON.(map[string]interface{})["header"]
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

			appCmd, err := cmd.Flags().GetString("command")
			if err != nil {
				t.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
			}

			if appCmd == "" {
				fmt.Println("Error: missing required -c flag\n")
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6\n`)
				}
				fmt.Println("Example usage:\n", cmd.Example, "\n")

				return errors.New("missing required -c flag")
			}
			appContainer, err := cmd.Flags().GetString("containerName")

			if err != nil {
				t.logger.Error("Failed to get the application's docker container name", zap.Error((err)))
			}

			var hasContainerName bool
			if isDockerCmd {
				for _, arg := range os.Args {
					if strings.Contains(arg, "--name") {
						hasContainerName = true
						break
					}
				}
				if !hasContainerName && appContainer == "" {
					fmt.Println("Error: missing required --containerName flag")
					fmt.Println("\nExample usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag")
				}
			}
			networkName, err := cmd.Flags().GetString("networkName")

			if err != nil {
				t.logger.Error("Failed to get the application's docker network name", zap.Error((err)))
			}

			testSets, err := cmd.Flags().GetStringSlice("testsets")

			if err != nil {
				t.logger.Error("Failed to get the testsets flag", zap.Error((err)))
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if err != nil {
				t.logger.Error("Failed to get the delay flag", zap.Error((err)))
			}

			if delay <= 5 {
				fmt.Printf("Warning: delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay\n", delay)
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName myApplicationImageName" --delay 6\n`)
				} else {
					fmt.Println("Example usage:\n", cmd.Example, "\n")
				}
			}

			apiTimeout, err := cmd.Flags().GetUint64("apiTimeout")
			if err != nil {
				t.logger.Error("Failed to get the apiTimeout flag", zap.Error((err)))
			}

			t.logger.Info("", zap.Any("keploy test and mock path", path), zap.Any("keploy testReport path", testReportPath))

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

			noiseConfig := map[string]interface{}{}
			t.getTestConfig(&path, &proxyPort, &appCmd, &testSets, &appContainer, &networkName, &delay, &ports, &apiTimeout, &noiseConfig)

			t.logger.Debug("the ports are", zap.Any("ports", ports))


			mongoPassword, err := cmd.Flags().GetString("mongoPassword")
			if err != nil {
				t.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}
			t.logger.Debug("the configuration for mocking mongo connection", zap.Any("password", mongoPassword))

			t.tester.Test(path, testReportPath, appCmd, test.TestOptions{
				Testsets: testSets,
				AppContainer: appContainer,
				AppNetwork: networkName,
				MongoPassword: mongoPassword,
				Delay: delay,
				PassThorughPorts: ports,
				ApiTimeout: apiTimeout,
				ProxyPort: proxyPort,
				NoiseConfig: noiseConfig,
			})
			return nil
		},
	}

	testCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")

	testCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")

	testCmd.Flags().StringP("command", "c", "", "Command to start the user application")

	testCmd.Flags().StringSliceP("testsets", "t", []string{}, "Testsets to run")

	testCmd.Flags().String("containerName", "", "Name of the application's docker container")

	testCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")
	testCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")

	testCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	testCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	testCmd.Flags().String("mongoPassword", "default123", "Authentication password for mocking MongoDB connection")

	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}
