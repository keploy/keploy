// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string

var gradientColors = []string{
	"\033[38;5;202m", // Red
	"\033[38;5;202m", // Orange-Red
	"\033[38;5;208m", // Orange
	"\033[38;5;214m", // Yellow-Orange
	"\033[38;5;220m", // Yellow
}

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func main() {
	printLogo()
	ctx := utils.NewCtx()
	start(ctx)
}

func printLogo() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	if binaryToDocker := os.Getenv("BINARY_TO_DOCKER"); binaryToDocker != "true" {
		const reset = "\033[0m"

		// Print each line of the logo with a different color from the gradient
		lines := strings.Split(logo, "\n")
		for i, line := range lines {
			color := gradientColors[i%len(gradientColors)]
			fmt.Println(color, line, reset)
		}

		fmt.Printf("version: %v\n\n", version)
	}
}

func start(ctx context.Context) {
	logger, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	defer utils.DeleteLogs(logger)
	defer utils.Recover(logger)
	configDb := configdb.NewConfigDb(logger)
	if dsn != "" {
		utils.SentryInit(logger, dsn)
		//logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	conf := config.New()
	svcProvider := provider.NewServiceProvider(logger, configDb, conf)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
