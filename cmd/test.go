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

func getTestConfig() (*models.Test, error) {
	file, err := os.OpenFile(filepath.Join(".", "keploy-config.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.Config
	err = decoder.Decode(&doc)
	if err != nil {
		return nil, fmt.Errorf(Emoji, "failed to decode the keploy-config.yaml. error: %v", err.Error())
	}
	return &doc.Test, nil
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

			confTest, err := getTestConfig()
			if err != nil {
				t.logger.Error("failed to get the test config from config file")
				return err
			}
			
			path, err := cmd.Flags().GetString("path")
			if err != nil {
				t.logger.Error("failed to read the testcase path input")
				return err
			}
			
			if len(path) == 0 {
				path = confTest.Path
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
				appCmd = confTest.Command
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

			if appContainer == "" {
				appContainer = confTest.ContainerName
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

			if networkName == "" {
				networkName = confTest.NetworkName
			}
        
			testSets, err := cmd.Flags().GetStringSlice("testsets")

			if err != nil {
				t.logger.Error("Failed to get the testsets flag", zap.Error((err)))
			}

			if len(testSets) == 0 {
				testSets = confTest.TestSets
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if err != nil {
				t.logger.Error("Failed to get the delay flag", zap.Error((err)))
			}

			if delay == 5 {
				delay = confTest.Delay
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

			if apiTimeout == 5 {
				apiTimeout = confTest.ApiTimeout
			}

			t.logger.Info("", zap.Any("keploy test and mock path", path), zap.Any("keploy testReport path", testReportPath))

			ports, err := cmd.Flags().GetUintSlice("passThroughPorts")
			if err != nil {
				t.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}

			if len(ports) == 0 {
				ports = confTest.PassThroughPorts
			}

			t.logger.Debug("the ports are", zap.Any("ports", ports))

			t.tester.Test(path, testReportPath, appCmd, testSets, appContainer, networkName, delay, ports, apiTimeout)
			return nil
		},
	}

	testCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")

	testCmd.Flags().StringP("command", "c", "", "Command to start the user application")

	testCmd.Flags().StringSliceP("testsets", "t", []string{}, "Testsets to run")
	
	testCmd.Flags().String("containerName", "", "Name of the application's docker container")

	testCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")
	testCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")

	testCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	testCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}
