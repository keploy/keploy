package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"go.keploy.io/server/pkg/service/record"
)

// NewCmdRecordSkeleton returns a new record, initialised only with the logger.
// User should explicitly create the recorder upon execution of Run function.
func NewCmdRecordSkeleton(logger *zap.Logger) *Record {
	return &Record{
		logger: logger,
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
			// Overwrite the logger with the one demanded by the user,
			// and use that logger to create the new recorder.
			r.UpdateFieldsFromUserDefinedFlags(cmd)
			defer r.logger.Sync()

			pid, _ := cmd.Flags().GetUint32("pid")
			path, err := cmd.Flags().GetString("path")
			if err != nil {
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

func (r *Record) UpdateFieldsFromUserDefinedFlags(cmd *cobra.Command) {
	r.logger = CreateLoggerFromFlags(cmd)
	r.recorder = record.NewRecorder(r.logger)
}
