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
		logger: logger,
	}
}

type Record struct {
	recorder record.Recorder
	logger *zap.Logger
}

func (r *Record) GetCmd() *cobra.Command {
	// record the keploy testcases/mocks for the user application
	var recordCmd = &cobra.Command{
		Use:   "record",
		Short: "record the keploy testcases from the API calls",
		Run: func(cmd *cobra.Command, args []string) {
			pid, _ := cmd.Flags().GetUint32("pid")
			path, err := cmd.Flags().GetString("path")
			if err!=nil {
				r.logger.Error("failed to read the testcase path input")
				return
			}

			if path == "" {
				path, err = os.Getwd()
				if err != nil {
					r.logger.Error("failed to get the path of current directory", zap.Error(err))
					return
				}
			}
			path += "/Keploy"
			tcsPath := path + "/tests"
			mockPath := path + "/mocks"

			r.recorder.CaptureTraffic(tcsPath, mockPath, pid)

			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	recordCmd.Flags().Uint32("pid", 0, "Process id on which your application is running.")
	recordCmd.MarkFlagRequired("pid")

	recordCmd.Flags().String("path", "", "Path to the local directory where generated testcases/mocks should be stored")
	// recordCmd.Flags().String("mockPath", "", "Path to the local directory where generated mocks should be stored")

	return recordCmd
}
