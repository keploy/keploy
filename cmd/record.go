package cmd

import "github.com/spf13/cobra"

func NewCmdRecord() *Record {
	return &Record{}
}

type Record struct {
}

func (r *Record) GetCmd() *cobra.Command {
	// record the keploy testcases/mocks for the user application
	var recordCmd = &cobra.Command{
		Use:   "record [port]",
		Short: "record the keploy testcases from the API calls",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 1 || args[0] == "" {
				// conf.Port = args[0]
				println("missing required parameter")
			}
			println("record cmd called!")
			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	return recordCmd
}
