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
		Run: func(cmd *cobra.Command, args []string) {
			server.Server(version, kServices, conf, logger)
			// server.Server(version)
		},
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
