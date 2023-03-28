package keploycli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.keploy.io/server/config"
	"go.keploy.io/server/pkg/service"
	"go.keploy.io/server/server"
	"go.uber.org/zap"
)

// Global context for CLI application
// var ctx = context.Background()

func CLI(version string, conf *config.Config, kServices *service.KServices, logger *zap.Logger) {
	// Root command
	var rootCmd = &cobra.Command{
		Use:   "keploy",
		Short: "Keploy CLI",
	}

	// start the keploy server
	var startKeploy = &cobra.Command{
		Use:   "start [port]",
		Short: "run the keploy API server",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 1 && args[0] != "" {
				conf.Port = args[0]
			}
			server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	// cmd for running the SDK recorded tests
	var test = &cobra.Command{
		// Use:   "test tcsPath mockPath host",
		Use:   "test",
		Short: "run the keploy tests",
		// Args:  cobra.ExactArgs(2),
		// Run: func(cmd *cobra.Command, args []string) {
		// 	// name := args[0]
		// 	fmt.Printf("testing through keploy, %s %s!\n", args[0], args[1])

		// },
	}

	var testExportedTcs = &cobra.Command{
		Use:   "local tcsPath mockPath host",
		Short: "run the exported yaml testcases",
		Args:  cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			tcsPath := args[0]
			if tcsPath[0] != '/' {
				path, err := filepath.Abs(tcsPath)
				if err != nil {
					logger.Error("Failed to get the absolute path from relative tcsPath", zap.Error(err))
				}
				tcsPath = path
			}
			mockPath := args[1]
			if mockPath[0] != '/' {
				path, err := filepath.Abs(mockPath)
				if err != nil {
					logger.Error("Failed to get the absolute path from relative mockPath", zap.Error(err))
				}
				mockPath = path
			}

			testRun(testInput{
				ctx:        cmd.Context(),
				conf:       conf,
				testExport: conf.EnableTestExport,
				appId:      "",
				tcsPath:    tcsPath,
				mockPath:   mockPath,
				host:       args[2],
				kServices:  kServices,
				logger:     logger,
			})
		},
	}

	var testMongoTcs = &cobra.Command{
		Use:   "mongo appId host",
		Short: "run the testcases from MongoDB",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			testRun(testInput{
				ctx:        cmd.Context(),
				conf:       conf,
				testExport: conf.EnableTestExport,
				appId:      args[0],
				tcsPath:    "",
				mockPath:   "",
				host:       args[1],
				kServices:  kServices,
				logger:     logger,
			})
		},
	}
	test.AddCommand(testExportedTcs)
	test.AddCommand(testMongoTcs)

	rootCmd.AddCommand(startKeploy)
	rootCmd.AddCommand(test)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
