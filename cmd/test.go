package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/test"
	"go.uber.org/zap"
)

func NewCmdTest(logger *zap.Logger) *Test {
	tester := test.NewTester(logger)
	return &Test{
		tester: tester,
		logger: logger,
	}
}

type Test struct {
	tester test.Tester
	logger *zap.Logger
}

func (t *Test) GetCmd() *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `sudo -E keploy test -c "/path/to/user/app" --delay 6`,
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

			// tcsPath := path + "/tests"
			// mockPath := path + "/mocks"

			testReportPath := path + "/testReports"

			appCmd, err := cmd.Flags().GetString("command")
			if err != nil {
				t.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
			}
			if appCmd == "" {
				fmt.Println("Error: missing required -c flag\n")
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName --rm myApplicationImageName" --delay 6\n`)
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
					fmt.Println("\nExample usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName --rm myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag")
				}
			}
			networkName, err := cmd.Flags().GetString("networkName")

			if err != nil {
				t.logger.Error("Failed to get the application's docker network name", zap.Error((err)))
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if delay <= 5 {
				fmt.Printf("Warning: delay is set to %d seconds, incase your app takes more time to start udse --delay to set custom delay\n", delay)
				if isDockerCmd {
					fmt.Println("Example usage:\n", `keploy test -c "docker run -p 8080:808 --network myNetworkName --rm myApplicationImageName" --delay 6\n`)
				} else {
					fmt.Println("Example usage:\n", cmd.Example, "\n")
				}
			}
			if err != nil {
				t.logger.Error("Failed to get the delay flag", zap.Error((err)))
			}
			t.logger.Info("", zap.Any("keploy test and mock path", path), zap.Any("keploy testReport path", testReportPath))

			// pid, err := cmd.Flags().GetUint32("pid")

			// if err != nil {
			// 	t.logger.Error(Emoji+"Failed to get the pid of the application", zap.Error((err)))
			// }

			t.tester.Test(path, testReportPath, appCmd, appContainer, networkName, delay)
			return nil
		},
	}

	// testCmd.Flags().Uint32("pid", 0, "Process id of your application.")

	testCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")
	testCmd.Flags().StringP("command", "c", "", "Command to start the user application")
	// testCmd.MarkFlagRequired("command")
	testCmd.Flags().String("containerName", "", "Name of the application's docker container")
	testCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")
	// recordCmd.MarkFlagRequired("networkName")
	testCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}
