package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/service/serve"
	"go.uber.org/zap"
)

func NewCmdServe(logger *zap.Logger) *Serve {
	server := serve.NewServer(logger)
	return &Serve{
		server: server,
		logger: logger,
	}
}

type Serve struct {
	server serve.Server
	logger *zap.Logger
}

func (s *Serve) GetCmd() *cobra.Command {
	var serveCmd = &cobra.Command{
		Use:   "serve",
		Short: "run the keploy server to expose test apis",
		Run: func(cmd *cobra.Command, args []string) {

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				s.logger.Error("failed to read the testcase path input")
				return
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					s.logger.Error("failed to get the absolute path from relative path", zap.Error(err))
					return
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				err := fmt.Errorf("could not find the test case path, please provide a valid one")
				s.logger.Error("", zap.Any("testPath", path), zap.Error(err))
				return
			} else {
				// user provided the absolute path
				s.logger.Debug("", zap.Any("testPath", path))
			}

			path += "/keploy"

			testReportPath := path + "/testReports"

			s.logger.Info("", zap.Any("keploy test and mock path", path), zap.Any("keploy testReport path", testReportPath))

			delay, err := cmd.Flags().GetUint64("delay")

			if err != nil {
				s.logger.Error("Failed to get the delay flag", zap.Error((err)))
				return
			}

			pid, err := cmd.Flags().GetUint32("pid")

			if err != nil {
				s.logger.Error("Failed to get the pid of the application", zap.Error((err)))
				return
			}

			apiTimeout, err := cmd.Flags().GetUint64("apiTimeout")
			if err != nil {
				s.logger.Error("Failed to get the apiTimeout flag", zap.Error((err)))
			}

			port, err := cmd.Flags().GetUint32("port")

			if err != nil {
				s.logger.Error("Failed to get the port of keploy server", zap.Error((err)))
				return
			}

			language, err := cmd.Flags().GetString("language")
			if err != nil {
				s.logger.Error("failed to read the programming language")
				return
			}

			ports, err := cmd.Flags().GetUintSlice("passThroughPorts")
			if err != nil {
				s.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return
			}
			// for _, v := range ports {

			// }

			s.logger.Debug("the ports are", zap.Any("ports", ports))

			s.server.Serve(path, testReportPath, delay, pid, port, language, ports, apiTimeout)
		},
	}

	serveCmd.Flags().Uint32("pid", 0, "Process id of your application.")
	serveCmd.MarkFlagRequired("pid")

	serveCmd.Flags().Uint32("port", 6789, "Port at which you want to run graphql Server")

	serveCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")
	serveCmd.MarkFlagRequired("path")

	serveCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
	serveCmd.MarkFlagRequired("delay")

	serveCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	serveCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	serveCmd.Flags().StringP("language", "l", "", "application programming language")
	serveCmd.MarkFlagRequired("language")

	serveCmd.Hidden = true

	return serveCmd
}
