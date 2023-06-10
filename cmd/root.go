package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"go.keploy.io/server/pkg/platform/yaml"
)

type Root struct {
	logger *zap.Logger
	// subCommands holds a list of registered plugins.
	subCommands []Plugins
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
		logger:      logger,
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

	rootCmd.PersistentFlags().String("log-file", "keploy.log",
		"The location of the file where logs would be stored")

	r.RegisterPlugin(NewCmdRecordSkeleton(r.logger), NewCmdTestSkeleton(r.logger))

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

	// UpdateFieldsFromUserDefinedFlags should update the fields of the plugin based on user flags.
	// The plugins are initialised before the parsing of the flags, but are executed after the flags are parsed.
	// Hence, it is the user's responsibility to invoke this function at the beginning of the Run function.
	UpdateFieldsFromUserDefinedFlags(cmd *cobra.Command)
}

// RegisterPlugin registers a plugin by appending it to the list of subCommands.
func (r *Root) RegisterPlugin(p ...Plugins) {
	r.subCommands = append(r.subCommands, p...)
}

// CreateLoggerFromFlags function should only be invoked from Command.Run of child commands.
// Use it to overwrite the logger based on user inputs.
// Invoking it beforehand will not respect the user defined flags.
func CreateLoggerFromFlags(cmd *cobra.Command) *zap.Logger {
	// Figure out if the user explicitly asked to redirect the logs to a file.
	shouldRedirectLogsToFile := cmd.Flags().Changed("log-file")
	logCfg := zap.NewDevelopmentConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	if shouldRedirectLogsToFile {
		logLocation, err := cmd.Flags().GetString("log-file")
		if err != nil {
			log.Panic("Failed to read --log-file flag ", err)
		}

		// If the file does not exist, zap would create it for us.
		// If the file exists, logs would be appended to the file.
		// We just need to ensure that the directory exists.

		// Remove the filename to get the path for the directory
		directoryPath := filepath.Dir(logLocation)

		// Create the nested directories if they don't exist.
		if err := os.MkdirAll(directoryPath, os.ModePerm); err != nil {
			log.Panic("Failed to create file to store logs ", err)
		}

		logCfg.OutputPaths = []string{logLocation}
	}

	logger, err := logCfg.Build()
	if err != nil {
		log.Panic("Failed to start the logger from the CLI flags")
		return nil
	}
	return logger
}

func ReadTCS(logger *zap.Logger) *cobra.Command {
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
			tcs, mocks, err := ys.Read(nil)
			fmt.Println("no of tsc:", len(tcs), "tcs: ", tcs)
			fmt.Println("mocks: ", mocks)
			// r.recorder.CaptureTraffic(tcsPath, mockPath, pid)

			// server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}
	return recordCmd
}
