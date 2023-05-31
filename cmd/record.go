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
		Use:   "record  [pid]",
		Short: "record the keploy testcases from the API calls",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 1 || args[0] == "" {
				// conf.Port = args[0]
				println("missing required parameter")
			}
			println("record cmd called!")
			path, err := os.Getwd()
			if err != nil {
				r.logger.Error("Failed to get the path of current directory", zap.Error(err))
			}
			path += "/Keploy-Tests-2"
			tcsPath := path + "/tests"
			mockPath := path + "/mocks"
			r.recorder.CaptureTraffic(tcsPath, mockPath)

			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	return recordCmd
}
