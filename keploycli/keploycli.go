package keploycli

import (
	"fmt"
	"os"

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

	rootCmd.AddCommand(startKeploy)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
