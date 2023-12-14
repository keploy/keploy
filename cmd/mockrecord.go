package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/mockrecord"
	"go.uber.org/zap"
)

func NewCmdMockRecord(logger *zap.Logger) *MockRecord {
	mockRecorder := mockrecord.NewMockRecorder(logger)
	return &MockRecord{
		mockRecorder: mockRecorder,
		logger:       logger,
	}
}

type MockRecord struct {
	mockRecorder mockrecord.MockRecorder
	logger       *zap.Logger
}

func (mr *MockRecord) GetCmd() *cobra.Command {
	var serveCmd = &cobra.Command{
		Use:   "mockRecord",
		Short: "run the keploy server to record Mocks",
		Run: func(cmd *cobra.Command, args []string) {

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				mr.logger.Error(Emoji + "failed to read the testset path input")
				return
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					mr.logger.Error(Emoji+"failed to get the absolute path from relative path", zap.Error(err))
					return
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				err := fmt.Errorf("could not find the test case path, please provide a valid one")
				mr.logger.Error(Emoji, zap.Any("testPath", path), zap.Error(err))
				return
			} else {
				// user provided the absolute path
				mr.logger.Debug(Emoji, zap.Any("testPath", path))
			}

			path += "/stubs"

			pid, err := cmd.Flags().GetUint32("pid")

			if err != nil {
				mr.logger.Error(Emoji+"Failed to get the pid of the application", zap.Error((err)))
			}

			dir, err := cmd.Flags().GetString("mockName")
			if err != nil {
				mr.logger.Error(Emoji + "failed to read the mockName name")
				return
			}

			disableTele, err := cmd.Flags().GetBool("disableTele")
			if err != nil {
				mr.logger.Error(Emoji + "failed to read the disableTele flag")
				return
			}

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				mr.logger.Error(Emoji + "failed to read the proxy port")
				return
			}

			mr.mockRecorder.MockRecord(path,proxyPort, pid, dir, disableTele)
		},
	}

	serveCmd.Flags().Uint32("pid", 0, "Process id of your application.")
	serveCmd.MarkFlagRequired("pid")

	serveCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")
	serveCmd.Flags().Bool("disableTele", false, "Switch for telemetry" )

	serveCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")
	serveCmd.MarkFlagRequired("path")
	serveCmd.Flags().StringP("mockName", "m", "", "User provided test suite")
	serveCmd.MarkFlagRequired("mockName")

	serveCmd.Hidden = true

	return serveCmd
}
