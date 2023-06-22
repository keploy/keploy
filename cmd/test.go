package cmd

import (
	"os"

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
		Use:   "test",
		Short: "run the recorded testcases and execute assertions",
		Run: func(cmd *cobra.Command, args []string) {

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				t.logger.Error(Emoji + "failed to read the testcase path input")
				return
			}

			if path == "" {
				path, err = os.Getwd()
				if err != nil {
					t.logger.Error(Emoji+"failed to get the path of current directory", zap.Error(err))
					return
				}
			}
			path += "/Keploy"
			tcsPath := path + "/tests"
			mockPath := path + "/mocks"

			testReportPath, err := os.Getwd()
			if err != nil {
				t.logger.Error(Emoji+"failed to get the path of current directory", zap.Error(err))
				return
			}
			testReportPath += "/Keploy/testReports"
			appCmd, err := cmd.Flags().GetString("c")

			if err != nil {
				t.logger.Error(Emoji+"Failed to get the command to run the user application", zap.Error((err)))
			}

			appContainer, err := cmd.Flags().GetString("containerName")

			if err != nil {
				t.logger.Error(Emoji+"Failed to get the application's docker container name", zap.Error((err)))
			}

			networkName, err := cmd.Flags().GetString("networkName")

			if err != nil {
				t.logger.Error(Emoji+"Failed to get the application's docker network name", zap.Error((err)))
			}

			delay, err := cmd.Flags().GetUint64("delay")

			if err != nil {
				t.logger.Error(Emoji+"Failed to get the delay flag", zap.Error((err)))
			}

			t.tester.Test(tcsPath, mockPath, testReportPath, appCmd, appContainer, networkName, delay)
		},
	}

	// testCmd.Flags().Uint32("pid", 0, "Process id on which your application is running.")
	// testCmd.MarkFlagRequired("pid")

	testCmd.Flags().String("path", "", "Path to local directory where generated testcases/mocks are stored")
	testCmd.Flags().String("c", "", "Command to start the user application")
	testCmd.MarkFlagRequired("c")
	testCmd.Flags().String("containerName", "", "Name of the application's docker container")
	testCmd.Flags().String("networkName", "", "Name of the application's docker network")
	// recordCmd.MarkFlagRequired("networkName")
	testCmd.Flags().Uint64("delay", 5, "User provided time to run its application")

	return testCmd
}
