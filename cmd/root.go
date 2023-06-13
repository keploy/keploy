package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.uber.org/zap"
)

type Root struct {
	logger *zap.Logger
	// subCommands holds a list of registered plugins.
	subCommands  []Plugins
}

func newRoot() *Root {
	// logger init
	logCfg := zap.NewDevelopmentConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger, err := logCfg.Build()
	if err != nil {
		log.Panic("failed to start the logger for the CLI")
		return nil
	}

	return &Root{
		logger: logger,
		subCommands: []Plugins{},
	}
}

// Execute adds all child commands to the root command.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	newRoot().execute()
}

// execute creates a root command for Cobra. The root cmd will be executed after attaching the subcommmands. 
func (r *Root) execute() {
	// Root command
	var rootCmd = &cobra.Command{
		Use:   "keploy",
		Short: "Keploy CLI",
	}
	// rootCmd.Flags().IntP("pid", "", 0, "Please enter the process id on which your application is running.")


	r.subCommands = append(r.subCommands, NewCmdRecord(r.logger), NewCmdTest(r.logger))

	// add the registered keploy plugins as subcommands to the rootCmd
	for _, sc := range r.subCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}
	rootCmd.AddCommand(ReadTCS(r.logger))

	if err := rootCmd.Execute(); err != nil {
		r.logger.Error("failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}

// Plugins is an interface used to define plugins.
type Plugins interface {
	GetCmd() *cobra.Command
}

// RegisterPlugin registers a plugin by appending it to the list of subCommands.
func (r *Root)RegisterPlugin(p Plugins) {
	r.subCommands = append(r.subCommands, p)
}

func ReadTCS (logger *zap.Logger) *cobra.Command{
	var recordCmd = &cobra.Command{
		Use:   "read",
		Short: "record the keploy testcases from the API calls",
		Run: func(cmd *cobra.Command, args []string) {




			// pid, _ := cmd.Flags().GetUint32("pid")
			// path, err := cmd.Flags().GetString("path")
			// if err!=nil {
			// 	logger.Error("failed to read the testcase path input")
			// 	return
			// }

			// if path == "" {
				path, err := os.Getwd()
				if err != nil {
					logger.Error("failed to get the path of current directory", zap.Error(err))
					return
				}
			// }
			path += "/Keploy"
			tcsPath := path + "/tests"
			mockPath := path + "/mocks"

			ys := yaml.NewYamlStore(tcsPath, mockPath, logger)
			tcs, err := ys.Read(nil)
			fmt.Println("no of tsc:", len(tcs), "tcs: ", tcs)
			// fmt.Println("mocks: ", mocks)
			// r.recorder.CaptureTraffic(tcsPath, mockPath, pid)

			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}
	return recordCmd
}