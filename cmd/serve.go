package cmd

import (
	"os"
	"path/filepath"
	"strconv"

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
				cdirPath, err := os.Getwd()
				if err != nil {
					s.logger.Error("failed to get the path of current directory", zap.Error(err))
				}
				path = cdirPath
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

			buildDelay, err := cmd.Flags().GetUint64("buildDelay")

			if err != nil {
				s.logger.Error("Failed to get the build-delay flag", zap.Error((err)))
				return
			}

			if buildDelay == 0 {
				buildDelay, _ = strconv.ParseUint(cmd.Flags().Lookup("buildDelay").DefValue, 10, 64)
				s.logger.Debug("the buildDelay set to default value", zap.Any("buildDelay", buildDelay))
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
			appCmd, err := cmd.Flags().GetString("command")

			if err != nil {
				s.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
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

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				s.logger.Error("failed to read the proxy port")
				return
			}
			s.logger.Debug("the ports are", zap.Any("ports", ports))

			s.server.Serve(path, proxyPort, testReportPath, delay, buildDelay, pid, port, language, ports, apiTimeout, appCmd)
		},
	}

	serveCmd.Flags().Uint32("pid", 0, "Process id of your application.")

	serveCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")

	serveCmd.Flags().Uint32("port", 6789, "Port at which you want to run graphql Server")

	serveCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")

	serveCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
	serveCmd.MarkFlagRequired("delay")

	serveCmd.Flags().Uint64P("buildDelay", "", 30, "User provided time to wait docker container build")

	serveCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	serveCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	serveCmd.Flags().StringP("language", "l", "", "application programming language")
	serveCmd.Flags().StringP("command", "c", "", "Command to start the user application")
	serveCmd.MarkFlagRequired("command")

	serveCmd.Hidden = true

	return serveCmd
}
