package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"go.keploy.io/server/pkg/service/test"
)

// NewCmdTestSkeleton returns a new test, initialised only with the logger.
// User should explicitly create the tester upon execution of Run function.
func NewCmdTestSkeleton(logger *zap.Logger) *Test {
	return &Test{
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
			// Overwrite the logger with the one demanded by the user,
			// and use that logger to create the new tester.
			t.UpdateFieldsFromUserDefinedFlags(cmd)
			t.logger.Sync()

			pid, err := cmd.Flags().GetUint32("pid")
			if err != nil {
				t.logger.Error("failed to read the process id flag")
				return
			}

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				t.logger.Error("failed to read the testcase path input")
				return
			}

			if path == "" {
				path, err = os.Getwd()
				if err != nil {
					t.logger.Error("failed to get the path of current directory", zap.Error(err))
					return
				}
			}
			path += "/Keploy"
			tcsPath := path + "/tests"
			mockPath := path + "/mocks"

			testReportPath, err := os.Getwd()
			if err != nil {
				t.logger.Error("failed to get the path of current directory", zap.Error(err))
				return
			}
			testReportPath += "/Keploy/testReports"
			t.tester.Test(tcsPath, mockPath, testReportPath, pid)
		},
	}

	testCmd.Flags().Uint32("pid", 0, "Process id on which your application is running.")
	testCmd.MarkFlagRequired("pid")

	testCmd.Flags().String("path", "", "Path to local directory where generated testcases/mocks are stored")

	return testCmd
}

func (t *Test) UpdateFieldsFromUserDefinedFlags(cmd *cobra.Command) {
	t.logger = CreateLoggerFromFlags(cmd)
	t.tester = test.NewTester(t.logger)
}
