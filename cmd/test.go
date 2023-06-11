package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"go.keploy.io/server/pkg/service/test"
	sysUtils "go.keploy.io/server/pkg/sys/utils"
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
			port, _ := cmd.Flags().GetUint16("port")
			pid, err := sysUtils.FindPidFromPort(port)
			if err != nil {
				t.logger.Error("failed to get pid from port", zap.Error(err))
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

	testCmd.Flags().Uint16("port", 0, "Port on which your application is running.")
	testCmd.MarkFlagRequired("port")

	testCmd.Flags().String("path", "", "Path to local directory where generated testcases/mocks are stored")

	return testCmd
}
