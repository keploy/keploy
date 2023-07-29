package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/record"
	"go.uber.org/zap"
)

func NewCmdRecord(logger *zap.Logger) *Record {
	recorder := record.NewRecorder(logger)
	return &Record{
		recorder: recorder,
		logger:   logger,
	}
}

type Record struct {
	recorder record.Recorder
	logger   *zap.Logger
}

func (r *Record) GetCmd() *cobra.Command {
	// record the keploy testcases/mocks for the user application
	var recordCmd = &cobra.Command{
		Use:   "record",
		Short: "record the keploy testcases from the API calls",
		Run: func(cmd *cobra.Command, args []string) {
			path, err := cmd.Flags().GetString("path")
			if err != nil {
				r.logger.Error(Emoji + "failed to read the testcase path input")
				return
			}

			if path == "" {
				path, err = os.Getwd()
				if err != nil {
					r.logger.Error(Emoji+"failed to get the path of current directory", zap.Error(err))
					return
				}
			}
			path += "/keploy"
			// tcsPath := path + "/tests"
			// mockPath := path + "/mocks"

			appCmd, err := cmd.Flags().GetString("c")

			if err != nil {
				r.logger.Error(Emoji+"Failed to get the command to run the user application", zap.Error((err)))
			}

			appContainer, err := cmd.Flags().GetString("containerName")

			if err != nil {
				r.logger.Error(Emoji+"Failed to get the application's docker container name", zap.Error((err)))
			}

			networkName, err := cmd.Flags().GetString("networkName")

			if err != nil {
				r.logger.Error(Emoji+"Failed to get the application's docker network name", zap.Error((err)))
			}

			delay, err := cmd.Flags().GetUint64("delay")

			if err != nil {
				r.logger.Error(Emoji+"Failed to get the delay flag", zap.Error((err)))
			}

			// r.recorder.CaptureTraffic(tcsPath, mockPath, appCmd, appContainer, networkName, delay)
			r.recorder.CaptureTraffic(path, appCmd, appContainer, networkName, delay)

			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	// recordCmd.Flags().Uint32("pid", 0, "Process id on which your application is running.")
	// recordCmd.MarkFlagRequired("pid")

	recordCmd.Flags().String("path", "", "Path to the local directory where generated testcases/mocks should be stored")
	// recordCmd.Flags().String("mockPath", "", "Path to the local directory where generated mocks should be stored")

	recordCmd.Flags().String("c", "", "Command to start the user application")
	recordCmd.MarkFlagRequired("c")

	recordCmd.Flags().String("containerName", "", "Name of the application's docker container")
	// recordCmd.MarkFlagRequired("containerName")

	recordCmd.Flags().String("networkName", "", "Name of the application's docker network")
	// recordCmd.MarkFlagRequired("networkName")

	recordCmd.Flags().Uint64("delay", 5, "User provided time to run its application")
	// recordCmd.MarkFlagRequired("delay")

	return recordCmd
}
