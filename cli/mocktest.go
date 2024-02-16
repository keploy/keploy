package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/mocktest"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func NewCmdMockTest(logger *zap.Logger) *MockTest {
	mockTester := mocktest.NewMockTester(logger)
	return &MockTest{
		mockTester: mockTester,
		logger:     logger,
	}
}

type MockTest struct {
	mockTester mocktest.MockTester
	logger     *zap.Logger
}

func (s *MockTest) GetCmd() *cobra.Command {
	var serveCmd = &cobra.Command{
		Use:   "mockTest",
		Short: "run the keploy server to test Mocks",
		Run: func(cmd *cobra.Command, args []string) {

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				s.logger.Error(utils.Emoji + "failed to read the testset path input")
				return
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					s.logger.Error(utils.Emoji+"failed to get the absolute path from relative path", zap.Error(err))
					return
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				err := fmt.Errorf("could not find the test case path, please provide a valid one")
				s.logger.Error(utils.Emoji, zap.Any("testPath", path), zap.Error(err))
				return
			} else {
				// user provided the absolute path
				s.logger.Debug(utils.Emoji, zap.Any("testPath", path))
			}

			path += "/stubs"

			pid, err := cmd.Flags().GetUint32("pid")

			if err != nil {
				s.logger.Error(utils.Emoji+"Failed to get the pid of the application", zap.Error((err)))
			}

			dir, err := cmd.Flags().GetString("mockName")
			if err != nil {
				s.logger.Error(utils.Emoji + "failed to read the mockName name")
				return
			}

			enableTele, err := cmd.Flags().GetBool("enableTele")
			if err != nil {
				s.logger.Error(utils.Emoji + "failed to read the enableTele flag")
				return
			}

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				s.logger.Error(utils.Emoji + "failed to read the proxyport")
				return
			}

			s.mockTester.MockTest(path, proxyPort, pid, dir, enableTele)
		},
	}

	serveCmd.Flags().Uint32("pid", 0, "Process id of your application.")
	serveCmd.MarkFlagRequired("pid")

	serveCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")
	serveCmd.MarkFlagRequired("path")
	serveCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")
	serveCmd.Flags().Bool("enableTele", true, "Switch for telemetry")
	serveCmd.Flags().MarkHidden("enableTele")

	serveCmd.Flags().StringP("mockName", "m", "", "User provided test suite")
	serveCmd.MarkFlagRequired("mockName")

	serveCmd.Hidden = true

	return serveCmd
}
